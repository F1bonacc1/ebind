package workflow_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// --- handlers ---

type approval struct {
	Approver string `json:"approver"`
	Note     string `json:"note,omitempty"`
}

func hDeploy(_ context.Context, ap approval, artifact int) (string, error) {
	return fmt.Sprintf("%s deployed %d", ap.Approver, artifact), nil
}

func hGate(_ context.Context) (string, error) { return "ran", nil }

// bufSigReleased gates hSpawnGated so the test controls WHEN the dynamic step
// is added — strictly after the signal was delivered (buffering proof). Reset
// per test run (a -count=N rerun must not close the same channel twice).
var (
	bufSigMu       sync.Mutex
	bufSigReleased chan struct{}
)

func resetBufSigChan() chan struct{} {
	bufSigMu.Lock()
	defer bufSigMu.Unlock()
	bufSigReleased = make(chan struct{})
	return bufSigReleased
}

func getBufSigChan() chan struct{} {
	bufSigMu.Lock()
	defer bufSigMu.Unlock()
	return bufSigReleased
}

func hSpawnGated(ctx context.Context) (string, error) {
	<-getBufSigChan()
	_, err := workflow.FromContext(ctx).StepOpts("dyn", hGate,
		[]workflow.StepOption{workflow.WaitForSignal("go")})
	if err != nil {
		return "", err
	}
	return "spawned", nil
}

func waitForStepStatus(t *testing.T, h *wfHarness, dagID, stepID string, want workflow.StepStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, _, err := h.wf.Store.GetStep(context.Background(), dagID, stepID)
		if err == nil && rec.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec, _, _ := h.wf.Store.GetStep(context.Background(), dagID, stepID)
	t.Fatalf("step %s never reached %s (status=%s)", stepID, want, rec.Status)
}

func TestSignal_E2E_ApprovalFlow(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDeploy)

	dag := workflow.New()
	build := dag.Step("build", hAdd, 20, 22)
	deploy := dag.Step("deploy", hDeploy, workflow.SignalRef("approval"), build.Ref())
	audit := dag.Step("audit", hAdd, 1, 2) // independent branch, no gate

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	// The ungated branch and the dep both finish while deploy stays pending.
	waitForStepDone(t, h, dag.ID(), "build", 5*time.Second)
	waitForStepDone(t, h, dag.ID(), "audit", 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	rec, _, err := h.wf.Store.GetStep(ctx, dag.ID(), "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != workflow.StatusPending {
		t.Fatalf("deploy must wait for the signal, got %s", rec.Status)
	}

	// Observability: the signal shows as awaited with deploy waiting.
	infos, err := workflow.ListSignalInfo(ctx, h.wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "approval" || infos[0].Delivered ||
		len(infos[0].Waiting) != 1 || infos[0].Waiting[0] != "deploy" {
		t.Fatalf("signal info: %+v", infos)
	}

	// The human approves.
	delivered, err := workflow.Signal(ctx, h.wf, dag.ID(), "approval", approval{Approver: "eugene"})
	if err != nil || !delivered {
		t.Fatalf("signal: delivered=%v err=%v", delivered, err)
	}
	got, err := workflow.Await[string](ctx, h.wf, dag.ID(), deploy)
	if err != nil {
		t.Fatal(err)
	}
	if got != "eugene deployed 42" {
		t.Errorf("deploy result: %q", got)
	}
	_ = audit

	// The whole DAG finalizes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		meta, _, _ := h.wf.Store.GetMeta(ctx, dag.ID())
		if meta.Status == workflow.DAGStatusDone {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	meta, _, _ := h.wf.Store.GetMeta(ctx, dag.ID())
	t.Fatalf("DAG status: want done, got %s", meta.Status)
}

func TestSignal_E2E_DurableAcrossRestart_CrossProcessSignal(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDeploy)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 1, 2)
	deploy := dag.Step("deploy", hDeploy, workflow.SignalRef("approval"), a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	waitForStepDone(t, h, dag.ID(), "a", 5*time.Second)

	// Kill the worker + scheduler, as if the process died while waiting.
	h.cancel()
	time.Sleep(200 * time.Millisecond)

	// A fresh instance (separate "process") sees the durable waiting state.
	wfB, err := workflow.NewFromNATS(ctx, h.nc, 1)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := workflow.ListSignalInfo(ctx, wfB, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Delivered || len(infos[0].Waiting) != 1 {
		t.Fatalf("waiting state must survive restart; got %+v", infos)
	}

	// Restart worker + scheduler on the fresh instance; deliver from it too.
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

	delivered, err := workflow.Signal(ctx, wfB, dag.ID(), "approval", approval{Approver: "ops"})
	if err != nil || !delivered {
		t.Fatalf("cross-process signal: delivered=%v err=%v", delivered, err)
	}
	got, err := workflow.Await[string](ctx, wfB, dag.ID(), deploy)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ops deployed 3" {
		t.Errorf("deploy result: %q", got)
	}
}

func TestSignal_E2E_BufferedForDynamicStep(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hSpawnGated)
	task.MustRegister(h.reg, hGate)
	release := resetBufSigChan()

	dag := workflow.New()
	dag.Step("parent", hSpawnGated)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	// Deliver the signal BEFORE the dynamic step exists, then let the handler
	// add it. The buffered record must satisfy the gate immediately.
	delivered, err := workflow.Signal(ctx, h.wf, dag.ID(), "go", nil)
	if err != nil || !delivered {
		t.Fatalf("signal: delivered=%v err=%v", delivered, err)
	}
	close(release)

	got, err := workflow.AwaitByID[string](ctx, h.wf, dag.ID(), "dyn")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ran" {
		t.Errorf("dyn result: %q", got)
	}
}

func TestSignal_E2E_PauseInterplay(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hGate)

	dag := workflow.New()
	gate := dag.StepOpts("gate", hGate, []workflow.StepOption{workflow.WaitForSignal("go")})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	if err := workflow.Pause(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.Signal(ctx, h.wf, dag.ID(), "go", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	rec, _, _ := h.wf.Store.GetStep(ctx, dag.ID(), "gate")
	if rec.Status != workflow.StatusPending {
		t.Fatalf("paused DAG must not run the step, got %s", rec.Status)
	}
	if err := workflow.Resume(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	got, err := workflow.Await[string](ctx, h.wf, dag.ID(), gate)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ran" {
		t.Errorf("gate result: %q", got)
	}
	waitForStepStatus(t, h, dag.ID(), "gate", workflow.StatusDone, 5*time.Second)
}
