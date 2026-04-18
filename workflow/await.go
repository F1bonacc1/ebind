package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Await blocks until the target step's result is available in the store (or the
// step has terminally failed/been skipped). Decodes the result into out.
// Usage: result, err := workflow.Await[Profile](ctx, wf, dagID, stepRef)
//
// Returns:
//   - ErrStepFailed if the step ended in status=failed
//   - ErrStepSkipped if the step was skipped (upstream cascade)
//   - context error if ctx expires before resolution
//
// Await is a thin wrapper over AwaitByID; prefer AwaitByID when the caller
// does not own the *Step handle (e.g., a different process resuming a DAG).
func Await[T any](ctx context.Context, wf *Workflow, dagID string, step *Step) (T, error) {
	return AwaitByID[T](ctx, wf, dagID, step.id)
}

// AwaitByID is the stateless variant of Await: it takes the step ID as a string
// so a process that did not build the DAG can still wait on a specific step's
// result. Use this to resume from a different instance — persist the
// (dagID, stepID) pair from the submitter and call AwaitByID from the resumer.
//
// Semantics are identical to Await.
func AwaitByID[T any](ctx context.Context, wf *Workflow, dagID, stepID string) (T, error) {
	var zero T

	// Subscribe to the result BEFORE checking status to avoid a race where the
	// step transitions to Done between our read and our watch. NatsStore's
	// WatchResult uses IncludeHistory(), so if the result was written before we
	// subscribed, we still receive it as the initial value.
	ch, err := wf.Store.WatchResult(ctx, dagID, stepID)
	if err != nil {
		return zero, err
	}

	// Check current status — maybe it's already terminal failed/skipped.
	rec, _, err := wf.Store.GetStep(ctx, dagID, stepID)
	if err == nil {
		switch rec.Status {
		case StatusFailed:
			return zero, fmt.Errorf("%w: %s", ErrStepFailed, rec.ErrorKind)
		case StatusSkipped:
			return zero, ErrStepSkipped
		}
	}

	// Also poll step status while waiting for result, so we notice terminal
	// failure (no result ever written) instead of hanging until ctx expires.
	statusTicker := time.NewTicker(500 * time.Millisecond)
	defer statusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case data, ok := <-ch:
			if !ok {
				return zero, ctx.Err()
			}
			var out T
			if err := json.Unmarshal(data, &out); err != nil {
				return zero, fmt.Errorf("await: unmarshal result: %w", err)
			}
			return out, nil
		case <-statusTicker.C:
			rec, _, err := wf.Store.GetStep(ctx, dagID, stepID)
			if err != nil {
				continue
			}
			switch rec.Status {
			case StatusFailed:
				return zero, fmt.Errorf("%w: %s", ErrStepFailed, rec.ErrorKind)
			case StatusSkipped:
				return zero, ErrStepSkipped
			}
		}
	}
}

// DAGInfo returns the current meta + step states for introspection. Useful in
// tests and for admin UIs.
func DAGInfo(ctx context.Context, wf *Workflow, dagID string) (DAGMeta, []StepRecord, error) {
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return DAGMeta{}, nil, err
	}
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return meta, nil, err
	}
	return meta, steps, nil
}
