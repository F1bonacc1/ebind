// Optional steps + RefOrDefault: a step is marked Optional so its failure
// doesn't fail the DAG. Downstream chooses between Ref() (cascade-skip if
// upstream failed) and RefOrDefault(v) (run anyway, substitute v).
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

func LoadCore(ctx context.Context, userID string) (string, error) {
	log.Printf("LoadCore(%s) — mandatory", userID)
	return "core:" + userID, nil
}

// EnrichWithThirdParty is flaky — we simulate failure.
// Because it's marked Optional in the DAG, its failure does NOT fail the DAG.
func EnrichWithThirdParty(ctx context.Context, userID string) (string, error) {
	log.Printf("EnrichWithThirdParty(%s) — optional, simulating failure", userID)
	return "", errors.New("third-party API is down")
}

// Render uses RefOrDefault to run with a fallback if enrichment failed.
func Render(ctx context.Context, core string, enrichment string) (string, error) {
	return fmt.Sprintf("rendered(%s + %s)", core, enrichment), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-optional-*")
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
	task.MustRegister(reg, LoadCore)
	task.MustRegister(reg, EnrichWithThirdParty)
	task.MustRegister(reg, Render)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Make retries fast — we're demonstrating optional failure, not retry behavior.
	noRetry := task.NoRetryPolicy()

	dag := workflow.New(workflow.WithRetry(noRetry))
	core := dag.Step("core", LoadCore, "user-7")
	enrich := dag.StepOpts("enrich", EnrichWithThirdParty,
		[]workflow.StepOption{workflow.Optional()}, // failure → DAG continues
		"user-7",
	)
	// render runs even if enrichment failed, with "[no enrichment]" substituted.
	render := dag.Step("render", Render,
		core.Ref(),
		enrich.RefOrDefault("[no enrichment]"),
	)

	check(dag.Submit(ctx, wf))

	waitCtx, wc := context.WithTimeout(ctx, 15*time.Second)
	defer wc()
	result, err := workflow.Await[string](waitCtx, wf, dag.ID(), render)
	check(err)
	log.Printf("result: %s", result)

	// DAG meta finalization happens after the last step's completion event
	// reaches the scheduler — poll briefly.
	var meta workflow.DAGMeta
	for i := 0; i < 50; i++ {
		meta, _, _ = wf.Store.GetMeta(ctx, dag.ID())
		if meta.Status != workflow.DAGStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Printf("DAG status: %s (done despite optional failure)", meta.Status)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
