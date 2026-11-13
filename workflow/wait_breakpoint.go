package workflow

import (
	"context"
	"errors"
	"time"
)

// rewatchDelay is how long WaitForBreakpoint waits before re-watching after
// the store closed its watch (node restart, consumer lost, slow reader).
const rewatchDelay = 200 * time.Millisecond

var errWatchEnded = errors.New("workflow: DAG watch ended")

// WaitForBreakpoint blocks until a step selected by m is stopped at m's
// position and returns that breakpoint. If one is already stopped it returns
// at once. When several are stopped it returns the first in
// ComputeBreakpoints order (step ID, before position first); to wait for each
// of several steps, call it once per step.
//
// "Stopped" is the computed gate, the same one the scheduler uses, never the
// advisory blocked mark: a before breakpoint is stopped when the step is
// pending, its deps are satisfied and its breakpoint is armed and not
// released; an after breakpoint is stopped when the step is done and its
// armed breakpoint holds its dependents. A matcher whose step never gets an
// armed breakpoint at that position never matches.
//
// It follows the DAG through StateStore.WatchDAG, so it reacts to changes as
// they are committed instead of polling, and it re-watches (with a fresh
// snapshot) if the watch ends, which makes it safe across scheduler, worker
// and NATS node restarts.
//
// Errors: the matcher's validation error (wrapping ErrInvalidMatcher);
// ErrDAGNotFound if the DAG does not exist when the watch syncs or is deleted
// while waiting; ErrDAGTerminal if the DAG is or becomes done, failed or
// canceled; ctx.Err() when ctx ends.
func WaitForBreakpoint(ctx context.Context, wf *Workflow, dagID string, m BPMatcher) (BreakpointInfo, error) {
	if err := m.Validate(); err != nil {
		return BreakpointInfo{}, err
	}
	for {
		info, err := waitForBreakpointOnce(ctx, wf, dagID, m)
		if !errors.Is(err, errWatchEnded) {
			return info, err
		}
		select {
		case <-ctx.Done():
			return BreakpointInfo{}, ctx.Err()
		case <-time.After(rewatchDelay):
		}
	}
}

// waitForBreakpointOnce runs one watch. It returns errWatchEnded if the
// store closed the watch while ctx is still alive.
func waitForBreakpointOnce(ctx context.Context, wf *Workflow, dagID string, m BPMatcher) (BreakpointInfo, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := wf.Store.WatchDAG(watchCtx, dagID)
	if err != nil {
		return BreakpointInfo{}, err
	}

	var meta *DAGMeta
	steps := map[string]StepRecord{}
	synced := false
	for {
		var c DAGChange
		var ok bool
		select {
		case <-ctx.Done():
			return BreakpointInfo{}, ctx.Err()
		case c, ok = <-ch:
		}
		if !ok {
			if ctx.Err() != nil {
				return BreakpointInfo{}, ctx.Err()
			}
			return BreakpointInfo{}, errWatchEnded
		}
		switch {
		case c.Synced:
			synced = true
		case c.Meta != nil:
			meta = c.Meta
		case c.Step != nil:
			steps[c.Step.StepID] = *c.Step
		case c.Deleted == DeletedMeta:
			meta = nil
		case c.Deleted != "":
			delete(steps, c.Deleted)
		}
		if !synced {
			continue
		}
		if info, done, err := evalWaitForBreakpoint(meta, steps, m); done {
			return info, err
		}
	}
}

// evalWaitForBreakpoint decides one WaitForBreakpoint step from a consistent
// view of the DAG: done reports whether the wait is over (with info or err).
func evalWaitForBreakpoint(meta *DAGMeta, steps map[string]StepRecord, m BPMatcher) (BreakpointInfo, bool, error) {
	if meta == nil {
		return BreakpointInfo{}, true, ErrDAGNotFound
	}
	switch meta.Status {
	case DAGStatusDone, DAGStatusFailed, DAGStatusCanceled:
		return BreakpointInfo{}, true, ErrDAGTerminal
	}
	recs := make([]StepRecord, 0, len(steps))
	for _, rec := range steps {
		recs = append(recs, rec)
	}
	for _, info := range ComputeBreakpoints(*meta, recs) {
		if info.State == BPStateBlocked && info.Position == m.Position && m.Matches(steps[info.StepID]) {
			return info, true, nil
		}
	}
	return BreakpointInfo{}, false, nil
}
