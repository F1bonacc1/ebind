package worker_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/internal/testutil"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

func echo(ctx context.Context, s string) (string, error) {
	return "echo:" + s, nil
}

func panicky(ctx context.Context) error {
	panic("oops")
}

var retryCount atomic.Int32

func flaky(ctx context.Context) (string, error) {
	n := retryCount.Add(1)
	if n < 3 {
		return "", errors.New("retry me")
	}
	return "succeeded", nil
}

func notRetryable(ctx context.Context) error {
	return &task.TaskError{Kind: "validation", Message: "bad input", Retryable: false}
}

func TestWorker_RoundTrip(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 2})
	task.MustRegister(h.Reg, echo)

	fut, err := client.Enqueue(h.Client, echo, "hello")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := client.Await[string](ctx, fut)
	if err != nil {
		t.Fatal(err)
	}
	if result != "echo:hello" {
		t.Errorf("got %q", result)
	}
}

func TestWorker_Panic_RecoveredAndDLQ(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{
		Concurrency: 2,
		MaxDeliver:  1,
		Backoff:     nil,
	})
	task.MustRegister(h.Reg, panicky)

	fut, err := client.Enqueue(h.Client, panicky)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = fut.Get(ctx, nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var te *task.TaskError
	if !errors.As(err, &te) || te.Kind != task.ErrKindPanic {
		t.Fatalf("want panic TaskError, got %v", err)
	}

	// Verify DLQ got an entry.
	dlqStream, err := h.JS.Stream(ctx, stream.DLQStream)
	if err != nil {
		t.Fatal(err)
	}
	// Give DLQ publish a moment.
	deadline := time.Now().Add(2 * time.Second)
	var msgs uint64
	for time.Now().Before(deadline) {
		info, _ := dlqStream.Info(ctx)
		msgs = info.State.Msgs
		if msgs > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if msgs == 0 {
		t.Error("DLQ has no messages after panic")
	}
}

func TestWorker_RetryThenSucceed(t *testing.T) {
	retryCount.Store(0)
	h := testutil.SingleNode(t, worker.Options{
		Concurrency: 1,
		MaxDeliver:  5,
		Backoff:     []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond},
		AckWait:     2 * time.Second,
	})
	task.MustRegister(h.Reg, flaky)

	fut, err := client.Enqueue(h.Client, flaky)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := client.Await[string](ctx, fut)
	if err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
	if result != "succeeded" {
		t.Errorf("got %q", result)
	}
	if retryCount.Load() < 3 {
		t.Errorf("handler invoked only %d times, want >=3", retryCount.Load())
	}
}

func TestWorker_NonRetryableError(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1, MaxDeliver: 5})
	task.MustRegister(h.Reg, notRetryable)

	fut, err := client.Enqueue(h.Client, notRetryable)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = fut.Get(ctx, nil)
	var te *task.TaskError
	if !errors.As(err, &te) {
		t.Fatalf("want TaskError, got %T: %v", err, err)
	}
	if te.Kind != "validation" {
		t.Errorf("want kind=validation, got %q", te.Kind)
	}

	_ = h
	_ = jetstream.ConsumerConfig{}
}

// Hook is a minimal StepHook capturing calls for assertions.
type captureHook struct {
	done       atomic.Int32
	failed     atomic.Int32
	lastTaskID string
	mu         sync.Mutex
}

func (c *captureHook) OnStepDone(_ context.Context, t *task.Task, _ []byte) error {
	c.mu.Lock()
	c.lastTaskID = t.ID
	c.mu.Unlock()
	c.done.Add(1)
	return nil
}
func (c *captureHook) OnStepFailed(_ context.Context, t *task.Task, _ *task.TaskError) error {
	c.mu.Lock()
	c.lastTaskID = t.ID
	c.mu.Unlock()
	c.failed.Add(1)
	return nil
}

func TestWorker_StepHook_OnSuccess(t *testing.T) {
	hook := &captureHook{}
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1, StepHook: hook})
	task.MustRegister(h.Reg, echo)

	fut, err := client.Enqueue(h.Client, echo, "hi")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Await[string](ctx, fut); err != nil {
		t.Fatal(err)
	}
	// hook may fire slightly after response; give it a beat.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hook.done.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if hook.done.Load() != 1 || hook.failed.Load() != 0 {
		t.Errorf("hook: done=%d failed=%d, want 1/0", hook.done.Load(), hook.failed.Load())
	}
}

func TestWorker_StepHook_OnFailure(t *testing.T) {
	hook := &captureHook{}
	h := testutil.SingleNode(t, worker.Options{
		Concurrency: 1, MaxDeliver: 1, Backoff: nil, StepHook: hook,
	})
	task.MustRegister(h.Reg, notRetryable)

	fut, err := client.Enqueue(h.Client, notRetryable)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := fut.Get(ctx, nil); err == nil {
		t.Fatal("want error")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hook.failed.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if hook.failed.Load() != 1 || hook.done.Load() != 0 {
		t.Errorf("hook: done=%d failed=%d, want 0/1", hook.done.Load(), hook.failed.Load())
	}
}

// Handler that counts attempts and always fails; task-level RetryPolicy limits to 2 tries.
var policyAttempts atomic.Int32

func policyFlaky(ctx context.Context) (string, error) {
	policyAttempts.Add(1)
	return "", errors.New("always fail")
}

func TestWorker_TaskLevelRetryPolicy_Overrides(t *testing.T) {
	policyAttempts.Store(0)
	h := testutil.SingleNode(t, worker.Options{
		Concurrency: 1,
		MaxDeliver:  10, // outer bound is generous
		AckWait:     time.Second,
		Backoff:     []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond},
	})
	task.MustRegister(h.Reg, policyFlaky)

	// Task carries a policy that caps at 2 attempts.
	policy := task.RetryPolicy{
		InitialInterval:    50 * time.Millisecond,
		BackoffCoefficient: 1.0,
		MaximumInterval:    time.Second,
		MaximumAttempts:    2,
	}
	fut, err := client.EnqueueOpts(h.Client, policyFlaky, client.EnqueueOptions{RetryPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fut.Get(ctx, nil); err == nil {
		t.Fatal("want failure")
	}
	if got := policyAttempts.Load(); got != 2 {
		t.Errorf("attempts: got %d, want 2 (capped by task policy, not worker MaxDeliver)", got)
	}
}

// TestWorker_MaxDeliverUnlimited_RetryPolicyBounds exercises the case where
// the worker's NATS-level MaxDeliver is -1 (unlimited) and the task-level
// RetryPolicy is the authoritative bound. The handler must be invoked exactly
// MaximumAttempts times, and the final failure must land in the DLQ.
func TestWorker_MaxDeliverUnlimited_RetryPolicyBounds(t *testing.T) {
	policyAttempts.Store(0)
	h := testutil.SingleNode(t, worker.Options{
		Concurrency: 1,
		MaxDeliver:  -1,
		AckWait:     time.Second,
		Backoff:     []time.Duration{50 * time.Millisecond},
	})
	task.MustRegister(h.Reg, policyFlaky)

	policy := task.RetryPolicy{
		InitialInterval:    50 * time.Millisecond,
		BackoffCoefficient: 1.0,
		MaximumInterval:    time.Second,
		MaximumAttempts:    3,
	}
	fut, err := client.EnqueueOpts(h.Client, policyFlaky, client.EnqueueOptions{RetryPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fut.Get(ctx, nil); err == nil {
		t.Fatal("want failure after RetryPolicy exhausted")
	}
	if got := policyAttempts.Load(); got != 3 {
		t.Errorf("attempts: got %d, want 3 (RetryPolicy is authoritative when MaxDeliver=-1)", got)
	}

	dlqStream, err := h.JS.Stream(ctx, stream.DLQStream)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var msgs uint64
	for time.Now().Before(deadline) {
		info, _ := dlqStream.Info(ctx)
		msgs = info.State.Msgs
		if msgs > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if msgs == 0 {
		t.Error("DLQ empty after RetryPolicy exhaustion")
	}
}

// TestWorker_ClaimsCached_BackgroundRefresh verifies that ClaimProvider.Claims
// is not called on every targeted message. A reasonable caching layer + a
// background refresher driven by ClaimRefreshInterval should keep the call
// count bounded regardless of message volume.
func TestWorker_ClaimsCached_BackgroundRefresh(t *testing.T) {
	var calls atomic.Int32
	claimsFn := worker.ClaimsFunc(func(ctx context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"primary"}, nil
	})

	h := testutil.SingleNode(t, worker.Options{
		Concurrency:          4,
		Claims:               claimsFn,
		ClaimRefreshInterval: 500 * time.Millisecond,
	})
	task.MustRegister(h.Reg, echo)

	const N = 50
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	futs := make([]*client.Future, 0, N)
	for i := 0; i < N; i++ {
		fut, err := client.EnqueueOpts(h.Client, echo, client.EnqueueOptions{Target: "primary"}, "hi")
		if err != nil {
			t.Fatal(err)
		}
		futs = append(futs, fut)
	}

	for _, fut := range futs {
		if _, err := client.Await[string](ctx, fut); err != nil {
			t.Fatalf("targeted task failed: %v", err)
		}
	}

	c := int(calls.Load())
	// With a 500ms refresh and handlers finishing under 2s total, at most ~5
	// refresh ticks should fire. Without caching, c would be >= N.
	if c >= N {
		t.Errorf("Claims() called %d times for %d targeted tasks; caching did not apply", c, N)
	}
	if c > 10 {
		t.Errorf("Claims() called %d times; expected <= 10 refreshes", c)
	}
}

// TestWorker_MaxDeliverUnlimited_NoPolicyDefaults verifies that when neither
// MaxDeliver nor a task-level RetryPolicy constrains retries, the worker
// falls back to task.DefaultRetryPolicy() (5 attempts). This guards against
// the "retry forever" regression that MaxDeliver=-1 could trigger.
func TestWorker_MaxDeliverUnlimited_NoPolicyDefaults(t *testing.T) {
	policyAttempts.Store(0)
	defaultPolicy := task.RetryPolicy{
		InitialInterval:    20 * time.Millisecond,
		BackoffCoefficient: 1.0,
		MaximumInterval:    20 * time.Millisecond,
		MaximumAttempts:    4,
	}
	h := testutil.SingleNode(t, worker.Options{
		Concurrency:        1,
		MaxDeliver:         -1,
		AckWait:            5 * time.Second,
		Backoff:            []time.Duration{10 * time.Millisecond},
		DefaultRetryPolicy: &defaultPolicy,
	})
	task.MustRegister(h.Reg, policyFlaky)

	fut, err := client.Enqueue(h.Client, policyFlaky)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fut.Get(ctx, nil); err == nil {
		t.Fatal("want failure")
	}
	if got := policyAttempts.Load(); got != 4 {
		t.Errorf("attempts: got %d, want 4 (DefaultRetryPolicy fallback)", got)
	}
}
