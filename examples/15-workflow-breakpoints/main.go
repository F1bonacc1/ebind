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
// Two ways to let a stopped line go:
//   - ResumeBreakpoint is debugger "continue": it releases everything currently
//     stopped on a label, and the label stays armed for later steps (including
//     dynamically added ones).
//   - ReleaseStep releases one step at one position. Other steps stopped on the
//     same label stay stopped, e.g. to let one branch of a fan-out through
//     while you look at the other.
//
// WaitForBreakpoint blocks until a matching step is stopped (no polling: it
// follows the DAG's KV records through a watch), which is how this example
// knows each stop has been reached.
//
// All breakpoint state lives in NATS KV, so blocked DAGs survive restarts and
// can be released from any process, or from the CLI:
//
//	ebctl dag bp ls <dag-id>
//	ebctl dag bp wait <dag-id> --before --step 'upload-*' --wait-timeout 1m
//	ebctl dag bp release <dag-id> <step-id> --position before
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

func Upload(_ context.Context, region string, rows int) (string, error) {
	return fmt.Sprintf("uploaded %d rows to %s", rows, region), nil
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

	// download ─(after-BP)→ parse ─┬─(before-BP)→ upload-eu
	//                               └─(before-BP)→ upload-us
	// audit (independent parallel line, never blocked)
	//
	// The breakpoint labels are part of the DAG's structure; which ones are
	// ARMED is a run-time decision made at Submit. Submit the same DAG without
	// WithActiveBreakpoints and it runs straight through.
	dag := workflow.New()
	download := dag.StepOpts("download", Download, []workflow.StepOption{workflow.BreakAfter("AfterDownload")})
	parse := dag.Step("parse", Parse, download.Ref())
	beforeUpload := []workflow.StepOption{workflow.BreakBefore("BeforeUpload")}
	uploadEU := dag.StepOpts("upload-eu", Upload, beforeUpload, "eu", parse.Ref())
	uploadUS := dag.StepOpts("upload-us", Upload, beforeUpload, "us", parse.Ref())
	_ = dag.Step("audit", Audit)

	check(dag.Submit(ctx, wf, workflow.WithActiveBreakpoints("AfterDownload", "BeforeUpload")))

	// Stop #1: download completes — its result is durable and inspectable —
	// but the after-breakpoint holds parse.
	raw, err := workflow.Await[string](ctx, wf, dag.ID(), download)
	check(err)
	waitStopped(ctx, wf, dag.ID(), workflow.AfterStep("download"))
	log.Printf("stopped after %q — its result is already persisted: %q", "download", raw)

	// The independent line is not affected by the stopped one.
	audit, err := workflow.AwaitByID[string](ctx, wf, dag.ID(), "audit")
	check(err)
	log.Printf("parallel line finished while stopped: %q", audit)

	printBreakpoints(ctx, wf, dag.ID())

	// Continue #1: release the after-gate; parse runs, then both uploads stop
	// at their before-breakpoint and are never dispatched.
	n, err := workflow.ResumeBreakpoint(ctx, wf, dag.ID(), "AfterDownload")
	check(err)
	log.Printf("ResumeBreakpoint(AfterDownload): released %d", n)

	rows, err := workflow.AwaitByID[int](ctx, wf, dag.ID(), "parse")
	check(err)
	// WaitForBreakpoint returns on the first match, so wait for each step.
	waitStopped(ctx, wf, dag.ID(), workflow.BeforeStep("upload-eu"))
	waitStopped(ctx, wf, dag.ID(), workflow.BeforeStep("upload-us"))
	log.Printf("stopped before both uploads; inspect parse's output first: %d rows", rows)

	printBreakpoints(ctx, wf, dag.ID())

	// Release one: only upload-eu runs. upload-us shares the label but stays
	// stopped.
	released, err := workflow.ReleaseStep(ctx, wf, dag.ID(), "upload-eu", workflow.BPPositionBefore)
	check(err)
	log.Printf("ReleaseStep(upload-eu, before): released=%v", released)

	eu, err := workflow.Await[string](ctx, wf, dag.ID(), uploadEU)
	check(err)
	log.Printf("upload-eu result: %q", eu)
	waitStopped(ctx, wf, dag.ID(), workflow.BeforeStep("upload-us")) // returns at once: still stopped
	log.Printf("upload-us is still stopped:")
	printBreakpoints(ctx, wf, dag.ID())

	// Continue #2: release whatever is still stopped on the label; the DAG
	// runs to completion.
	n, err = workflow.ResumeBreakpoint(ctx, wf, dag.ID(), "BeforeUpload")
	check(err)
	log.Printf("ResumeBreakpoint(BeforeUpload): released %d", n)

	us, err := workflow.Await[string](ctx, wf, dag.ID(), uploadUS)
	check(err)
	log.Printf("upload-us result: %q", us)

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

// waitStopped blocks until a step selected by m is stopped at m's position.
func waitStopped(ctx context.Context, wf *workflow.Workflow, dagID string, m workflow.BPMatcher) {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := workflow.WaitForBreakpoint(waitCtx, wf, dagID, m)
	if err != nil {
		log.Fatalf("waiting for %s: %v", m.Key(), err)
	}
	log.Printf("WaitForBreakpoint(%s): %s stopped %s", m.Key(), info.StepID, info.Position)
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
