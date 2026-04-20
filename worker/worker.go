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
	Durable     string        // consumer durable name; default "ebind-worker"
	Concurrency int           // max in-flight handlers; default 16
	AckWait     time.Duration // default 30s
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

	// activeClaimSubs tracks subscriptions to per-claim targeted durables so
	// the worker only receives targeted messages it actually owns — eliminating
	// the wrong-target NAK loop entirely. Keyed by claim string.
	claimSubsMu     sync.Mutex
	activeClaimSubs map[string]jetstream.ConsumeContext
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
	if _, err := w.refreshClaims(ctx); err != nil {
		return fmt.Errorf("worker: initial claims: %w", err)
	}

	sem := make(chan struct{}, w.opts.Concurrency)
	var wg sync.WaitGroup

	w.activeClaimSubs = map[string]jetstream.ConsumeContext{}

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
	generalCC, err := w.startConsumer(ctx, generalCons, sem, &wg)
	if err != nil {
		return fmt.Errorf("worker: consume: %w", err)
	}

	// Seed per-claim subscriptions from the initial claim set.
	if err := w.reconcileClaimSubs(ctx, tasksStream, sem, &wg); err != nil {
		generalCC.Stop()
		return fmt.Errorf("worker: claim subs: %w", err)
	}

	go w.runClaimRefresher(ctx, tasksStream, sem, &wg)

	<-ctx.Done()
	generalCC.Stop()
	w.stopAllClaimSubs()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.opts.ShutdownGrace):
		return fmt.Errorf("worker: shutdown timeout after %s, %d handlers still in flight",
			w.opts.ShutdownGrace, len(sem))
	}
	return nil
}

func (w *Worker) startConsumer(ctx context.Context, cons jetstream.Consumer, sem chan struct{}, wg *sync.WaitGroup) (jetstream.ConsumeContext, error) {
	return cons.Consume(func(msg jetstream.Msg) {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			w.handle(ctx, msg)
		}()
	})
}

// claimDurable returns the durable consumer name for a given claim. Workers
// that share a claim share this durable (and thus queue-load-balance targeted
// messages); workers with a concrete per-worker claim have a unique durable.
func claimDurable(claim string) string {
	return "ebind-targets-" + stream.TargetToken(claim)
}

// reconcileClaimSubs ensures the worker subscribes to every currently-held
// claim. Subscriptions for dropped claims are intentionally NOT stopped:
// in-flight handlers for a claim-sub we're about to Stop() can get their
// messages redelivered before they Ack, which breaks at-least-once semantics
// during claim churn. Instead, subs remain open and the ownsTarget check in
// handle() NAKs any stray delivery so it can be picked up by the current
// owner. Subs only go away on worker shutdown.
func (w *Worker) reconcileClaimSubs(ctx context.Context, ts jetstream.Stream, sem chan struct{}, wg *sync.WaitGroup) error {
	desired := map[string]struct{}{}
	for _, c := range w.cachedClaims() {
		desired[c] = struct{}{}
	}

	w.claimSubsMu.Lock()
	defer w.claimSubsMu.Unlock()
	for claim := range desired {
		if _, ok := w.activeClaimSubs[claim]; ok {
			continue
		}
		cc, err := w.subscribeClaim(ctx, ts, claim, sem, wg)
		if err != nil {
			return fmt.Errorf("subscribe %q: %w", claim, err)
		}
		w.activeClaimSubs[claim] = cc
	}
	return nil
}

func (w *Worker) subscribeClaim(ctx context.Context, ts jetstream.Stream, claim string, sem chan struct{}, wg *sync.WaitGroup) (jetstream.ConsumeContext, error) {
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
	return w.startConsumer(ctx, cons, sem, wg)
}

func (w *Worker) stopAllClaimSubs() {
	w.claimSubsMu.Lock()
	defer w.claimSubsMu.Unlock()
	for claim, cc := range w.activeClaimSubs {
		cc.Stop()
		delete(w.activeClaimSubs, claim)
	}
}

// refreshClaims recomputes the effective claim set and atomically swaps it into
// the cache. Called once synchronously at Run() startup and then periodically
// from the background refresher.
func (w *Worker) refreshClaims(ctx context.Context) ([]string, error) {
	seen := map[string]struct{}{}
	claims := []string{ConcreteTarget(w.opts.WorkerID)}
	seen[claims[0]] = struct{}{}
	if w.opts.Claims != nil {
		provided, err := w.opts.Claims.Claims(ctx)
		if err != nil {
			return nil, err
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
	return cp, nil
}

// runClaimRefresher ticks ClaimRefreshInterval, refreshes the claim cache,
// and reconciles the per-claim subscription set. Transient Claims() errors
// are ignored — the last good cache stays in place so one flaky lookup
// doesn't blackhole targeted tasks.
func (w *Worker) runClaimRefresher(ctx context.Context, ts jetstream.Stream, sem chan struct{}, wg *sync.WaitGroup) {
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
			if _, err := w.refreshClaims(ctx); err != nil {
				continue
			}
			_ = w.reconcileClaimSubs(ctx, ts, sem, wg)
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
		te, _ := callErr.(*task.TaskError)
		if te == nil {
			te = &task.TaskError{Kind: task.ErrKindHandler, Message: callErr.Error(), Retryable: true}
		}
		if w.shouldRetry(&t, te) {
			_ = msg.NakWithDelay(w.delayFor(&t))
			return
		}
		resp.Error = te
		w.publishResponse(ctx, &t, &resp)
		w.publishDLQ(ctx, &t, te)
		if w.opts.StepHook != nil {
			_ = w.opts.StepHook.OnStepFailed(ctx, &t, te)
		}
		_ = msg.Term()
		return
	}

	resp.Result = result
	w.publishResponse(ctx, &t, &resp)
	if w.opts.StepHook != nil {
		_ = w.opts.StepHook.OnStepDone(ctx, &t, result)
	}
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
