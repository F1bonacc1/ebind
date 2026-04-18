// Package testutil provides test helpers: embedded NATS + streams + worker.
package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

type Harness struct {
	Node   *embed.Node
	Conn   *nats.Conn
	JS     jetstream.JetStream
	Reg    *task.Registry
	Worker *worker.Worker
	Client *client.Client
	Cancel context.CancelFunc
	Errs   chan error
}

// SingleNode starts an embedded NATS, creates streams, registers a worker, and returns a ready Harness.
func SingleNode(t *testing.T, opts worker.Options) *Harness {
	t.Helper()
	storeDir := t.TempDir()
	node, err := embed.StartNode(embed.NodeConfig{
		ServerName: "test-" + t.Name(),
		Port:       -1,
		StoreDir:   storeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Shutdown)

	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer setupCancel()
	if err := stream.EnsureStreams(setupCtx, js, stream.Config{Replicas: 1}); err != nil {
		t.Fatal(err)
	}

	reg := task.NewRegistry()
	w, err := worker.New(nc, reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { errs <- w.Run(ctx) }()
	time.Sleep(200 * time.Millisecond) // let consumer bind

	c, err := client.New(ctx, nc, client.Options{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	t.Cleanup(cancel)

	return &Harness{
		Node:   node,
		Conn:   nc,
		JS:     js,
		Reg:    reg,
		Worker: w,
		Client: c,
		Cancel: cancel,
		Errs:   errs,
	}
}
