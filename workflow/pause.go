package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pause requests a graceful pause of a running DAG. Uses step-level fencing:
// pending steps are CAS-held before writing meta, so no concurrent enqueue can
// sneak in between (Blocking Issue #1 fix). If the DAG has no in-flight steps
// after holding, it transitions directly to paused. Otherwise transitions to
// pausing and the scheduler auto-transitions to paused when the last in-flight
// step completes. Uses KV CAS with up to 5 retries on stale revision.
//
// Drain semantics: an in-flight (running) step is not interrupted — it keeps
// its full retry chain, including backoff redeliveries, until it reaches a
// terminal state. Pause only prevents NEW steps from being enqueued.
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

		// Step-level fencing: CAS-hold every pending step. If any CAS fails
		// (concurrent enqueue won the race), retry from the top.
		if err := holdPendingSteps(ctx, wf, dagID, state); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return err
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

// holdPendingSteps CAS-writes Held=true on every pending step (status=pending, not
// already terminal, not already held). Running/in-flight steps are left alone.
// If any CAS fails, the caller retries Pause from the top. The scheduler's
// persistStatus refuses held→running, so this fences against concurrent enqueue.
func holdPendingSteps(ctx context.Context, wf *Workflow, dagID string, state *DAGState) error {
	for id, step := range state.Steps {
		if step.Status != StatusPending || step.Held {
			continue
		}
		rec, rev, err := wf.Store.GetStep(ctx, dagID, id)
		if err != nil {
			return err
		}
		if rec.Status != StatusPending {
			// Started running (or finished) since our snapshot. Refresh the
			// snapshot so HasInFlightSteps sees the live status — otherwise a
			// pending→running transition in this window would let Pause go
			// direct to paused while the step is still executing.
			state.Steps[id] = rec
			continue
		}
		rec.Held = true
		if err := wf.Store.PutStep(ctx, dagID, id, rec, rev); err != nil {
			return err
		}
		state.Steps[id] = rec
	}
	return nil
}

// Resume resumes a paused (or pausing) DAG. Releases held steps back to
// pending, transitions meta to running, then publishes EventResumed so the
// scheduler re-evaluates ready steps. Uses KV CAS with up to 5 retries on
// stale revision.
//
// Idempotent: if meta is already running (e.g. prior publish failure), releases
// any held steps and re-publishes EventResumed rather than returning an error.
// This fixes the "stuck running" window described in Blocking Issue #5.
//
// Returns ErrDAGNotPaused if the DAG is not in a resumable state (running,
// done, failed, canceled).
func Resume(ctx context.Context, wf *Workflow, dagID string) error {
	for attempt := 0; attempt < 5; attempt++ {
		meta, rev, err := wf.Store.GetMeta(ctx, dagID)
		if err != nil {
			return err
		}

		// Idempotent retry: if meta is already running, release held steps
		// and re-publish EventResumed.
		if meta.Status == DAGStatusRunning {
			if err := releaseHeldSteps(ctx, wf, dagID); err != nil {
				if errors.Is(err, ErrStaleRevision) {
					continue
				}
				return err
			}
			return publishResumeEvent(ctx, wf, dagID)
		}

		if !(&DAGState{Meta: meta}).CanResume() {
			return fmt.Errorf("ebind: cannot resume DAG %s: status is %s: %w", dagID, meta.Status, ErrDAGNotPaused)
		}

		meta.Status = DAGStatusRunning
		meta.PausedAt = time.Time{}
		if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return err
		}
		// Release held steps AFTER the meta CAS: if we crash between the two,
		// a retried Resume lands in the idempotent running-branch above, which
		// releases the remaining held steps and re-publishes the event.
		if err := releaseHeldSteps(ctx, wf, dagID); err != nil {
			return err
		}
		// Publish resume event for scheduler to re-evaluate ready steps.
		return publishResumeEvent(ctx, wf, dagID)
	}
	return ErrStaleRevision
}

// releaseHeldSteps finds all held steps and CAS-writes them back to pending
// (Held=false). Used by Resume to undo the step-level fencing applied by Pause.
func releaseHeldSteps(ctx context.Context, wf *Workflow, dagID string) error {
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return err
	}
	for _, rec := range steps {
		if !rec.Held {
			continue
		}
		for attempt := 0; attempt < 5; attempt++ {
			cur, rev, err := wf.Store.GetStep(ctx, dagID, rec.StepID)
			if err != nil {
				return err
			}
			if !cur.Held {
				break // already released by concurrent writer
			}
			cur.Held = false
			if err := wf.Store.PutStep(ctx, dagID, rec.StepID, cur, rev); err != nil {
				if errors.Is(err, ErrStaleRevision) {
					continue
				}
				return err
			}
			break
		}
	}
	return nil
}

// publishResumeEvent publishes an EventResumed for the given DAG. Extracted as
// a helper for Resume's idempotent retry path.
func publishResumeEvent(ctx context.Context, wf *Workflow, dagID string) error {
	ev := Event{Kind: EventResumed, DAGID: dagID, StepID: dagID}
	data, err := MarshalEvent(ev)
	if err != nil {
		return err
	}
	return wf.Bus.Publish(ctx, EventSubject(ev), data)
}
