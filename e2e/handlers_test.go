//go:build e2e

package e2e

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/workflow"
)

// gate blocks a handler until the test releases it, creating a deterministic
// window for failure injection while the owning DAG is mid-flight. Handlers run
// in worker goroutines inside the test process, so the test coordinates through
// these channels instead of sleeping. Both close operations are sync.Once-guarded
// so redelivered executions (lost ack during a node kill) pass straight through
// an already-released gate.
type gate struct {
	reached     chan struct{}
	reachedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newGate() *gate {
	return &gate{reached: make(chan struct{}), release: make(chan struct{})}
}

// Reached closes once the handler is in-flight — the deterministic injection point.
func (g *gate) Reached() <-chan struct{} { return g.reached }

func (g *gate) Release() { g.releaseOnce.Do(func() { close(g.release) }) }

func (g *gate) pass(ctx context.Context, x int) (int, error) {
	g.reachedOnce.Do(func() { close(g.reached) })
	select {
	case <-g.release:
		return x, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Package-level gate slots referenced by the fixed-signature gate handlers
// below. Reset by newHarness before each run.
var (
	gate1      *gate // long DAG: kill-follower window
	gate2      *gate // long DAG: pause/resume window
	gate3      *gate // long DAG: meta-leader-kill window (held across the node restart)
	gateCancel *gate // cancel DAG: keeps the root in-flight while Cancel lands
)

func resetGates() {
	gate1, gate2, gate3, gateCancel = newGate(), newGate(), newGate(), newGate()
}

func releaseAllGates() {
	for _, g := range []*gate{gate1, gate2, gate3, gateCancel} {
		if g != nil {
			g.Release()
		}
	}
}

func eGate1(ctx context.Context, x int) (int, error)      { return gate1.pass(ctx, x) }
func eGate2(ctx context.Context, x int) (int, error)      { return gate2.pass(ctx, x) }
func eGate3(ctx context.Context, x int) (int, error)      { return gate3.pass(ctx, x) }
func eGateCancel(ctx context.Context, x int) (int, error) { return gateCancel.pass(ctx, x) }

// Pure arithmetic handlers — every value flowing through Refs is exactly
// verifiable at the fan-in.

func eAdd(_ context.Context, a, b int) (int, error)     { return a + b, nil }
func eAdd3(_ context.Context, a, b, c int) (int, error) { return a + b + c, nil }
func eDouble(_ context.Context, x int) (int, error)     { return x * 2, nil }
func eInc(_ context.Context, x int) (int, error)        { return x + 1, nil }

var (
	sideEffectCount atomic.Int32
	flakyAttempts   atomic.Int32
	exhaustAttempts atomic.Int32
)

// eSideEffect covers the error-only handler signature plus fire-and-forget enqueues.
func eSideEffect(_ context.Context) error {
	sideEffectCount.Add(1)
	return nil
}

func eFlakyOnce(_ context.Context) (string, error) {
	if flakyAttempts.Add(1) == 1 {
		return "", errors.New("transient: first attempt fails")
	}
	return "recovered", nil
}

func eRetryExhaust(_ context.Context) (int, error) {
	exhaustAttempts.Add(1)
	return 0, errors.New("always fails")
}

func eNonRetryable(_ context.Context) (int, error) {
	return 0, &task.TaskError{Kind: "validation", Message: "bad input", Retryable: false}
}

func ePanics(_ context.Context) error { panic("e2e: deliberate panic") }

func eFailOptional(_ context.Context) (int, error) { return 0, errors.New("optional step fails") }

// eWhere reports which worker executed the step — placement assertions.
func eWhere(ctx context.Context) (string, error) {
	return workflow.CurrentWorkerID(ctx), nil
}

// eDynamicParent adds a follow-up step pinned to the current worker. Step
// creation is idempotent library-side, so re-execution after a lost ack is safe.
func eDynamicParent(ctx context.Context, x int) (int, error) {
	d := workflow.FromContext(ctx)
	if d == nil {
		return 0, errors.New("e2e: missing workflow context")
	}
	if _, err := d.StepOpts("dyn-followup", eDouble, []workflow.StepOption{workflow.ColocateHere()}, x); err != nil {
		return 0, err
	}
	return x, nil
}

// WhoAmI is a client-side stub: its canonical name "e2e.WhoAmI" is what the
// task envelope carries, and each worker registers its own closure under that
// name via task.WithName — so the result identifies the executing worker.
func WhoAmI(_ context.Context) (string, error) { return "", errors.New("stub: never executed") }

// LegacyAdd is a client-side stub whose canonical name "e2e.LegacyAdd" resolves
// through the alias registered for eAdd — the handler-rename compatibility path.
func LegacyAdd(_ context.Context, a, b int) (int, error) {
	_ = a
	_ = b
	return 0, errors.New("stub: never executed")
}

// registerAll installs the shared handler set plus the per-worker identity
// closure into one worker's registry.
func registerAll(reg *task.Registry, workerID string) error {
	// One plain Register call to cover the error-returning API; the rest use MustRegister.
	if err := task.Register(reg, eAdd, task.Alias("e2e.LegacyAdd")); err != nil {
		return err
	}
	for _, fn := range []any{
		eAdd3, eDouble, eInc, eSideEffect,
		eGate1, eGate2, eGate3, eGateCancel,
		eFlakyOnce, eRetryExhaust, eNonRetryable, ePanics, eFailOptional,
		eWhere, eDynamicParent,
	} {
		task.MustRegister(reg, fn)
	}
	me := workerID
	task.MustRegister(reg, func(_ context.Context) (string, error) { return me, nil }, task.WithName("e2e.WhoAmI"))
	return nil
}
