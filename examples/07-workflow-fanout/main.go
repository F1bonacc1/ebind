// Fan-out / fan-in: kick off two independent steps in parallel, then combine.
// The scheduler enqueues both A and B as roots; once both complete, C becomes
// ready and consumes their results.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

func FetchProfile(ctx context.Context, userID string) (string, error) {
	log.Printf("fetching profile for %s", userID)
	time.Sleep(100 * time.Millisecond) // pretend to be I/O bound
	return fmt.Sprintf("profile(%s)", userID), nil
}

func FetchHistory(ctx context.Context, userID string) (int, error) {
	log.Printf("fetching history for %s", userID)
	time.Sleep(100 * time.Millisecond)
	return 42, nil
}

func Combine(ctx context.Context, profile string, historyCount int) (string, error) {
	return fmt.Sprintf("%s has %d history entries", profile, historyCount), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-fanout-*")
	defer os.RemoveAll(storeDir)

	node, err := embed.StartNode(embed.NodeConfig{Port: -1, StoreDir: storeDir})
	check(err)
	defer node.Shutdown()

	nc, err := nats.Connect(node.ClientURL())
	check(err)
	defer nc.Close()

	js, _ := jetstream.New(nc)
	check(stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1}))

	wf, err := workflow.NewFromNATS(ctx, nc, 1)
	check(err)

	reg := task.NewRegistry()
	task.MustRegister(reg, FetchProfile)
	task.MustRegister(reg, FetchHistory)
	task.MustRegister(reg, Combine)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4, // both roots run in parallel
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	dag := workflow.New()
	// Two independent roots.
	a := dag.Step("profile", FetchProfile, "user-42")
	b := dag.Step("history", FetchHistory, "user-42")
	// Fan-in: C depends on both. Scheduler waits for both Done before enqueuing.
	c := dag.Step("combine", Combine, a.Ref(), b.Ref())

	start := time.Now()
	check(dag.Submit(ctx, wf))

	waitCtx, wc := context.WithTimeout(ctx, 15*time.Second)
	defer wc()
	result, err := workflow.Await[string](waitCtx, wf, dag.ID(), c)
	check(err)

	// Total wall time should be ~100ms (parallel), not ~200ms (serial).
	log.Printf("result: %s (took %v)", result, time.Since(start))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
