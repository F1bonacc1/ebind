package worker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/internal/testutil"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

// --- Handlers. Package-scoped so their canonical names are stable; their
// state is keyed by the id argument so tests never share it.

var hbRunCounts sync.Map // id -> *atomic.Int32

var hbIDSeq atomic.Int64

// hbID returns an id no other test run uses, so per-id handler state starts
// empty even under -count=N.
func hbID(prefix string) string { return fmt.Sprintf("%s-%d", prefix, hbIDSeq.Add(1)) }

func hbRuns(id string) *atomic.Int32 {
	v, _ := hbRunCounts.LoadOrStore(id, new(atomic.Int32))
	return v.(*atomic.Int32)
}

// hbLong runs 3.5s, well past the 1s AckWait its tests use.
func hbLong(ctx context.Context, id string) (string, error) {
	hbRuns(id).Add(1)
	select {
	case <-time.After(3500 * time.Millisecond):
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// blockState drives hbBlockFirst: the first run blocks until its context
// ends and reports why; later runs succeed at once.
type blockState struct {
	runs     atomic.Int32
	started  chan struct{}
	canceled chan error     // context.Cause of the first run's context
	second   chan time.Time // when the second run started
}

var blockStates sync.Map // id -> *blockState

func newBlockState(id string) *blockState {
	s := &blockState{
		started:  make(chan struct{}),
		canceled: make(chan error, 1),
		second:   make(chan time.Time, 1),
	}
	blockStates.Store(id, s)
	return s
}

func hbBlockFirst(ctx context.Context, id string) (string, error) {
	v, _ := blockStates.Load(id)
	s := v.(*blockState)
	if s.runs.Add(1) == 1 {
		close(s.started)
		<-ctx.Done()
		s.canceled <- context.Cause(ctx)
		return "", ctx.Err()
	}
	select {
	case s.second <- time.Now():
	default:
	}
	return "second", nil
}

// slowState drives hbSlow: every run sleeps d, ignoring its context, like a
// handler that does not check for cancellation.
type slowState struct {
	d        time.Duration
	runs     atomic.Int32
	once     sync.Once
	started  chan struct{}
	finished atomic.Int32
}

var slowStates sync.Map // id -> *slowState

func newSlowState(id string, d time.Duration) *slowState {
	s := &slowState{d: d, started: make(chan struct{})}
	slowStates.Store(id, s)
	return s
}

func hbSlow(ctx context.Context, id string) (string, error) {
	v, _ := slowStates.Load(id)
	s := v.(*slowState)
	s.runs.Add(1)
	s.once.Do(func() { close(s.started) })
	time.Sleep(s.d)
	s.finished.Add(1)
	return "ok", nil
}

// hbNakDelay fails its first run after 150ms, long enough for several 50ms
// heartbeats, and succeeds on the next. It records each run's start.
var nakStarts sync.Map // id -> *startTimes

type startTimes struct {
	mu sync.Mutex
	ts []time.Time
}

func hbNakDelay(ctx context.Context, id string) (string, error) {
	v, _ := nakStarts.LoadOrStore(id, &startTimes{})
	st := v.(*startTimes)
	st.mu.Lock()
	st.ts = append(st.ts, time.Now())
	n := len(st.ts)
	st.mu.Unlock()
	if n == 1 {
		time.Sleep(150 * time.Millisecond)
		return "", errors.New("retry me")
	}
	return "ok", nil
}

func hbRegistry() *task.Registry {
	reg := task.NewRegistry()
	task.MustRegister(reg, hbLong)
	task.MustRegister(reg, hbBlockFirst)
	task.MustRegister(reg, hbSlow)
	task.MustRegister(reg, hbNakDelay)
	return reg
}

// --- Harness: one embedded node plus workers on connections of their own,
// so a test can kill or freeze one worker's connection.

type hbEnv struct {
	node   *embed.Node
	js     jetstream.JetStream
	client *client.Client
}

func startHBNode(t *testing.T) *hbEnv {
	t.Helper()
	node, err := embed.StartNode(embed.NodeConfig{ServerName: "hb-" + t.Name(), Port: -1, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Shutdown)
	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setupCancel()
	if err := stream.EnsureStreams(setupCtx, js, stream.Config{Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(ctx, nc, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &hbEnv{node: node, js: js, client: c}
}

type runningWorker struct {
	nc      *nats.Conn
	cancel  context.CancelFunc
	stopped chan struct{}
	err     error
}

var hbWorkerSeq atomic.Int32

// startHBWorker runs a worker connected to url and returns once it is fully
// started: its own concrete-claim durable has a pull waiting, so Run is past
// startup and an injected failure hits a running worker, not its setup.
func startHBWorker(t *testing.T, env *hbEnv, url string, opts worker.Options) *runningWorker {
	t.Helper()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.WorkerID == "" {
		opts.WorkerID = fmt.Sprintf("hb-%d", hbWorkerSeq.Add(1))
	}
	w, err := worker.New(nc, hbRegistry(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rw := &runningWorker{nc: nc, cancel: cancel, stopped: make(chan struct{})}
	go func() {
		rw.err = w.Run(ctx)
		close(rw.stopped)
	}()
	t.Cleanup(func() {
		if !rw.stop(15 * time.Second) {
			t.Error("worker did not stop")
		}
		nc.Close()
	})

	own := "ebind-targets-" + stream.TargetToken(worker.ConcreteTarget(opts.WorkerID))
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ready := false
		if ts, err := env.js.Stream(ctx, stream.TaskStream); err == nil {
			if c, err := ts.Consumer(ctx, own); err == nil {
				if info, err := c.Info(ctx); err == nil && info.NumWaiting > 0 {
					ready = true
				}
			}
		}
		cancel()
		if ready {
			return rw
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s not ready after 10s", opts.WorkerID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop cancels Run and waits up to timeout for it to return.
func (rw *runningWorker) stop(timeout time.Duration) bool {
	rw.cancel()
	select {
	case <-rw.stopped:
		return true
	case <-time.After(timeout):
		return false
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for %s", timeout, what)
	}
}

func awaitString(t *testing.T, fut *client.Future, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	got, err := client.Await[string](ctx, fut)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	return got
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// --- Tests.

// A live worker keeps its task for as long as the handler runs, even when
// that is several AckWait periods.
func TestWorker_Heartbeat_LongHandlerDeliveredOnce(t *testing.T) {
	logs := &syncBuffer{}
	h := testutil.SingleNode(t, worker.Options{
		AckWait: time.Second,
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	})
	task.MustRegister(h.Reg, hbLong)

	id := hbID("long-once")
	fut, err := client.Enqueue(h.Client, hbLong, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := awaitString(t, fut, 10*time.Second); got != "done" {
		t.Fatalf("got %q", got)
	}
	time.Sleep(1500 * time.Millisecond) // a redelivery would surface within one AckWait
	if n := hbRuns(id).Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1: heartbeats did not hold the task\nworker log:\n%s", n, logs)
	}
}

// Control for the test above: with heartbeats disabled the same handler is
// redelivered while it runs, so the test above does detect missing heartbeats.
func TestWorker_Heartbeat_DisabledRedeliversLongHandler(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{AckWait: time.Second, HeartbeatInterval: -1})
	task.MustRegister(h.Reg, hbLong)

	id := hbID("long-no-heartbeat")
	fut, err := client.Enqueue(h.Client, hbLong, id)
	if err != nil {
		t.Fatal(err)
	}
	awaitString(t, fut, 10*time.Second)
	if n := hbRuns(id).Load(); n < 2 {
		t.Fatalf("handler ran %d times; without heartbeats a 3.5s handler must be redelivered under a 1s AckWait", n)
	}
}

// A worker that dies mid-handler stops heartbeating; another worker gets the
// task within about AckWait, and the dead worker never settles it.
func TestWorker_Heartbeat_DeadWorkerRedeliveredWithinAckWait(t *testing.T) {
	const ackWait = 2 * time.Second
	env := startHBNode(t)
	id := hbID("dead-worker")
	s := newBlockState(id)
	hookA, hookB := &captureHook{}, &captureHook{}

	a := startHBWorker(t, env, env.node.ClientURL(), worker.Options{AckWait: ackWait, StepHook: hookA})
	fut, err := client.Enqueue(env.client, hbBlockFirst, id)
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, s.started, 5*time.Second, "the first run to start")
	startHBWorker(t, env, env.node.ClientURL(), worker.Options{AckWait: ackWait, StepHook: hookB})

	died := time.Now()
	a.nc.Close() // no more heartbeats or settlements from worker A

	if got := awaitString(t, fut, 10*time.Second); got != "second" {
		t.Fatalf("got %q, want the redelivery's result", got)
	}
	if d := (<-s.second).Sub(died); d > ackWait+time.Second {
		t.Errorf("redelivered %v after worker A died; want within about AckWait (%v)", d, ackWait)
	}
	select {
	case cause := <-s.canceled:
		if !errors.Is(cause, worker.ErrLeaseLost) {
			t.Errorf("first run's context cause = %v, want ErrLeaseLost", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker A never canceled its handler after losing the lease")
	}
	if n := hookA.done.Load() + hookA.failed.Load(); n != 0 {
		t.Errorf("worker A ran its StepHook %d times after losing the lease", n)
	}
	if n := hookB.done.Load(); n != 1 {
		t.Errorf("worker B StepHook done = %d, want 1", n)
	}
}

// A connection that silently stops passing traffic must still cost the
// worker its lease. Here every publish "succeeds" into the frozen socket, so
// only a confirmed heartbeat can tell the cut apart from a healthy link.
func TestWorker_Heartbeat_FrozenConnectionLosesLease(t *testing.T) {
	const ackWait = 2 * time.Second
	env := startHBNode(t)
	proxy := testutil.StartProxy(t, env.node.ClientURL())
	id := hbID("frozen")
	s := newBlockState(id)
	hookA, hookB := &captureHook{}, &captureHook{}
	logs := &syncBuffer{}

	startHBWorker(t, env, proxy.URL(), worker.Options{
		AckWait:  ackWait,
		StepHook: hookA,
		Logger:   slog.New(slog.NewTextHandler(logs, nil)),
	})
	fut, err := client.Enqueue(env.client, hbBlockFirst, id)
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, s.started, 5*time.Second, "the first run to start")
	startHBWorker(t, env, env.node.ClientURL(), worker.Options{AckWait: ackWait, StepHook: hookB})

	frozen := time.Now()
	proxy.Freeze()

	select {
	case cause := <-s.canceled:
		if !errors.Is(cause, worker.ErrLeaseLost) {
			t.Fatalf("handler context cause = %v, want ErrLeaseLost", cause)
		}
		if d := time.Since(frozen); d > ackWait+time.Second {
			t.Errorf("handler canceled %v after the freeze; want within about AckWait (%v)", d, ackWait)
		}
	case <-time.After(ackWait + 3*time.Second):
		t.Fatal("handler context was not canceled after the connection froze")
	}
	if got := awaitString(t, fut, 10*time.Second); got != "second" {
		t.Fatalf("got %q, want the redelivery's result", got)
	}
	if n := hookA.done.Load() + hookA.failed.Load(); n != 0 {
		t.Errorf("worker A ran its StepHook %d times after losing the lease", n)
	}
	if n := hookB.done.Load(); n != 1 {
		t.Errorf("worker B StepHook done = %d, want 1", n)
	}
	out := logs.String()
	if n := strings.Count(out, "task heartbeat failed"); n != 1 {
		t.Errorf("logged %d heartbeat failures, want the first of the run only:\n%s", n, out)
	}
	if !strings.Contains(out, "task lease lost") {
		t.Errorf("lost lease not logged:\n%s", out)
	}
}

// A heartbeat that reaches JetStream after NakWithDelay restarts the full
// AckWait clock and overrides the retry delay. The lease stops heartbeating
// before it settles, so retries come after the retry delay.
func TestWorker_Heartbeat_NakDelayNotOverridden(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{
		Concurrency:       8,
		AckWait:           3 * time.Second,
		HeartbeatInterval: 50 * time.Millisecond,
		DefaultRetryPolicy: &task.RetryPolicy{
			InitialInterval:    200 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumInterval:    200 * time.Millisecond,
			MaximumAttempts:    3,
		},
	})
	task.MustRegister(h.Reg, hbNakDelay)

	ids := make([]string, 8)
	futs := make([]*client.Future, len(ids))
	for i := range ids {
		ids[i] = hbID("nak")
		fut, err := client.Enqueue(h.Client, hbNakDelay, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		futs[i] = fut
	}
	for _, fut := range futs {
		awaitString(t, fut, 15*time.Second)
	}
	for _, id := range ids {
		v, _ := nakStarts.Load(id)
		st := v.(*startTimes)
		st.mu.Lock()
		gap := st.ts[1].Sub(st.ts[0])
		st.mu.Unlock()
		// First run 150ms + 200ms retry delay; a late heartbeat would push the
		// retry out to the 3s AckWait.
		if gap > 1500*time.Millisecond {
			t.Errorf("%s retried %v after its first run started; want about 350ms (retry delay overridden by a heartbeat?)", id, gap)
		}
	}
}

// Handlers still draining after shutdown starts keep their tasks, even when
// the drain outlasts AckWait.
func TestWorker_Heartbeat_DrainKeepsLease(t *testing.T) {
	const ackWait = time.Second
	env := startHBNode(t)
	id := hbID("drain")
	s := newSlowState(id, 2500*time.Millisecond)

	a := startHBWorker(t, env, env.node.ClientURL(), worker.Options{AckWait: ackWait, ShutdownGrace: 10 * time.Second})
	if _, err := client.Enqueue(env.client, hbSlow, id); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, s.started, 5*time.Second, "the handler to start")
	startHBWorker(t, env, env.node.ClientURL(), worker.Options{AckWait: ackWait}) // would take a redelivery

	if !a.stop(15 * time.Second) {
		t.Fatal("worker A did not drain")
	}
	if a.err != nil {
		t.Fatalf("Run: %v", a.err)
	}
	time.Sleep(1500 * time.Millisecond) // a redelivery would surface within one AckWait
	if n := s.runs.Load(); n != 1 {
		t.Fatalf("handler ran %d times; a draining handler lost its task", n)
	}
}

// When Run gives up on a handler at ShutdownGrace, the task goes back to
// JetStream, and the abandoned handler, finishing later, settles nothing.
func TestWorker_Heartbeat_ShutdownTimeoutReleasesTask(t *testing.T) {
	const ackWait = time.Second
	env := startHBNode(t)
	id := hbID("abandon")
	s := newSlowState(id, 3*time.Second)
	hookA, hookB := &captureHook{}, &captureHook{}

	a := startHBWorker(t, env, env.node.ClientURL(), worker.Options{
		AckWait:       ackWait,
		ShutdownGrace: 300 * time.Millisecond,
		StepHook:      hookA,
	})
	fut, err := client.Enqueue(env.client, hbSlow, id)
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, s.started, 5*time.Second, "the handler to start")
	startHBWorker(t, env, env.node.ClientURL(), worker.Options{AckWait: ackWait, StepHook: hookB})

	if !a.stop(5 * time.Second) {
		t.Fatal("Run did not return after ShutdownGrace")
	}
	if a.err == nil {
		t.Fatal("Run returned nil; want a shutdown-timeout error")
	}
	if got := awaitString(t, fut, 10*time.Second); got != "ok" {
		t.Fatalf("got %q", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.finished.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := s.runs.Load(); n != 2 {
		t.Errorf("handler ran %d times, want 2 (abandoned run + redelivery)", n)
	}
	if n := hookA.done.Load() + hookA.failed.Load(); n != 0 {
		t.Errorf("abandoned handler ran worker A's StepHook %d times", n)
	}
	if n := hookB.done.Load(); n != 1 {
		t.Errorf("worker B StepHook done = %d, want 1", n)
	}
}

// subscribeClaim reuses an existing per-claim durable; a changed AckWait must
// still reach it, or targeted tasks keep the old value.
func TestWorker_ClaimDurable_FollowsAckWait(t *testing.T) {
	env := startHBNode(t)
	claimDurable := "ebind-targets-" + stream.TargetToken("primary")
	ackWaitOf := func(name string) time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ts, err := env.js.Stream(ctx, stream.TaskStream)
		if err != nil {
			return 0
		}
		c, err := ts.Consumer(ctx, name)
		if err != nil {
			return 0
		}
		return c.CachedInfo().Config.AckWait
	}
	waitAckWait := func(name string, want time.Duration) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		var got time.Duration
		for time.Now().Before(deadline) {
			if got = ackWaitOf(name); got == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s AckWait = %v, want %v", name, got, want)
	}

	first := startHBWorker(t, env, env.node.ClientURL(), worker.Options{
		AckWait: 30 * time.Second,
		Claims:  worker.StaticClaims{"primary"},
	})
	waitAckWait(claimDurable, 30*time.Second)
	if !first.stop(15 * time.Second) {
		t.Fatal("first worker did not stop")
	}

	startHBWorker(t, env, env.node.ClientURL(), worker.Options{
		AckWait: 5 * time.Second,
		Claims:  worker.StaticClaims{"primary"},
	})
	waitAckWait(claimDurable, 5*time.Second)
	waitAckWait("ebind-worker", 5*time.Second)
}

func TestNew_RejectsHeartbeatIntervalNotBelowAckWait(t *testing.T) {
	env := startHBNode(t)
	nc, err := nats.Connect(env.node.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := worker.New(nc, task.NewRegistry(), worker.Options{AckWait: time.Second, HeartbeatInterval: time.Second}); err == nil {
		t.Error("New accepted HeartbeatInterval == AckWait")
	}
	if _, err := worker.New(nc, task.NewRegistry(), worker.Options{AckWait: time.Second, HeartbeatInterval: -1}); err != nil {
		t.Errorf("New rejected disabled heartbeats: %v", err)
	}
}
