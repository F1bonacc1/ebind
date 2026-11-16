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

// TestStartPaused_E2E_SignalRestartResume: a DAG submitted with StartPaused
// runs nothing, takes a signal while paused, stays paused across a process
// restart, and completes after Resume on the new process.
func TestStartPaused_E2E_SignalRestartResume(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)
	task.MustRegister(h.reg, hDeploy)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 2, 3)
	b := dag.Step("b", hDouble, a.Ref())
	deploy := dag.Step("deploy", hDeploy, workflow.SignalRef("approval"), b.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.StartPaused()); err != nil {
		t.Fatal(err)
	}

	// Nothing runs: the root's result never appears.
	shortCtx, shortCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_, err := workflow.Await[int](shortCtx, h.wf, dag.ID(), a)
	shortCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("root ran while start-paused: Await err = %v, want DeadlineExceeded", err)
	}
	if ok, err := workflow.Signal(ctx, h.wf, dag.ID(), "approval", approval{Approver: "ops"}); err != nil || !ok {
		t.Fatalf("Signal while paused = %v, %v", ok, err)
	}

	// Process dies; a fresh one comes up on the same node.
	h.cancel()
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

	meta, _, err := workflow.DAGInfo(ctx, wfB, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != workflow.DAGStatusPaused {
		t.Fatalf("status after restart = %s, want paused", meta.Status)
	}
	steps, err := wfB.Store.ListSteps(ctx, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.Status != workflow.StatusPending || !s.Held {
			t.Fatalf("step %s after restart: status=%s held=%v, want pending+held", s.StepID, s.Status, s.Held)
		}
	}

	if err := workflow.Resume(ctx, wfB, dag.ID()); err != nil {
		t.Fatal(err)
	}
	got, err := workflow.Await[string](ctx, wfB, dag.ID(), deploy)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := hDeploy(ctx, approval{Approver: "ops"}, 10); got != want {
		t.Errorf("deploy = %q, want %q", got, want)
	}
	waitForStatus(t, &wfHarness{wf: wfB}, dag.ID(), workflow.DAGStatusDone, 5*time.Second)
}
