// Basic ebind usage: register one handler, enqueue a task, await the typed result.
// Single process — embedded NATS JetStream runs in the same binary.
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

// The handler — a regular Go function. First arg is context.Context, last return is error.
// Everything else is arbitrary.
func Greet(ctx context.Context, name string, excited bool) (string, error) {
	punctuation := "."
	if excited {
		punctuation = "!"
	}
	return fmt.Sprintf("hello %s%s", name, punctuation), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-basic-*")
	defer os.RemoveAll(storeDir)

	node, err := embed.StartNode(embed.NodeConfig{Port: -1, StoreDir: storeDir})
	check(err)
	defer node.Shutdown()

	nc, err := nats.Connect(node.ClientURL())
	check(err)
	defer nc.Close()

	js, _ := jetstream.New(nc)
	check(stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1}))

	// Register the handler — reflection extracts the signature.
	reg := task.NewRegistry()
	task.MustRegister(reg, Greet)

	// Start a worker.
	w, err := worker.New(nc, reg, worker.Options{Concurrency: 4})
	check(err)
	go func() { _ = w.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	// Client enqueues + awaits the typed result.
	c, err := client.New(ctx, nc, client.Options{})
	check(err)
	defer c.Close()

	fut, err := client.Enqueue(c, Greet, "world", true)
	check(err)

	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	result, err := client.Await[string](waitCtx, fut)
	check(err)

	log.Printf("result: %s", result)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
