package workflow

import (
	"context"
	"testing"
	"time"
)

func TestDeleteDAG_RemovesAllRecords(t *testing.T) {
	store := NewMemStore()
	bus := NewMemBus()
	enq := &captureEnq{}
	wf := NewWorkflow(store, bus, enq)

	ctx := context.Background()
	dagID := "dag-del-1"
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusDone, CreatedAt: time.Now().UTC()}, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := store.PutStep(ctx, dagID, id, StepRecord{DAGID: dagID, StepID: id, Status: StatusDone}, 0); err != nil {
			t.Fatal(err)
		}
		if err := store.PutResult(ctx, dagID, id, []byte(`"ok"`)); err != nil {
			t.Fatal(err)
		}
	}

	if err := DeleteDAG(ctx, wf, dagID); err != nil {
		t.Fatalf("DeleteDAG: %v", err)
	}

	if _, _, err := store.GetMeta(ctx, dagID); err != ErrDAGNotFound {
		t.Errorf("meta still present: err=%v", err)
	}
	steps, err := store.ListSteps(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Errorf("steps still present: %d", len(steps))
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.GetResult(ctx, dagID, id); err != ErrStepNotFound {
			t.Errorf("result %s still present: err=%v", id, err)
		}
	}
}

func TestDeleteDAG_Idempotent(t *testing.T) {
	store := NewMemStore()
	bus := NewMemBus()
	enq := &captureEnq{}
	wf := NewWorkflow(store, bus, enq)

	ctx := context.Background()
	if err := DeleteDAG(ctx, wf, "nonexistent"); err != nil {
		t.Errorf("deleting nonexistent DAG should be nil, got %v", err)
	}
}
