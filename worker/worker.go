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
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
)

type Options struct {
	Durable              string        // consumer durable name; default "ebind-worker"
	Concurrency          int           // max in-flight handlers; default 16
	AckWait              time.Duration // default 30s
	MaxDeliver           int           // default 5 — upper bound from NATS consumer; task-level RetryPolicy can tighten but not exceed.
	Backoff              []time.Duration
	ShutdownGrace        time.Duration // default 30s
	Middleware           []Middleware
	StepHook             StepHook // optional; called on terminal success/failure (workflow integration)
	WorkerID             string
	Claims               ClaimProvider
	ClaimRefreshInterval time.Duration
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
		o.MaxDeliver = 5
	}
	if len(o.Backoff) == 0 {
		o.Backoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}
	}
	// NATS rejects configs where len(BackOff) > MaxDeliver.
	if len(o.Backoff) > o.MaxDeliver {
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

	sem := make(chan struct{}, w.opts.Concurrency)
	var wg sync.WaitGroup

	generalCons, err := tasksStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       w.opts.Durable,
		FilterSubject: stream.TaskSubjectPrefix + ">",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       w.opts.AckWait,
		MaxDeliver:    w.opts.MaxDeliver,
		BackOff:       w.opts.Backoff,
	})
	if err != nil {
		return fmt.Errorf("worker: consumer: %w", err)
	}
	generalCC, err := w.startConsumer(ctx, generalCons, sem, &wg)
	if err != nil {
		return fmt.Errorf("worker: consume: %w", err)
	}

	targetedCons, err := tasksStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       w.targetedDurable(),
		FilterSubject: stream.TargetedTaskSubjectPrefix + ">",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       w.opts.AckWait,
		MaxDeliver:    w.opts.MaxDeliver,
		BackOff:       w.opts.Backoff,
	})
	if err != nil {
		generalCC.Stop()
		return fmt.Errorf("worker: targeted consumer: %w", err)
	}
	targetedCC, err := w.startConsumer(ctx, targetedCons, sem, &wg)
	if err != nil {
		generalCC.Stop()
		return fmt.Errorf("worker: targeted consume: %w", err)
	}

	<-ctx.Done()
	generalCC.Stop()
	targetedCC.Stop()

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

func (w *Worker) targetedDurable() string {
	return w.opts.Durable + "-targets"
}

func (w *Worker) currentClaims(ctx context.Context) ([]string, error) {
	seen := map[string]struct{}{}
	claims := []string{ConcreteTarget(w.opts.WorkerID)}
	seen[claims[0]] = struct{}{}
	if w.opts.Claims == nil {
		return claims, nil
	}
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
	sort.Strings(claims)
	return claims, nil
}

func (w *Worker) ownsTarget(ctx context.Context, target string) bool {
	claims, err := w.currentClaims(ctx)
	if err != nil {
		return false
	}
	for _, claim := range claims {
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
	if t.Target != "" && !w.ownsTarget(ctx, t.Target) {
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

// shouldRetry prefers task-level RetryPolicy; falls back to worker's MaxDeliver.
func (w *Worker) shouldRetry(t *task.Task, te *task.TaskError) bool {
	if t.RetryPolicy != nil {
		return t.RetryPolicy.ShouldRetry(te, t.Attempt)
	}
	return te.Retryable && t.Attempt < w.opts.MaxDeliver
}

// delayFor prefers task-level RetryPolicy; falls back to worker's Backoff slice.
func (w *Worker) delayFor(t *task.Task) time.Duration {
	if t.RetryPolicy != nil {
		return t.RetryPolicy.NextDelay(t.Attempt)
	}
	return w.nextBackoff(t.Attempt)
}

func (w *Worker) nextBackoff(attempt int) time.Duration {
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
