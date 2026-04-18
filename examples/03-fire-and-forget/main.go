// Fire-and-forget: EnqueueAsync returns as soon as the task is published.
// No response subscription is created. Useful for one-shot side-effects where
// the producer doesn't care about the result.
package main

import (
	"context"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

var processed atomic.Int32

// Audit records an event asynchronously. No caller waits for its completion.
func Audit(ctx context.Context, event, user string) error {
	processed.Add(1)
	log.Printf("AUDIT: event=%s user=%s", event, user)
	return nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-async-*")
	defer os.RemoveAll(storeDir)

	node, err := embed.StartNode(embed.NodeConfig{Port: -1, StoreDir: storeDir})
	check(err)
	defer node.Shutdown()

	nc, err := nats.Connect(node.ClientURL())
	check(err)
	defer nc.Close()

	js, _ := jetstream.New(nc)
	check(stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1}))

	reg := task.NewRegistry()
	task.MustRegister(reg, Audit)

	w, err := worker.New(nc, reg, worker.Options{Concurrency: 8})
	check(err)
	go func() { _ = w.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	c, err := client.New(ctx, nc, client.Options{})
	check(err)
	defer c.Close()

	// Fire 10 audit tasks without awaiting any.
	for i := 0; i < 10; i++ {
		id, err := client.EnqueueAsync(c, Audit, "login", "alice")
		check(err)
		log.Printf("enqueued task %d: %s", i, id)
	}

	// In a real system you'd never poll here — this is just so the example exits
	// cleanly after the worker has drained the queue.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && processed.Load() < 10 {
		time.Sleep(50 * time.Millisecond)
	}
	log.Printf("processed %d tasks", processed.Load())
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
