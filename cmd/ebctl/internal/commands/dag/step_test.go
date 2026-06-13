package dag

import (
	"context"
	"strings"
	"testing"

	"github.com/f1bonacc1/ebind/workflow"
)

func putStep(t *testing.T, store *workflow.MemStore, rec workflow.StepRecord) {
	t.Helper()
	if _, err := store.PutStep(context.Background(), rec.DAGID, rec.StepID, rec, 0); err != nil {
		t.Fatalf("put step: %v", err)
	}
}

func TestNoResultError_FailedStepShowsMessage(t *testing.T) {
	store := workflow.NewMemStore()
	wf := &workflow.Workflow{Store: store}
	putStep(t, store, workflow.StepRecord{
		DAGID:        "dag1",
		StepID:       "s1",
		Status:       workflow.StatusFailed,
		ErrorKind:    "handler",
		ErrorMessage: "dial tcp: connection refused",
	})

	err := noResultError(context.Background(), wf, "dag1", "s1")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"step failed", "handler", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q missing %q", err.Error(), want)
		}
	}
}

func TestNoResultError_StatusAware(t *testing.T) {
	store := workflow.NewMemStore()
	wf := &workflow.Workflow{Store: store}
	putStep(t, store, workflow.StepRecord{DAGID: "d", StepID: "skip", Status: workflow.StatusSkipped})
	putStep(t, store, workflow.StepRecord{DAGID: "d", StepID: "pend", Status: workflow.StatusPending})

	if err := noResultError(context.Background(), wf, "d", "skip"); err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Errorf("skip: %v", err)
	}
	if err := noResultError(context.Background(), wf, "d", "pend"); err == nil || !strings.Contains(err.Error(), "not done yet") {
		t.Errorf("pend: %v", err)
	}
}

func TestNoResultError_MissingStep(t *testing.T) {
	store := workflow.NewMemStore()
	wf := &workflow.Workflow{Store: store}
	if err := noResultError(context.Background(), wf, "nope", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing: %v", err)
	}
}
