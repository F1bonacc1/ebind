// Retry policy: a task with a RetryPolicy attached to it.
// The handler fails twice then succeeds; the policy caps attempts + sets backoff.
package main

import (
	"context"
	"errors"
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

var attempts atomic.Int32

// Flaky fails the first 2 calls, succeeds on the 3rd.
func Flaky(ctx context.Context, input string) (string, error) {
	n := attempts.Add(1)
	log.Printf("Flaky called, attempt #%d", n)
	if n < 3 {
		return "", errors.New("transient")
	}
	return "ok after " + input, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-retry-*")
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
	task.MustRegister(reg, Flaky)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 1,
		AckWait:     time.Second,
		// MaxDeliver is the outer bound from NATS. The per-task RetryPolicy can be
		// tighter but not higher. Give it headroom so the policy is the actual cap.
		MaxDeliver: 10,
		Backoff: []time.Duration{
			50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond,
			50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond,
			50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond,
		},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	c, err := client.New(ctx, nc, client.Options{})
	check(err)
	defer c.Close()

	// Retry policy — exponential backoff with a cap and attempt limit.
	policy := task.RetryPolicy{
		InitialInterval:    50 * time.Millisecond,
		BackoffCoefficient: 2.0,
		MaximumInterval:    500 * time.Millisecond,
		MaximumAttempts:    5,
		// Example of kinds the policy should NOT retry even if Retryable=true:
		NonRetryableErrorKinds: []string{"validation", "auth"},
	}

	fut, err := client.EnqueueOpts(c, Flaky,
		client.EnqueueOptions{RetryPolicy: &policy},
		"retry-demo",
	)
	check(err)

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	result, err := client.Await[string](waitCtx, fut)
	check(err)

	log.Printf("final result: %s (after %d attempts)", result, attempts.Load())
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
