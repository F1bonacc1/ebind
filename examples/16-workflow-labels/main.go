// Example 16: labeling & querying workflow history.
//
// Attach immutable string tags to a workflow at creation with
// WithLabels("billing", "nightly"). Labels are fixed at New, persisted into the
// DAG's meta record at Submit, and never change for the DAG's lifetime — they're
// a durable way to group workflows by topic.
//
// Retrieve the relevant slice of history with ListDAGsByLabels: it returns every
// DAG carrying ALL of the given labels (AND semantics), newest-first. With no
// labels it returns every DAG. It's a client-side filter over the same KV history
// `ebctl dag ls` scans, so it needs no secondary index. The CLI exposes the same
// query and surfaces labels in its listings:
//
//	ebctl dag ls --label billing                 # only "billing" workflows
//	ebctl dag ls --label billing --label nightly # AND: carry both labels
//	ebctl dag get <dag-id>                        # includes a `labels:` line
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// A trivial two-step pipeline — the point of the example is the labels, not the
// work the steps do.
func Extract(_ context.Context, source string) (int, error) {
	return len(source), nil // pretend: rows extracted
}

func Report(_ context.Context, rows int) (string, error) {
	return fmt.Sprintf("report over %d rows", rows), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-labels-*")
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
	task.MustRegister(reg, Report)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Run a few workflows, each tagged with the topic(s) it belongs to. Labels
	// are the only thing that varies — the pipeline is identical.
	runLabeled(ctx, wf, "nightly billing", "billing", "nightly")
	runLabeled(ctx, wf, "ad-hoc billing", "billing")
	runLabeled(ctx, wf, "weekly reports", "reports")

	// Now query the history by topic. Each call is a fresh read of NATS KV — the
	// labels outlive the *DAG handles above, so a completely separate process
	// could run these same queries.
	report(ctx, wf, "all 'billing' workflows", "billing")
	report(ctx, wf, "'billing' AND 'nightly' (must carry both)", "billing", "nightly")
	report(ctx, wf, "all 'reports' workflows", "reports")
	report(ctx, wf, "everything (no label filter)")
}

// runLabeled submits a two-step pipeline tagged with the given labels and waits
// for it to finish.
func runLabeled(ctx context.Context, wf *workflow.Workflow, source string, labels ...string) {
	dag := workflow.New(workflow.WithLabels(labels...))
	extract := dag.Step("extract", Extract, source)
	report := dag.Step("report", Report, extract.Ref())
	check(dag.Submit(ctx, wf))

	out, err := workflow.Await[string](ctx, wf, dag.ID(), report)
	check(err)
	log.Printf("submitted %s  labels=%v → %q", dag.ID(), labels, out)
}

// report runs a label query and prints what came back.
func report(ctx context.Context, wf *workflow.Workflow, title string, labels ...string) {
	metas, err := workflow.ListDAGsByLabels(ctx, wf, labels...)
	check(err)
	log.Printf("── %s: %d match", title, len(metas))
	for _, m := range metas {
		log.Printf("     %s  [%s]  labels=[%s]", m.ID, m.Status, strings.Join(m.Labels, ","))
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
