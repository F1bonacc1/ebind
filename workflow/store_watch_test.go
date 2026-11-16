package workflow

import (
	"context"
	"testing"
	"time"
)

// runWatchDAGContract pins the StateStore.WatchDAG behavior every store must
// provide. It is part of runStoreContract and also runs against NatsStore.
func runWatchDAGContract(t *testing.T, newStore func(t *testing.T) StateStore) {
	recv := func(t *testing.T, ch <-chan DAGChange) DAGChange {
		t.Helper()
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatal("watch channel closed early")
			}
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("no change within 5s")
		}
		return DAGChange{}
	}
	// snapshot reads changes up to and including Synced.
	snapshot := func(t *testing.T, ch <-chan DAGChange) (meta *DAGMeta, steps map[string]StepRecord) {
		t.Helper()
		steps = map[string]StepRecord{}
		for {
			c := recv(t, ch)
			switch {
			case c.Synced:
				return meta, steps
			case c.Meta != nil:
				meta = c.Meta
			case c.Step != nil:
				steps[c.Step.StepID] = *c.Step
			case c.Deleted == DeletedMeta:
				meta = nil
			case c.Deleted != "":
				delete(steps, c.Deleted)
			}
		}
	}

	t.Run("WatchDAG_SnapshotThenSynced", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := s.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusRunning}, 0); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a", "b"} {
			if _, err := s.PutStep(ctx, "d", id, StepRecord{DAGID: "d", StepID: id, Status: StatusPending}, 0); err != nil {
				t.Fatal(err)
			}
		}
		// Another DAG, including one whose ID extends "d", must not leak in.
		_ = s.PutMeta(ctx, "d2", DAGMeta{ID: "d2"}, 0)
		_, _ = s.PutStep(ctx, "d2", "a", StepRecord{DAGID: "d2", StepID: "a"}, 0)

		ch, err := s.WatchDAG(ctx, "d")
		if err != nil {
			t.Fatal(err)
		}
		meta, steps := snapshot(t, ch)
		if meta == nil || meta.ID != "d" {
			t.Fatalf("snapshot meta = %+v, want d", meta)
		}
		if len(steps) != 2 || steps["a"].DAGID != "d" || steps["b"].DAGID != "d" {
			t.Fatalf("snapshot steps = %+v, want a and b of d", steps)
		}
	})

	t.Run("WatchDAG_EmptyDAGSyncsImmediately", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := s.WatchDAG(ctx, "ghost")
		if err != nil {
			t.Fatal(err)
		}
		meta, steps := snapshot(t, ch)
		if meta != nil || len(steps) != 0 {
			t.Fatalf("empty DAG snapshot = %+v %+v", meta, steps)
		}
	})

	t.Run("WatchDAG_LivePutsAndDeletes", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := s.WatchDAG(ctx, "d")
		if err != nil {
			t.Fatal(err)
		}
		snapshot(t, ch)

		if err := s.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusRunning}, 0); err != nil {
			t.Fatal(err)
		}
		if c := recv(t, ch); c.Meta == nil || c.Meta.Status != DAGStatusRunning {
			t.Fatalf("meta put: got %+v", c)
		}
		rev, err := s.PutStep(ctx, "d", "a", StepRecord{DAGID: "d", StepID: "a", Status: StatusPending}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if c := recv(t, ch); c.Step == nil || c.Step.StepID != "a" || c.Step.Status != StatusPending {
			t.Fatalf("step create: got %+v", c)
		}
		if _, err := s.PutStep(ctx, "d", "a", StepRecord{DAGID: "d", StepID: "a", Status: StatusRunning}, rev); err != nil {
			t.Fatal(err)
		}
		if c := recv(t, ch); c.Step == nil || c.Step.Status != StatusRunning {
			t.Fatalf("step update: got %+v", c)
		}
		// Writes to results and signals of the DAG are not part of the watch.
		_ = s.PutResult(ctx, "d", "a", []byte(`1`))
		_ = s.PutSignal(ctx, "d", SignalRecord{Name: "go"})
		if err := s.DeleteStep(ctx, "d", "a"); err != nil {
			t.Fatal(err)
		}
		if c := recv(t, ch); c.Deleted != "a" {
			t.Fatalf("step delete: got %+v, want Deleted=a", c)
		}
		if err := s.DeleteMeta(ctx, "d"); err != nil {
			t.Fatal(err)
		}
		if c := recv(t, ch); c.Deleted != DeletedMeta {
			t.Fatalf("meta delete: got %+v, want Deleted=DeletedMeta", c)
		}
	})

	t.Run("WatchDAG_CancelCloses", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		ch, err := s.WatchDAG(ctx, "d")
		if err != nil {
			t.Fatal(err)
		}
		snapshot(t, ch)
		cancel()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-deadline:
				t.Fatal("channel not closed after cancel")
			}
		}
	})
}

func TestNatsStore_WatchDAGContract(t *testing.T) {
	runWatchDAGContract(t, func(t *testing.T) StateStore { return newTestNatsStore(t) })
}

// A subscriber that stops reading must never block writers: MemStore closes
// its channel once the buffer is full.
func TestMemStore_WatchDAG_SlowSubscriberDoesNotBlockWriters(t *testing.T) {
	s := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.WatchDAG(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var rev uint64
		for i := 0; i < memDAGWatchBuffer*2; i++ {
			r, err := s.PutStep(ctx, "d", "a", StepRecord{DAGID: "d", StepID: "a"}, rev)
			if err != nil {
				t.Error(err)
				return
			}
			rev = r
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer blocked on a slow WatchDAG subscriber")
	}
	n := 0
	for range ch {
		n++
	}
	if n > memDAGWatchBuffer+2 {
		t.Errorf("received %d changes, more than the buffer allows", n)
	}
}
