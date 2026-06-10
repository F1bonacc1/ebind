package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Scheduler consumes completion + step-added events, advances DAG state, and
// enqueues newly-ready steps. Every worker starts one; leader gating via
// Workflow.Elector ensures at-most-one processes events at a time across workers;
// within a single scheduler, mu serializes event handling to avoid CAS races.
//
// On a false→true edge of IsLeader(), the scheduler runs a full sweep of all
// in-flight DAGs to re-enqueue any ready-but-stranded steps.
type Scheduler struct {
	wf *Workflow
	mu sync.Mutex

	leaderMu     sync.Mutex
	wasLeader    bool
	sweepRunning bool
}

// Run subscribes to all DAG.>.completed.> and DAG.>.step-added.> events and
// dispatches them. Also spawns a leadership watcher that triggers a sweep on
// every false→true edge of IsLeader(). Blocks until ctx is done.
func (s *Scheduler) Run(ctx context.Context) error {
	sub, err := s.wf.Bus.Subscribe(ctx, "DAG.>", func(ev Event) {
		s.onEvent(ctx, ev)
	})
	if err != nil {
		return fmt.Errorf("scheduler: subscribe: %w", err)
	}
	go s.watchLeadership(ctx)
	<-ctx.Done()
	_ = sub.Stop()
	return nil
}

// watchLeadership polls IsLeader() and triggers a sweep on each false→true edge
// plus periodically while leader so that crash recovery (pausing→paused,
// paused→finalized) is not dependent on leadership transitions.
// Initial state is wasLeader=false, so a scheduler that starts as leader
// performs a startup sweep on its very first tick.
func (s *Scheduler) watchLeadership(ctx context.Context) {
	interval := s.wf.SweepCheckInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			isLeader := s.wf.Elector.IsLeader()
			s.leaderMu.Lock()
			s.wasLeader = isLeader
			shouldSweep := isLeader && !s.sweepRunning
			if shouldSweep {
				s.sweepRunning = true
			}
			s.leaderMu.Unlock()
			if shouldSweep {
				go s.runSweep(ctx)
			}
		}
	}
}

// runSweep wraps sweep with overlap guard + per-sweep timeout.
func (s *Scheduler) runSweep(ctx context.Context) {
	defer func() {
		s.leaderMu.Lock()
		s.sweepRunning = false
		s.leaderMu.Unlock()
	}()
	timeout := s.wf.SweepTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	sweepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = s.sweep(sweepCtx)
}

// sweep lists every running DAG in the store, loads its state, and re-enqueues
// any steps that are ready but stranded (Pending with all deps terminal).
// Idempotent: persistStatus + Nats-Msg-Id dedupe prevent duplicate work even
// if event-driven scheduling is already running for the same DAG.
func (s *Scheduler) sweep(ctx context.Context) error {
	dags, err := s.wf.Store.ListDAGs(ctx)
	if err != nil {
		return err
	}
	for _, dag := range dags {
		switch dag.Status {
		case DAGStatusRunning:
			state, err := s.loadState(ctx, dag.ID)
			if err != nil {
				continue
			}
			s.mu.Lock()
			ready := state.ReadyToRun()
			err = s.enqueueReady(ctx, state, ready)
			s.mu.Unlock()
			_ = err

		case DAGStatusPausing:
			// D-16: Recovery for leader crash while pausing — transition to paused.
			state, err := s.loadState(ctx, dag.ID)
			if err != nil {
				continue
			}
			if state.HasInFlightSteps() {
				continue // still draining; do nothing
			}
			meta, rev, err := s.wf.Store.GetMeta(ctx, dag.ID)
			if err != nil {
				continue
			}
			if meta.Status != DAGStatusPausing {
				continue // concurrent writer changed it
			}
			meta.Status = DAGStatusPaused
			meta.PausedAt = time.Now().UTC()
			_ = s.wf.Store.PutMeta(ctx, dag.ID, meta, rev) // CAS; benign fail on race

		case DAGStatusPaused:
			// D-18: Skip non-terminal paused DAGs — zero CPU.
			// D-17: Auto-finalize if all steps are terminal (repair-only —
			// the primary path is finalizeDAG in the event pipeline).
			// Can't use state.Terminal() here: it returns (running,false)
			// when meta is paused (by design). Check steps directly.
			state, err := s.loadState(ctx, dag.ID)
			if err != nil {
				continue
			}
			if !state.AllStepsTerminal() {
				continue // not all terminal — skip (zero CPU per D-18)
			}
			meta, rev, err := s.wf.Store.GetMeta(ctx, dag.ID)
			if err != nil {
				continue
			}
			if meta.Status != DAGStatusPaused {
				continue
			}
			meta.Status = state.DeriveFinalStatus()
			_ = s.wf.Store.PutMeta(ctx, dag.ID, meta, rev)

		default:
			continue // done/failed/canceled — existing behavior
		}
	}
	return nil
}

func (s *Scheduler) onEvent(ctx context.Context, ev Event) {
	if !s.wf.Elector.IsLeader() {
		if ev.Nak != nil {
			_ = ev.Nak()
		}
		return
	}
	// Serialize intra-process event handling so CAS on step records doesn't race
	// between concurrent event deliveries. Cross-process serialization is the
	// user's LeaderElector responsibility.
	s.mu.Lock()
	err := s.handleEvent(ctx, ev)
	s.mu.Unlock()
	_ = err // production impl should log; swallowing here keeps durable delivery alive
	if ev.Ack != nil {
		_ = ev.Ack()
	}
}

// handleEvent is the glue between the pure state machine and IO. It:
//  1. Loads the DAG state from the store
//  2. Applies the event (MarkDone / MarkFailed on the state)
//  3. For each newly-ready step, resolves args and enqueues
//  4. Writes back updated step records
//  5. Finalizes the DAG meta status if terminal
//
// The state-machine transitions themselves (MarkDone/MarkFailed/cascadeSkipFrom)
// are exercised in state_test.go against an in-memory DAGState — this function
// is tested in scheduler_test.go with fake store + bus + enqueuer.
func (s *Scheduler) handleEvent(ctx context.Context, ev Event) error {
	state, err := s.loadState(ctx, ev.DAGID)
	if err != nil {
		return err
	}
	// Canceled and fully paused DAGs — gate ALL events (no processing).
	if state.Meta.Status == DAGStatusCanceled || state.Meta.Status == DAGStatusPaused {
		return nil
	}
	// Pausing DAGs: gate step-added events (no new work during drain), but
	// allow completion events through so the pausing→paused auto-transition
	// in onCompleted can fire when the last in-flight step finishes (D-13).
	if state.Meta.Status == DAGStatusPausing && ev.Kind == EventStepAdded {
		return nil
	}
	switch ev.Kind {
	case EventCompleted:
		return s.onCompleted(ctx, state, ev)
	case EventStepAdded:
		return s.onStepAdded(ctx, state)
	case EventResumed:
		return s.onResumed(ctx, state)
	}
	return nil
}

// onResumed handles EventResumed. Before re-evaluating ready steps it re-applies
// cascade-skip for any failed/skipped step: completion events that arrived while
// the DAG was paused were gated, so After()-linked dependents never got
// cascade-skipped (Ref-linked ones are covered by ResolveArgs at enqueue time).
func (s *Scheduler) onResumed(ctx context.Context, state *DAGState) error {
	for id, step := range state.Steps {
		if step.Status != StatusFailed && step.Status != StatusSkipped {
			continue
		}
		for _, skipped := range state.cascadeSkipFrom(id) {
			if err := s.persistStatus(ctx, state.Meta.ID, skipped, StatusSkipped); err != nil {
				return err
			}
		}
	}
	return s.onStepAdded(ctx, state)
}

func (s *Scheduler) onCompleted(ctx context.Context, state *DAGState, ev Event) error {
	if _, exists := state.Steps[ev.StepID]; !exists {
		return ErrStepNotFound
	}
	// Advance the pure state machine and collect cascade side effects.
	// The hook may have already written the status to the store before this
	// event arrived, so MarkX returns nil newlyReady on the idempotent path.
	// We always re-compute ReadyToRun after applying the transition.
	var newlySkipped []string
	switch ev.Status {
	case StatusDone:
		_, _ = state.MarkDone(ev.StepID)
	case StatusFailed:
		_, newlySkipped, _ = state.MarkFailed(ev.StepID, ev.ErrorKind, ev.ErrorMessage)
	case StatusSkipped:
		newlySkipped, _ = state.MarkSkipped(ev.StepID)
	default:
		return fmt.Errorf("scheduler: unknown completion status %q", ev.Status)
	}
	newlyReady := state.ReadyToRun()
	for _, skipped := range newlySkipped {
		if err := s.persistStatus(ctx, ev.DAGID, skipped, StatusSkipped); err != nil {
			return err
		}
	}

	// Enqueue newly ready steps. Build a results/statuses snapshot for ResolveArgs.
	if err := s.enqueueReady(ctx, state, newlyReady); err != nil {
		return err
	}

	// ----- pausing→paused auto-transition (SG-04) -----
	if state.Meta.Status == DAGStatusPausing && !state.HasInFlightSteps() {
		meta, rev, err := s.wf.Store.GetMeta(ctx, state.Meta.ID)
		if err != nil {
			return err
		}
		if meta.Status != DAGStatusPausing {
			return nil // concurrent Resume or Cancel changed it; benign (D-14)
		}
		meta.Status = DAGStatusPaused
		meta.PausedAt = time.Now().UTC()
		if err := s.wf.Store.PutMeta(ctx, state.Meta.ID, meta, rev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				return nil // benign CAS race (Resume won) — sweep handles it
			}
			return err
		}
		// If all steps are already terminal, finalize immediately instead of
		// staying paused (all-terminal paused DAG should be done/failed).
		if state.AllStepsTerminal() {
			return s.finalizeDAG(ctx, state)
		}
		return nil
	}
	// ----- end pausing→paused -----

	// Finalize DAG if all terminal.
	return s.maybeFinalize(ctx, state)
}

func (s *Scheduler) onStepAdded(ctx context.Context, state *DAGState) error {
	if err := s.enqueueReady(ctx, state, state.ReadyToRun()); err != nil {
		return err
	}
	// If this is a resume and all steps are already terminal, finalize.
	if state.AllStepsTerminal() {
		return s.finalizeDAG(ctx, state)
	}
	return nil
}

// enqueueReady resolves args and publishes task envelopes for each ready step.
// If arg resolution returns cascade-skip, we mark the step skipped instead.
func (s *Scheduler) enqueueReady(ctx context.Context, state *DAGState, ready []string) error {
	if len(ready) == 0 || state.Meta.Status == DAGStatusCanceled ||
		state.Meta.Status == DAGStatusPausing ||
		state.Meta.Status == DAGStatusPaused {
		return nil
	}
	results, statuses, err := s.snapshotUpstream(ctx, state)
	if err != nil {
		return err
	}
	for _, id := range ready {
		rec := state.Steps[id]
		// Transition to running in the store (CAS). If a concurrent writer
		// (e.g. workflow.Cancel) already wrote a terminal status, we must not
		// enqueue the step — our snapshot of rec is stale.
		if err := s.persistStatus(ctx, state.Meta.ID, id, StatusRunning); err != nil {
			if errors.Is(err, errStepAlreadyTerminal) {
				continue
			}
			return err
		}
		var rawArgs []json.RawMessage
		if err := json.Unmarshal(rec.ArgsJSON, &rawArgs); err != nil {
			return err
		}
		_, skip, err := ResolveArgs(rawArgs, results, statuses)
		if err != nil {
			return err
		}
		if skip {
			// Cascade-skip required-ref dependent.
			_ = s.persistStatus(ctx, state.Meta.ID, id, StatusSkipped)
			// Publish a synthetic skipped event so dependents can also advance.
			ev := Event{Kind: EventCompleted, DAGID: state.Meta.ID, StepID: id, Status: StatusSkipped}
			data, _ := MarshalEvent(ev)
			_ = s.wf.Bus.Publish(ctx, EventSubject(ev), data)
			continue
		}
		if err := enqueueStep(ctx, s.wf.Enq, rec, state.Steps, results, statuses); err != nil {
			return err
		}
	}
	return nil
}

// snapshotUpstream collects result bytes + statuses for all completed upstream steps.
func (s *Scheduler) snapshotUpstream(ctx context.Context, state *DAGState) (map[string]json.RawMessage, map[string]StepStatus, error) {
	results := map[string]json.RawMessage{}
	statuses := map[string]StepStatus{}
	for id, step := range state.Steps {
		statuses[id] = step.Status
		if step.Status == StatusDone {
			data, err := s.wf.Store.GetResult(ctx, state.Meta.ID, id)
			if err != nil {
				return nil, nil, err
			}
			results[id] = data
		}
	}
	return results, statuses, nil
}

// persistStatus CAS-updates a step's status in the store. Retries on stale.
// Returns errStepAlreadyTerminal when the stored record is in a terminal state
// that the requested status cannot override — callers must treat this as
// "don't enqueue" rather than a silent no-op.
func (s *Scheduler) persistStatus(ctx context.Context, dagID, stepID string, status StepStatus) error {
	for attempt := 0; attempt < 5; attempt++ {
		rec, rev, err := s.wf.Store.GetStep(ctx, dagID, stepID)
		if err != nil {
			return err
		}
		if rec.Status == status {
			return nil // already set
		}
		if rec.Held && status == StatusRunning {
			return errStepAlreadyTerminal
		}
		if rec.IsTerminal() && status != StatusSkipped {
			return errStepAlreadyTerminal
		}
		rec.Status = status
		if status == StatusRunning && rec.StartedAt.IsZero() {
			rec.StartedAt = time.Now().UTC()
		}
		err = s.wf.Store.PutStep(ctx, dagID, stepID, rec, rev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return err
		}
	}
	return ErrStaleRevision
}

// finalizeDAG transitions the DAG to its final status (done/failed) using
// AllStepsTerminal + DeriveFinalStatus. Reads fresh meta for CAS safety.
func (s *Scheduler) finalizeDAG(ctx context.Context, state *DAGState) error {
	finalStatus := state.DeriveFinalStatus()
	meta, rev, err := s.wf.Store.GetMeta(ctx, state.Meta.ID)
	if err != nil {
		return err
	}
	if meta.Status == DAGStatusDone || meta.Status == DAGStatusFailed || meta.Status == DAGStatusCanceled {
		return nil // already finalized
	}
	if meta.Status == DAGStatusRunning || meta.Status == DAGStatusPaused {
		meta.Status = finalStatus
		return s.wf.Store.PutMeta(ctx, state.Meta.ID, meta, rev)
	}
	return nil
}

// maybeFinalize checks if all steps are terminal; if so, updates DAG meta status.
func (s *Scheduler) maybeFinalize(ctx context.Context, state *DAGState) error {
	status, done := state.Terminal()
	if !done {
		return nil
	}
	meta, rev, err := s.wf.Store.GetMeta(ctx, state.Meta.ID)
	if err != nil {
		return err
	}
	if meta.Status == DAGStatusCanceled || meta.Status == DAGStatusPausing || meta.Status == DAGStatusPaused {
		return nil
	}
	if meta.Status == status {
		return nil
	}
	meta.Status = status
	return s.wf.Store.PutMeta(ctx, state.Meta.ID, meta, rev)
}

// loadState fetches DAG meta + all step records into an in-memory DAGState.
func (s *Scheduler) loadState(ctx context.Context, dagID string) (*DAGState, error) {
	meta, _, err := s.wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return nil, err
	}
	steps, err := s.wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]StepRecord, len(steps))
	for _, s := range steps {
		m[s.StepID] = s
	}
	return &DAGState{Meta: meta, Steps: m}, nil
}
