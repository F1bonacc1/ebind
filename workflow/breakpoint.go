package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// BreakpointInfo describes one breakpoint occurrence in a DAG. Computed view —
// see ComputeBreakpoints.
type BreakpointInfo struct {
	StepID   string     `json:"step_id"`
	Position BPPosition `json:"position"` // before | after
	Labels   []string   `json:"labels"`
	Armed    bool       `json:"armed"` // labels ∩ ActiveBreakpoints ≠ ∅
	// State is the computed lifecycle: "" (not hit), blocked (currently
	// stopping work), or released (a ResumeBreakpoint passed it).
	State        BPState   `json:"state,omitempty"`
	BlockedSince time.Time `json:"blocked_since,omitempty"` // from the advisory mark, if written
	Holding      []string  `json:"holding,omitempty"`       // after-BP: direct dependents currently gated
}

// ComputeBreakpoints is the pure projection of a DAG's breakpoints from its
// meta + step records. State is computed from the live gate predicates, not
// the advisory blocked marks, so it is correct even if a scheduler has not
// written the marks yet. Results are ordered by step ID, before-position first.
//
// A terminal DAG (done/failed/canceled) reports nothing as blocked: no work
// can be stopped anymore. Without this, a leaf step's armed after-breakpoint —
// which holds no dependents, so the DAG finalizes despite it — would read as
// "blocked" forever with no way to resume it.
func ComputeBreakpoints(meta DAGMeta, steps []StepRecord) []BreakpointInfo {
	state := &DAGState{Meta: meta, Steps: make(map[string]StepRecord, len(steps))}
	for _, s := range steps {
		state.Steps[s.StepID] = s
	}
	var blockedBefore, holdingAfter []string
	switch meta.Status {
	case DAGStatusDone, DAGStatusFailed, DAGStatusCanceled:
		// terminal: released/declared states still render, blocked never does
	default:
		blockedBefore = state.BlockedAtBefore()
		holdingAfter = state.HoldingAtAfter()
	}

	var out []BreakpointInfo
	for _, rec := range steps {
		if len(rec.BreakBefore) > 0 {
			info := BreakpointInfo{
				StepID:   rec.StepID,
				Position: BPPositionBefore,
				Labels:   rec.BreakBefore,
				Armed:    breakpointArmed(rec.BreakBefore, meta.ActiveBreakpoints),
			}
			switch {
			case rec.BPBefore == BPStateReleased:
				info.State = BPStateReleased
			case slices.Contains(blockedBefore, rec.StepID):
				info.State = BPStateBlocked
				info.BlockedSince = rec.BPBlockedAt
			}
			out = append(out, info)
		}
		if len(rec.BreakAfter) > 0 {
			info := BreakpointInfo{
				StepID:   rec.StepID,
				Position: BPPositionAfter,
				Labels:   rec.BreakAfter,
				Armed:    breakpointArmed(rec.BreakAfter, meta.ActiveBreakpoints),
			}
			switch {
			case rec.BPAfter == BPStateReleased:
				info.State = BPStateReleased
			case slices.Contains(holdingAfter, rec.StepID):
				info.State = BPStateBlocked
				info.BlockedSince = rec.BPBlockedAt
				for id, other := range state.Steps {
					if other.Status == StatusPending &&
						(slices.Contains(other.Deps, rec.StepID) || slices.Contains(other.OptionalDeps, rec.StepID)) {
						info.Holding = append(info.Holding, id)
					}
				}
				sort.Strings(info.Holding)
			}
			out = append(out, info)
		}
	}
	posRank := func(p BPPosition) int {
		if p == BPPositionBefore {
			return 0
		}
		return 1
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StepID != out[j].StepID {
			return out[i].StepID < out[j].StepID
		}
		return posRank(out[i].Position) < posRank(out[j].Position)
	})
	return out
}

// CountBlocked returns how many breakpoints in the computed infos are
// currently stopping work. Shared by Debug, ebctl, and tests so the
// "currently blocked" semantics live in one place.
func CountBlocked(infos []BreakpointInfo) int {
	n := 0
	for _, bp := range infos {
		if bp.State == BPStateBlocked {
			n++
		}
	}
	return n
}

// ListBreakpoints loads the DAG from the store and returns its breakpoints.
// Returns ErrDAGNotFound if the DAG's meta key is missing.
func ListBreakpoints(ctx context.Context, wf *Workflow, dagID string) ([]BreakpointInfo, error) {
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return nil, err
	}
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return nil, err
	}
	return ComputeBreakpoints(meta, steps), nil
}

// ResumeBreakpoint releases every step in the DAG currently blocked at a
// breakpoint carrying the given label, then publishes EventResumed so the
// scheduler re-evaluates ready steps. Debugger "continue" semantics: the label
// stays armed for the rest of the run — a later step (including dynamically
// added ones) carrying the same label blocks again, and each ResumeBreakpoint
// call releases only what is currently stopped.
//
// If the breakpoint has multiple labels, any one of them releases it. Returns
// the number of breakpoint instances released.
//
// Idempotent: a repeat call finds nothing blocked (released is monotonic),
// returns 0, and still re-publishes EventResumed — so a crash between the CAS
// writes and the publish converges on retry, mirroring Resume.
//
// A release on a paused/pausing DAG is persisted but takes effect only when
// the DAG-level Resume re-opens scheduling. Terminal DAGs return (0, nil);
// an unknown DAG returns ErrDAGNotFound.
func ResumeBreakpoint(ctx context.Context, wf *Workflow, dagID, label string) (int, error) {
	state, err := loadBPState(ctx, wf, dagID)
	if err != nil || state == nil {
		return 0, err
	}

	released := 0
	for _, id := range state.BlockedAtBefore() {
		if !slices.Contains(state.Steps[id].BreakBefore, label) {
			continue
		}
		ok, err := releaseInstance(ctx, wf, dagID, id, BPPositionBefore, []string{label})
		if err != nil {
			return released, err
		}
		if ok {
			released++
		}
	}
	for _, id := range state.HoldingAtAfter() {
		if !slices.Contains(state.Steps[id].BreakAfter, label) {
			continue
		}
		ok, err := releaseInstance(ctx, wf, dagID, id, BPPositionAfter, []string{label})
		if err != nil {
			return released, err
		}
		if ok {
			released++
		}
	}

	// Always publish, even when released==0: a retry after a crash between the
	// CAS writes above and this publish must still trigger re-evaluation.
	if err := publishResumeEvent(ctx, wf, dagID); err != nil {
		return released, err
	}
	return released, nil
}

// ReleaseStep releases one step's breakpoint at pos, if that step is stopped
// there right now, then publishes EventResumed so the scheduler re-evaluates
// ready steps. Other steps stopped at the same label stay stopped: this is
// "release this one instance", where ResumeBreakpoint is "continue everything
// stopped at this label".
//
// For BPPositionBefore the step must be pending with its deps satisfied and
// its before-breakpoint armed and not released. For BPPositionAfter the step
// must be done with its after-breakpoint armed and holding its dependents;
// releasing it lets only that step's dependents start.
//
// Returns true if this call released it. A step that is not stopped at pos
// yet (still waiting on deps, not run yet, or already released) returns
// false and is NOT pre-released: the breakpoint still stops it when it
// arrives. Like ResumeBreakpoint, every call re-publishes EventResumed, so a
// crash between the CAS write and the publish converges on retry.
//
// A release on a paused/pausing DAG is persisted but takes effect only when
// the DAG-level Resume re-opens scheduling. Terminal DAGs return (false, nil);
// an unknown DAG returns ErrDAGNotFound; an unknown step returns
// ErrStepNotFound.
func ReleaseStep(ctx context.Context, wf *Workflow, dagID, stepID string, pos BPPosition) (bool, error) {
	if pos != BPPositionBefore && pos != BPPositionAfter {
		return false, fmt.Errorf("workflow: ReleaseStep: unknown position %q (want %q or %q)", pos, BPPositionBefore, BPPositionAfter)
	}
	state, err := loadBPState(ctx, wf, dagID)
	if err != nil || state == nil {
		return false, err
	}
	rec, ok := state.Steps[stepID]
	if !ok {
		return false, fmt.Errorf("%w: %q in DAG %q", ErrStepNotFound, stepID, dagID)
	}

	var stopped bool
	var labels []string
	if pos == BPPositionBefore {
		stopped = slices.Contains(state.BlockedAtBefore(), stepID)
		labels = rec.BreakBefore
	} else {
		stopped = slices.Contains(state.HoldingAtAfter(), stepID)
		labels = rec.BreakAfter
	}

	released := false
	if stopped {
		released, err = releaseInstance(ctx, wf, dagID, stepID, pos, labels)
		if err != nil {
			return false, err
		}
	}
	if err := publishResumeEvent(ctx, wf, dagID); err != nil {
		return released, err
	}
	return released, nil
}

// loadBPState loads the DAG for a breakpoint release. It returns (nil, nil)
// for a terminal DAG, where nothing can be stopped, and ErrDAGNotFound for an
// unknown one.
func loadBPState(ctx context.Context, wf *Workflow, dagID string) (*DAGState, error) {
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return nil, err
	}
	switch meta.Status {
	case DAGStatusDone, DAGStatusFailed, DAGStatusCanceled:
		return nil, nil
	}
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return nil, err
	}
	state := &DAGState{Meta: meta, Steps: make(map[string]StepRecord, len(steps))}
	for _, s := range steps {
		state.Steps[s.StepID] = s
	}
	return state, nil
}

// releaseInstance releases one stopped breakpoint instance and, if this call
// won the CAS, announces it with an informational bp_resumed event carrying
// labels. Every release path (ResumeBreakpoint, ReleaseStep) goes through it,
// so release-time bookkeeping is added in one place. The caller publishes
// EventResumed.
func releaseInstance(ctx context.Context, wf *Workflow, dagID, stepID string, pos BPPosition, labels []string) (bool, error) {
	ok, err := releaseBP(ctx, wf, dagID, stepID, pos)
	if err != nil || !ok {
		return false, err
	}
	// Informational (see EventBPResumed): best-effort for live observers only.
	_ = publishEvent(ctx, wf.Bus, Event{Kind: EventBPResumed, DAGID: dagID,
		StepID: stepID, BPPosition: pos, BPLabels: labels})
	return true, nil
}

// releaseBP CAS-writes the released flag for one step's breakpoint position.
// Re-verifies against a fresh read each attempt: skips (false, nil) if a
// concurrent writer already released it or moved the step out of the
// position's expected status. Preserves Held — the pause fence and the
// breakpoint gate are independent and compose.
func releaseBP(ctx context.Context, wf *Workflow, dagID, stepID string, pos BPPosition) (bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		rec, rev, err := wf.Store.GetStep(ctx, dagID, stepID)
		if err != nil {
			return false, err
		}
		switch pos {
		case BPPositionBefore:
			if rec.Status != StatusPending || rec.BPBefore == BPStateReleased {
				return false, nil
			}
			rec.BPBefore = BPStateReleased
		case BPPositionAfter:
			if rec.Status != StatusDone || rec.BPAfter == BPStateReleased {
				return false, nil
			}
			rec.BPAfter = BPStateReleased
		}
		rec.BPBlockedAt = time.Time{}
		_, err = wf.Store.PutStep(ctx, dagID, stepID, rec, rev)
		if err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return false, err
		}
		return true, nil
	}
	return false, ErrStaleRevision
}
