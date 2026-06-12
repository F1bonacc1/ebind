// Example 15: step breakpoints — stop a DAG line at a specific step, inspect
// intermediate state, then continue.
//
// A breakpoint is declared on a step with BreakBefore(labels...) (the step
// stays pending, never dispatched) or BreakAfter(labels...) (the step runs and
// its result is persisted, but its direct dependents are held). Breakpoints
// are inactive by default: the same DAG submitted without
// WithActiveBreakpoints runs straight through. Only the lines that hit an
// armed breakpoint stop — independent parallel branches keep running.
//
// ResumeBreakpoint is debugger "continue": it releases whatever is currently
// stopped on a label, and the label stays armed for later steps (including
// dynamically added ones). All breakpoint state lives in NATS KV, so blocked
// DAGs survive restarts and can be resumed from any process — or from the CLI:
//
//	ebctl dag bp ls <dag-id>
//	ebctl dag bp resume <dag-id> <label>
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

func Download(context.Context) (string, error) { return "raw,csv,data", nil }

func Parse(_ context.Context, raw string) (int, error) {
	return len(raw), nil // pretend: rows parsed
}

func Upload(_ context.Context, rows int) (string, error) {
	return fmt.Sprintf("uploaded %d rows", rows), nil
}

func Audit(context.Context) (string, error) { return "audit ok", nil }

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-bp-*")
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
	task.MustRegister(reg, Download)
	task.MustRegister(reg, Parse)
	task.MustRegister(reg, Upload)
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

	// download ─(after-BP)→ parse ─(before-BP)→ upload
	// audit (independent parallel line, never blocked)
	//
	// The breakpoint labels are part of the DAG's structure; which ones are
	// ARMED is a run-time decision made at Submit. Submit the same DAG without
	// WithActiveBreakpoints and it runs straight through.
	dag := workflow.New()
	download := dag.StepOpts("download", Download, []workflow.StepOption{workflow.BreakAfter("AfterDownload")})
	parse := dag.Step("parse", Parse, download.Ref())
	upload := dag.StepOpts("upload", Upload, []workflow.StepOption{workflow.BreakBefore("BeforeUpload")}, parse.Ref())
	_ = dag.Step("audit", Audit)

	check(dag.Submit(ctx, wf, workflow.WithActiveBreakpoints("AfterDownload", "BeforeUpload")))

	// Stop #1: download completes — its result is durable and inspectable —
	// but the after-breakpoint holds parse.
	raw, err := workflow.Await[string](ctx, wf, dag.ID(), download)
	check(err)
	waitBlocked(ctx, wf, dag.ID(), 1)
	log.Printf("stopped after %q — its result is already persisted: %q", "download", raw)

	// The independent line is not affected by the stopped one.
	audit, err := workflow.AwaitByID[string](ctx, wf, dag.ID(), "audit")
	check(err)
	log.Printf("parallel line finished while stopped: %q", audit)

	printBreakpoints(ctx, wf, dag.ID())

	// Continue #1: release the after-gate; parse runs, then the line stops
	// again at upload's before-breakpoint — upload is never dispatched.
	n, err := workflow.ResumeBreakpoint(ctx, wf, dag.ID(), "AfterDownload")
	check(err)
	log.Printf("ResumeBreakpoint(AfterDownload): released %d", n)

	rows, err := workflow.AwaitByID[int](ctx, wf, dag.ID(), "parse")
	check(err)
	waitBlocked(ctx, wf, dag.ID(), 1)
	log.Printf("stopped before %q — inspect parse's output first: %d rows", "upload", rows)

	printBreakpoints(ctx, wf, dag.ID())

	// Continue #2: release the line; the DAG runs to completion.
	n, err = workflow.ResumeBreakpoint(ctx, wf, dag.ID(), "BeforeUpload")
	check(err)
	log.Printf("ResumeBreakpoint(BeforeUpload): released %d", n)

	result, err := workflow.Await[string](ctx, wf, dag.ID(), upload)
	check(err)
	log.Printf("upload result: %q", result)

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

// waitBlocked polls until exactly n breakpoints report blocked state.
func waitBlocked(ctx context.Context, wf *workflow.Workflow, dagID string, n int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		infos, err := workflow.ListBreakpoints(ctx, wf, dagID)
		if err == nil && workflow.CountBlocked(infos) == n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("never reached %d blocked breakpoint(s)", n)
}

func printBreakpoints(ctx context.Context, wf *workflow.Workflow, dagID string) {
	infos, err := workflow.ListBreakpoints(ctx, wf, dagID)
	check(err)
	for _, bp := range infos {
		state := string(bp.State)
		if state == "" {
			state = "not hit"
		}
		log.Printf("  bp: step=%s position=%s labels=%v armed=%v state=%s holding=%v",
			bp.StepID, bp.Position, bp.Labels, bp.Armed, state, bp.Holding)
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
