package embed

import (
	"fmt"
	"testing"
	"time"
)

// TestFreePorts_Distinct pins the contract StartCluster depends on: every port
// from one call is different from every other. freePorts holds all its
// listeners open until the last one is chosen, so this holds by construction —
// the test exists because the property is invisible at the call site and easy
// to break by "simplifying" the function to reserve one port at a time.
//
// Worth running with -count=500: a regression here is probabilistic, not
// deterministic, so a single pass proves little.
func TestFreePorts_Distinct(t *testing.T) {
	for _, n := range []int{2, 6, 16} {
		ports, err := freePorts(n)
		if err != nil {
			t.Fatalf("freePorts(%d): %v", n, err)
		}
		if len(ports) != n {
			t.Fatalf("freePorts(%d): got %d ports, want %d", n, len(ports), n)
		}
		seen := make(map[int]bool, n)
		for _, p := range ports {
			if p == 0 {
				t.Errorf("freePorts(%d): got port 0", n)
			}
			if seen[p] {
				t.Errorf("freePorts(%d): port %d returned twice: %v", n, p, ports)
			}
			seen[p] = true
		}
	}
}

// TestStartCluster_ClientAndClusterPortsDisjoint guards the failure that took
// down a nightly e2e run: when the client and cluster port sets overlap, some
// node is told to bind the same number twice, one of the two listeners fails,
// and the whole cluster start times out with that node never becoming ready.
//
// Drawing both sets from a single reservation makes the overlap impossible;
// this asserts the property end to end rather than trusting the arithmetic.
func TestStartCluster_ClientAndClusterPortsDisjoint(t *testing.T) {
	c, err := StartCluster(ClusterConfig{Size: 3, Name: "test-ports", BaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()

	seen := make(map[int]string, 2*len(c.Nodes))
	for i, n := range c.Nodes {
		for kind, port := range map[string]int{
			"client":  n.opts.Port,
			"cluster": n.opts.Cluster.Port,
		} {
			if port == 0 {
				t.Fatalf("node %d: %s port is 0", i, kind)
			}
			if prev, dup := seen[port]; dup {
				t.Errorf("port %d assigned twice: %s and node %d %s", port, prev, i, kind)
			}
			seen[port] = fmt.Sprintf("node %d %s", i, kind)
		}
	}
}

// TestStartCluster_ReadyPromptly bounds the readiness path. A cluster start is
// either fast or broken: nominal is tens of milliseconds, and the observed
// failure mode is a node that never binds at all and burns the entire
// ReadyWait. Anything in between would be a new behavior worth noticing.
//
// The reporting path for a start that does fail is covered by
// TestStartNode_ReportsBindFailure — a port the cluster is about to claim
// cannot be held from outside StartCluster, so it is tested through a node.
func TestStartCluster_ReadyPromptly(t *testing.T) {
	start := time.Now()
	c, err := StartCluster(ClusterConfig{Size: 3, Name: "test-prompt", BaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	// Nominal is tens of milliseconds; a loaded CI runner is given room to be
	// two orders of magnitude slower before this is considered a regression.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("StartCluster took %s, want well under the 30s ReadyWait", elapsed)
	}
}
