package workflow

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// raceStore wraps a StateStore and blocks the first GetResult call (which the
// scheduler makes inside snapshotUpstream, AFTER it loaded DAG state and AFTER
// the enqueueReady status gate passed on the stale in-memory meta). This opens
// a deterministic window to run Pause() mid-handleEvent.
type raceStore struct {
	StateStore
	inWindow chan struct{} // signaled when scheduler enters the window
	unblock  chan struct{} // closed by the test to let the scheduler proceed
	fired    bool
}

func (r *raceStore) GetResult(ctx context.Context, dagID, stepID string) ([]byte, error) {
	if !r.fired {
		r.fired = true
		close(r.inWindow)
		<-r.unblock
	}
	return r.StateStore.GetResult(ctx, dagID, stepID)
}

// TestPauseFence_LostPauseRace is the regression test for review Blocking
// Issue #1 (adapted from the reviewer's PoC). A Pause() that lands between the
// scheduler's loadState and its enqueue of the next ready step must NOT be
// lost: step-level fencing (Held flag + persistStatus refusal) blocks the
// stale enqueue.
func TestPauseFence_LostPauseRace(t *testing.T) {
	rs := &raceStore{
		StateStore: NewMemStore(),
		inWindow:   make(chan struct{}),
		unblock:    make(chan struct{}),
	}
	enq := &captureEnq{}
	wf := NewWorkflow(rs, NewMemBus(), enq)
	dag := New()
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, nil, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second) // root a enqueued

	// Worker finishes step a: hook persists Done + result, then publishes.
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)

	select {
	case <-rs.inWindow:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler never reached snapshotUpstream")
	}

	// Scheduler is between loadState (saw running) and enqueue of b.
	// a=done, b=pending -> no in-flight -> Pause holds b and goes direct to paused.
	if err := Pause(context.Background(), wf, dag.ID()); err != nil {
		t.Fatalf("Pause() error: %v", err)
	}
	meta, _, _ := wf.Store.GetMeta(context.Background(), dag.ID())
	if meta.Status != DAGStatusPaused {
		t.Fatalf("precondition: expected paused, got %s", meta.Status)
	}

	close(rs.unblock)
	time.Sleep(200 * time.Millisecond)

	// The pause must be honored: meta stays paused, b is NOT enqueued and
	// NOT running.
	meta, _, _ = wf.Store.GetMeta(context.Background(), dag.ID())
	stepB, _, _ := wf.Store.GetStep(context.Background(), dag.ID(), "b")
	if meta.Status != DAGStatusPaused {
		t.Errorf("meta should remain paused, got %s", meta.Status)
	}
	if stepB.Status != StatusPending {
		t.Errorf("step b should remain pending, got %s", stepB.Status)
	}
	if !stepB.Held {
		t.Error("step b should be held")
	}
	if enq.count() != 1 {
		t.Errorf("step b must not be enqueued during pause: want 1 enqueue (a only), got %d", enq.count())
	}
}

// TestPauseFence_PersistStatusRefusesHeld unit-tests the fence directly:
// persistStatus must refuse a held→running transition.
func TestPauseFence_PersistStatusRefusesHeld(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	s := &Scheduler{wf: wf}
	ctx := context.Background()
	dagID := "fence-dag"

	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPaused}, 0)
	_ = store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusPending, Held: true, ArgsJSON: json.RawMessage(`[]`),
	}, 0)

	err := s.persistStatus(ctx, dagID, "a", StatusRunning)
	if err == nil {
		t.Fatal("persistStatus must refuse held→running")
	}
	rec, _, _ := store.GetStep(ctx, dagID, "a")
	if rec.Status != StatusPending {
		t.Errorf("held step must stay pending, got %s", rec.Status)
	}
}

// TestResume_AllTerminalPaused_Finalizes is the regression test for review
// Blocking Issue #2 (adapted from the reviewer's PoC). Resuming a paused DAG
// whose steps are all terminal must finalize it (done/failed), not leave it
// stuck in running forever.
func TestResume_AllTerminalPaused_Finalizes(t *testing.T) {
	enq := &captureEnq{}
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), enq)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	dagID := "terminal-paused"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPaused, PausedAt: time.Now().UTC()}, 0)
	_ = store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusDone, ArgsJSON: json.RawMessage(`[]`),
	}, 0)
	_ = store.PutResult(ctx, dagID, "a", json.RawMessage(`1`))

	if err := Resume(ctx, wf, dagID); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, _, _ := store.GetMeta(ctx, dagID)
		if m.Status == DAGStatusDone {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _, _ := store.GetMeta(ctx, dagID)
	t.Fatalf("all-terminal resumed DAG should finalize to done; stuck in %q", m.Status)
}

// TestResume_Idempotent_RunningMeta verifies review Blocking Issue #5: a
// Resume retry after a partial failure (meta written to running but event not
// published / steps not released) must succeed — releasing remaining held
// steps and re-publishing EventResumed — instead of failing ErrDAGNotPaused.
func TestResume_Idempotent_RunningMeta(t *testing.T) {
	store := NewMemStore()
	bus := NewMemBus()
	wf := NewWorkflow(store, bus, &captureEnq{})
	ctx := context.Background()
	dagID := "idem-dag"

	// Simulate the partial-failure state: meta already running, step still held.
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0)
	_ = store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusPending, Held: true, ArgsJSON: json.RawMessage(`[]`),
	}, 0)

	var mu sync.Mutex
	var gotResumed bool
	sub, err := bus.Subscribe(ctx, "DAG.>", func(ev Event) {
		if ev.Kind == EventResumed && ev.DAGID == dagID {
			mu.Lock()
			gotResumed = true
			mu.Unlock()
		}
		if ev.Ack != nil {
			_ = ev.Ack()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()

	if err := Resume(ctx, wf, dagID); err != nil {
		t.Fatalf("idempotent Resume on running meta should succeed: %v", err)
	}

	rec, _, _ := store.GetStep(ctx, dagID, "a")
	if rec.Held {
		t.Error("held step must be released by idempotent Resume")
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	resumed := gotResumed
	mu.Unlock()
	if !resumed {
		t.Error("EventResumed must be re-published by idempotent Resume")
	}
}

// TestResume_ReappliesCascade_AfterDeps verifies the non-blocking review item
// "cascade divergence on resume": a step that failed while the DAG was paused
// has its completion event gated, so an After()-linked dependent (no Ref in
// args) never got cascade-skipped. The resume path must re-apply the cascade
// instead of running the dependent.
func TestResume_ReappliesCascade_AfterDeps(t *testing.T) {
	enq := &captureEnq{}
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), enq)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	dagID := "cascade-dag"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPaused, PausedAt: time.Now().UTC()}, 0)
	// a failed while paused (its event was gated). b depends on a via After()
	// only — empty args, Deps contains a.
	_ = store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusFailed, ErrorKind: "handler", ArgsJSON: json.RawMessage(`[]`),
	}, 0)
	_ = store.PutStep(ctx, dagID, "b", StepRecord{
		DAGID: dagID, StepID: "b", Status: StatusPending, Deps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`),
	}, 0)

	if err := Resume(ctx, wf, dagID); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, _, _ := store.GetStep(ctx, dagID, "b")
		if rec.Status == StatusSkipped {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec, _, _ := store.GetStep(ctx, dagID, "b")
	if rec.Status != StatusSkipped {
		t.Errorf("After()-linked dependent of failed step must be cascade-skipped on resume, got %s", rec.Status)
	}
	if enq.count() != 0 {
		t.Errorf("cascade-skipped step must not be enqueued, got %d enqueues", enq.count())
	}
}
