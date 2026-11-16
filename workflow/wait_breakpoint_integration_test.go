package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

func TestWaitForBreakpoint_E2E_StopsAndTerminal(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.StepOpts("a", hAdd, []workflow.StepOption{workflow.BreakAfter("AfterA")}, 2, 3)
	_ = dag.StepOpts("b", hDouble, []workflow.StepOption{workflow.BreakBefore("BeforeB")}, a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.WithActiveBreakpoints("AfterA", "BeforeB")); err != nil {
		t.Fatal(err)
	}

	info, err := workflow.WaitForBreakpoint(ctx, h.wf, dag.ID(), workflow.AfterStep("a"))
	if err != nil || info.StepID != "a" || info.Position != workflow.BPPositionAfter {
		t.Fatalf("after a: %+v, %v", info, err)
	}
	if n, err := workflow.ResumeBreakpoint(ctx, h.wf, dag.ID(), "AfterA"); err != nil || n != 1 {
		t.Fatalf("resume AfterA: n=%d err=%v", n, err)
	}
	info, err = workflow.WaitForBreakpoint(ctx, h.wf, dag.ID(), workflow.BeforeStep("b"))
	if err != nil || info.StepID != "b" || info.Position != workflow.BPPositionBefore {
		t.Fatalf("before b: %+v, %v", info, err)
	}

	short, sc := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = workflow.WaitForBreakpoint(short, h.wf, dag.ID(), workflow.AfterStep("b"))
	sc()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no such stop: err = %v, want DeadlineExceeded", err)
	}

	if err := workflow.Cancel(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.WaitForBreakpoint(ctx, h.wf, dag.ID(), workflow.BeforeStep("b")); !errors.Is(err, workflow.ErrDAGTerminal) {
		t.Fatalf("canceled DAG: err = %v, want ErrDAGTerminal", err)
	}
	if _, err := workflow.WaitForBreakpoint(ctx, h.wf, "ghost", workflow.BeforeStep("b")); !errors.Is(err, workflow.ErrDAGNotFound) {
		t.Fatalf("unknown DAG: err = %v, want ErrDAGNotFound", err)
	}
}

// TestWaitForBreakpoint_E2E_SurvivesWorkerAndSchedulerRestart: the waiter is
// started before anything runs, the worker and scheduler die and are
// replaced, and the waiter still returns when the step stops.
func TestWaitForBreakpoint_E2E_SurvivesWorkerAndSchedulerRestart(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 2, 3)
	_ = dag.StepOpts("b", hDouble, []workflow.StepOption{workflow.BreakBefore("BeforeB")}, a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.StartPaused(), workflow.WithActiveBreakpoints("BeforeB")); err != nil {
		t.Fatal(err)
	}
	type result struct {
		info workflow.BreakpointInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, err := workflow.WaitForBreakpoint(ctx, h.wf, dag.ID(), workflow.BeforeStep("b"))
		done <- result{info, err}
	}()

	h.cancel() // worker + scheduler die
	time.Sleep(200 * time.Millisecond)
	wfB, err := workflow.NewFromNATS(ctx, h.nc, 1)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)
	w, err := worker.New(h.nc, h.reg, worker.Options{
		Concurrency: 4,
		AckWait:     2 * time.Second,
		MaxDeliver:  5,
		Backoff:     []time.Duration{50 * time.Millisecond, 100 * time.Millisecond},
		StepHook:    wfB.Hook(),
		Middleware:  []worker.Middleware{wfB.ContextMiddleware()},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(runCtx) }()
	go func() { _ = wfB.RunScheduler(runCtx) }()
	time.Sleep(300 * time.Millisecond)

	select {
	case r := <-done:
		t.Fatalf("waiter returned before the step stopped: %+v, %v", r.info, r.err)
	default:
	}
	if err := workflow.Resume(ctx, wfB, dag.ID()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.info.StepID != "b" {
			t.Fatalf("waiter = %+v, %v; want b", r.info, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter did not return after the step stopped")
	}
}
