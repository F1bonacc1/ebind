package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/workflow"
)

// TestIntegration_FailedStep_PersistsErrorMessage verifies end-to-end (real
// in-process NATS, real worker + scheduler) that a failed DAG step's error
// message is persisted into the durable step record — not only the error kind,
// and independently of the DLQ. This is what lets `ebctl dag step get` surface
// the failure reason. Reuses setup() and hFailMandatory from integration_test.go.
func TestIntegration_FailedStep_PersistsErrorMessage(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hFailMandatory)

	dag := workflow.New(workflow.WithRetry(task.NoRetryPolicy()))
	a := dag.Step("a", hFailMandatory)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.Await[int](ctx, h.wf, dag.ID(), a); !errors.Is(err, workflow.ErrStepFailed) {
		t.Fatalf("want ErrStepFailed, got %v", err)
	}

	// By the time Await reports the failure, the same CAS write that set
	// Status=Failed also stored the message, so GetStep must see it.
	rec, _, err := h.wf.Store.GetStep(ctx, dag.ID(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != workflow.StatusFailed {
		t.Fatalf("status = %s, want failed", rec.Status)
	}
	if rec.ErrorKind == "" {
		t.Errorf("ErrorKind empty, want it set")
	}
	if !strings.Contains(rec.ErrorMessage, "boom") {
		t.Errorf("ErrorMessage = %q, want it to contain the handler error %q", rec.ErrorMessage, "boom")
	}
}
