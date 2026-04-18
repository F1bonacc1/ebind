// Cross-instance resume: a second instance picks up waiting on a DAG that
// a first instance submitted and then "crashed" (the example simulates this
// by exiting).
//
// The DAG state lives in NATS KV — workers keep executing, results persist.
// To resume waiting, instance B only needs (dagID, stepID) as strings;
// workflow.AwaitByID fetches the result from the shared KV store.
//
// Usage:
//
//	# first run: submits a DAG and exits without waiting
//	go run ./examples/11-workflow-resume
//
//	# prints something like: "submitted DAG a1b2c3...; resume with --resume a1b2c3..."
//
//	# second run: attaches to the existing NATS, resumes the wait
//	go run ./examples/11-workflow-resume -resume <dag-id>
//
// For the example to be self-contained, both runs share the same on-disk
// JetStream StoreDir (hard-coded below). In production, both instances would
// point at the same external NATS cluster.
package main

import (
	"context"
	"flag"
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

const (
	sharedStoreDir = "/tmp/ebind-resume-demo"
	stepFinalID    = "final"
)

type Profile struct {
	Name  string `json:"name"`
	Total int    `json:"total"`
}

func Fetch(ctx context.Context, id int) (int, error) {
	time.Sleep(300 * time.Millisecond)
	return id * 10, nil
}

func Finalize(ctx context.Context, x int) (Profile, error) {
	return Profile{Name: "alice", Total: x + 1}, nil
}

func main() {
	resumeDAG := flag.String("resume", "", "DAG id to resume waiting on")
	flag.Parse()

	_ = os.MkdirAll(sharedStoreDir, 0o755)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Embedded NATS with a shared on-disk StoreDir so the second run sees
	// whatever the first run wrote. In prod: use a real cluster instead.
	node, err := embed.StartNode(embed.NodeConfig{Port: -1, StoreDir: sharedStoreDir})
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
	task.MustRegister(reg, Fetch)
	task.MustRegister(reg, Finalize)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	if *resumeDAG != "" {
		resume(ctx, wf, *resumeDAG)
		return
	}
	submit(ctx, wf)
}

func submit(ctx context.Context, wf *workflow.Workflow) {
	dag := workflow.New()
	a := dag.Step("fetch", Fetch, 4)
	_ = dag.Step(stepFinalID, Finalize, a.Ref())

	check(dag.Submit(ctx, wf))
	fmt.Printf("submitted DAG %s\n", dag.ID())
	fmt.Printf("resume with:\n  go run ./examples/11-workflow-resume -resume %s\n", dag.ID())
}

func resume(ctx context.Context, wf *workflow.Workflow, dagID string) {
	log.Printf("resuming wait on DAG %s / step %q", dagID, stepFinalID)
	waitCtx, wc := context.WithTimeout(ctx, 30*time.Second)
	defer wc()

	// Only strings are required — no *Step, no DAG builder.
	profile, err := workflow.AwaitByID[Profile](waitCtx, wf, dagID, stepFinalID)
	if err != nil {
		log.Fatalf("AwaitByID: %v", err)
	}
	log.Printf("got profile: %+v", profile)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
