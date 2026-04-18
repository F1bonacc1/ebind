package workflow

import (
	"context"
	"time"
)

// Workflow bundles the three IO dependencies (store, bus, enqueuer) plus an optional
// leader elector for the scheduler loop. DAG.Submit takes one; Scheduler.Run uses one.
type Workflow struct {
	Store    StateStore
	Bus      EventBus
	Enq      Enqueuer
	Elector  LeaderElector // nil ⇒ always leader
	NakDelay time.Duration // default 1s; used by scheduler when non-leader sees an event

	// SweepCheckInterval is the cadence at which the scheduler polls IsLeader()
	// to detect leadership acquisition. On a false→true edge it runs a sweep of
	// all in-flight DAGs to re-enqueue stranded ready steps. Default 5s.
	SweepCheckInterval time.Duration
	// SweepTimeout is the max wall-clock a single sweep may take. Default 60s.
	SweepTimeout time.Duration
}

// NewWorkflow constructs a Workflow with the three dependencies. Defaults:
// Elector = always-leader, NakDelay = 1s.
func NewWorkflow(store StateStore, bus EventBus, enq Enqueuer) *Workflow {
	return &Workflow{Store: store, Bus: bus, Enq: enq, Elector: alwaysLeader{}, NakDelay: time.Second}
}

// WithElector replaces the default always-leader elector.
func (wf *Workflow) WithElector(le LeaderElector) *Workflow {
	wf.Elector = le
	return wf
}

// Hook returns a worker.StepHook that persists step outcomes to the store and
// publishes completion events to the bus. Wire it into worker.Options.StepHook.
func (wf *Workflow) Hook() *StepHook {
	return &StepHook{store: wf.Store, bus: wf.Bus}
}

// RunScheduler drives the scheduler loop — subscribes to completion events,
// processes ready steps, applies state transitions. Blocks until ctx is canceled.
func (wf *Workflow) RunScheduler(ctx context.Context) error {
	s := &Scheduler{wf: wf}
	return s.Run(ctx)
}
