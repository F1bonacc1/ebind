// Package worker consumes tasks from the TASKS stream, dispatches via the registry,
// and publishes responses to each task's ReplyTo subject.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
)

type Options struct {
	Durable     string // consumer durable name; default "ebind-worker"
	Concurrency int    // max in-flight handlers; default 16
	// AckWait is how long JetStream waits for a delivered task to be settled
	// before redelivering it. Default 30s. The worker pulls a task only when
	// it has a free slot to run it, so the clock covers the handler's run
	// time, not time spent queued in this worker; AckWait must still exceed
	// the slowest handler.
	AckWait time.Duration
	// MaxDeliver bounds how many times NATS will redeliver a message before
	// giving up. Default is -1 (unlimited at the NATS layer) so that the
	// worker's RetryPolicy is authoritative: the handler decides when to
	// terminate (Term + DLQ). Set a positive value only if you want an
	// infrastructure-level cap that can override an over-generous RetryPolicy.
	MaxDeliver    int
	Backoff       []time.Duration
	ShutdownGrace time.Duration // default 30s
	Middleware    []Middleware
	StepHook      StepHook // optional; called on terminal success/failure (workflow integration)
	// DefaultRetryPolicy is applied to tasks that carry no per-task RetryPolicy.
	// Used by the worker (not NATS) to decide when to stop retrying. Default is
	// task.DefaultRetryPolicy().
	DefaultRetryPolicy   *task.RetryPolicy
	WorkerID             string
	Claims               ClaimProvider
	ClaimRefreshInterval time.Duration // background refresh of the claim cache; default 2s
	ClaimRetryDelay      time.Duration
}

func (o *Options) setDefaults() {
	if o.Durable == "" {
		o.Durable = "ebind-worker"
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 16
	}
	if o.AckWait == 0 {
		o.AckWait = 30 * time.Second
	}
	if o.MaxDeliver == 0 {
		o.MaxDeliver = -1
	}
	if len(o.Backoff) == 0 {
		o.Backoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}
	}
	// NATS rejects configs where len(BackOff) > MaxDeliver when MaxDeliver is finite.
	if o.MaxDeliver > 0 && len(o.Backoff) > o.MaxDeliver {
		o.Backoff = o.Backoff[:o.MaxDeliver]
	}
	if o.ShutdownGrace == 0 {
		o.ShutdownGrace = 30 * time.Second
	}
	if o.WorkerID == "" {
		o.WorkerID = uuid.NewString()
	}
	if o.ClaimRefreshInterval == 0 {
		o.ClaimRefreshInterval = 2 * time.Second
	}
	if o.ClaimRetryDelay == 0 {
		o.ClaimRetryDelay = time.Second
	}
}

type Worker struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	reg    *task.Registry
	opts   Options
	invoke Handler

	claimCache atomic.Pointer[[]string]

	// claimLoops tracks the pull loops on per-claim targeted durables so the
	// worker only receives targeted messages it actually owns, eliminating
	// the wrong-target NAK loop. Keyed by claim string.
	claimLoopsMu sync.Mutex
	claimLoops   map[string]*pullLoop
}

func New(nc *nats.Conn, reg *task.Registry, opts Options) (*Worker, error) {
	opts.setDefaults()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("worker: jetstream: %w", err)
	}
	w := &Worker{nc: nc, js: js, reg: reg, opts: opts}
	// Compose middleware chain. Recover is always innermost so user middleware
	// (Log, Metrics) can observe the recovered panic error.
	base := w.baseHandler()
	chained := Chain(append([]Middleware{Recover()}, opts.Middleware...)...)(base)
	w.invoke = chained
	return w, nil
}

func (w *Worker) Use(mws ...Middleware) {
	base := w.baseHandler()
	chained := Chain(append([]Middleware{Recover()}, append(w.opts.Middleware, mws...)...)...)(base)
	w.opts.Middleware = append(w.opts.Middleware, mws...)
	w.invoke = chained
}

// Run blocks until ctx is canceled, then drains in-flight handlers up to ShutdownGrace.
func (w *Worker) Run(ctx context.Context) error {
	tasksStream, err := w.js.Stream(ctx, stream.TaskStream)
	if err != nil {
		return fmt.Errorf("worker: tasks stream: %w", err)
	}

	// Seed the claim cache synchronously so ownsTarget never returns false
	// due to an unpopulated cache.
	if err := w.refreshClaims(ctx); err != nil {
		return fmt.Errorf("worker: initial claims: %w", err)
	}

	s := newSlots(w.opts.Concurrency)
	var wg sync.WaitGroup

	w.claimLoopsMu.Lock()
	w.claimLoops = map[string]*pullLoop{}
	w.claimLoopsMu.Unlock()

	generalCons, err := tasksStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       w.opts.Durable,
		FilterSubject: stream.TaskSubjectPrefix + ">",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       w.opts.AckWait,
		MaxDeliver:    w.opts.MaxDeliver,
		// Deliberately NOT passing BackOff: in NATS 2.10+ BackOff[0] overrides
		// AckWait for the first delivery, so a short Backoff would silently
		// shrink the handler's effective ack window. All retry timing is
		// driven app-side via msg.NakWithDelay(w.delayFor(&t)).
	})
	if err != nil {
		return fmt.Errorf("worker: consumer: %w", err)
	}
	// runCtx lets a failed startup stop the loops it already started; on a
	// normal shutdown it ends together with ctx.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	general := w.startPullLoop(runCtx, generalCons, s, &wg)

	// Seed per-claim loops from the initial claim set.
	if err := w.reconcileClaimSubs(runCtx, tasksStream, s, &wg); err != nil {
		cancelRun()
		_ = w.drain(general, nil, s, &wg)
		return fmt.Errorf("worker: claim subs: %w", err)
	}

	refresherDone := make(chan struct{})
	go func() {
		defer close(refresherDone)
		w.runClaimRefresher(runCtx, tasksStream, s, &wg)
	}()

	<-ctx.Done()
	return w.drain(general, refresherDone, s, &wg)
}

// drain runs the shutdown sequence once the loops' context has ended. Order
// matters:
//  1. Wait for the claim refresher (if started), so it cannot start another
//     loop.
//  2. Wait for every pull loop to exit. A loop never starts a handler after
//     its context ends, so from here on wg only counts down and wg.Wait
//     cannot race a late wg.Add.
//  3. Wait for in-flight handlers, up to ShutdownGrace.
func (w *Worker) drain(general *pullLoop, refresherDone <-chan struct{}, s *slots, wg *sync.WaitGroup) error {
	if refresherDone != nil {
		<-refresherDone
	}
	<-general.done
	w.waitClaimLoops()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.opts.ShutdownGrace):
		return fmt.Errorf("worker: shutdown timeout after %s, %d handlers still in flight",
			w.opts.ShutdownGrace, s.inUse())
	}
	return nil
}

// claimDurable returns the durable consumer name for a given claim. Workers
// that share a claim share this durable (and thus queue-load-balance targeted
// messages); workers with a concrete per-worker claim have a unique durable.
func claimDurable(claim string) string {
	return "ebind-targets-" + stream.TargetToken(claim)
}

// reconcileClaimSubs ensures the worker runs a pull loop for every
// currently-held claim. Loops for dropped claims are NOT stopped: they keep
// running until worker shutdown, and the ownsTarget check in handle() NAKs
// any stray delivery so the current owner can pick it up.
func (w *Worker) reconcileClaimSubs(ctx context.Context, ts jetstream.Stream, s *slots, wg *sync.WaitGroup) error {
	desired := map[string]struct{}{}
	for _, c := range w.cachedClaims() {
		desired[c] = struct{}{}
	}

	w.claimLoopsMu.Lock()
	defer w.claimLoopsMu.Unlock()
	for claim := range desired {
		if _, ok := w.claimLoops[claim]; ok {
			continue
		}
		l, err := w.subscribeClaim(ctx, ts, claim, s, wg)
		if err != nil {
			return fmt.Errorf("subscribe %q: %w", claim, err)
		}
		w.claimLoops[claim] = l
	}
	return nil
}

func (w *Worker) subscribeClaim(ctx context.Context, ts jetstream.Stream, claim string, s *slots, wg *sync.WaitGroup) (*pullLoop, error) {
	// Prefer fetching an existing durable: calling CreateOrUpdate with identical
	// config on a durable that already has in-flight deliveries can reset the
	// consumer's pending state in some NATS server versions, which surfaces as
	// spurious redeliveries during claim churn. Fetch first, create if absent.
	cons, err := ts.Consumer(ctx, claimDurable(claim))
	if err != nil {
		cons, err = ts.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Durable:       claimDurable(claim),
			FilterSubject: stream.TargetedTaskFilter(claim),
			AckPolicy:     jetstream.AckExplicitPolicy,
			AckWait:       w.opts.AckWait,
			MaxDeliver:    w.opts.MaxDeliver,
			// BackOff intentionally omitted — see comment on generalCons.
		})
		if err != nil {
			return nil, err
		}
	}
	return w.startPullLoop(ctx, cons, s, wg), nil
}

// waitClaimLoops waits for every claim loop to exit. The loops watch the run
// context, which has already ended when this is called.
func (w *Worker) waitClaimLoops() {
	w.claimLoopsMu.Lock()
	loops := make([]*pullLoop, 0, len(w.claimLoops))
	for claim, l := range w.claimLoops {
		loops = append(loops, l)
		delete(w.claimLoops, claim)
	}
	w.claimLoopsMu.Unlock()
	for _, l := range loops {
		<-l.done
	}
}

// refreshClaims recomputes the effective claim set and atomically swaps it into
// the cache. Called once synchronously at Run() startup and then periodically
// from the background refresher.
func (w *Worker) refreshClaims(ctx context.Context) error {
	seen := map[string]struct{}{}
	claims := []string{ConcreteTarget(w.opts.WorkerID)}
	seen[claims[0]] = struct{}{}
	if w.opts.Claims != nil {
		provided, err := w.opts.Claims.Claims(ctx)
		if err != nil {
			return err
		}
		for _, claim := range provided {
			claim = strings.TrimSpace(claim)
			if claim == "" {
				continue
			}
			if _, ok := seen[claim]; ok {
				continue
			}
			seen[claim] = struct{}{}
			claims = append(claims, claim)
		}
	}
	sort.Strings(claims)
	cp := append([]string(nil), claims...)
	w.claimCache.Store(&cp)
	return nil
}

// runClaimRefresher ticks ClaimRefreshInterval, refreshes the claim cache,
// and reconciles the per-claim subscription set. Transient Claims() errors
// are ignored — the last good cache stays in place so one flaky lookup
// doesn't blackhole targeted tasks.
func (w *Worker) runClaimRefresher(ctx context.Context, ts jetstream.Stream, s *slots, wg *sync.WaitGroup) {
	if w.opts.Claims == nil {
		return // claims are static; nothing to reconcile
	}
	t := time.NewTicker(w.opts.ClaimRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.refreshClaims(ctx); err != nil {
				continue
			}
			_ = w.reconcileClaimSubs(ctx, ts, s, wg)
		}
	}
}

func (w *Worker) cachedClaims() []string {
	p := w.claimCache.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (w *Worker) ownsTarget(target string) bool {
	for _, claim := range w.cachedClaims() {
		if claim == target {
			return true
		}
	}
	return false
}

// baseHandler is the terminal Handler that decodes the task payload and calls the
// registered function. All middleware wraps around this.
func (w *Worker) baseHandler() Handler {
	return func(ctx context.Context, t *task.Task) ([]byte, error) {
		d, ok := w.reg.Get(t.Name)
		if !ok {
			return nil, &task.TaskError{
				Kind:      task.ErrKindUnknownHandler,
				Message:   fmt.Sprintf("no handler registered for %q", t.Name),
				Retryable: false,
			}
		}
		return d.Call(ctx, t.Payload)
	}
}

func (w *Worker) handle(ctx context.Context, msg jetstream.Msg) {
	var t task.Task
	if err := json.Unmarshal(msg.Data(), &t); err != nil {
		_ = msg.Term()
		return
	}
	if md, err := msg.Metadata(); err == nil {
		t.Attempt = int(md.NumDelivered)
	} else {
		t.Attempt = 1
	}
	// Per-claim durables only deliver messages this worker owns; any stray
	// wrong-target delivery (e.g. a claim was just dropped) goes back to the
	// queue for a worker that still owns it.
	if t.Target != "" && !w.ownsTarget(t.Target) {
		_ = msg.NakWithDelay(w.opts.ClaimRetryDelay)
		return
	}
	t.WorkerID = w.opts.WorkerID

	resp := task.Response{TaskID: t.ID, Attempts: t.Attempt}

	handlerCtx := ctx
	if !t.Deadline.IsZero() {
		if time.Now().After(t.Deadline) {
			te := &task.TaskError{Kind: task.ErrKindDeadline, Message: "deadline exceeded before dispatch", Retryable: false}
			resp.Error = te
			resp.CompletedAt = time.Now().UTC()
			w.publishResponse(ctx, &t, &resp)
			w.publishDLQ(ctx, &t, te)
			if w.opts.StepHook != nil {
				_ = w.opts.StepHook.OnStepFailed(ctx, &t, te)
			}
			_ = msg.Term()
			return
		}
		var cancel context.CancelFunc
		handlerCtx, cancel = context.WithDeadline(ctx, t.Deadline)
		defer cancel()
	}

	result, callErr := w.invoke(handlerCtx, &t)
	resp.CompletedAt = time.Now().UTC()

	if callErr != nil {
		var te *task.TaskError
		if !errors.As(callErr, &te) {
			te = &task.TaskError{Kind: task.ErrKindHandler, Message: callErr.Error(), Retryable: true}
		}
		if w.shouldRetry(&t, te) {
			_ = msg.NakWithDelay(w.delayFor(&t))
			return
		}
		resp.Error = te
		// Persist the terminal failure status BEFORE acking/terminating and
		// before publishing the response/DLQ. If the status write fails (e.g. a
		// stale-revision CAS on a freshly-formed cluster), do NOT Term — that
		// would drop the only signal the scheduler has and strand the step in
		// "running" forever. Redeliver instead so the failure is retried.
		if w.opts.StepHook != nil {
			if hookErr := w.opts.StepHook.OnStepFailed(ctx, &t, te); hookErr != nil {
				_ = msg.NakWithDelay(w.delayFor(&t))
				return
			}
		}
		w.publishResponse(ctx, &t, &resp)
		w.publishDLQ(ctx, &t, te)
		_ = msg.Term()
		return
	}

	resp.Result = result
	// Persist the completion (step -> done + result + completed event) BEFORE
	// acking. The hook's CAS can fail on a stale direct-get read while the
	// cluster is still converging; acking anyway would silently lose the
	// completion and leave the step stranded in "running". Redeliver instead so
	// the (idempotent) handler re-runs and the completion is retried.
	if w.opts.StepHook != nil {
		if hookErr := w.opts.StepHook.OnStepDone(ctx, &t, result); hookErr != nil {
			_ = msg.NakWithDelay(w.delayFor(&t))
			return
		}
	}
	w.publishResponse(ctx, &t, &resp)
	_ = msg.Ack()
}

// shouldRetry asks the effective policy (task-level > DefaultRetryPolicy Option >
// legacy Backoff/MaxDeliver fallback > task.DefaultRetryPolicy) whether to retry.
// With MaxDeliver=-1 (default), NATS will redeliver forever unless this returns false.
func (w *Worker) shouldRetry(t *task.Task, te *task.TaskError) bool {
	if t.RetryPolicy != nil {
		return t.RetryPolicy.ShouldRetry(te, t.Attempt)
	}
	if w.opts.DefaultRetryPolicy != nil {
		return w.opts.DefaultRetryPolicy.ShouldRetry(te, t.Attempt)
	}
	// Legacy: when the user configured MaxDeliver > 0 or a Backoff slice,
	// honor that as the attempt cap (preserves pre-MaxDeliver=-1 behavior).
	if cap := w.legacyAttemptCap(); cap > 0 {
		return te.Retryable && t.Attempt < cap
	}
	p := task.DefaultRetryPolicy()
	return p.ShouldRetry(te, t.Attempt)
}

func (w *Worker) delayFor(t *task.Task) time.Duration {
	if t.RetryPolicy != nil {
		return t.RetryPolicy.NextDelay(t.Attempt)
	}
	if w.opts.DefaultRetryPolicy != nil {
		return w.opts.DefaultRetryPolicy.NextDelay(t.Attempt)
	}
	if len(w.opts.Backoff) > 0 {
		return w.nextBackoff(t.Attempt)
	}
	p := task.DefaultRetryPolicy()
	return p.NextDelay(t.Attempt)
}

// legacyAttemptCap returns the effective attempt cap derived from user-set
// MaxDeliver / Backoff. Returns 0 when neither is configured for capping.
func (w *Worker) legacyAttemptCap() int {
	if w.opts.MaxDeliver > 0 {
		return w.opts.MaxDeliver
	}
	return 0
}

func (w *Worker) nextBackoff(attempt int) time.Duration {
	if len(w.opts.Backoff) == 0 {
		return time.Second
	}
	if attempt <= 0 {
		return w.opts.Backoff[0]
	}
	if attempt-1 < len(w.opts.Backoff) {
		return w.opts.Backoff[attempt-1]
	}
	return w.opts.Backoff[len(w.opts.Backoff)-1]
}

func (w *Worker) publishResponse(ctx context.Context, t *task.Task, resp *task.Response) {
	if t.ReplyTo == "" {
		return
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return
	}
	publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := w.js.Publish(publishCtx, t.ReplyTo, body); err != nil && !errors.Is(err, context.Canceled) {
		_ = err
	}
}

func (w *Worker) publishDLQ(ctx context.Context, t *task.Task, te *task.TaskError) {
	_ = dlq.Publish(ctx, w.js, t, te)
}
