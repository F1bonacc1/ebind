package embed

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// waitClusterReady blocks until a meta-leader is elected AND the meta group
// has discovered all peers, i.e. stream placement with N replicas can succeed.
func waitClusterReady(t *testing.T, c *Cluster, timeout time.Duration) {
	t.Helper()
	want := len(c.Nodes)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leader := false
		peersOK := false
		for _, n := range c.Nodes {
			if n.srv.JetStreamIsLeader() {
				leader = true
				peers := n.srv.JetStreamClusterPeers()
				if len(peers) >= want {
					peersOK = true
				}
			}
		}
		if leader && peersOK {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("cluster not ready within %s", timeout)
}

// publishWithRetry retries js.Publish until it succeeds or the timeout expires.
// CreateStream returns once accepted by the meta-leader, but the stream's Raft
// group and subject subscription can lag — publishes in that window return
// "no response from stream". Same pattern is needed after node loss while the
// stream leader re-elects, so the retry is shared.
func publishWithRetry(t *testing.T, ctx context.Context, js jetstream.JetStream, subj string, data []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, lastErr = js.Publish(attemptCtx, subj, data)
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("publish %q did not succeed within %s: %v", subj, timeout, lastErr)
}

func TestCluster_FormsQuorum(t *testing.T) {
	c, err := StartCluster(ClusterConfig{Size: 3, Name: "test-quorum"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()

	waitClusterReady(t, c, 15*time.Second)

	nc, err := nats.Connect(c.ClientURLs())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "CL_TEST",
		Subjects: []string{"cl.test.>"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("create replicated stream: %v", err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.Cluster == nil {
		t.Fatal("stream info: no cluster details")
	}
	total := 1 + len(info.Cluster.Replicas)
	if total != 3 {
		t.Errorf("stream replicas: got %d, want 3 (leader=%s replicas=%v)",
			total, info.Cluster.Leader, info.Cluster.Replicas)
	}
}

func TestCluster_RestartNode(t *testing.T) {
	c, err := StartCluster(ClusterConfig{Size: 3, Name: "test-restart"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()

	waitClusterReady(t, c, 15*time.Second)

	nc, err := nats.Connect(c.ClientURLs(), nats.ReconnectWait(100*time.Millisecond), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	s, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "RESTART_TEST",
		Subjects: []string{"rt.>"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	publishWithRetry(t, ctx, js, "rt.pre", []byte("before"), 15*time.Second)

	victim := -1
	for i, n := range c.Nodes {
		if !n.srv.JetStreamIsLeader() {
			victim = i
			break
		}
	}
	if victim < 0 {
		t.Fatal("no follower to kill")
	}
	victimURL := c.Nodes[victim].ClientURL()
	t.Logf("killing node %d (%s)", victim, victimURL)
	c.ShutdownNode(victim)

	publishWithRetry(t, ctx, js, "rt.during", []byte("degraded"), 15*time.Second)

	if err := c.RestartNode(victim); err != nil {
		t.Fatalf("restart node %d: %v", victim, err)
	}
	if got := c.Nodes[victim].ClientURL(); got != victimURL {
		t.Fatalf("restarted node client URL changed: got %s, want %s", got, victimURL)
	}
	waitClusterReady(t, c, 30*time.Second)
	if err := c.WaitNodeHealthy(victim, 60*time.Second); err != nil {
		t.Fatal(err)
	}

	// A client that only knows the restarted node must be able to connect and
	// use JetStream through it.
	vnc, err := nats.Connect(victimURL)
	if err != nil {
		t.Fatalf("connect to restarted node: %v", err)
	}
	defer vnc.Close()
	vjs, err := jetstream.New(vnc)
	if err != nil {
		t.Fatal(err)
	}
	publishWithRetry(t, ctx, vjs, "rt.post", []byte("after"), 15*time.Second)

	// The stream must converge back to 3 current replicas.
	deadline := time.Now().Add(60 * time.Second)
	for {
		info, err := s.Info(ctx)
		if err == nil && info.Cluster != nil && len(info.Cluster.Replicas) == 2 {
			current := true
			for _, r := range info.Cluster.Replicas {
				if !r.Current {
					current = false
				}
			}
			if current {
				if info.State.Msgs < 3 {
					t.Errorf("want >=3 msgs in stream, got %d", info.State.Msgs)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream did not converge to 3 current replicas: %+v", info)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestCluster_SurvivesOneNodeLoss(t *testing.T) {
	c, err := StartCluster(ClusterConfig{Size: 3, Name: "test-failover"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()

	waitClusterReady(t, c, 15*time.Second)

	nc, err := nats.Connect(c.ClientURLs(), nats.ReconnectWait(100*time.Millisecond), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err = js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "FAIL_TEST",
		Subjects: []string{"ft.>"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	publishWithRetry(t, ctx, js, "ft.pre", []byte("before"), 15*time.Second)

	// Kill a non-leader follower to minimize restart time.
	victim := -1
	for i, n := range c.Nodes {
		if !n.srv.JetStreamIsLeader() {
			victim = i
			break
		}
	}
	if victim < 0 {
		t.Fatal("no follower to kill")
	}
	t.Logf("killing node %d", victim)
	c.ShutdownNode(victim)

	publishWithRetry(t, ctx, js, "ft.post", []byte("after"), 15*time.Second)

	s, err := js.Stream(ctx, "FAIL_TEST")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs < 2 {
		t.Errorf("want >=2 msgs in stream, got %d", info.State.Msgs)
	}
}
