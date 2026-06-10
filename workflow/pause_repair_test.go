package workflow

import (
	"context"
	"encoding/json"
	"testing"
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
