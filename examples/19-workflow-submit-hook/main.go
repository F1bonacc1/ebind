// Example 19: a submit hook — decide submit options for every DAG in one
// place instead of at each Submit call site.
//
// submitBilling and submitReports stand in for code in different packages.
// They build and submit DAGs with labels and no submit options at all. One
// hook registered on the Workflow with UseSubmitHook makes the run-time
// decision for all of them: a DAG labeled "debug" is started paused with a
// breakpoint armed before main.Charge; every other DAG passes through.
//
// The hook runs inside Submit, before anything is persisted, and sees the
// DAG's ID and labels. Its options are added after the caller's. If it
// returns an error, Submit fails, nothing is stored, and the same *DAG can be
// submitted again once the cause is fixed (part 2).
//
// From the CLI, the paused debug DAG is driven like any other:
//
//	ebctl dag ls --label debug
//	ebctl dag resume <dag-id>
//	ebctl dag bp wait <dag-id> --before --fn main.Charge
//	ebctl dag bp continue <dag-id> --before --fn main.Charge
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

func Quote(_ context.Context, cents int) (int, error) { return cents + cents/10, nil } // + tax

func Charge(_ context.Context, cents int) (string, error) {
	return fmt.Sprintf("charged %d.%02d", cents/100, cents%100), nil
}

func Render(context.Context) (string, error) { return "report.pdf", nil }

// --- call sites: no breakpoint or pause options here -----------------------

func submitBilling(ctx context.Context, wf *workflow.Workflow, labels ...string) (*workflow.DAG, error) {
	dag := workflow.New(workflow.WithLabels(append([]string{"billing"}, labels...)...))
	q := dag.Step("quote", Quote, 1990)
	dag.Step("charge", Charge, q.Ref())
	return dag, dag.Submit(ctx, wf)
}

func submitReports(ctx context.Context, wf *workflow.Workflow) (*workflow.DAG, error) {
	dag := workflow.New(workflow.WithLabels("reports"))
	dag.Step("render", Render)
	return dag, dag.Submit(ctx, wf)
}

// --------------------------------------------------------------------------

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-hook-*")
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
	task.MustRegister(reg, Quote)
	task.MustRegister(reg, Charge)
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

	// The one place that decides: debug DAGs start paused and stop before
	// any charge.
	wf.UseSubmitHook(func(_ context.Context, meta workflow.DAGMeta) ([]workflow.SubmitOption, error) {
		if !slices.Contains(meta.Labels, "debug") {
			return nil, nil
		}
		log.Printf("hook: %s is labeled debug: start paused, stop before main.Charge", meta.ID)
		return []workflow.SubmitOption{
			workflow.StartPaused(),
			workflow.WithArmedBreakpoints(workflow.BeforeFn("main.Charge")),
		}, nil
	})

	billing, err := submitBilling(ctx, wf, "debug")
	check(err)
	reports, err := submitReports(ctx, wf)
	check(err)

	// The reports DAG is untouched by the hook and just runs.
	out, err := workflow.AwaitByID[string](ctx, wf, reports.ID(), "render")
	check(err)
	log.Printf("reports DAG ran straight through: %q", out)

	meta, _, err := workflow.DAGInfo(ctx, wf, billing.ID())
	check(err)
	log.Printf("billing DAG %s: status=%s armed=%v", billing.ID(), meta.Status, keys(meta.ArmedMatchers))

	check(workflow.Resume(ctx, wf, billing.ID()))
	waitCtx, wc := context.WithTimeout(ctx, 10*time.Second)
	info, err := workflow.WaitForBreakpoint(waitCtx, wf, billing.ID(), workflow.BeforeFn("main.Charge"))
	wc()
	check(err)
	total, err := workflow.AwaitByID[int](ctx, wf, billing.ID(), "quote")
	check(err)
	log.Printf("stopped before %q; the quote to inspect is %d cents", info.StepID, total)

	n, err := workflow.ReleaseMatching(ctx, wf, billing.ID(), workflow.BeforeFn("main.Charge"))
	check(err)
	log.Printf("ReleaseMatching(before:fn:main.Charge): released %d", n)
	charged, err := workflow.AwaitByID[string](ctx, wf, billing.ID(), "charge")
	check(err)
	log.Printf("billing DAG result: %q", charged)

	rejectingHook(ctx, wf)
}

// rejectingHook is part 2: a hook that refuses submissions while a flag is
// set. The refused DAG is not stored and can be submitted again later.
func rejectingHook(ctx context.Context, wf *workflow.Workflow) {
	var frozen atomic.Bool
	frozen.Store(true)
	wf.UseSubmitHook(func(_ context.Context, meta workflow.DAGMeta) ([]workflow.SubmitOption, error) {
		if frozen.Load() && slices.Contains(meta.Labels, "billing") {
			return nil, errors.New("billing is frozen for month-end close")
		}
		return nil, nil
	})

	dag := workflow.New(workflow.WithLabels("billing"))
	q := dag.Step("quote", Quote, 500)
	dag.Step("charge", Charge, q.Ref())

	err := dag.Submit(ctx, wf)
	log.Printf("part 2: submit while frozen: %v", err)
	if _, _, err := workflow.DAGInfo(ctx, wf, dag.ID()); errors.Is(err, workflow.ErrDAGNotFound) {
		log.Printf("nothing was stored for %s", dag.ID())
	} else {
		log.Fatalf("expected ErrDAGNotFound, got %v", err)
	}

	frozen.Store(false)
	check(dag.Submit(ctx, wf)) // the same *DAG, submitted again
	charged, err := workflow.AwaitByID[string](ctx, wf, dag.ID(), "charge")
	check(err)
	log.Printf("resubmitted after the freeze lifted: %q", charged)
}

func keys(ms []workflow.BPMatcher) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Key())
	}
	return out
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
