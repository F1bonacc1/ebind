// Example 17: signals — pause a DAG line on an external event (human
// approval, a webhook, another DAG), deliver the event with a payload, and
// let the waiting step consume that payload as a typed handler argument.
//
// A step declares what it waits for with WaitForSignal("name") (wait only) or
// by taking workflow.SignalRef("name") as an argument (wait AND receive the
// payload). workflow.Signal(ctx, wf, dagID, name, payload) delivers — from
// any process on the cluster. Delivery is one-shot, buffered, first-wins:
// a duplicate send is an idempotent no-op, and a signal sent before any step
// waits on it (even one added dynamically later) is kept for the DAG's
// lifetime. Independent parallel branches keep running while a line waits.
//
// Signal state lives in NATS KV, so waiting DAGs survive restarts and can be
// inspected and released from any process — or from the CLI:
//
//	ebctl dag signal ls <dag-id>
//	ebctl dag signal <dag-id> approval '{"approver":"eugene"}'
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

type Approval struct {
	Approver string `json:"approver"`
	Note     string `json:"note,omitempty"`
}

func Build(context.Context) (string, error) { return "app-v1.2.3.tar.gz", nil }

// Deploy waits for the "approval" signal (implied by the SignalRef arg) and
// receives its payload as the first argument.
func Deploy(_ context.Context, ap Approval, artifact string) (string, error) {
	return fmt.Sprintf("%s deployed by %s (%s)", artifact, ap.Approver, ap.Note), nil
}

func Audit(context.Context) (string, error) { return "audit ok", nil }

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-sig-*")
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
	task.MustRegister(reg, Build)
	task.MustRegister(reg, Deploy)
	task.MustRegister(reg, Audit)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// build ─→ deploy (waits for "approval", consumes its payload)
	// audit (independent parallel line, never gated)
	dag := workflow.New()
	build := dag.Step("build", Build)
	deploy := dag.Step("deploy", Deploy, workflow.SignalRef("approval"), build.Ref())
	_ = dag.Step("audit", Audit)

	check(dag.Submit(ctx, wf))
	log.Printf("submitted DAG %s", dag.ID())

	// build finishes; deploy arrives at the signal gate and waits.
	artifact, err := workflow.Await[string](ctx, wf, dag.ID(), build)
	check(err)
	log.Printf("build done: %q — deploy now waits for approval", artifact)

	// The independent line is not affected by the waiting one.
	audit, err := workflow.AwaitByID[string](ctx, wf, dag.ID(), "audit")
	check(err)
	log.Printf("parallel line finished while waiting: %q", audit)

	printSignals(ctx, wf, dag.ID())

	// The "human" approves — from this process here, but workflow.Signal works
	// from any process on the cluster, and `ebctl dag signal` from the shell.
	delivered, err := workflow.Signal(ctx, wf, dag.ID(), "approval",
		Approval{Approver: "eugene", Note: "ship it"})
	check(err)
	log.Printf("Signal(approval): delivered=%v", delivered)

	// A duplicate delivery is an idempotent no-op — the first payload wins.
	delivered, err = workflow.Signal(ctx, wf, dag.ID(), "approval",
		Approval{Approver: "mallory", Note: "too late"})
	check(err)
	log.Printf("Signal(approval) again: delivered=%v (first payload kept)", delivered)

	result, err := workflow.Await[string](ctx, wf, dag.ID(), deploy)
	check(err)
	log.Printf("deploy result: %q", result)

	printSignals(ctx, wf, dag.ID())

	waitForStatus(ctx, wf, dag.ID(), workflow.DAGStatusDone)
	log.Printf("DAG status: done")
}

func printSignals(ctx context.Context, wf *workflow.Workflow, dagID string) {
	infos, err := workflow.ListSignalInfo(ctx, wf, dagID)
	check(err)
	for _, si := range infos {
		state := "awaited"
		if si.Delivered {
			state = "delivered " + string(si.Payload)
		}
		log.Printf("  signal %-10s %s  waiting=%v", si.Name, state, si.Waiting)
	}
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
