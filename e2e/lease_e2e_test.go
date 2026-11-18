//go:build e2e

package e2e

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// TestClusterLeaseE2E checks the worker's heartbeat lease on a 3-node R=3
// cluster. A worker keeps a task it is running, past AckWait, through the
// loss of its own node and of its consumer's raft leader; and a task held by
// a worker that dies goes to a surviving worker within about AckWait.
//
// It runs its own cluster with a short AckWait so each phase only has to hold
// a task for a few seconds past it.
func TestClusterLeaseE2E(t *testing.T) {
	const wait = 8 * time.Second // workers' AckWait; heartbeats every wait/3
	h := newHarnessAckWait(t, wait)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	ok := true
	phase := func(name string, fn func(t *testing.T)) {
		if !ok {
			t.Logf("skipping phase %s: an earlier phase failed", name)
			return
		}
		ok = t.Run(name, fn)
	}

	// holdSingleRun keeps the gate closed for d and fails as soon as the task
	// starts a second time: JetStream redelivered a task its worker still
	// held.
	holdSingleRun := func(t *testing.T, d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if ws := leaseLog.workers(); len(ws) != 1 {
				t.Fatalf("task redelivered while its worker held it; runs by %v", ws)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	phase("L1_KillHoldingWorkersNode", func(t *testing.T) {
		resetLease()
		fut := h.enqueueOptsWithRetry(t, 30*time.Second, eLeaseHold,
			client.EnqueueOptions{Target: worker.ConcreteTarget("w1")}, 1)
		waitReached(t, leaseGate, 30*time.Second)

		w1 := h.workers[0]
		victim := h.nodeIndexByURL(w1.nc.ConnectedUrl())
		if victim < 0 {
			t.Fatalf("w1 connected to unknown URL %q", w1.nc.ConnectedUrl())
		}
		t.Logf("killing node %d, the node w1 is connected to, while w1 holds the task", victim)
		h.cluster.ShutdownNode(victim)
		waitFor(t, 15*time.Second, "w1 fails over to a surviving node", func() bool {
			return w1.nc.Status() == nats.CONNECTED && w1.nc.ConnectedUrl() != h.urls[victim]
		})

		holdSingleRun(t, wait+4*time.Second)
		leaseGate.Release()
		awaitInt(t, fut, 60*time.Second, 1)
		if ws := leaseLog.workers(); !slices.Equal(ws, []string{"w1"}) {
			t.Fatalf("runs by %v, want exactly one run on w1", ws)
		}
		h.restoreNode(t, victim)
	})

	phase("L2_KillConsumerLeader", func(t *testing.T) {
		resetLease()
		// Prefer a worker whose own durable is led from another node, so this
		// kill moves the consumer's raft leader without cutting the worker's
		// connection.
		var holder *workerProc
		leader := -1
		for _, p := range h.workers {
			l := h.consumerLeaderIndex(t, "ebind-targets-"+stream.TargetToken(worker.ConcreteTarget(p.id)))
			if l < 0 {
				continue
			}
			if holder == nil || l != h.nodeIndexByURL(p.nc.ConnectedUrl()) {
				holder, leader = p, l
			}
			if l != h.nodeIndexByURL(p.nc.ConnectedUrl()) {
				break
			}
		}
		if holder == nil {
			t.Fatal("no consumer leader found for any worker's durable")
		}
		fut := h.enqueueOptsWithRetry(t, 30*time.Second, eLeaseHold,
			client.EnqueueOptions{Target: worker.ConcreteTarget(holder.id)}, 2)
		waitReached(t, leaseGate, 30*time.Second)

		t.Logf("killing node %d, leader of %s's consumer (%s is connected to node %d)",
			leader, holder.id, holder.id, h.nodeIndexByURL(holder.nc.ConnectedUrl()))
		h.cluster.ShutdownNode(leader)

		holdSingleRun(t, wait+4*time.Second)
		leaseGate.Release()
		awaitInt(t, fut, 60*time.Second, 2)
		if ws := leaseLog.workers(); !slices.Equal(ws, []string{holder.id}) {
			t.Fatalf("runs by %v, want exactly one run on %s", ws, holder.id)
		}
		h.restoreNode(t, leader)
	})

	phase("L3_DeadWorkerFailover", func(t *testing.T) {
		resetLease()
		const dagID = "lease-dead-worker"
		dag := workflow.New(workflow.WithDAGID(dagID))
		dag.Step("hold", eLeaseHold, 3)
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Minute)
		defer pcancel()
		if err := dag.Submit(pctx, h.wf); err != nil {
			t.Fatal(err)
		}
		waitReached(t, leaseGate, 30*time.Second)

		first := leaseLog.workers()[0]
		var victim *workerProc
		for _, p := range h.workers {
			if p.id == first {
				victim = p
			}
		}
		t.Logf("worker %s holds the step; closing its connection (the process dies)", first)
		died := time.Now()
		victim.nc.Close()

		waitFor(t, wait+10*time.Second, "a surviving worker takes the step over", func() bool {
			return len(leaseLog.workers()) >= 2
		})
		if took := time.Since(died); took > wait+3*time.Second {
			t.Errorf("takeover %v after the worker died; want within about AckWait (%v)", took, wait)
		}
		runs := leaseLog.snapshot()
		if runs[1].worker == first {
			t.Fatalf("the dead worker %s ran the step again", first)
		}
		leaseGate.Release()
		if got, err := workflow.AwaitByID[int](pctx, h.wf, dagID, "hold"); err != nil || got != 3 {
			t.Fatalf("hold: got %d, %v; want 3", got, err)
		}

		runs = leaseLog.snapshot()
		if len(runs) != 2 {
			t.Fatalf("runs by %v, want the dead worker's and one survivor's", leaseLog.workers())
		}
		if !errors.Is(runs[0].cause, worker.ErrLeaseLost) {
			t.Errorf("dead worker's run ended with cause %v, want ErrLeaseLost", runs[0].cause)
		}
		rec, _, err := h.wf.Store.GetStep(pctx, dagID, "hold")
		if err != nil {
			t.Fatal(err)
		}
		if rec.WorkerID != runs[1].worker {
			t.Errorf("step record WorkerID = %q, want the survivor %q", rec.WorkerID, runs[1].worker)
		}
	})
}

func waitReached(t *testing.T, g *gate, timeout time.Duration) {
	t.Helper()
	select {
	case <-g.Reached():
	case <-time.After(timeout):
		t.Fatalf("gated handler did not start within %v", timeout)
	}
}

func awaitInt(t *testing.T, fut *client.Future, timeout time.Duration, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if got, err := client.Await[int](ctx, fut); err != nil || got != want {
		t.Fatalf("got %d, %v; want %d", got, err, want)
	}
}

// enqueueOptsWithRetry is enqueueWithRetry for client.EnqueueOpts.
func (h *harness) enqueueOptsWithRetry(t *testing.T, timeout time.Duration, fn any, opts client.EnqueueOptions, args ...any) *client.Future {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		fut, err := client.EnqueueOpts(h.cli, fn, opts, args...)
		if err == nil {
			return fut
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("enqueue did not succeed within %s: %v", timeout, lastErr)
	return nil
}

func (h *harness) nodeIndexByURL(url string) int {
	return slices.Index(h.urls, url)
}

// consumerLeaderIndex returns the node index leading the named EBIND_TASKS
// consumer, or -1.
func (h *harness) consumerLeaderIndex(t *testing.T, durable string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := h.js.Stream(ctx, stream.TaskStream)
	if err != nil {
		return -1
	}
	c, err := s.Consumer(ctx, durable)
	if err != nil {
		return -1
	}
	info, err := c.Info(ctx)
	if err != nil || info.Cluster == nil {
		return -1
	}
	for i, n := range h.cluster.Nodes {
		if n.Server().Name() == info.Cluster.Leader {
			return i
		}
	}
	return -1
}

// restoreNode restarts a killed node and waits until the cluster is whole
// again, with the task stream back to full replication.
func (h *harness) restoreNode(t *testing.T, i int) {
	t.Helper()
	if err := h.cluster.RestartNode(i); err != nil {
		t.Fatal(err)
	}
	if err := h.cluster.WaitReady(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := h.cluster.WaitNodeHealthy(i, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "TASKS stream back to 3 current replicas", func() bool {
		return h.streamHasCurrentReplicas(stream.TaskStream)
	})
}
