package embed

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

type ClusterConfig struct {
	// Size is the number of nodes (must be >= 1; 3 for HA quorum).
	Size int
	// Name is the cluster name shared by all nodes.
	Name string
	// BaseDir is the parent dir for per-node StoreDir. If empty, uses os.MkdirTemp.
	BaseDir string
	// ReadyWait is the per-node timeout waiting for connections.
	ReadyWait time.Duration
}

type Cluster struct {
	Nodes []*Node
	dir   string
	owned bool // true if BaseDir was auto-created and we should clean it up
}

// StartCluster spins up an in-process cluster of Size nodes on loopback with random free ports.
// All nodes share cfg.Name. Returns once all nodes accept connections; call WaitMetaLeader via
// the stream package before creating streams.
func StartCluster(cfg ClusterConfig) (*Cluster, error) {
	if cfg.Size < 1 {
		return nil, fmt.Errorf("embed: cluster size must be >= 1")
	}
	if cfg.Name == "" {
		cfg.Name = "ebind"
	}
	if cfg.ReadyWait == 0 {
		cfg.ReadyWait = 30 * time.Second
	}

	dir := cfg.BaseDir
	owned := false
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "ebind-cluster-*")
		if err != nil {
			return nil, err
		}
		owned = true
	}

	// Reserve ports up front so every node knows every peer's cluster URL at boot.
	clientPorts, err := freePorts(cfg.Size)
	if err != nil {
		if owned {
			os.RemoveAll(dir)
		}
		return nil, err
	}
	clusterPorts, err := freePorts(cfg.Size)
	if err != nil {
		if owned {
			os.RemoveAll(dir)
		}
		return nil, err
	}

	cluster := &Cluster{dir: dir, owned: owned}

	// Phase 1: construct all servers and start them concurrently so routes can form
	// before any single node commits to a singleton JetStream meta group.
	servers := make([]*natsserver.Server, cfg.Size)
	for i := 0; i < cfg.Size; i++ {
		nodeDir := filepath.Join(dir, fmt.Sprintf("node-%d", i))
		if err := os.MkdirAll(nodeDir, 0o755); err != nil {
			cluster.Shutdown()
			return nil, err
		}
		routes := make([]*url.URL, 0, cfg.Size-1)
		for j, p := range clusterPorts {
			if j == i {
				continue
			}
			u, _ := url.Parse(fmt.Sprintf("nats-route://127.0.0.1:%d", p))
			routes = append(routes, u)
		}
		opts := &natsserver.Options{
			ServerName: fmt.Sprintf("%s-%d", cfg.Name, i),
			Host:       "127.0.0.1",
			Port:       clientPorts[i],
			Cluster: natsserver.ClusterOpts{
				Name: cfg.Name,
				Host: "127.0.0.1",
				Port: clusterPorts[i],
			},
			Routes:    routes,
			JetStream: true,
			StoreDir:  nodeDir,
			NoLog:     true,
			NoSigs:    true,
		}
		srv, err := natsserver.NewServer(opts)
		if err != nil {
			cluster.Shutdown()
			return nil, fmt.Errorf("embed: cluster node %d: %w", i, err)
		}
		servers[i] = srv
		cluster.Nodes = append(cluster.Nodes, &Node{srv: srv, cfg: NodeConfig{
			ServerName: opts.ServerName,
			Host:       opts.Host,
			Port:       opts.Port,
			StoreDir:   opts.StoreDir,
		}})
	}

	// Phase 2: start all servers concurrently. Routes can discover each other during startup
	// rather than each node committing to a singleton meta group.
	for _, srv := range servers {
		go srv.Start()
	}
	for i, srv := range servers {
		if !srv.ReadyForConnections(cfg.ReadyWait) {
			cluster.Shutdown()
			return nil, fmt.Errorf("embed: cluster node %d not ready within %s", i, cfg.ReadyWait)
		}
	}
	return cluster, nil
}

// ClientURLs returns a comma-separated URL list suitable for nats.Connect.
func (c *Cluster) ClientURLs() string {
	var s string
	for i, n := range c.Nodes {
		if i > 0 {
			s += ","
		}
		s += n.ClientURL()
	}
	return s
}

// Shutdown stops all nodes. Does not wait between shutdowns — callers doing graceful
// rolling shutdowns should call Node.Shutdown() individually with appropriate gaps.
func (c *Cluster) Shutdown() {
	for _, n := range c.Nodes {
		if n != nil && n.srv != nil {
			n.srv.Shutdown()
		}
	}
	for _, n := range c.Nodes {
		if n != nil && n.srv != nil {
			n.srv.WaitForShutdown()
		}
	}
	if c.owned && c.dir != "" {
		os.RemoveAll(c.dir)
	}
}

// ShutdownNode stops a single node; useful for failover testing.
func (c *Cluster) ShutdownNode(i int) {
	if i < 0 || i >= len(c.Nodes) {
		return
	}
	c.Nodes[i].Shutdown()
}

// WaitReady blocks until the JetStream meta-leader is elected and the meta group
// has discovered all peers. Required before creating streams with Replicas > 1.
// Returns an error if the deadline passes first.
func (c *Cluster) WaitReady(timeout time.Duration) error {
	want := len(c.Nodes)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leader := false
		peersOK := false
		for _, n := range c.Nodes {
			if n.srv.JetStreamIsLeader() {
				leader = true
				if len(n.srv.JetStreamClusterPeers()) >= want {
					peersOK = true
				}
			}
		}
		if leader && peersOK {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("embed: cluster not ready within %s", timeout)
}

func freePorts(n int) ([]int, error) {
	listeners := make([]*net.TCPListener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			for _, x := range listeners {
				x.Close()
			}
			return nil, err
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		l.Close()
	}
	return ports, nil
}
