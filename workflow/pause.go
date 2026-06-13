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
// after fencing, it transitions directly to paused. Otherwise transitions to
// pausing and the scheduler auto-transitions to paused when the last in-flight
// step completes. Uses KV CAS with up to 5 retries on stale revision.
//
// Drain semantics: an in-flight (running) step is not interrupted — it keeps
// its full retry chain, including backoff redeliveries, until it reaches a
// terminal state. Pause only prevents NEW steps from being enqueued.
//
// Crash recovery: if the caller dies between fencing and the meta write, held
// steps remain under a running meta and the DAG stalls. The scheduler sweep
// auto-releases such orphaned holds after heldOrphanAge; retrying Pause (or
// calling Resume) repairs the state immediately.
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

		inFlight, err := fencePendingSteps(ctx, wf.Store, dagID)
		if err != nil {
			return err
		}

		if inFlight {
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

// fencePendingSteps CAS-holds (Held=true) every pending step of the DAG and
// reports whether any step is in flight (running). The scheduler's
// persistStatus refuses held→running, so a held step can never be enqueued.
//
// It re-lists and repeats until a full pass changes nothing, so steps added
// dynamically while the fence is being applied get fenced too. This converges
// because a dynamic add (workflow.FromContext) is written strictly before its
// adding handler completes: a pass that observes no running steps has
// therefore also observed every add. The final stable pass decides in-flight.
//
// If the loop cannot stabilize within 5 passes (sustained concurrent writes),
// it conservatively reports in-flight so callers treat the DAG as draining —
// the next completion event or sweep retries the fence.
func fencePendingSteps(ctx context.Context, store StateStore, dagID string) (bool, error) {
	for pass := 0; pass < 5; pass++ {
		steps, err := store.ListSteps(ctx, dagID)
		if err != nil {
			return false, err
		}
		inFlight := false
		changed := false
		for _, snap := range steps {
			if snap.Status == StatusRunning {
				inFlight = true
				continue
			}
			if snap.Status != StatusPending || snap.Held {
				continue
			}
			rec, rev, err := store.GetStep(ctx, dagID, snap.StepID)
			if err != nil {
				return false, err
			}
			if rec.Status == StatusRunning {
				inFlight = true
				continue
			}
			if rec.Status != StatusPending || rec.Held {
				continue
			}
			rec.Held = true
			rec.HeldAt = time.Now().UTC()
			if _, err := store.PutStep(ctx, dagID, snap.StepID, rec, rev); err != nil {
				if errors.Is(err, ErrStaleRevision) {
					changed = true // concurrent writer; re-observe next pass
					continue
				}
				return false, err
			}
			changed = true
		}
		if !changed {
			return inFlight, nil
		}
	}
	return true, nil
}

// Resume resumes a paused (or pausing) DAG. Transitions meta to running,
// releases held steps back to pending, then publishes EventResumed so the
// scheduler re-evaluates ready steps. Uses KV CAS with up to 5 retries on
// stale revision.
//
// Idempotent: calling Resume on a DAG whose meta is already running succeeds —
// it releases any held steps and re-publishes EventResumed. This makes a retry
// after a partial failure (crash or publish error between the meta write and
// the event publish) converge instead of failing with ErrDAGNotPaused, and
// doubles as the immediate manual repair for holds orphaned by a crashed
// Pause.
//
// Returns ErrDAGNotPaused if the DAG is in a terminal state (done, failed,
// canceled) or does not exist.
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
			cur.HeldAt = time.Time{}
			if _, err := wf.Store.PutStep(ctx, dagID, rec.StepID, cur, rev); err != nil {
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
