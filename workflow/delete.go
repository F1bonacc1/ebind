package workflow

import (
	"context"
	"errors"
	"fmt"
)

// DeleteDAG removes all KV records for a DAG: each step, each result, and the meta.
// Safe to call against a non-existent DAG (returns ErrDAGNotFound if meta is missing
// — but only after step/result cleanup has been attempted).
//
// Callers that want to prevent accidental deletion of in-flight work should inspect
// DAGMeta.Status first and refuse unless it's terminal.
func DeleteDAG(ctx context.Context, wf *Workflow, dagID string) error {
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return fmt.Errorf("workflow: list steps: %w", err)
	}
	for _, s := range steps {
		if err := wf.Store.DeleteResult(ctx, dagID, s.StepID); err != nil {
			return fmt.Errorf("workflow: delete result %s: %w", s.StepID, err)
		}
		if err := wf.Store.DeleteStep(ctx, dagID, s.StepID); err != nil {
			return fmt.Errorf("workflow: delete step %s: %w", s.StepID, err)
		}
	}
	if err := wf.Store.DeleteMeta(ctx, dagID); err != nil {
		if errors.Is(err, ErrDAGNotFound) {
			return nil
		}
		return fmt.Errorf("workflow: delete meta: %w", err)
	}
	return nil
}
