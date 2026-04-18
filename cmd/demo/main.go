// cmd/demo: single-process end-to-end round-trip.
// Starts an embedded NATS server, registers a handler, enqueues a task, awaits the response.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

type Meta struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Foo: the user's example — (ctx, int, string, struct) (string, error)
func Foo(ctx context.Context, count int, name string, meta Meta) (string, error) {
	return fmt.Sprintf("Foo got count=%d name=%q tag=%q extra=%d",
		count, name, meta.Tag, meta.Count), nil
}

// Greet: error-only signature.
func Greet(ctx context.Context, who string) error {
	log.Printf("[handler] Greet: hello %s", who)
	return nil
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, err := os.MkdirTemp("", "ebind-demo-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(storeDir)

	node, err := embed.StartNode(embed.NodeConfig{
		ServerName: "ebind-demo",
		Port:       -1,
		StoreDir:   storeDir,
	})
	if err != nil {
		return err
	}
	defer node.Shutdown()
	log.Printf("embedded NATS listening at %s", node.ClientURL())

	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		return err
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := stream.EnsureStreams(setupCtx, js, stream.Config{Replicas: 1}); err != nil {
		setupCancel()
		return err
	}
	setupCancel()

	reg := task.NewRegistry()
	if err := task.Register(reg, Foo); err != nil {
		return err
	}
	if err := task.Register(reg, Greet); err != nil {
		return err
	}
	log.Printf("registered handlers: %v", reg.Names())

	w, err := worker.New(nc, reg, worker.Options{Concurrency: 4})
	if err != nil {
		return err
	}
	workerErr := make(chan error, 1)
	go func() { workerErr <- w.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	c, err := client.New(ctx, nc, client.Options{})
	if err != nil {
		return err
	}
	defer c.Close()

	fut, err := client.Enqueue(c, Foo, 42, "widgets", Meta{Tag: "prod", Count: 7})
	if err != nil {
		return fmt.Errorf("enqueue Foo: %w", err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := client.Await[string](waitCtx, fut)
	waitCancel()
	if err != nil {
		return fmt.Errorf("await Foo: %w", err)
	}
	log.Printf("Foo result: %s", result)

	fut2, err := client.Enqueue(c, Greet, "world")
	if err != nil {
		return fmt.Errorf("enqueue Greet: %w", err)
	}
	waitCtx2, waitCancel2 := context.WithTimeout(ctx, 5*time.Second)
	err = fut2.Get(waitCtx2, nil)
	waitCancel2()
	if err != nil {
		return fmt.Errorf("await Greet: %w", err)
	}
	log.Printf("Greet completed")

	if _, err := client.Enqueue(c, Foo, "wrong", 1, Meta{}); err == nil {
		return fmt.Errorf("expected arg validation error, got nil")
	} else {
		log.Printf("arg validation correctly rejected: %v", err)
	}

	cancel()
	if err := <-workerErr; err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	log.Printf("demo complete")
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("demo failed: %v", err)
	}
}
