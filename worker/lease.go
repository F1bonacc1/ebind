package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/task"
)

// ErrLeaseLost is the cause on a handler's context when the worker has lost
// its hold on the task: no heartbeat reached JetStream within AckWait, so
// JetStream may already have redelivered the task to another worker. The
// worker then neither runs the StepHook nor settles the delivery; the
// redelivery owns the task.
var ErrLeaseLost = errors.New("worker: task lease lost; JetStream redelivers the task")

// progressAck is JetStream's "work in progress" ack. Sent on a delivery's
// reply subject, it restarts that delivery's AckWait clock.
var progressAck = []byte("+WPI")

// minHeartbeatRetry bounds how fast a failed heartbeat is retried.
const minHeartbeatRetry = 50 * time.Millisecond

// leaseClock decides when this worker's hold on a delivery has run out,
// using local monotonic time only. It is conservative: it never reports the
// lease as held once JetStream may have redelivered.
//
// JetStream redelivers once AckWait has passed since its latest record of
// the delivery: the delivery itself, or the last +WPI it processed. The
// server processes a heartbeat after we send it, so the send time of a
// heartbeat the server acknowledged is a lower bound for that record, and
// anchor+AckWait a lower bound for the redelivery. Until the first
// acknowledged heartbeat the anchor is the time the message was received;
// the server stamped the delivery earlier by the one-way delivery latency,
// which this ignores.
type leaseClock struct {
	ackWait time.Duration
	anchor  time.Time
	lost    bool
}

func newLeaseClock(received time.Time, ackWait time.Duration) leaseClock {
	return leaseClock{ackWait: ackWait, anchor: received}
}

// expiry is when the lease runs out unless a heartbeat is acknowledged first.
func (c *leaseClock) expiry() time.Time { return c.anchor.Add(c.ackWait) }

// expired reports whether the lease has run out at now. Once it has, it stays
// lost.
func (c *leaseClock) expired(now time.Time) bool {
	if !c.lost && !now.Before(c.expiry()) {
		c.lost = true
	}
	return c.lost
}

// confirm records a heartbeat sent at sent whose acknowledgment arrived at
// replied. An acknowledgment that arrives only after the lease ran out does
// not revive it: JetStream may have redelivered before it processed the
// heartbeat.
func (c *leaseClock) confirm(sent, replied time.Time) {
	if c.expired(replied) {
		return
	}
	if sent.After(c.anchor) {
		c.anchor = sent
	}
}

// lease is this worker's hold on one delivery. While the handler and the
// StepHook run, it heartbeats the delivery so JetStream does not redeliver
// it. Every settlement goes through the lease, which stops the heartbeat
// first and refuses to settle once the lease is lost.
type lease struct {
	msg      jetstream.Msg
	nc       *nats.Conn
	interval time.Duration // <= 0: heartbeats disabled
	log      *slog.Logger
	taskID   string
	taskName string
	onLost   func()

	lost     atomic.Bool
	stopping atomic.Bool
	cancel   context.CancelFunc // ends the heartbeat goroutine; nil when disabled
	done     chan struct{}      // closed once that goroutine has exited
	stopOnce sync.Once
}

// startLease starts heartbeating msg, received at received. Heartbeats run
// until stop, or until lifeCtx ends (Run has returned), which counts as a
// lost lease since nothing heartbeats the delivery any more. onLost runs at
// most once, from the heartbeat goroutine, when the lease is lost.
func (w *Worker) startLease(lifeCtx context.Context, msg jetstream.Msg, t *task.Task, received time.Time, onLost func()) *lease {
	l := &lease{
		msg:      msg,
		nc:       w.nc,
		interval: w.opts.HeartbeatInterval,
		log:      w.opts.Logger,
		taskID:   t.ID,
		taskName: t.Name,
		onLost:   onLost,
	}
	if l.interval <= 0 {
		return l
	}
	ctx, cancel := context.WithCancel(lifeCtx)
	l.cancel = cancel
	l.done = make(chan struct{})
	go l.run(ctx, newLeaseClock(received, w.opts.AckWait))
	return l
}

// Lost reports whether the lease has been lost. After stop it no longer
// changes.
func (l *lease) Lost() bool { return l.lost.Load() }

// stop ends the heartbeat and waits for its goroutine to exit, so no +WPI can
// follow a settlement: a +WPI processed after NakWithDelay would restart the
// full AckWait clock and override the retry delay. Safe to call repeatedly.
func (l *lease) stop() {
	l.stopOnce.Do(func() {
		l.stopping.Store(true)
		if l.cancel != nil {
			l.cancel()
			<-l.done
		}
	})
}

func (l *lease) Ack() error  { return l.settle(l.msg.Ack) }
func (l *lease) Nak() error  { return l.settle(l.msg.Nak) }
func (l *lease) Term() error { return l.settle(l.msg.Term) }
func (l *lease) NakWithDelay(d time.Duration) error {
	return l.settle(func() error { return l.msg.NakWithDelay(d) })
}

func (l *lease) settle(f func() error) error {
	l.stop()
	if l.lost.Load() {
		return ErrLeaseLost
	}
	return f()
}

// run heartbeats until stopped or until the lease is lost. A heartbeat is a
// +WPI sent as a request: the server answers it once processed, so a reply
// proves the delivery's clock restarted. A plain InProgress() publish cannot
// prove that; while the connection is down nats.go buffers it and reports
// success.
func (l *lease) run(ctx context.Context, clock leaseClock) {
	defer close(l.done)
	next := l.interval
	failing := false
	for {
		if !sleepCtx(ctx, min(next, time.Until(clock.expiry()))) {
			if !l.stopping.Load() {
				l.markLost("worker stopped while the task was still running")
			}
			return
		}
		sent := time.Now()
		if clock.expired(sent) {
			l.markLost("no heartbeat reached JetStream within AckWait")
			return
		}
		// Cap each request at one interval, so a request lost in a consumer
		// leader change is retried rather than waited out to expiry.
		reqCtx, cancel := context.WithDeadline(ctx, earliest(clock.expiry(), sent.Add(l.interval)))
		_, err := l.nc.RequestWithContext(reqCtx, l.msg.Reply(), progressAck)
		cancel()
		if ctx.Err() != nil {
			continue // stopping, or Run returned; the sleep above sorts out which
		}
		if err == nil {
			clock.confirm(sent, time.Now())
			if clock.lost {
				l.markLost("heartbeat acknowledged only after AckWait ran out")
				return
			}
			if failing {
				failing = false
				l.log.Info("worker: task heartbeat recovered", "task_id", l.taskID, "name", l.taskName)
			}
			next = l.interval
			continue
		}
		if !failing {
			failing = true
			l.log.Warn("worker: task heartbeat failed", "task_id", l.taskID, "name", l.taskName, "error", err)
		}
		next = max(l.interval/4, minHeartbeatRetry)
	}
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (l *lease) markLost(reason string) {
	l.lost.Store(true)
	l.log.Error("worker: task lease lost; leaving the task to its redelivery",
		"task_id", l.taskID, "name", l.taskName, "reason", reason)
	if l.onLost != nil {
		l.onLost()
	}
}
