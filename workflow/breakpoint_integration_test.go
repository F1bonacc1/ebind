package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// waitForBPState polls the step record until the breakpoint state at the given
// position matches want (advisory mark written by Submit/scheduler).
func waitForBPState(t *testing.T, h *wfHarness, dagID, stepID string, pos workflow.BPPosition, want workflow.BPState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, _, err := h.wf.Store.GetStep(context.Background(), dagID, stepID)
		if err == nil {
			got := rec.BPBefore
			if pos == workflow.BPPositionAfter {
				got = rec.BPAfter
			}
			if got == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec, _, _ := h.wf.Store.GetStep(context.Background(), dagID, stepID)
	t.Fatalf("step %s never reached %s=%q (status=%s bp_before=%q bp_after=%q)",
		stepID, pos, want, rec.Status, rec.BPBefore, rec.BPAfter)
}

func TestBreakpoint_E2E_Before(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)
	task.MustRegister(h.reg, hConcat)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 2, 3)
	b := dag.StepOpts("b", hDouble, []workflow.StepOption{workflow.BreakBefore("BeforeDouble")}, a.Ref())
	c := dag.Step("c", hConcat, "result", b.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.WithActiveBreakpoints("BeforeDouble")); err != nil {
		t.Fatal(err)
	}
	waitForStepDone(t, h, dag.ID(), "a", 5*time.Second)
	waitForBPState(t, h, dag.ID(), "b", workflow.BPPositionBefore, workflow.BPStateBlocked, 5*time.Second)

	rec, _, _ := h.wf.Store.GetStep(ctx, dag.ID(), "b")
	if rec.Status != workflow.StatusPending {
		t.Fatalf("blocked step b must stay pending, got %s", rec.Status)
	}
	meta, _, _ := h.wf.Store.GetMeta(ctx, dag.ID())
	if meta.Status != workflow.DAGStatusRunning {
		t.Fatalf("DAG must stay running while blocked, got %s", meta.Status)
	}

	// Observability: ListBreakpoints reports the blocked instance.
	infos, err := workflow.ListBreakpoints(ctx, h.wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].State != workflow.BPStateBlocked || !infos[0].Armed {
		t.Fatalf("ListBreakpoints = %+v, want one armed+blocked entry", infos)
	}

	n, err := workflow.ResumeBreakpoint(ctx, h.wf, dag.ID(), "BeforeDouble")
	if err != nil || n != 1 {
		t.Fatalf("ResumeBreakpoint: n=%d err=%v", n, err)
	}
	result, err := workflow.Await[string](ctx, h.wf, dag.ID(), c)
	if err != nil {
		t.Fatal(err)
	}
	if result != "result:10" { // (2+3)*2
		t.Errorf("got %q, want result:10", result)
	}
	waitForStatus(t, h, dag.ID(), workflow.DAGStatusDone, 5*time.Second)
}

func TestBreakpoint_E2E_After_ResultPersistedDependentHeld(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.StepOpts("a", hAdd, []workflow.StepOption{workflow.BreakAfter("AfterA")}, 1, 2)
	b := dag.Step("b", hDouble, a.Ref())
	// Independent parallel branch — must complete while the a→b line is gated.
	x := dag.Step("x", hAdd, 10, 10)
	_ = dag.Step("y", hDouble, x.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.WithActiveBreakpoints("AfterA")); err != nil {
		t.Fatal(err)
	}

	// a completes normally with its result persisted; its after-gate holds b.
	waitForStepDone(t, h, dag.ID(), "a", 5*time.Second)
	waitForBPState(t, h, dag.ID(), "a", workflow.BPPositionAfter, workflow.BPStateBlocked, 5*time.Second)
	if res, err := h.wf.Store.GetResult(ctx, dag.ID(), "a"); err != nil || string(res) != "3" {
		t.Fatalf("a's result must be persisted while gated: %q err=%v", res, err)
	}

	// Parallel branch runs to completion while b stays pending.
	waitForStepDone(t, h, dag.ID(), "y", 10*time.Second)
	bRec, _, _ := h.wf.Store.GetStep(ctx, dag.ID(), "b")
	if bRec.Status != workflow.StatusPending {
		t.Fatalf("b must be held by a's after-gate, got %s", bRec.Status)
	}

	n, err := workflow.ResumeBreakpoint(ctx, h.wf, dag.ID(), "AfterA")
	if err != nil || n != 1 {
		t.Fatalf("ResumeBreakpoint: n=%d err=%v", n, err)
	}
	// b receives a's real (not default) result: (1+2)*2 = 6.
	result, err := workflow.Await[int](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatal(err)
	}
	if result != 6 {
		t.Errorf("b result = %d, want 6", result)
	}
	waitForStatus(t, h, dag.ID(), workflow.DAGStatusDone, 5*time.Second)
}

// hDynamicWithBP adds a follow-up step carrying a breakpoint label.
func hDynamicWithBP(ctx context.Context, x int) (int, error) {
	d := workflow.FromContext(ctx)
	if d != nil {
		_, err := d.StepOpts("dyn-bp", hDouble, []workflow.StepOption{workflow.BreakBefore("DynLabel")}, x)
		if err != nil {
			return 0, err
		}
	}
	return x, nil
}

func TestBreakpoint_E2E_DynamicStep_BlocksOnArmedLabel(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hDynamicWithBP)
	task.MustRegister(h.reg, hDouble)

	// The label is armed at submit; the step carrying it is added dynamically.
	dag := workflow.New()
	_ = dag.Step("parent", hDynamicWithBP, 21)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.WithActiveBreakpoints("DynLabel")); err != nil {
		t.Fatal(err)
	}
	waitForStepDone(t, h, dag.ID(), "parent", 5*time.Second)
	waitForBPState(t, h, dag.ID(), "dyn-bp", workflow.BPPositionBefore, workflow.BPStateBlocked, 5*time.Second)

	n, err := workflow.ResumeBreakpoint(ctx, h.wf, dag.ID(), "DynLabel")
	if err != nil || n != 1 {
		t.Fatalf("ResumeBreakpoint: n=%d err=%v", n, err)
	}
	result, err := workflow.AwaitByID[int](ctx, h.wf, dag.ID(), "dyn-bp")
	if err != nil {
		t.Fatal(err)
	}
	if result != 42 {
		t.Errorf("dyn-bp result = %d, want 42", result)
	}
}

func TestBreakpoint_E2E_PauseInterplay(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 1, 2)
	b := dag.StepOpts("b", hDouble, []workflow.StepOption{workflow.BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitForStepDone(t, h, dag.ID(), "a", 5*time.Second)
	waitForBPState(t, h, dag.ID(), "b", workflow.BPPositionBefore, workflow.BPStateBlocked, 5*time.Second)

	// Pause the whole DAG (no in-flight work → straight to paused), then
	// resume it. The pause hold composes with — and must not release — the BP.
	if err := workflow.Pause(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, h, dag.ID(), workflow.DAGStatusPaused, 5*time.Second)
	if err := workflow.Resume(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, h, dag.ID(), workflow.DAGStatusRunning, 5*time.Second)

	time.Sleep(500 * time.Millisecond) // give the scheduler a chance to (wrongly) dispatch
	rec, _, _ := h.wf.Store.GetStep(ctx, dag.ID(), "b")
	if rec.Status != workflow.StatusPending {
		t.Fatalf("b must still be blocked after DAG pause/resume, got %s", rec.Status)
	}
	if rec.Held {
		t.Fatal("DAG resume must have released the pause hold")
	}

	// Only the breakpoint resume releases the line.
	n, err := workflow.ResumeBreakpoint(ctx, h.wf, dag.ID(), "X")
	if err != nil || n != 1 {
		t.Fatalf("ResumeBreakpoint: n=%d err=%v", n, err)
	}
	result, err := workflow.Await[int](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatal(err)
	}
	if result != 6 {
		t.Errorf("b result = %d, want 6", result)
	}
	waitForStatus(t, h, dag.ID(), workflow.DAGStatusDone, 5*time.Second)
}

func TestBreakpoint_E2E_DurableAcrossRestart(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 1, 2)
	b := dag.StepOpts("b", hDouble, []workflow.StepOption{workflow.BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitForStepDone(t, h, dag.ID(), "a", 5*time.Second)
	waitForBPState(t, h, dag.ID(), "b", workflow.BPPositionBefore, workflow.BPStateBlocked, 5*time.Second)

	// Kill the worker + scheduler, as if the process died while blocked.
	h.cancel()
	time.Sleep(200 * time.Millisecond)

	// A fresh instance sees the durable blocked state.
	wfB, err := workflow.NewFromNATS(ctx, h.nc, 1)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := workflow.ListBreakpoints(ctx, wfB, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].State != workflow.BPStateBlocked {
		t.Fatalf("blocked state must survive restart; got %+v", infos)
	}

	// Restart worker + scheduler on the fresh instance and resume.
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
	time.Sleep(200 * time.Millisecond)

	n, err := workflow.ResumeBreakpoint(ctx, wfB, dag.ID(), "X")
	if err != nil || n != 1 {
		t.Fatalf("ResumeBreakpoint after restart: n=%d err=%v", n, err)
	}
	result, err := workflow.Await[int](ctx, wfB, dag.ID(), b)
	if err != nil {
		t.Fatal(err)
	}
	if result != 6 {
		t.Errorf("b result = %d, want 6", result)
	}
}
