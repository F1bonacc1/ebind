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
