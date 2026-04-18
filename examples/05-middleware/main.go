// Custom middleware: a timing wrapper that logs every handler's duration.
// ebind's built-ins (Recover, Log) are in worker/middleware.go; you compose
// your own the same way.
package main

import (
	"context"
	"log"
	"log/slog"
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

func SlowProcess(ctx context.Context, work string) (string, error) {
	time.Sleep(50 * time.Millisecond) // pretend to do work
	return "done: " + work, nil
}

// Timing is a custom middleware that logs every handler's wall-clock duration.
// It's a function matching the worker.Middleware signature: takes a Handler and
// returns a wrapped Handler.
func Timing() worker.Middleware {
	return func(next worker.Handler) worker.Handler {
		return func(ctx context.Context, t *task.Task) ([]byte, error) {
			start := time.Now()
			result, err := next(ctx, t)
			log.Printf("[timing] %s: %v (err=%v)", t.Name, time.Since(start), err)
			return result, err
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-mw-*")
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
	task.MustRegister(reg, SlowProcess)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Compose: built-in Log + your custom Timing middleware.
	// Recover is always added first by the worker — panics never escape.
	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 2,
		Middleware: []worker.Middleware{
			worker.Log(logger),
			Timing(),
		},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	c, err := client.New(ctx, nc, client.Options{})
	check(err)
	defer c.Close()

	fut, err := client.Enqueue(c, SlowProcess, "widgets")
	check(err)
	waitCtx, wc := context.WithTimeout(ctx, 5*time.Second)
	defer wc()
	result, err := client.Await[string](waitCtx, fut)
	check(err)
	log.Printf("handler result: %s", result)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
