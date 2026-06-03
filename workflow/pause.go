package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pause requests a graceful pause of a running DAG. If the DAG has no in-flight
// steps, it transitions directly to paused. Otherwise it transitions to pausing
// and the scheduler auto-transitions to paused when the last in-flight step
// completes. Uses KV CAS with up to 5 retries on stale revision, consistent
// with Cancel().
//
// Returns ErrDAGNotRunning if the DAG is not in a running state (including
// already pausing/paused, done, failed, or canceled).
func Pause(ctx context.Context, wf *Workflow, dagID string) error {
	for attempt := 0; attempt < 5; attempt++ {
		meta, rev, err := wf.Store.GetMeta(ctx, dagID)
		if err != nil {
			return err
		}
		if !(&DAGState{Meta: meta}).CanPause() {
			return fmt.Errorf("ebind: cannot pause DAG %s: status is %s: %w", dagID, meta.Status, ErrDAGNotRunning)
		}
		steps, err := wf.Store.ListSteps(ctx, dagID)
		if err != nil {
			return err
		}
		state := &DAGState{Meta: meta, Steps: make(map[string]StepRecord, len(steps))}
		for _, s := range steps {
			state.Steps[s.StepID] = s
		}
		if state.HasInFlightSteps() {
			meta.Status = DAGStatusPausing
		} else {
			meta.Status = DAGStatusPaused
			meta.PausedAt = time.Now().UTC()
		}
		if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return err
		}
		return nil
	}
	return ErrStaleRevision
}

// Resume resumes a paused DAG. Transitions paused to running, then publishes a
// synthetic EventResumed on the event bus so the scheduler re-evaluates ready
// steps through its normal serialized event path. Uses KV CAS with up to 5
// retries on stale revision.
//
// Returns ErrDAGNotPaused if the DAG is not in a paused state (including
// running, done, failed, canceled, or pausing).
func Resume(ctx context.Context, wf *Workflow, dagID string) error {
	for attempt := 0; attempt < 5; attempt++ {
		meta, rev, err := wf.Store.GetMeta(ctx, dagID)
		if err != nil {
			return err
		}
		if !(&DAGState{Meta: meta}).CanResume() {
			return fmt.Errorf("ebind: cannot resume DAG %s: status is %s: %w", dagID, meta.Status, ErrDAGNotPaused)
		}
		meta.Status = DAGStatusRunning
		if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return err
		}
		// Publish resume event for scheduler to re-evaluate ready steps.
		// Uses dagID as StepID to ensure a valid NATS subject (empty StepID
		// would produce an invalid trailing-dot subject).
		ev := Event{Kind: EventResumed, DAGID: dagID, StepID: dagID}
		data, marshalErr := MarshalEvent(ev)
		if marshalErr != nil {
			return marshalErr
		}
		return wf.Bus.Publish(ctx, EventSubject(ev), data)
	}
	return ErrStaleRevision
}
