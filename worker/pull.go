package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// idlePollWait bounds one long-poll on a consumer. The loop holds a slot
	// for the whole poll, so it also bounds how long a consumer whose loop
	// started waiting for that slot mid-poll is kept waiting. Short enough to
	// keep that latency low, long enough that an idle worker sends only a
	// pull request or two per second per consumer.
	idlePollWait = 2 * time.Second

	// contendedBackoff{Min,Max} bound the pause after a no-wait poll comes
	// back empty while another loop waits for a slot. Without the pause, two
	// idle loops sharing one slot would hand it back and forth with no-wait
	// polls as fast as the round trip allows.
	contendedBackoffMin = 50 * time.Millisecond
	contendedBackoffMax = time.Second

	// fetchErrBackoff{Min,Max} bound the pause after a failed pull (connection
	// down, consumer leader election, consumer missing).
	fetchErrBackoffMin = 100 * time.Millisecond
	fetchErrBackoffMax = 2 * time.Second
)

// slots bounds in-flight handlers (Options.Concurrency) and counts loops
// blocked waiting for one, so a loop can tell whether parking its slot on an
// idle long-poll would keep another consumer from running work.
type slots struct {
	sem     chan struct{}
	waiting atomic.Int32
}

func newSlots(n int) *slots { return &slots{sem: make(chan struct{}, n)} }

// acquire blocks until a slot is free or ctx ends. It returns false, holding
// no slot, once ctx has ended.
func (s *slots) acquire(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case s.sem <- struct{}{}:
		return true
	default:
	}
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	select {
	case s.sem <- struct{}{}:
		// select picks at random when both cases are ready; never hand out a
		// slot after ctx ended.
		if ctx.Err() != nil {
			s.release()
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// tryAcquire takes up to n more slots without blocking and returns how many
// it got. A slot is only free here if no loop is blocked waiting for it, so
// this never takes a slot out from under a waiter.
func (s *slots) tryAcquire(n int) int {
	got := 0
	for got < n {
		select {
		case s.sem <- struct{}{}:
			got++
		default:
			return got
		}
	}
	return got
}

func (s *slots) release() { <-s.sem }

func (s *slots) releaseN(n int) {
	for range n {
		<-s.sem
	}
}

func (s *slots) size() int { return cap(s.sem) }

// contended reports whether another loop is blocked waiting for a slot.
func (s *slots) contended() bool { return s.waiting.Load() > 0 }

func (s *slots) inUse() int { return len(s.sem) }

// pullLoop is one running consumer loop; done closes when it has exited.
type pullLoop struct{ done chan struct{} }

func (w *Worker) startPullLoop(ctx context.Context, cons jetstream.Consumer, rs *runState) *pullLoop {
	l := &pullLoop{done: make(chan struct{})}
	go func() {
		defer close(l.done)
		w.runPullLoop(ctx, cons, rs)
	}()
	return l
}

// runPullLoop feeds one consumer's messages to handlers. It never asks for
// more messages than the slots it holds, so every message it receives starts
// running at once.
//
// JetStream counts a message as delivered, and starts its AckWait clock, the
// moment it sends it. A worker that buffers messages it has no slot for (a
// prefetching Consume) runs that clock through the buffer wait as well as
// the handler, so a backlog longer than AckWait makes JetStream redeliver
// tasks that are still queued or running here. Pulling into free slots
// keeps the clock to the handler's own run time.
//
// Each round holds one slot, plus, right after a round that received work,
// every other slot that is free at that moment, and pulls that many
// messages without waiting, so a backlog drains in batches rather than one
// round trip per task. When there is nothing to grab, it long-polls on a
// single slot.
//
// Every worker runs at least two loops on the shared slots: the general
// durable and its own concrete-claim durable. An empty long-poll holds its
// slot for up to idlePollWait, so when another loop is already waiting for a
// slot this loop polls without waiting and, if that comes back empty, backs
// off before competing again. Without that, at Concurrency=1 every task
// would wait out the other consumer's empty long-poll.
//
// The loop exits once ctx ends and never starts a handler after that, so
// after every loop has exited the handler WaitGroup only counts down.
func (w *Worker) runPullLoop(ctx context.Context, cons jetstream.Consumer, rs *runState) {
	s := rs.slots
	var idleBackoff, errBackoff time.Duration
	hot := false // the previous round received work
	for {
		if !s.acquire(ctx) {
			return
		}
		held := 1
		if hot {
			held += s.tryAcquire(s.size() - 1)
		}
		contended := s.contended()
		noWait := contended || held > 1
		started, err := w.pullInto(ctx, cons, rs, held, noWait)
		held -= started
		if started == 0 && err == nil && noWait && !contended && ctx.Err() == nil {
			// The backlog drained since the last round: wait for new work
			// on a single slot.
			s.releaseN(held - 1)
			held = 1
			started, err = w.pullInto(ctx, cons, rs, 1, false)
			held -= started
		}
		s.releaseN(held)
		if ctx.Err() != nil {
			return
		}
		hot = started > 0
		var pause time.Duration
		switch {
		case started > 0:
			idleBackoff, errBackoff = 0, 0
		case err != nil:
			errBackoff = growBackoff(errBackoff, fetchErrBackoffMin, fetchErrBackoffMax)
			pause = errBackoff
		case contended:
			errBackoff = 0
			idleBackoff = growBackoff(idleBackoff, contendedBackoffMin, contendedBackoffMax)
			pause = idleBackoff
		default:
			// An uncontended long-poll expired empty: poll again at once.
			idleBackoff, errBackoff = 0, 0
		}
		if !sleepCtx(ctx, pause) {
			return
		}
	}
}

// pullInto pulls up to n messages, where n is at most the number of slots
// the caller holds, and starts a handler for each; every handler takes over
// one of those slots and releases it when done. It returns how many handlers
// it started; the caller still holds, and must release, the other n-started
// slots. With noWait the server answers at once with whatever is available;
// otherwise it long-polls for up to idlePollWait. Ending empty is not an
// error.
func (w *Worker) pullInto(ctx context.Context, cons jetstream.Consumer, rs *runState, n int, noWait bool) (int, error) {
	var (
		batch jetstream.MessageBatch
		err   error
	)
	if noWait {
		batch, err = cons.FetchNoWait(n)
	} else {
		// FetchContext sets the pull request's server-side expiry a little
		// before this deadline, so the server ends the request (408) while we
		// still listen and cannot deliver into an abandoned inbox. If ctx is
		// canceled instead, the server skips the request once our inbox
		// subscription is gone.
		pollCtx, cancel := context.WithTimeout(ctx, idlePollWait)
		defer cancel()
		batch, err = cons.Fetch(n, jetstream.FetchContext(pollCtx))
	}
	if err != nil {
		return 0, err
	}
	started := 0
	for msg := range batch.Messages() {
		if ctx.Err() != nil {
			// Shutdown raced the pull: give the message straight back.
			_ = msg.Nak()
			continue
		}
		started++
		rs.wg.Add(1)
		go func() {
			defer func() {
				rs.slots.release()
				rs.wg.Done()
			}()
			w.handle(ctx, rs.leaseCtx, msg)
		}()
	}
	err = batch.Error()
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		// Our own poll deadline fired before the server's 408 arrived.
		err = nil
	}
	return started, err
}

func growBackoff(cur, lo, hi time.Duration) time.Duration {
	if cur < lo {
		return lo
	}
	return min(cur*2, hi)
}

// sleepCtx pauses for d; it returns false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
