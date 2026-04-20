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

func hWhere(ctx context.Context) (string, error) {
	return workflow.CurrentWorkerID(ctx), nil
}

func isWorkerID(id string) bool {
	return id == "w1" || id == "w2"
}

type mutableClaims struct {
	mu     sync.Mutex
	claims []string
}

func (m *mutableClaims) Claims(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.claims))
	copy(out, m.claims)
	return out, nil
}

func (m *mutableClaims) Set(claims ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims = append([]string(nil), claims...)
}

type multiWorkerHarness struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	wf     *workflow.Workflow
	regs   []*task.Registry
	cancel context.CancelFunc
}

type workerSpec struct {
	id     string
	claims *mutableClaims
}

func setupMultiWorker(t *testing.T, specs ...workerSpec) *multiWorkerHarness {
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

	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)

	regs := make([]*task.Registry, 0, len(specs))
	for _, spec := range specs {
		reg := task.NewRegistry()
		w, err := worker.New(nc, reg, worker.Options{
			Concurrency:          4,
			AckWait:              2 * time.Second,
			Backoff:              []time.Duration{25 * time.Millisecond},
			StepHook:             wf.Hook(),
			Middleware:           []worker.Middleware{wf.ContextMiddleware()},
			WorkerID:             spec.id,
			Claims:               spec.claims,
			ClaimRefreshInterval: 50 * time.Millisecond,
			ClaimRetryDelay:      25 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		regs = append(regs, reg)
		go func(workerInstance *worker.Worker) { _ = workerInstance.Run(runCtx) }(w)
	}

	go func() { _ = wf.RunScheduler(runCtx) }()
	time.Sleep(300 * time.Millisecond)

	return &multiWorkerHarness{nc: nc, js: js, wf: wf, regs: regs, cancel: runCancel}
}

func (h *multiWorkerHarness) register(t *testing.T, fn any) {
	t.Helper()
	for _, reg := range h.regs {
		task.MustRegister(reg, fn)
	}
}

func newBlockedWhereHandler() (func(context.Context) (string, error), <-chan struct{}, chan<- struct{}) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fn := func(ctx context.Context) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return workflow.CurrentWorkerID(ctx), nil
	}
	return fn, started, release
}

func newAddColocatedHereHandler() (func(context.Context) (string, error), <-chan struct{}, chan<- struct{}) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fn := func(ctx context.Context) (string, error) {
		d := workflow.FromContext(ctx)
		if d == nil {
			return "", errors.New("missing workflow context")
		}
		if _, err := d.StepOpts("child", hWhere, []workflow.StepOption{workflow.ColocateHere()}); err != nil {
			return "", err
		}
		once.Do(func() { close(started) })
		<-release
		return workflow.CurrentWorkerID(ctx), nil
	}
	return fn, started, release
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

func TestIntegration_OnTarget_RunsOnClaimOwner(t *testing.T) {
	primary := &mutableClaims{claims: []string{"primary"}}
	secondary := &mutableClaims{claims: []string{"secondary"}}
	h := setupMultiWorker(t,
		workerSpec{id: "w1", claims: primary},
		workerSpec{id: "w2", claims: secondary},
	)
	h.register(t, hWhere)

	dag := workflow.New()
	a := dag.StepOpts("a", hWhere, []workflow.StepOption{workflow.OnTarget("primary")})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	result, err := workflow.Await[string](ctx, h.wf, dag.ID(), a)
	if err != nil {
		t.Fatal(err)
	}
	if result != "w1" {
		t.Errorf("targeted step ran on %q, want w1", result)
	}
}

func TestIntegration_ColocateWith_UsesConcreteWorker(t *testing.T) {
	blockWhere, started, release := newBlockedWhereHandler()
	primary := &mutableClaims{claims: []string{"primary"}}
	secondary := &mutableClaims{claims: []string{"secondary"}}
	h := setupMultiWorker(t,
		workerSpec{id: "w1", claims: primary},
		workerSpec{id: "w2", claims: secondary},
	)
	h.register(t, blockWhere)
	h.register(t, hWhere)

	dag := workflow.New()
	a := dag.StepOpts("a", blockWhere, []workflow.StepOption{workflow.OnTarget("primary")})
	b := dag.StepOpts("b", hWhere, []workflow.StepOption{workflow.ColocateWith(a)})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	<-started
	primary.Set("secondary")
	secondary.Set("primary")
	time.Sleep(200 * time.Millisecond)
	close(release)

	aResult, err := workflow.Await[string](ctx, h.wf, dag.ID(), a)
	if err != nil {
		t.Fatal(err)
	}
	bResult, err := workflow.Await[string](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatal(err)
	}
	if !isWorkerID(aResult) {
		t.Fatalf("a ran on %q, want one of w1/w2", aResult)
	}
	if bResult != aResult {
		t.Errorf("colocated step ran on %q, want %q", bResult, aResult)
	}
}

func TestIntegration_FollowTargetOf_ReResolvesLogicalTarget(t *testing.T) {
	blockWhere, started, release := newBlockedWhereHandler()
	primary := &mutableClaims{claims: []string{"primary"}}
	secondary := &mutableClaims{claims: []string{"secondary"}}
	h := setupMultiWorker(t,
		workerSpec{id: "w1", claims: primary},
		workerSpec{id: "w2", claims: secondary},
	)
	h.register(t, blockWhere)
	h.register(t, hWhere)

	dag := workflow.New()
	a := dag.StepOpts("a", blockWhere, []workflow.StepOption{workflow.OnTarget("primary")})
	b := dag.StepOpts("b", hWhere, []workflow.StepOption{workflow.FollowTargetOf(a)})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	<-started
	primary.Set("secondary")
	secondary.Set("primary")
	time.Sleep(200 * time.Millisecond)
	close(release)

	aResult, err := workflow.Await[string](ctx, h.wf, dag.ID(), a)
	if err != nil {
		t.Fatal(err)
	}
	bResult, err := workflow.Await[string](ctx, h.wf, dag.ID(), b)
	if err != nil {
		t.Fatal(err)
	}
	if !isWorkerID(aResult) {
		t.Fatalf("a ran on %q, want one of w1/w2", aResult)
	}
	if bResult != "w2" {
		t.Errorf("follow-target step ran on %q, want w2", bResult)
	}
}

func TestIntegration_ColocateHere_BindsDynamicStepToCurrentWorker(t *testing.T) {
	addColocatedHere, started, release := newAddColocatedHereHandler()
	primary := &mutableClaims{claims: []string{"primary"}}
	secondary := &mutableClaims{claims: []string{"secondary"}}
	h := setupMultiWorker(t,
		workerSpec{id: "w1", claims: primary},
		workerSpec{id: "w2", claims: secondary},
	)
	h.register(t, addColocatedHere)
	h.register(t, hWhere)

	dag := workflow.New()
	parent := dag.StepOpts("parent", addColocatedHere, []workflow.StepOption{workflow.OnTarget("primary")})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	<-started
	primary.Set("secondary")
	secondary.Set("primary")
	time.Sleep(200 * time.Millisecond)
	close(release)

	parentResult, err := workflow.Await[string](ctx, h.wf, dag.ID(), parent)
	if err != nil {
		t.Fatal(err)
	}
	childResult, err := workflow.AwaitByID[string](ctx, h.wf, dag.ID(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if !isWorkerID(parentResult) {
		t.Fatalf("parent ran on %q, want one of w1/w2", parentResult)
	}
	if childResult != parentResult {
		t.Errorf("ColocateHere child ran on %q, want %q", childResult, parentResult)
	}
}

func TestIntegration_Cancel_StopsNewWorkButLetsRunningFinish(t *testing.T) {
	blockWhere, started, release := newBlockedWhereHandler()
	h := setup(t)
	task.MustRegister(h.reg, blockWhere)
	task.MustRegister(h.reg, hWhere)

	dag := workflow.New()
	a := dag.Step("a", blockWhere)
	b := dag.StepOpts("b", hWhere, []workflow.StepOption{workflow.After(a)})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	<-started
	if err := workflow.Cancel(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}

	metaDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(metaDeadline) {
		meta, _, err := workflow.DAGInfo(ctx, h.wf, dag.ID())
		if err == nil && meta.Status == workflow.DAGStatusCanceled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	close(release)

	if _, err := workflow.Await[string](ctx, h.wf, dag.ID(), a); err != nil {
		t.Fatalf("running step should still finish: %v", err)
	}
	if _, err := workflow.Await[string](ctx, h.wf, dag.ID(), b); !errors.Is(err, workflow.ErrStepCanceled) {
		t.Fatalf("downstream step should be canceled, got %v", err)
	}

	meta, steps, err := workflow.DAGInfo(ctx, h.wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != workflow.DAGStatusCanceled {
		t.Fatalf("meta status = %s, want canceled", meta.Status)
	}
	for _, step := range steps {
		if step.StepID == "b" && step.Status != workflow.StatusCanceled {
			t.Fatalf("b status = %s, want canceled", step.Status)
		}
	}
}

// --- Multi-worker placement stress tests ---
// These exercise the failure modes in commit 3d77032 where targeted tasks
// bounce across non-owning workers via NakWithDelay.

var targetedHits atomic.Int32
var targetedRanOn atomic.Value // stores string

func hTargetedCounter(ctx context.Context) (string, error) {
	targetedHits.Add(1)
	wid := workflow.CurrentWorkerID(ctx)
	targetedRanOn.Store(wid)
	return wid, nil
}

// TestIntegration_Targeted_SurvivesLargeNonOwnerPool verifies that targeted
// tasks are not silently dropped when they must traverse many non-owning
// workers before reaching the owner. With the pre-fix default of MaxDeliver=5
// and many bystanders, the expected non-owner NAK count exceeds the budget
// and NATS silently stops redelivering — the task disappears. We submit many
// tasks so that even a small per-task drop probability turns into a near-
// certain failure without the fix.
func TestIntegration_Targeted_SurvivesLargeNonOwnerPool(t *testing.T) {
	targetedHits.Store(0)
	targetedRanOn.Store("")

	owner := &mutableClaims{claims: []string{"primary"}}
	specs := []workerSpec{{id: "owner", claims: owner}}
	for i := 0; i < 6; i++ {
		specs = append(specs, workerSpec{
			id:     fmt.Sprintf("bystander-%d", i),
			claims: &mutableClaims{claims: nil},
		})
	}
	h := setupMultiWorker(t, specs...)
	h.register(t, hTargetedCounter)

	const N = 25
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dags := make([]*workflow.DAG, 0, N)
	steps := make([]*workflow.Step, 0, N)
	for i := 0; i < N; i++ {
		dag := workflow.New()
		step := dag.StepOpts("s", hTargetedCounter, []workflow.StepOption{workflow.OnTarget("primary")})
		if err := dag.Submit(ctx, h.wf); err != nil {
			t.Fatal(err)
		}
		dags = append(dags, dag)
		steps = append(steps, step)
	}

	for i, dag := range dags {
		result, err := workflow.Await[string](ctx, h.wf, dag.ID(), steps[i])
		if err != nil {
			t.Fatalf("task %d disappeared in a 7-worker pool: %v", i, err)
		}
		if result != "owner" {
			t.Errorf("task %d ran on %q, want owner", i, result)
		}
	}
	if got := targetedHits.Load(); got != N {
		t.Errorf("handler hits: got %d, want %d", got, N)
	}

	dlqStream, err := h.js.Stream(ctx, stream.DLQStream)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := dlqStream.Info(ctx)
	if info.State.Msgs != 0 {
		t.Errorf("DLQ got %d messages; targeted nak loop leaked to DLQ", info.State.Msgs)
	}
}

var inflateAttempts atomic.Int32

func hTargetedFlaky(ctx context.Context) (string, error) {
	n := inflateAttempts.Add(1)
	if n < 2 {
		return "", errors.New("first attempt fails")
	}
	return workflow.CurrentWorkerID(ctx), nil
}

// TestIntegration_Targeted_AttemptNotInflatedByWrongTargetNaks verifies that
// when a targeted task has to bounce through several non-owning workers via
// NAK before reaching its owner, the owner's attempt counter still starts
// at 1 on first invocation. Otherwise a task with MaximumAttempts=2 that
// bounced through 2 non-owners would be out of retry budget before the
// handler ever ran.
func TestIntegration_Targeted_AttemptNotInflatedByWrongTargetNaks(t *testing.T) {
	inflateAttempts.Store(0)

	owner := &mutableClaims{claims: []string{"primary"}}
	specs := []workerSpec{{id: "owner", claims: owner}}
	for i := 0; i < 3; i++ {
		specs = append(specs, workerSpec{
			id:     fmt.Sprintf("bystander-%d", i),
			claims: &mutableClaims{claims: nil},
		})
	}
	h := setupMultiWorker(t, specs...)
	h.register(t, hTargetedFlaky)

	policy := task.RetryPolicy{
		InitialInterval:    50 * time.Millisecond,
		BackoffCoefficient: 1.0,
		MaximumInterval:    time.Second,
		MaximumAttempts:    2,
	}
	dag := workflow.New()
	step := dag.StepOpts("s", hTargetedFlaky, []workflow.StepOption{
		workflow.OnTarget("primary"),
		workflow.WithStepRetry(policy),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf); err != nil {
		t.Fatal(err)
	}

	result, err := workflow.Await[string](ctx, h.wf, dag.ID(), step)
	if err != nil {
		t.Fatalf("owner did not get fair retry budget: %v", err)
	}
	if result != "owner" {
		t.Errorf("ran on %q, want owner", result)
	}
	if got := inflateAttempts.Load(); got != 2 {
		t.Errorf("handler invocations: got %d, want 2 (fail-then-succeed)", got)
	}
}
