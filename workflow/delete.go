package workflow

import (
	"context"
	"errors"
	"fmt"
)

// DeleteDAG removes all KV records for a DAG: each step, each result, each
// delivered signal, and the meta. Safe to call against a non-existent DAG
// (returns ErrDAGNotFound if meta is missing — but only after step/result
// cleanup has been attempted).
//
// Signal cleanup matters beyond hygiene: a DAG re-created with the same pinned
// WithDAGID would otherwise inherit the old run's delivered signals.
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
	sigs, err := wf.Store.ListSignals(ctx, dagID)
	if err != nil {
		return fmt.Errorf("workflow: list signals: %w", err)
	}
	for _, sig := range sigs {
		if err := wf.Store.DeleteSignal(ctx, dagID, sig.Name); err != nil {
			return fmt.Errorf("workflow: delete signal %s: %w", sig.Name, err)
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
