// Dynamic DAG: a handler inspects its input and adds more steps to the
// currently-running DAG via workflow.FromContext(ctx).Step(...). The new
// step integrates into the graph as a normal dependent of the current step.
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

// Scan discovers how many pages need to be processed, then dynamically adds
// a processing step per page. In a real system these might be thousands of
// parallel workers triggered by a single top-level scan.
func Scan(ctx context.Context, target string) (int, error) {
	log.Printf("scanning %s...", target)
	pageCount := 3 // pretend we discovered 3 pages

	d := workflow.FromContext(ctx)
	if d == nil {
		return 0, fmt.Errorf("not running inside a DAG")
	}

	for i := 0; i < pageCount; i++ {
		stepID := fmt.Sprintf("page-%d", i)
		// Each dynamic page step implicitly depends on the current (scan) step.
		if _, err := d.Step(stepID, ProcessPage, i); err != nil {
			return 0, err
		}
	}
	log.Printf("scan added %d dynamic page steps", pageCount)
	return pageCount, nil
}

func ProcessPage(ctx context.Context, pageNum int) (string, error) {
	log.Printf("processing page %d", pageNum)
	time.Sleep(50 * time.Millisecond)
	return fmt.Sprintf("page-%d-done", pageNum), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-dyn-*")
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
	task.MustRegister(reg, Scan)
	task.MustRegister(reg, ProcessPage)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 8,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()}, // injects DAG context
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Start with a single scan step. The handler adds the per-page work dynamically.
	dag := workflow.New()
	dag.Step("scan", Scan, "/some/directory")

	check(dag.Submit(ctx, wf))

	// Poll DAGInfo until the DAG is Done — we don't know the step IDs up front
	// (they're added dynamically) so we can't Await a specific step handle.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		meta, steps, err := workflow.DAGInfo(ctx, wf, dag.ID())
		if err == nil && meta.Status == workflow.DAGStatusDone {
			log.Printf("DAG done with %d total steps:", len(steps))
			for _, s := range steps {
				log.Printf("  - %s (%s)", s.StepID, s.Status)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Fatal("DAG did not complete in time")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
