// Package client is the producer side: Enqueue tasks and await responses.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
)

type Client struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	id      string
	waiters sync.Map // taskID -> chan *task.Response

	consumeCtx jetstream.ConsumeContext
	closeOnce  sync.Once
	closed     chan struct{}
}

type Options struct {
	// ClientID scopes response subjects to this client. Defaults to a random uuid.
	ClientID string
	// DefaultTimeout is the context timeout used if Enqueue is called with context.Background.
	DefaultTimeout time.Duration
}

func New(ctx context.Context, nc *nats.Conn, opts Options) (*Client, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("client: jetstream: %w", err)
	}
	if opts.ClientID == "" {
		opts.ClientID = uuid.NewString()
	}

	c := &Client{
		nc:     nc,
		js:     js,
		id:     opts.ClientID,
		closed: make(chan struct{}),
	}

	respStream, err := js.Stream(ctx, stream.ResponseStream)
	if err != nil {
		return nil, fmt.Errorf("client: response stream %q missing (call stream.EnsureStreams first): %w", stream.ResponseStream, err)
	}
	// Ephemeral consumer: responses are per-client-process; on restart we'd start fresh.
	cons, err := respStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Name:          "ebind-resp-" + c.id,
		FilterSubject: stream.ResponseSubjectPrefix + c.id + ".>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		InactiveThreshold: 10 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("client: response consumer: %w", err)
	}

	consumeCtx, err := cons.Consume(c.handleResponse)
	if err != nil {
		return nil, fmt.Errorf("client: response consume: %w", err)
	}
	c.consumeCtx = consumeCtx
	return c, nil
}

func (c *Client) ID() string { return c.id }

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.consumeCtx != nil {
			c.consumeCtx.Stop()
		}
		close(c.closed)
	})
}

func (c *Client) handleResponse(msg jetstream.Msg) {
	var resp task.Response
	if err := json.Unmarshal(msg.Data(), &resp); err != nil {
		msg.Term()
		return
	}
	if ch, ok := c.waiters.LoadAndDelete(resp.TaskID); ok {
		ch.(chan *task.Response) <- &resp
	}
	msg.Ack()
}

type EnqueueOptions struct {
	Deadline     time.Time         // absolute deadline for handler execution
	TraceCtx     map[string]string
	RetryPolicy  *task.RetryPolicy // per-task override; worker falls back to its Options defaults when nil
	DAGID        string            // workflow-level: identifies the parent DAG
	StepID       string            // workflow-level: identifies this step within the DAG
	// SkipResponse: don't register a waiter, return nil Future (fire-and-forget).
	SkipResponse bool
}

// Enqueue publishes a task. fn is a function reference used as type witness and name source.
// args must match fn's signature (ctx is supplied automatically on the worker side).
func Enqueue(c *Client, fn any, args ...any) (*Future, error) {
	return EnqueueOpts(c, fn, EnqueueOptions{}, args...)
}

func EnqueueOpts(c *Client, fn any, opts EnqueueOptions, args ...any) (*Future, error) {
	d, err := task.Describe(fn)
	if err != nil {
		return nil, err
	}
	if err := d.ValidateArgs(args); err != nil {
		return nil, fmt.Errorf("ebind: Enqueue %s: %w", d.Name, err)
	}

	if args == nil {
		args = []any{}
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("ebind: Enqueue %s: marshal args: %w", d.Name, err)
	}

	id := uuid.NewString()
	t := task.Task{
		ID:          id,
		Name:        d.Name,
		Payload:     payload,
		EnqueuedAt:  time.Now().UTC(),
		Deadline:    opts.Deadline,
		TraceCtx:    opts.TraceCtx,
		RetryPolicy: opts.RetryPolicy,
		DAGID:       opts.DAGID,
		StepID:      opts.StepID,
	}
	if !opts.SkipResponse {
		t.ReplyTo = stream.ResponseSubjectPrefix + c.id + "." + id
	}

	var fut *Future
	if !opts.SkipResponse {
		fut = &Future{id: id, ch: make(chan *task.Response, 1)}
		c.waiters.Store(id, fut.ch)
	}

	body, err := json.Marshal(t)
	if err != nil {
		if !opts.SkipResponse {
			c.waiters.Delete(id)
		}
		return nil, fmt.Errorf("ebind: Enqueue %s: marshal task: %w", d.Name, err)
	}

	ctx := context.Background()
	subject := stream.TaskSubjectPrefix + d.Name
	if _, err := c.js.Publish(ctx, subject, body, jetstream.WithMsgID(id)); err != nil {
		if !opts.SkipResponse {
			c.waiters.Delete(id)
		}
		return nil, fmt.Errorf("ebind: Enqueue %s: publish: %w", d.Name, err)
	}
	return fut, nil
}

// EnqueueAsync is fire-and-forget. Returns the task ID.
func EnqueueAsync(c *Client, fn any, args ...any) (string, error) {
	return enqueueAsync(c, fn, EnqueueOptions{SkipResponse: true}, args...)
}

func enqueueAsync(c *Client, fn any, opts EnqueueOptions, args ...any) (string, error) {
	opts.SkipResponse = true
	d, err := task.Describe(fn)
	if err != nil {
		return "", err
	}
	if err := d.ValidateArgs(args); err != nil {
		return "", fmt.Errorf("ebind: EnqueueAsync %s: %w", d.Name, err)
	}
	if args == nil {
		args = []any{}
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	t := task.Task{
		ID:          id,
		Name:        d.Name,
		Payload:     payload,
		EnqueuedAt:  time.Now().UTC(),
		Deadline:    opts.Deadline,
		TraceCtx:    opts.TraceCtx,
		RetryPolicy: opts.RetryPolicy,
		DAGID:       opts.DAGID,
		StepID:      opts.StepID,
	}
	body, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	if _, err := c.js.Publish(context.Background(), stream.TaskSubjectPrefix+d.Name, body, jetstream.WithMsgID(id)); err != nil {
		return "", fmt.Errorf("ebind: EnqueueAsync %s: publish: %w", d.Name, err)
	}
	return id, nil
}

// ErrClientClosed is returned when a future is waited on after the client closes.
var ErrClientClosed = errors.New("ebind: client closed")
