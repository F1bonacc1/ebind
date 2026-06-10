package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// injectOnListStore simulates a dynamic step add (workflow.FromContext) racing
// the pause fence: right after the first ListSteps returns, a new pending step
// appears in the store — invisible to that pass, visible to the next.
type injectOnListStore struct {
	StateStore
	dagID    string
	stepID   string
	injected bool
}

func (s *injectOnListStore) ListSteps(ctx context.Context, dagID string) ([]StepRecord, error) {
	steps, err := s.StateStore.ListSteps(ctx, dagID)
	if err == nil && !s.injected && dagID == s.dagID {
		s.injected = true
		_ = s.StateStore.PutStep(ctx, dagID, s.stepID, StepRecord{
			DAGID: dagID, StepID: s.stepID, Status: StatusPending, ArgsJSON: json.RawMessage(`[]`),
		}, 0)
	}
	return steps, err
}

// TestPauseFence_DynamicAddDuringFence verifies the fence converges over steps
// added while it runs: a step injected after the fence's first ListSteps pass
// must still end up held, so a paused DAG can never have an unheld pending
// step that a stale scheduler snapshot could enqueue.
func TestPauseFence_DynamicAddDuringFence(t *testing.T) {
	ctx := context.Background()
	dagID := "dyn-add-dag"
	store := &injectOnListStore{StateStore: NewMemStore(), dagID: dagID, stepID: "dyn"}
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})

	if err := store.StateStore.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.StateStore.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusPending, ArgsJSON: json.RawMessage(`[]`),
	}, 0); err != nil {
		t.Fatal(err)
	}

	if err := Pause(ctx, wf, dagID); err != nil {
		t.Fatalf("Pause() error: %v", err)
	}

	meta, _, err := store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusPaused {
		t.Fatalf("expected paused (nothing in flight), got %s", meta.Status)
	}
	for _, id := range []string{"a", "dyn"} {
		rec, _, err := store.GetStep(ctx, dagID, id)
		if err != nil {
			t.Fatalf("get step %s: %v", id, err)
		}
		if rec.Status != StatusPending {
			t.Errorf("step %s: status = %s, want pending", id, rec.Status)
		}
		if !rec.Held {
			t.Errorf("step %s must be held after Pause — dynamic adds must not escape the fence", id)
		}
		if rec.HeldAt.IsZero() {
			t.Errorf("step %s: HeldAt must be stamped when holding", id)
		}
	}
}

// TestSweep_ReleasesOrphanedHolds verifies the sweep repairs a crash between
// Pause's fence and its meta write: a stale hold under a running meta is
// released and the step is enqueued in the same sweep pass.
func TestSweep_ReleasesOrphanedHolds(t *testing.T) {
	store := NewMemStore()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, NewMemBus(), enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 20 * time.Millisecond
	wf.SweepTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dagID := "orphan-hold"
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash leftover: pending step held long ago, meta running.
	if err := store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusPending,
		Held: true, HeldAt: time.Now().UTC().Add(-10 * time.Minute),
		ArgsJSON: json.RawMessage(`[1]`),
	}, 0); err != nil {
		t.Fatal(err)
	}

	startScheduler(t, wf, ctx)
	elector.set(true) // leadership edge -> sweep

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if enq.count() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if enq.count() < 1 {
		rec, _, _ := store.GetStep(ctx, dagID, "a")
		t.Fatalf("orphaned-held step was not repaired+enqueued by sweep (held=%v status=%s)", rec.Held, rec.Status)
	}
	rec, _, _ := store.GetStep(ctx, dagID, "a")
	if rec.Held {
		t.Error("hold must be released by the sweep repair")
	}
}

// TestSweep_KeepsFreshHolds verifies the age gate: a hold placed moments ago
// (an in-progress Pause) is NOT released by the sweep — releasing it would
// reopen the lost-pause race.
func TestSweep_KeepsFreshHolds(t *testing.T) {
	store := NewMemStore()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, NewMemBus(), enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 20 * time.Millisecond
	wf.SweepTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dagID := "fresh-hold"
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusPending,
		Held: true, HeldAt: time.Now().UTC(),
		ArgsJSON: json.RawMessage(`[1]`),
	}, 0); err != nil {
		t.Fatal(err)
	}

	startScheduler(t, wf, ctx)
	elector.set(true)

	time.Sleep(300 * time.Millisecond) // several sweep ticks
	rec, _, _ := store.GetStep(ctx, dagID, "a")
	if !rec.Held {
		t.Error("fresh hold must NOT be released by the sweep")
	}
	if got := enq.count(); got != 0 {
		t.Errorf("held step must not be enqueued, got %d enqueues", got)
	}
}
