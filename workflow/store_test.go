package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// runStoreContract runs the contract tests against any StateStore implementation.
// Both store_mem and store_nats must pass these.
func runStoreContract(t *testing.T, newStore func(t *testing.T) StateStore) {
	t.Run("PutMeta_CreateOnce", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		m := DAGMeta{ID: "d1", Status: DAGStatusRunning, CreatedAt: time.Now().UTC()}
		if err := s.PutMeta(ctx, "d1", m, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.PutMeta(ctx, "d1", m, 0); !errors.Is(err, ErrStaleRevision) {
			t.Errorf("second create should fail with ErrStaleRevision, got %v", err)
		}
	})

	t.Run("PutMeta_CAS", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		m := DAGMeta{ID: "d", Status: DAGStatusRunning}
		_ = s.PutMeta(ctx, "d", m, 0)
		_, rev, _ := s.GetMeta(ctx, "d")
		if err := s.PutMeta(ctx, "d", m, rev); err != nil {
			t.Errorf("valid CAS: %v", err)
		}
		if err := s.PutMeta(ctx, "d", m, rev); !errors.Is(err, ErrStaleRevision) {
			t.Errorf("stale CAS: got %v", err)
		}
	})

	t.Run("Step_CRUD", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		rec := StepRecord{DAGID: "d", StepID: "a", FnName: "X", Status: StatusPending}
		if err := s.PutStep(ctx, "d", "a", rec, 0); err != nil {
			t.Fatal(err)
		}
		got, rev, err := s.GetStep(ctx, "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		if got.FnName != "X" {
			t.Errorf("got %+v", got)
		}
		rec.Status = StatusRunning
		if err := s.PutStep(ctx, "d", "a", rec, rev); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("Step_StaleCAS", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		rec := StepRecord{DAGID: "d", StepID: "a", Status: StatusPending}
		_ = s.PutStep(ctx, "d", "a", rec, 0)
		if err := s.PutStep(ctx, "d", "a", rec, 999); !errors.Is(err, ErrStaleRevision) {
			t.Errorf("stale CAS: %v", err)
		}
	})

	t.Run("ListSteps", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		_ = s.PutStep(ctx, "d", "a", StepRecord{StepID: "a"}, 0)
		_ = s.PutStep(ctx, "d", "b", StepRecord{StepID: "b"}, 0)
		steps, err := s.ListSteps(ctx, "d")
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 2 {
			t.Errorf("got %d steps", len(steps))
		}
	})

	t.Run("Result_PutGet", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		payload := json.RawMessage(`{"hello":"world"}`)
		if err := s.PutResult(ctx, "d", "a", payload); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetResult(ctx, "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(payload) {
			t.Errorf("got %s", got)
		}
	})

	t.Run("WatchResult_Immediate", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		_ = s.PutResult(ctx, "d", "a", []byte(`42`))
		ch, err := s.WatchResult(ctx, "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		select {
		case data := <-ch:
			if string(data) != `42` {
				t.Errorf("got %s", data)
			}
		case <-time.After(time.Second):
			t.Fatal("watch did not fire immediately when value exists")
		}
	})

	t.Run("WatchResult_Async", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ch, err := s.WatchResult(ctx, "d", "a")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = s.PutResult(ctx, "d", "a", []byte(`"x"`))
		}()
		select {
		case data := <-ch:
			if string(data) != `"x"` {
				t.Errorf("got %s", data)
			}
		case <-ctx.Done():
			t.Fatal("watch timed out")
		}
	})

	t.Run("Delete_Meta_Step_Result", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		_ = s.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusDone}, 0)
		_ = s.PutStep(ctx, "d", "a", StepRecord{StepID: "a"}, 0)
		_ = s.PutResult(ctx, "d", "a", []byte(`42`))

		if err := s.DeleteResult(ctx, "d", "a"); err != nil {
			t.Fatalf("DeleteResult: %v", err)
		}
		if _, err := s.GetResult(ctx, "d", "a"); !errors.Is(err, ErrStepNotFound) {
			t.Errorf("expected ErrStepNotFound after result delete, got %v", err)
		}
		if err := s.DeleteStep(ctx, "d", "a"); err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if _, _, err := s.GetStep(ctx, "d", "a"); !errors.Is(err, ErrStepNotFound) {
			t.Errorf("expected ErrStepNotFound after step delete, got %v", err)
		}
		if err := s.DeleteMeta(ctx, "d"); err != nil {
			t.Fatalf("DeleteMeta: %v", err)
		}
		if _, _, err := s.GetMeta(ctx, "d"); !errors.Is(err, ErrDAGNotFound) {
			t.Errorf("expected ErrDAGNotFound after meta delete, got %v", err)
		}

		if err := s.DeleteMeta(ctx, "d"); err != nil {
			t.Errorf("DeleteMeta idempotent: %v", err)
		}
		if err := s.DeleteStep(ctx, "d", "a"); err != nil {
			t.Errorf("DeleteStep idempotent: %v", err)
		}
		if err := s.DeleteResult(ctx, "d", "a"); err != nil {
			t.Errorf("DeleteResult idempotent: %v", err)
		}
	})

	t.Run("ListDAGs", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		_ = s.PutMeta(ctx, "d1", DAGMeta{ID: "d1", Status: DAGStatusRunning}, 0)
		_ = s.PutMeta(ctx, "d2", DAGMeta{ID: "d2", Status: DAGStatusDone}, 0)
		dags, err := s.ListDAGs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(dags) != 2 {
			t.Errorf("got %d DAGs: %+v", len(dags), dags)
		}
	})

	t.Run("ConcurrentCAS", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		rec := StepRecord{StepID: "a", Attempt: 0}
		_ = s.PutStep(ctx, "d", "a", rec, 0)

		var wg sync.WaitGroup
		var successes, failures int
		var muCounts sync.Mutex
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, rev, _ := s.GetStep(ctx, "d", "a")
				got.Attempt++
				err := s.PutStep(ctx, "d", "a", got, rev)
				muCounts.Lock()
				if err == nil {
					successes++
				} else {
					failures++
				}
				muCounts.Unlock()
			}()
		}
		wg.Wait()
		if successes == 0 {
			t.Error("expected at least one concurrent CAS to succeed")
		}
		if successes+failures != 10 {
			t.Errorf("lost updates: %d+%d != 10", successes, failures)
		}
	})
}

func TestMemStore_Contract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) StateStore { return NewMemStore() })
}
