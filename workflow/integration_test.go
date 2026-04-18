package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// toggleElector lets an integration test flip leadership at runtime.
type toggleElector struct {
	mu     sync.Mutex
	leader bool
}

func (t *toggleElector) IsLeader() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.leader
}

func (t *toggleElector) set(v bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leader = v
}

// wfHarness extends testutil.SingleNode with workflow wiring.
type wfHarness struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	wf     *workflow.Workflow
	reg    *task.Registry
	worker *worker.Worker
	cancel context.CancelFunc
}

func setup(t *testing.T) *wfHarness {
	t.Helper()
	storeDir, err := os.MkdirTemp("", "ebind-wf-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storeDir) })

	node, err := embed.StartNode(embed.NodeConfig{
		ServerName: "wf-" + t.Name(),
		Port:       -1,
		StoreDir:   storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Shutdown)

	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)

	js, _ := jetstream.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	wf, err := workflow.NewFromNATS(ctx, nc, 1)
	if err != nil {
		t.Fatal(err)
	}
	reg := task.NewRegistry()

	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		AckWait:     2 * time.Second,
		MaxDeliver:  5,
		Backoff:     []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond},
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(runCtx) }()
	go func() { _ = wf.RunScheduler(runCtx) }()
	time.Sleep(200 * time.Millisecond) // allow consumers to bind

	return &wfHarness{nc: nc, js: js, wf: wf, reg: reg, worker: w, cancel: runCancel}
}

// --- Handler library for integration tests ---

func hAdd(_ context.Context, a, b int) (int, error) { return a + b, nil }
func hDouble(_ context.Context, x int) (int, error) { return x * 2, nil }
func hConcat(_ context.Context, s string, n int) (string, error) {
	return fmt.Sprintf("%s:%d", s, n), nil
}
func hFailMandatory(_ context.Context) (int, error) { return 0, errors.New("boom") }
func hFailOptional(_ context.Context) (int, error)  { return 0, errors.New("not critical") }

// Ordering observers — used to assert that After-dep step ran after its upstream.
var (
	orderMu    sync.Mutex
	orderSeen  []string
	orderStart = map[string]time.Time{}
	orderEnd   = map[string]time.Time{}
)

func hTrackedA(_ context.Context, ms int) (string, error) {
	orderMu.Lock()
	orderStart["a"] = time.Now()
	orderMu.Unlock()
	time.Sleep(time.Duration(ms) * time.Millisecond)
	orderMu.Lock()
	orderEnd["a"] = time.Now()
	orderSeen = append(orderSeen, "a")
	orderMu.Unlock()
	return "a-done", nil
}

func hTrackedB(_ context.Context) (string, error) {
	orderMu.Lock()
	orderStart["b"] = time.Now()
	orderSeen = append(orderSeen, "b")
	orderEnd["b"] = time.Now()
	orderMu.Unlock()
	return "b-done", nil
}

// resetOrdering clears the shared ordering state between tests.
func resetOrdering() {
	orderMu.Lock()
	orderSeen = nil
	orderStart = map[string]time.Time{}
	orderEnd = map[string]time.Time{}
	orderMu.Unlock()
}

var dynamicCounter atomic.Int32

func hDynamic(ctx context.Context, x int) (int, error) {
	// Adds a follow-up step during execution.
	d := workflow.FromContext(ctx)
	if d != nil && dynamicCounter.Add(1) == 1 {
		_, err := d.Step("dyn-followup", hDouble, x)
		if err != nil {
			return 0, err
		}
	}
	return x + 100, nil
}

var attemptCounter atomic.Int32

func hRetryExhaust(_ context.Context) (int, error) {
	attemptCounter.Add(1)
	return 0, errors.New("always fails")
}

func hNonRetryable(_ context.Context) (int, error) {
	return 0, &task.TaskError{Kind: "validation", Message: "bad", Retryable: false}
}

// --- Tests ---

func TestIntegration_AwaitByID_DifferentInstance(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	// Instance A: build + submit a DAG, capture (dagID, stepID) as strings,
	// then discard the workflow handle as if the process died.
	dag := workflow.New()
	_ = dag.Step("a", hAdd, 5, 7)
	b := dag.Step("b", hDouble, workflow.Ref{StepID: "a", Mode: workflow.RefModeRequired})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	dagID, stepID := dag.ID(), b.ID()

	// Instance B: construct a fresh Workflow against the same NATS. Only the
	// string IDs are shared — no *Step handle, no DAG builder state.
	wfB, err := workflow.NewFromNATS(ctx, h.nc, 1)
	if err != nil {
		t.Fatal(err)
	}

	result, err := workflow.AwaitByID[int](ctx, wfB, dagID, stepID)
	if err != nil {
		t.Fatalf("AwaitByID: %v", err)
	}
	if result != 24 { // (5+7)*2
		t.Errorf("got %d, want 24", result)
	}
}

func TestIntegration_Linear(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)
	task.MustRegister(h.reg, hConcat)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 2, 3)
	b := dag.Step("b", hDouble, a.Ref())
	c := dag.Step("c", hConcat, "result", b.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	result, err := workflow.Await[string](ctx, h.wf, dag.ID(), c)
	if err != nil {
		t.Fatal(err)
	}
	if result != "result:10" {
		t.Errorf("got %q", result)
	}

	// Debug() snapshot should reflect a terminal DAG with all steps done
	// and non-zero execution durations.
	dbg, err := workflow.Debug(ctx, h.wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if dbg.Counts[workflow.StatusDone] != 3 {
		t.Errorf("Debug counts: done=%d, want 3 (full=%+v)", dbg.Counts[workflow.StatusDone], dbg.Counts)
	}
	for _, s := range dbg.Steps {
		if s.ExecDuration <= 0 {
			t.Errorf("step %s ExecDuration = %v, want > 0", s.StepID, s.ExecDuration)
		}
	}
	if len(dbg.Blockers) != 0 {
		t.Errorf("expected no blockers, got %v", dbg.Blockers)
	}
}

func TestIntegration_FanOutFanIn(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)
	task.MustRegister(h.reg, hConcat)

	dag := workflow.New()
	a := dag.Step("a", hDouble, 5)
	b := dag.Step("b", hDouble, 10)
	c := dag.Step("c", hConcat, "combined", a.Ref()) // Waits on a
	_ = b
	_ = c

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	cResult, err := workflow.Await[string](ctx, h.wf, dag.ID(), c)
	if err != nil {
		t.Fatal(err)
	}
	if cResult != "combined:10" {
		t.Errorf("got %q", cResult)
	}
}

func TestIntegration_MandatoryFailure_CascadesSkip(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hFailMandatory)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New(workflow.WithRetry(task.NoRetryPolicy()))
	a := dag.Step("a", hFailMandatory)
	b := dag.Step("b", hDouble, a.Ref()) // b depends on a (Required) — should skip when a fails
	c := dag.Step("c", hDouble, b.Ref()) // c depends on b (Required) — transitive skip

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	_, err := workflow.Await[int](ctx, h.wf, dag.ID(), a)
	if !errors.Is(err, workflow.ErrStepFailed) {
		t.Errorf("a should fail: %v", err)
	}
	_, err = workflow.Await[int](ctx, h.wf, dag.ID(), b)
	if !errors.Is(err, workflow.ErrStepSkipped) {
		t.Errorf("b should be skipped: %v", err)
	}
	_, err = workflow.Await[int](ctx, h.wf, dag.ID(), c)
	if !errors.Is(err, workflow.ErrStepSkipped) {
		t.Errorf("c should be skipped (transitive): %v", err)
	}
	// DAG should be failed.
	meta, _, err := workflow.DAGInfo(ctx, h.wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != workflow.DAGStatusFailed {
		t.Errorf("DAG status: %s, want failed", meta.Status)
	}
}

func TestIntegration_OptionalFailure_RefOrDefault_Substitutes(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hFailOptional)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.StepOpts("a", hFailOptional, []workflow.StepOption{workflow.Optional()})
	// D takes an int; a.RefOrDefault(99) substitutes 99 if a fails.
	d := dag.Step("d", hDouble, a.RefOrDefault(99))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	result, err := workflow.Await[int](ctx, h.wf, dag.ID(), d)
	if err != nil {
		t.Fatal(err)
	}
	if result != 198 {
		t.Errorf("got %d, want 198 (double of default 99)", result)
	}
}

func TestIntegration_RetryExhaustion(t *testing.T) {
	h := setup(t)
	attemptCounter.Store(0)
	task.MustRegister(h.reg, hRetryExhaust)

	policy := task.RetryPolicy{
		InitialInterval:    50 * time.Millisecond,
		BackoffCoefficient: 1.0,
		MaximumInterval:    time.Second,
		MaximumAttempts:    3,
	}
	dag := workflow.New(workflow.WithRetry(policy))
	a := dag.Step("a", hRetryExhaust)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	_, err := workflow.Await[int](ctx, h.wf, dag.ID(), a)
	if !errors.Is(err, workflow.ErrStepFailed) {
		t.Errorf("want ErrStepFailed, got %v", err)
	}
	if attempt := attemptCounter.Load(); attempt != 3 {
		t.Errorf("want 3 attempts, got %d", attempt)
	}
}

func TestIntegration_NonRetryable_FailsImmediately(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hNonRetryable)

	// Policy allows 5 attempts, but validation-kind is non-retryable.
	policy := task.RetryPolicy{
		InitialInterval:        50 * time.Millisecond,
		BackoffCoefficient:     1.0,
		MaximumInterval:        time.Second,
		MaximumAttempts:        5,
		NonRetryableErrorKinds: []string{"validation"},
	}
	dag := workflow.New(workflow.WithRetry(policy))
	a := dag.Step("a", hNonRetryable)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	_, err := workflow.Await[int](ctx, h.wf, dag.ID(), a)
	if !errors.Is(err, workflow.ErrStepFailed) {
		t.Errorf("want ErrStepFailed, got %v", err)
	}
}

// setupWithElector builds a harness but with a caller-provided leader elector
// and exposes the worker context so tests can verify leadership-driven behavior.
func setupWithElector(t *testing.T, elector workflow.LeaderElector, sweepInterval time.Duration) *wfHarness {
	t.Helper()
	storeDir, err := os.MkdirTemp("", "ebind-wf-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storeDir) })

	node, err := embed.StartNode(embed.NodeConfig{
		ServerName: "wf-" + t.Name(),
		Port:       -1,
		StoreDir:   storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Shutdown)

	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)

	js, _ := jetstream.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	wf, err := workflow.NewFromNATS(ctx, nc, 1)
	if err != nil {
		t.Fatal(err)
	}
	wf.Elector = elector
	wf.SweepCheckInterval = sweepInterval
	wf.SweepTimeout = 10 * time.Second
	reg := task.NewRegistry()

	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		AckWait:     2 * time.Second,
		MaxDeliver:  5,
		Backoff:     []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond},
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(runCtx) }()
	go func() { _ = wf.RunScheduler(runCtx) }()
	time.Sleep(200 * time.Millisecond)

	return &wfHarness{nc: nc, js: js, wf: wf, reg: reg, worker: w, cancel: runCancel}
}

func TestIntegration_Sweep_RecoversStrandedStep(t *testing.T) {
	elector := &toggleElector{leader: false}
	h := setupWithElector(t, elector, 100*time.Millisecond)

	task.MustRegister(h.reg, hDouble)

	// Submit a two-step DAG: a → b. Roots are enqueued by Submit bypassing the
	// scheduler, so a runs even without a leader.
	dag := workflow.New()
	a := dag.Step("a", hDouble, 5)
	b := dag.Step("b", hDouble, a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	// While elector is false: a runs and hook writes Done. Completion event is
	// published to the DAG events stream but every scheduler worker Naks it.
	// Therefore b is never enqueued via the event path.
	// Wait for a to complete — verify by polling its step record.
	aDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(aDeadline) {
		rec, _, err := h.wf.Store.GetStep(ctx, dag.ID(), "a")
		if err == nil && rec.Status == workflow.StatusDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec, _, _ := h.wf.Store.GetStep(ctx, dag.ID(), "a")
	if rec.Status != workflow.StatusDone {
		t.Fatalf("a never completed; status=%s", rec.Status)
	}
	// Confirm b is still stranded.
	bRec, _, _ := h.wf.Store.GetStep(ctx, dag.ID(), "b")
	if bRec.Status != workflow.StatusPending {
		t.Fatalf("b should be pending while non-leader; status=%s", bRec.Status)
	}

	// Flip to leader — sweep should detect the edge and enqueue b.
	elector.set(true)

	result, err := workflow.Await[int](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatalf("b never completed after leader acquire: %v", err)
	}
	if result != 20 {
		t.Errorf("b result = %d, want 20", result)
	}
}

func TestIntegration_After_Ordering(t *testing.T) {
	resetOrdering()
	h := setup(t)
	task.MustRegister(h.reg, hTrackedA)
	task.MustRegister(h.reg, hTrackedB)

	dag := workflow.New()
	a := dag.Step("a", hTrackedA, 150) // takes 150ms
	// b has no data dep on a; uses After() for pure temporal ordering.
	b := dag.StepOpts("b", hTrackedB, []workflow.StepOption{workflow.After(a)})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	_, err := workflow.Await[string](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatal(err)
	}

	orderMu.Lock()
	seen := append([]string(nil), orderSeen...)
	aEnd := orderEnd["a"]
	bStart := orderStart["b"]
	orderMu.Unlock()

	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Errorf("expected order [a, b], got %v", seen)
	}
	if !bStart.After(aEnd) {
		t.Errorf("b started before a ended: bStart=%v aEnd=%v", bStart, aEnd)
	}
}

func TestIntegration_After_CascadeSkips(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hFailMandatory)
	task.MustRegister(h.reg, hTrackedB)

	dag := workflow.New(workflow.WithRetry(task.NoRetryPolicy()))
	a := dag.Step("a", hFailMandatory)
	b := dag.StepOpts("b", hTrackedB, []workflow.StepOption{workflow.After(a)})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	_, err := workflow.Await[int](ctx, h.wf, dag.ID(), a)
	if !errors.Is(err, workflow.ErrStepFailed) {
		t.Errorf("a: want ErrStepFailed, got %v", err)
	}
	_, err = workflow.Await[string](ctx, h.wf, dag.ID(), b)
	if !errors.Is(err, workflow.ErrStepSkipped) {
		t.Errorf("b: want ErrStepSkipped, got %v", err)
	}
}

func TestIntegration_AfterAny_RunsRegardless(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hFailMandatory)
	task.MustRegister(h.reg, hTrackedB)

	dag := workflow.New(workflow.WithRetry(task.NoRetryPolicy()))
	a := dag.Step("a", hFailMandatory)
	// b uses AfterAny — runs even if a fails.
	b := dag.StepOpts("b", hTrackedB, []workflow.StepOption{workflow.AfterAny(a)})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	// a fails.
	_, err := workflow.Await[int](ctx, h.wf, dag.ID(), a)
	if !errors.Is(err, workflow.ErrStepFailed) {
		t.Errorf("a should fail, got %v", err)
	}
	// b must still run and return its own result.
	result, err := workflow.Await[string](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatalf("b should run regardless of a's failure: %v", err)
	}
	if result != "b-done" {
		t.Errorf("b result: %q, want b-done", result)
	}
}

func TestIntegration_Dynamic_StepAdded(t *testing.T) {
	h := setup(t)
	dynamicCounter.Store(0)
	task.MustRegister(h.reg, hDynamic)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	_ = dag.Step("parent", hDynamic, 7)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}
	// Poll DAGInfo to observe "dyn-followup" step being added + completed.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		meta, steps, err := workflow.DAGInfo(ctx, h.wf, dag.ID())
		if err != nil {
			continue
		}
		if meta.Status == workflow.DAGStatusDone {
			// All steps done — check dyn-followup exists.
			for _, s := range steps {
				if s.StepID == "dyn-followup" && s.Status == workflow.StatusDone {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	meta, steps, _ := workflow.DAGInfo(ctx, h.wf, dag.ID())
	t.Fatalf("dynamic step did not complete; meta=%+v steps=%+v", meta, steps)
}
