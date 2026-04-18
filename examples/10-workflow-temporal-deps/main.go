// Time-only dependencies: After() and AfterAny().
//
// Use After(step) when you need B to run only after A completes successfully,
// but B doesn't consume any of A's output.
//
// Use AfterAny(step) when you need B to wait for A to finish but to run
// regardless of whether A succeeded or failed (e.g. a cleanup/reporting step).
package main

import (
	"context"
	"errors"
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

// DownloadFile is a mandatory step that sometimes fails.
func DownloadFile(ctx context.Context, url string) (int, error) {
	log.Printf("downloading %s", url)
	time.Sleep(80 * time.Millisecond)
	if url == "bad://missing" {
		return 0, errors.New("download failed")
	}
	return 1024, nil // pretend bytes downloaded
}

// ProcessFile depends ONLY temporally on DownloadFile — it opens a file the
// previous step wrote to disk. No in-memory handoff; After() is the right
// mechanism. If DownloadFile fails, ProcessFile should NOT run.
func ProcessFile(ctx context.Context) (string, error) {
	log.Printf("processing downloaded file")
	time.Sleep(40 * time.Millisecond)
	return "processed", nil
}

// RecordMetrics runs after the above regardless of outcome — whether we
// downloaded+processed or we failed, we want the metrics row written.
// AfterAny waits for upstream to finish but runs regardless of success/failure.
func RecordMetrics(ctx context.Context) (string, error) {
	log.Printf("recording metrics (runs whether upstream succeeded or failed)")
	return "metrics-ok", nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-tempdeps-*")
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
	task.MustRegister(reg, DownloadFile)
	task.MustRegister(reg, ProcessFile)
	task.MustRegister(reg, RecordMetrics)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	fmt.Println("--- Scenario 1: all steps succeed ---")
	runScenario(ctx, wf, "https://example.com/file")

	fmt.Println("\n--- Scenario 2: download fails ---")
	runScenario(ctx, wf, "bad://missing")
}

func runScenario(ctx context.Context, wf *workflow.Workflow, url string) {
	dag := workflow.New(workflow.WithRetry(task.NoRetryPolicy()))
	download := dag.Step("download", DownloadFile, url)
	// process needs temporal ordering only (file on disk); no data handoff.
	// After() cascades — if download fails, process is skipped.
	process := dag.StepOpts("process", ProcessFile, []workflow.StepOption{
		workflow.After(download),
	})
	// metrics runs whether upstream succeeded or not.
	// AfterAny() waits but doesn't cascade.
	metrics := dag.StepOpts("metrics", RecordMetrics, []workflow.StepOption{
		workflow.AfterAny(download, process),
	})

	check(dag.Submit(ctx, wf))

	waitCtx, wc := context.WithTimeout(ctx, 15*time.Second)
	defer wc()

	// Await each step individually and report its outcome.
	for _, step := range []struct {
		name string
		ref  *workflow.Step
	}{
		{"download", download},
		{"process", process},
		{"metrics", metrics},
	} {
		result, err := workflow.Await[string](waitCtx, wf, dag.ID(), step.ref)
		switch {
		case errors.Is(err, workflow.ErrStepSkipped):
			log.Printf("  %s: SKIPPED", step.name)
		case errors.Is(err, workflow.ErrStepFailed):
			log.Printf("  %s: FAILED", step.name)
		case err != nil:
			// For DownloadFile the return type is int, not string — Await errors here with
			// decode failure. Use a second type-specific helper.
			if _, err2 := workflow.Await[int](waitCtx, wf, dag.ID(), step.ref); err2 != nil {
				if errors.Is(err2, workflow.ErrStepFailed) {
					log.Printf("  %s: FAILED", step.name)
				} else {
					log.Printf("  %s: error %v", step.name, err2)
				}
			} else {
				log.Printf("  %s: DONE (int)", step.name)
			}
		default:
			log.Printf("  %s: DONE (%s)", step.name, result)
		}
	}

	// Wait briefly for meta finalization.
	var meta workflow.DAGMeta
	for i := 0; i < 50; i++ {
		meta, _, _ = wf.Store.GetMeta(ctx, dag.ID())
		if meta.Status != workflow.DAGStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Printf("  DAG status: %s", meta.Status)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
