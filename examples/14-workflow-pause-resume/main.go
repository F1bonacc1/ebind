// Example 14: pause and resume a DAG durably.
//
// Pause is a graceful drain: in-flight steps are never interrupted (they keep
// their full retry chain), pending steps are fenced so no new work can start,
// and the DAG transitions pausing → paused when the last in-flight step
// completes. The paused state lives in NATS KV, so it survives process
// restarts and costs no scheduler CPU. Resume releases the fence and the
// scheduler picks up exactly where the DAG left off.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

var (
	transformStarted chan struct{}
	transformRelease chan struct{}
	transformOnce    sync.Once
)

func Extract(context.Context) (int, error) { return 42, nil }

// Transform blocks until released so the example can pause the DAG while this
// step is reliably in flight — stand-in for any long-running handler.
func Transform(_ context.Context, n int) (int, error) {
	transformOnce.Do(func() { close(transformStarted) })
	<-transformRelease
	return n * 2, nil
}

func Load(_ context.Context, n int) (string, error) {
	return fmt.Sprintf("loaded %d rows", n), nil
}

func main() {
	transformStarted = make(chan struct{})
	transformRelease = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-pause-*")
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
	task.MustRegister(reg, Extract)
	task.MustRegister(reg, Transform)
	task.MustRegister(reg, Load)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// extract → transform → load
	dag := workflow.New()
	extract := dag.Step("extract", Extract)
	transform := dag.Step("transform", Transform, extract.Ref())
	load := dag.Step("load", Load, transform.Ref())

	check(dag.Submit(ctx, wf))

	// Wait until transform is in flight, then pause mid-pipeline.
	select {
	case <-transformStarted:
	case <-time.After(10 * time.Second):
		log.Fatal("transform step did not start")
	}

	check(workflow.Pause(ctx, wf, dag.ID()))
	meta, _, err := workflow.DAGInfo(ctx, wf, dag.ID())
	check(err)
	log.Printf("after Pause: status=%s (transform drains; load is fenced)", meta.Status)

	// Let the in-flight step finish — the scheduler auto-transitions to paused.
	close(transformRelease)
	waitForStatus(ctx, wf, dag.ID(), workflow.DAGStatusPaused)
	log.Printf("DAG is paused — durable in KV, survives restarts")

	// The fenced step never started: still pending, held by the pause.
	rec, _, err := wf.Store.GetStep(ctx, dag.ID(), "load")
	check(err)
	log.Printf("step %q while paused: status=%s held=%v", rec.StepID, rec.Status, rec.Held)

	check(workflow.Resume(ctx, wf, dag.ID()))
	log.Printf("resumed — scheduler re-evaluates ready steps")

	waitCtx, wc := context.WithTimeout(ctx, 20*time.Second)
	defer wc()
	result, err := workflow.Await[string](waitCtx, wf, dag.ID(), load)
	check(err)
	log.Printf("load result: %s", result)

	waitForStatus(ctx, wf, dag.ID(), workflow.DAGStatusDone)
	log.Printf("DAG status: done")
}

func waitForStatus(ctx context.Context, wf *workflow.Workflow, dagID string, want workflow.DAGStatus) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		meta, _, err := workflow.DAGInfo(ctx, wf, dagID)
		if err == nil && meta.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("DAG did not reach status %s in time", want)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
