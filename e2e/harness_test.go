//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

const (
	numNodes = 3
	replicas = 3
	// ackWait bounds the redelivery stall after a lost ack during a node kill.
	// Gate holds that must stay short (gate1, gate2) are released well inside
	// this window; gate3 is deliberately held across a node restart so its
	// redeliveries exercise the idempotent re-execution path.
	ackWait = 15 * time.Second
)

// countingMetrics counts handler dispatches per worker via worker.WithMetrics.
type countingMetrics struct{ observed atomic.Int64 }

func (m *countingMetrics) Observe(string, int, time.Duration, error) { m.observed.Add(1) }

type workerProc struct {
	id      string
	nc      *nats.Conn
	wf      *workflow.Workflow
	metrics *countingMetrics
	done    chan error // receives worker.Run and RunScheduler exits
}

type harness struct {
	cluster *embed.Cluster
	urls    []string // per-node client URLs, index-aligned with cluster.Nodes
	nc      *nats.Conn
	js      jetstream.JetStream
	wf      *workflow.Workflow // driver-side instance, separate from the workers'
	cli     *client.Client
	workers []*workerProc

	// Failure-injection state shared across phases.
	victim    int // node killed in the follower phase; -1 until then
	victimURL string
	canary    *nats.Conn // knows only the victim's URL; proves the restarted node serves clients
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	resetGates()

	c, err := embed.StartCluster(embed.ClusterConfig{
		Size:      numNodes,
		Name:      "e2e",
		BaseDir:   t.TempDir(),
		ReadyWait: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{cluster: c, victim: -1}
	if err := c.WaitReady(30 * time.Second); err != nil {
		c.Shutdown()
		t.Fatal(err)
	}
	for _, n := range c.Nodes {
		h.urls = append(h.urls, n.ClientURL())
	}

	h.nc, err = nats.Connect(strings.Join(h.urls, ","),
		nats.ReconnectWait(100*time.Millisecond),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		t.Fatal(err)
	}
	h.js, err = jetstream.New(h.nc)
	if err != nil {
		t.Fatal(err)
	}

	setupRetry(t, "ensure streams", func(ctx context.Context) error {
		return stream.EnsureStreams(ctx, h.js, stream.Config{Replicas: replicas})
	})
	setupRetry(t, "driver workflow", func(ctx context.Context) error {
		var err error
		h.wf, err = workflow.NewFromNATS(ctx, h.nc, replicas)
		return err
	})

	runCtx, runCancel := context.WithCancel(context.Background())
	// Register cleanup before anything that can t.Fatal below, so a failed
	// setup still tears the cluster down. Fields are nil-guarded because the
	// harness may be partially constructed.
	t.Cleanup(func() {
		releaseAllGates() // unblock any handler still parked on a gate
		runCancel()
		for _, p := range h.workers {
			for i := 0; i < 2; i++ {
				select {
				case <-p.done:
				case <-time.After(25 * time.Second):
				}
			}
		}
		if h.cli != nil {
			h.cli.Close()
		}
		if h.canary != nil {
			h.canary.Close()
		}
		for _, p := range h.workers {
			p.nc.Close()
		}
		h.nc.Close()
		c.Shutdown()
	})

	claims := map[string][]string{"w1": {"primary"}, "w2": {"secondary"}, "w3": nil}
	for k := 0; k < numNodes; k++ {
		id := fmt.Sprintf("w%d", k+1)
		// Rotate the URL list so this worker's initial TCP connection lands on
		// node k — a node kill then genuinely severs one worker's connection,
		// while the remaining URLs let it fail over and keep working.
		rotated := append(append([]string{}, h.urls[k:]...), h.urls[:k]...)
		nc, err := nats.Connect(strings.Join(rotated, ","),
			nats.DontRandomize(),
			nats.ReconnectWait(100*time.Millisecond),
			nats.MaxReconnects(-1),
		)
		if err != nil {
			t.Fatal(err)
		}
		var wf *workflow.Workflow
		setupRetry(t, "worker "+id+" workflow", func(ctx context.Context) error {
			var err error
			wf, err = workflow.NewFromNATS(ctx, nc, replicas)
			return err
		})
		// Fast repair: events lost to transient failures during injection are
		// recovered by the periodic sweep instead of waiting out the defaults.
		wf.SweepCheckInterval = 500 * time.Millisecond
		wf.SweepInterval = 5 * time.Second
		wf.SweepTimeout = 30 * time.Second

		reg := task.NewRegistry()
		if err := registerAll(reg, id); err != nil {
			t.Fatal(err)
		}
		m := &countingMetrics{}
		w, err := worker.New(nc, reg, worker.Options{
			Concurrency:   8,
			AckWait:       ackWait,
			ShutdownGrace: 20 * time.Second,
			StepHook:      wf.Hook(),
			Middleware: []worker.Middleware{
				wf.ContextMiddleware(),
				worker.Log(slog.New(slog.NewTextHandler(io.Discard, nil))),
				worker.WithMetrics(m),
			},
			WorkerID:             id,
			Claims:               worker.StaticClaims(claims[id]),
			ClaimRefreshInterval: 100 * time.Millisecond,
			ClaimRetryDelay:      50 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		p := &workerProc{id: id, nc: nc, wf: wf, metrics: m, done: make(chan error, 2)}
		// Supervise both loops: startup on a fresh R=3 cluster can transiently
		// fail (consumer creation timeouts while raft groups place), and a
		// one-shot goroutine would leave a silently dead worker. Restart until
		// the harness shuts down — what a process supervisor does in production.
		go supervise(runCtx, p.done, func() error { return w.Run(runCtx) })
		go supervise(runCtx, p.done, func() error { return wf.RunScheduler(runCtx) })
		h.workers = append(h.workers, p)
	}

	setupRetry(t, "task client", func(ctx context.Context) error {
		var err error
		h.cli, err = client.New(ctx, h.nc, client.Options{})
		return err
	})

	// Behavioral readiness: every worker must answer a task targeted at its
	// concrete claim before any phase runs. This proves the general and
	// per-claim consumers are live — no blind settle sleeps.
	for _, p := range h.workers {
		probe := p
		waitFor(t, 90*time.Second, "worker "+probe.id+" serves targeted tasks", func() bool {
			fut, err := client.EnqueueOpts(h.cli, WhoAmI, client.EnqueueOptions{Target: worker.ConcreteTarget(probe.id)})
			if err != nil {
				return false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := client.Await[string](ctx, fut)
			return err == nil && got == probe.id
		})
	}
	return h
}

// supervise re-runs fn until ctx is canceled, then reports the final exit on
// done. Early exits (startup errors) are retried after a short pause.
func supervise(ctx context.Context, done chan<- error, fn func() error) {
	for {
		err := fn()
		if ctx.Err() != nil {
			done <- err
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// setupRetry retries a JetStream asset-creation call until it succeeds or a
// 60s deadline passes. Creating R=3 assets right after cluster start can
// transiently time out while raft groups are placed, so each call gets its own
// attempt timeout instead of sharing one setup context.
func setupRetry(t *testing.T, desc string, fn func(ctx context.Context) error) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		lastErr = fn(ctx)
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s did not succeed within 60s: %v", desc, lastErr)
}

// waitFor polls cond every 100ms until true, failing the test after timeout.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, desc)
}

func (h *harness) stepStatus(ctx context.Context, dagID, stepID string) workflow.StepStatus {
	rec, _, err := h.wf.Store.GetStep(ctx, dagID, stepID)
	if err != nil {
		return ""
	}
	return rec.Status
}

func (h *harness) dagStatus(ctx context.Context, dagID string) workflow.DAGStatus {
	meta, _, err := h.wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return ""
	}
	return meta.Status
}

// enqueueWithRetry retries client.Enqueue across transient publish failures —
// stream-leader re-election windows surface as "no response from stream".
func (h *harness) enqueueWithRetry(t *testing.T, timeout time.Duration, fn any, args ...any) *client.Future {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		fut, err := client.Enqueue(h.cli, fn, args...)
		if err == nil {
			return fut
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("enqueue did not succeed within %s: %v", timeout, lastErr)
	return nil
}

// metaLeaderIndex returns the index of the running JetStream meta-leader, or -1.
func (h *harness) metaLeaderIndex() int {
	for i, n := range h.cluster.Nodes {
		if n.Server().Running() && n.Server().JetStreamIsLeader() {
			return i
		}
	}
	return -1
}

// followerIndex returns the index of a running non-meta-leader node, or -1.
func (h *harness) followerIndex() int {
	for i, n := range h.cluster.Nodes {
		if n.Server().Running() && !n.Server().JetStreamIsLeader() {
			return i
		}
	}
	return -1
}

// waitMetaLeader waits until some running node reports JetStream meta leadership.
// Unlike Cluster.WaitReady it tolerates a node being down, so it is the right
// readiness check immediately after a kill.
func (h *harness) waitMetaLeader(t *testing.T, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "meta-leader elected", func() bool { return h.metaLeaderIndex() >= 0 })
}

// ebindStreams lists every stream the system depends on, including the
// workflow KV bucket's backing stream.
func ebindStreams() []string {
	return []string{stream.TaskStream, stream.ResponseStream, stream.DLQStream, workflow.DAGEventsStream, "KV_ebind-dags"}
}

// waitStreamLeadersSettled waits until every ebind stream reports an elected
// raft leader. Publishes and KV writes stop failing transiently once this
// holds, so gated handlers released after this point complete reliably while
// the cluster still runs degraded.
func (h *harness) waitStreamLeadersSettled(t *testing.T, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "stream leaders elected", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, name := range ebindStreams() {
			s, err := h.js.Stream(ctx, name)
			if err != nil {
				return false
			}
			info, err := s.Info(ctx)
			if err != nil || info.Cluster == nil || info.Cluster.Leader == "" {
				return false
			}
		}
		return true
	})
}

// streamHasCurrentReplicas reports whether the stream has the full replica set
// and every replica is caught up.
func (h *harness) streamHasCurrentReplicas(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := h.js.Stream(ctx, name)
	if err != nil {
		return false
	}
	info, err := s.Info(ctx)
	if err != nil || info.Cluster == nil || 1+len(info.Cluster.Replicas) != replicas {
		return false
	}
	for _, r := range info.Cluster.Replicas {
		if !r.Current {
			return false
		}
	}
	return true
}

// dlqEntries drains the DLQ stream through an ephemeral consumer and returns
// entry counts grouped by "<task name>/<error kind>".
func (h *harness) dlqEntries(t *testing.T, ctx context.Context) map[string]int {
	t.Helper()
	s, err := h.js.Stream(ctx, stream.DLQStream)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := s.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		AckPolicy:         jetstream.AckExplicitPolicy,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		InactiveThreshold: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for {
		batch, err := cons.Fetch(50, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			break
		}
		n := 0
		for msg := range batch.Messages() {
			var e dlq.Entry
			if err := json.Unmarshal(msg.Data(), &e); err != nil {
				t.Fatalf("decode DLQ entry: %v", err)
			}
			counts[e.Task.Name+"/"+e.Error.Kind]++
			_ = msg.Ack()
			n++
		}
		if n == 0 {
			break
		}
	}
	return counts
}
