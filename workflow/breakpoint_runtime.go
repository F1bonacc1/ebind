package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ArmReport is what ArmBreakpoints did. Instances are "step@position".
type ArmReport struct {
	// Added are the matcher keys newly added to the DAG's armed set; empty
	// when every matcher was already armed.
	Added []string `json:"added"`
	// Stopped are the instances that will stop: a pending step fenced
	// before it runs, a pending or running step whose dependents will be
	// held when it completes, or a done step whose pending dependents are
	// now held.
	Stopped []string `json:"stopped"`
	// Missed are the instances the matchers select but that are already past
	// the point: a step running or finished (or already released) at the
	// before position, or a done step with a dependent that already started
	// (or an after position already released). Arming never interrupts or
	// recalls work.
	Missed []string `json:"missed"`
}

// ArmBreakpoints arms matchers on a DAG that is already running (or paused):
// every step the matchers select stops at the matched position from now on,
// including steps added dynamically later. Debugger "set breakpoint" on a
// live process. The matchers join DAGMeta.ArmedMatchers, so they behave
// exactly like matchers armed at Submit: ReleaseMatching and ReleaseStep
// continue an instance and the matcher stays armed; DisarmBreakpoints
// removes it.
//
// Idempotent: a repeat returns no Added keys and reports the same instances
// again. Terminal DAGs return the zero report and nil; an unknown DAG
// returns ErrDAGNotFound; an invalid matcher returns an error wrapping
// ErrInvalidMatcher before anything is written.
//
// Every worker must run v0.10.0 or later before any DAG is armed at runtime:
// an older scheduler ignores both ArmedMatchers and BPFence and dispatches
// through the breakpoint.
//
// # Why one fence pass is enough
//
// The meta write is the truth; BPFence tokens on pending steps are the CAS
// guard persistStatus checks before pending→running. Arm commits meta at C,
// then lists steps and fences them at P > C. Every scheduler dispatch lists
// steps (at L) and then reads meta from the stream leader (at M > L) before
// it dispatches anything; Submit does the same for roots. For a step S:
//   - M > C: the scheduler sees the matcher and the pure gate stops S.
//   - M < C: then L < M < C < P, so S existed when arm listed steps. Arm
//     either fenced S (persistStatus sees the token or loses the CAS) or
//     found it already running and reported it Missed.
//
// Steps created after P are listed after P, so their dispatcher reads meta
// after C. No loop until stable is needed.
func ArmBreakpoints(ctx context.Context, wf *Workflow, dagID string, ms ...BPMatcher) (ArmReport, error) {
	ms, err := validRuntimeMatchers("ArmBreakpoints", ms)
	if err != nil {
		return ArmReport{}, err
	}

	var added []string
	terminal, err := updateArmedMatchers(ctx, wf, dagID, func(cur []BPMatcher) []BPMatcher {
		added = added[:0]
		next := slices.Clone(cur)
		for _, m := range ms {
			if !slices.ContainsFunc(next, func(x BPMatcher) bool { return x.Key() == m.Key() }) {
				next = append(next, m)
				added = append(added, m.Key())
			}
		}
		return next
	})
	if err != nil || terminal {
		return ArmReport{}, err
	}

	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return ArmReport{}, err
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].StepID < steps[j].StepID })

	var stopped, missed []string
	for _, rec := range steps {
		for _, m := range ms {
			if !m.Matches(rec) {
				continue
			}
			inst := rec.StepID + "@" + string(m.Position)
			if m.Position == BPPositionBefore {
				ok, err := addFenceToken(ctx, wf.Store, dagID, rec.StepID, beforeFenceToken(m.Key()),
					func(cur StepRecord) bool {
						return cur.Status == StatusPending && cur.BPBefore != BPStateReleased
					})
				if err != nil {
					return ArmReport{}, err
				}
				if ok {
					stopped = append(stopped, inst)
				} else {
					missed = append(missed, inst)
				}
				continue
			}
			s, mi, err := fenceAfter(ctx, wf.Store, dagID, rec, steps)
			if err != nil {
				return ArmReport{}, err
			}
			if s {
				stopped = append(stopped, inst)
			}
			if mi {
				missed = append(missed, inst)
			}
		}
	}

	// A runtime arm wakes no scheduler, so write the advisory blocked marks
	// (and their bp_hit events) for whatever is stopped right now. Best
	// effort, never load-bearing.
	if state, err := loadLatestState(ctx, wf, dagID); err == nil {
		markBlockedBreakpoints(ctx, wf, state)
	}

	return ArmReport{
		Added:   nonNil(added),
		Stopped: nonNil(dedupeStrings(stopped)),
		Missed:  nonNil(dedupeStrings(missed)),
	}, nil
}

// fenceAfter fences the after position of rec (selected by an armed
// matcher). A pending or running step needs no token: when it completes, its
// dispatcher reads meta after the step list and the gate holds its
// dependents. A done step gets an "a:" token on each pending direct
// dependent; a dependent that already started is a miss.
func fenceAfter(ctx context.Context, store StateStore, dagID string, rec StepRecord, steps []StepRecord) (stopped, missed bool, err error) {
	switch rec.Status {
	case StatusPending, StatusRunning:
		return true, false, nil
	case StatusDone:
	default:
		return false, false, nil // after gates only gate done steps
	}
	if rec.BPAfter == BPStateReleased {
		return false, true, nil
	}
	for _, dep := range steps {
		if !slices.Contains(dep.Deps, rec.StepID) && !slices.Contains(dep.OptionalDeps, rec.StepID) {
			continue
		}
		ok, err := addFenceToken(ctx, store, dagID, dep.StepID, afterFenceToken(rec.StepID),
			func(cur StepRecord) bool { return cur.Status == StatusPending })
		if err != nil {
			return false, false, err
		}
		if ok {
			stopped = true
		} else {
			missed = true
		}
	}
	return stopped, missed, nil
}

// DisarmBreakpoints removes matchers from a DAG's armed set at runtime and
// lets every step they alone were stopping continue. Unlike ReleaseMatching
// it changes what is armed: a later step the matchers would have selected
// runs straight through. It never writes the released flag, so disarming
// and then re-arming stops a step again, which a release would not.
//
// Matchers that are not armed are ignored. Labels armed by a pre-v0.9.0
// WithActiveBreakpoints (DAGMeta.ActiveBreakpoints) cannot be disarmed;
// release their stops instead.
//
// Returns how many breakpoint instances were stopped before and are not
// stopped now. Always publishes EventResumed, so a crash between the writes
// and the publish converges on retry; a crash before the token cleanup is
// repaired by the leader sweep. Terminal DAGs return (0, nil); an unknown
// DAG returns ErrDAGNotFound; an invalid matcher returns an error wrapping
// ErrInvalidMatcher before anything is written.
func DisarmBreakpoints(ctx context.Context, wf *Workflow, dagID string, ms ...BPMatcher) (int, error) {
	ms, err := validRuntimeMatchers("DisarmBreakpoints", ms)
	if err != nil {
		return 0, err
	}
	before, err := loadLatestState(ctx, wf, dagID)
	if err != nil {
		return 0, err
	}
	if before.isTerminal() {
		return 0, nil
	}
	blockedBefore := blockedInstances(before)

	terminal, err := updateArmedMatchers(ctx, wf, dagID, func(cur []BPMatcher) []BPMatcher {
		return slices.DeleteFunc(slices.Clone(cur), func(x BPMatcher) bool {
			return slices.ContainsFunc(ms, func(m BPMatcher) bool { return m.Key() == x.Key() })
		})
	})
	if err != nil || terminal {
		return 0, err
	}

	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return 0, err
	}
	for _, rec := range steps {
		if rec.Status != StatusPending || len(rec.BPFence) == 0 {
			continue
		}
		if _, err := pruneStaleFenceTokens(ctx, wf.Store, dagID, rec.StepID); err != nil {
			return 0, err
		}
	}

	released := 0
	if after, err := loadLatestState(ctx, wf, dagID); err == nil {
		stillBlocked := blockedInstances(after)
		for _, bp := range blockedBefore {
			if slices.ContainsFunc(stillBlocked, func(x BreakpointInfo) bool {
				return x.StepID == bp.StepID && x.Position == bp.Position
			}) {
				continue
			}
			released++
			// Informational (see EventBPResumed): best effort.
			_ = publishEvent(ctx, wf.Bus, Event{Kind: EventBPResumed, DAGID: dagID,
				StepID: bp.StepID, BPPosition: bp.Position, BPLabels: bp.Labels})
		}
	}

	if err := publishResumeEvent(ctx, wf, dagID); err != nil {
		return released, err
	}
	return released, nil
}

func validRuntimeMatchers(op string, ms []BPMatcher) ([]BPMatcher, error) {
	if len(ms) == 0 {
		return nil, fmt.Errorf("workflow: %s: %w: no matchers", op, ErrInvalidMatcher)
	}
	for _, m := range ms {
		if err := m.Validate(); err != nil {
			return nil, err
		}
	}
	return dedupeMatchers(ms), nil
}

// updateArmedMatchers CAS-rewrites meta.ArmedMatchers with fn, reading meta
// from the leader each attempt. Reports true (nothing written) for a
// done/failed/canceled DAG.
func updateArmedMatchers(ctx context.Context, wf *Workflow, dagID string, fn func([]BPMatcher) []BPMatcher) (bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		meta, rev, err := wf.Store.GetMetaLatest(ctx, dagID)
		if err != nil {
			return false, err
		}
		if metaTerminal(meta.Status) {
			return true, nil
		}
		next := fn(meta.ArmedMatchers)
		if slices.Equal(next, meta.ArmedMatchers) {
			return false, nil
		}
		meta.ArmedMatchers = next
		if len(next) == 0 {
			meta.ArmedMatchers = nil
		}
		err = wf.Store.PutMeta(ctx, dagID, meta, rev)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return false, err
		}
	}
	return false, ErrStaleRevision
}

// addFenceToken CAS-adds tok to a step's BPFence while ok(rec) holds.
// Returns false (no write) if ok fails on a fresh read: the step is already
// past the point. It writes even when tok is already present: the revision
// bump makes a concurrent pruneStaleFenceTokens, which decided from an
// older meta, lose its CAS and decide again.
func addFenceToken(ctx context.Context, store StateStore, dagID, stepID, tok string, ok func(StepRecord) bool) (bool, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		rec, rev, err := store.GetStep(ctx, dagID, stepID)
		if errors.Is(err, ErrStepNotFound) {
			// The step was listed through the leader, so it exists; a
			// lagging replica answered this direct get. Try again.
			lastErr = err
			continue
		}
		if err != nil {
			return false, err
		}
		if !ok(rec) {
			return false, nil
		}
		if !slices.Contains(rec.BPFence, tok) {
			rec.BPFence = append(rec.BPFence, tok)
		}
		_, err = store.PutStep(ctx, dagID, stepID, rec, rev)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return false, err
		}
		lastErr = err
	}
	return false, lastErr
}

// liveFenceTokens returns rec's tokens that still stop it, without meta: a
// "b:" token yields to rec's released before position, an "a:X" token to
// X's released after position (both monotonic). Used by the persistStatus
// guard. A token whose matcher was disarmed stays live here; disarm and the
// sweep remove it.
func liveFenceTokens(ctx context.Context, store StateStore, dagID string, rec StepRecord) ([]string, error) {
	var live []string
	for _, tok := range rec.BPFence {
		if up, ok := cutAfterToken(tok); ok {
			urec, _, err := store.GetStep(ctx, dagID, up)
			if err != nil && !errors.Is(err, ErrStepNotFound) {
				return nil, err
			}
			// A lagging replica can only show an older, unreleased upstream
			// (or none): both keep the token, the safe direction.
			if err == nil && urec.BPAfter == BPStateReleased {
				continue
			}
			live = append(live, tok)
			continue
		}
		if rec.BPBefore == BPStateReleased {
			continue
		}
		live = append(live, tok)
	}
	return live, nil
}

// pruneStaleFenceTokens removes stepID's tokens that nothing justifies any
// more: a "b:<key>" whose key is not armed or whose before position is
// released, an "a:X" whose upstream X is not armed after or is released.
// The step is read before meta (from the leader), so a token written by an
// arm is judged against a meta that already holds that arm's matcher; a
// token touched after the step read fails the CAS and is judged again.
// Reports whether a token was removed.
func pruneStaleFenceTokens(ctx context.Context, store StateStore, dagID, stepID string) (bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		rec, rev, err := store.GetStep(ctx, dagID, stepID)
		if err != nil {
			return false, err
		}
		if rec.Status != StatusPending || len(rec.BPFence) == 0 {
			return false, nil
		}
		meta, _, err := store.GetMetaLatest(ctx, dagID)
		if err != nil {
			return false, err
		}
		view := &DAGState{Meta: meta}
		armed := func(key string) bool {
			return slices.ContainsFunc(meta.ArmedMatchers, func(m BPMatcher) bool { return m.Key() == key })
		}
		keep := make([]string, 0, len(rec.BPFence))
		for _, tok := range rec.BPFence {
			if up, ok := cutAfterToken(tok); ok {
				urec, _, err := store.GetStep(ctx, dagID, up)
				if err != nil {
					if errors.Is(err, ErrStepNotFound) {
						keep = append(keep, tok) // possibly a lagging replica: keep
						continue
					}
					return false, err
				}
				if urec.BPAfter != BPStateReleased && view.bpArmed(urec, BPPositionAfter) {
					keep = append(keep, tok)
				}
				continue
			}
			key, ok := strings.CutPrefix(tok, fenceBeforePrefix)
			if !ok || (rec.BPBefore != BPStateReleased && armed(key)) {
				keep = append(keep, tok) // unknown token kinds are kept
			}
		}
		if len(keep) == len(rec.BPFence) {
			return false, nil
		}
		rec.BPFence = keep
		if len(keep) == 0 {
			rec.BPFence = nil
		}
		_, err = store.PutStep(ctx, dagID, stepID, rec, rev)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return false, err
		}
	}
	return false, ErrStaleRevision
}

// releaseOrphanedBPFences is the sweep repair: it prunes stale tokens from
// every pending step of a running DAG. A disarm or release that crashed
// before its token cleanup leaves a token persistStatus would honor forever.
// No age gate: arm writes meta before tokens, so a token whose key is absent
// from a leader-read meta was disarmed (or lost a race with a disarm).
func releaseOrphanedBPFences(ctx context.Context, store StateStore, dagID string, state *DAGState) bool {
	pruned := false
	for id, rec := range state.Steps {
		if rec.Status != StatusPending || len(rec.BPFence) == 0 {
			continue
		}
		if ok, err := pruneStaleFenceTokens(ctx, store, dagID, id); err == nil && ok {
			pruned = true
		}
	}
	return pruned
}

// clearAfterFence removes "a:<upstreamID>" tokens from upstreamID's pending
// dependents once its after position is released. Idempotent; a crash here
// is repaired by the sweep.
func clearAfterFence(ctx context.Context, store StateStore, dagID, upstreamID string) error {
	steps, err := store.ListSteps(ctx, dagID)
	if err != nil {
		return err
	}
	for _, rec := range steps {
		if rec.Status != StatusPending || !afterFenced(rec, upstreamID) {
			continue
		}
		if _, err := pruneStaleFenceTokens(ctx, store, dagID, rec.StepID); err != nil {
			return err
		}
	}
	return nil
}

func cutAfterToken(tok string) (string, bool) { return strings.CutPrefix(tok, fenceAfterPrefix) }

// loadLatestState lists steps, then reads meta from the leader.
func loadLatestState(ctx context.Context, wf *Workflow, dagID string) (*DAGState, error) {
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return nil, err
	}
	meta, _, err := wf.Store.GetMetaLatest(ctx, dagID)
	if err != nil {
		return nil, err
	}
	state := &DAGState{Meta: meta, Steps: make(map[string]StepRecord, len(steps))}
	for _, s := range steps {
		state.Steps[s.StepID] = s
	}
	return state, nil
}

func (s *DAGState) isTerminal() bool { return metaTerminal(s.Meta.Status) }

func metaTerminal(st DAGStatus) bool {
	return st == DAGStatusDone || st == DAGStatusFailed || st == DAGStatusCanceled
}

// blockedInstances returns the breakpoint instances stopping work right now.
func blockedInstances(state *DAGState) []BreakpointInfo {
	return FilterBreakpoints(state.breakpoints(), OnlyBlocked())
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}
