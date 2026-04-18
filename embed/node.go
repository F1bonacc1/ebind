// Package embed starts and supervises NATS JetStream servers in-process.
package embed

import (
	"fmt"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

type NodeConfig struct {
	ServerName string
	Host       string
	Port       int
	StoreDir   string
	ReadyWait  time.Duration
}

type Node struct {
	srv *natsserver.Server
	cfg NodeConfig
}

func StartNode(cfg NodeConfig) (*Node, error) {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.ReadyWait == 0 {
		cfg.ReadyWait = 10 * time.Second
	}
	opts := &natsserver.Options{
		ServerName: cfg.ServerName,
		Host:       cfg.Host,
		Port:       cfg.Port,
		JetStream:  true,
		StoreDir:   cfg.StoreDir,
		NoLog:      true,
		NoSigs:     true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("embed: new server: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(cfg.ReadyWait) {
		srv.Shutdown()
		return nil, fmt.Errorf("embed: server not ready within %s", cfg.ReadyWait)
	}
	return &Node{srv: srv, cfg: cfg}, nil
}

func (n *Node) ClientURL() string { return n.srv.ClientURL() }

func (n *Node) Shutdown() {
	n.srv.Shutdown()
	n.srv.WaitForShutdown()
}

func (n *Node) Server() *natsserver.Server { return n.srv }
