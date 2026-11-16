// Example 18: breakpoint matchers — stop steps that declare no breakpoints,
// chosen when the DAG is submitted.
//
// Example 15 declares breakpoint labels on steps (BreakBefore/BreakAfter) and
// arms some of them at Submit. Here the code that builds the DAG knows nothing
// about breakpoints: buildRollout() is a plain canary rollout. The caller arms
// stops by matcher at Submit:
//
//	BeforeStep("upgrade-node-*")  stop every node before it upgrades
//	AfterFn("main.Prepare")       hold everything that depends on Prepare
//
// Matchers select by step-ID glob (BeforeStep/AfterStep), by function name
// (BeforeFn/AfterFn; the canonical name of a function in an example binary
// is "main.<Name>") or by declared label (BeforeLabel/AfterLabel). Globs use
// path.Match: '*' does not cross '/', so "node-*" does not match "node-1/db".
//
// The walkthrough is a canary: release node 1 alone with ReleaseStep, check
// it, then continue the rest with ReleaseMatching. Submitting with
// StartPaused means nothing runs before the waiters are in place. The same
// session from the CLI:
//
//	ebctl dag resume <dag-id>
//	ebctl dag bp ls <dag-id>
//	ebctl dag bp wait <dag-id> --after --fn main.Prepare
//	ebctl dag bp release <dag-id> prepare --position after
//	ebctl dag bp release <dag-id> upgrade-node-1 --position before
//	ebctl dag bp continue <dag-id> --before --step 'upgrade-node-*'
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

func Prepare(context.Context) (string, error) { return "v2.4.1", nil }

func Upgrade(_ context.Context, node, version string) (string, error) {
	return fmt.Sprintf("%s on %s", node, version), nil
}

func Verify(_ context.Context, a, b, c string) (string, error) {
	return fmt.Sprintf("healthy: [%s] [%s] [%s]", a, b, c), nil
}

var nodes = []string{"upgrade-node-1", "upgrade-node-2", "upgrade-node-3"}

// buildRollout is the plain DAG, with no breakpoint configuration:
// prepare → upgrade-node-1..3 (fan-out) → verify.
func buildRollout() (*workflow.DAG, *workflow.Step) {
	dag := workflow.New()
	prepare := dag.Step("prepare", Prepare)
	var ups []any
	for i, id := range nodes {
		ups = append(ups, dag.Step(id, Upgrade, fmt.Sprintf("node-%d", i+1), prepare.Ref()).Ref())
	}
	verify := dag.Step("verify", Verify, ups...)
	return dag, verify
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-matchers-*")
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
	task.MustRegister(reg, Prepare)
	task.MustRegister(reg, Upgrade)
	task.MustRegister(reg, Verify)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	dag, verify := buildRollout()
	check(dag.Submit(ctx, wf,
		workflow.StartPaused(),
		workflow.WithArmedBreakpoints(
			workflow.BeforeStep("upgrade-node-*"),
			workflow.AfterFn("main.Prepare"),
		),
	))
	log.Printf("submitted %s paused, armed: before:step:upgrade-node-*, after:fn:main.Prepare", dag.ID())
	printBreakpoints(ctx, wf, dag.ID())

	check(workflow.Resume(ctx, wf, dag.ID()))

	// Stop 1: prepare ran; its result is persisted, and the upgrades are held.
	waitStopped(ctx, wf, dag.ID(), workflow.AfterFn("main.Prepare"))
	version, err := workflow.AwaitByID[string](ctx, wf, dag.ID(), "prepare")
	check(err)
	log.Printf("prepare produced %q; looks right, releasing it", version)
	_, err = workflow.ReleaseStep(ctx, wf, dag.ID(), "prepare", workflow.BPPositionAfter)
	check(err)

	// Stop 2: every node stops before upgrading. WaitForBreakpoint returns on
	// the first match, so wait for each node by name.
	for _, id := range nodes {
		waitStopped(ctx, wf, dag.ID(), workflow.BeforeStep(id))
	}
	printBreakpoints(ctx, wf, dag.ID())

	// Canary: upgrade node 1 alone and check it.
	_, err = workflow.ReleaseStep(ctx, wf, dag.ID(), "upgrade-node-1", workflow.BPPositionBefore)
	check(err)
	canary, err := workflow.AwaitByID[string](ctx, wf, dag.ID(), "upgrade-node-1")
	check(err)
	log.Printf("canary done: %q; nodes 2 and 3 are still stopped:", canary)
	printBreakpoints(ctx, wf, dag.ID())

	// Continue the rest with one matcher. It stays armed: a node added later
	// would stop again.
	n, err := workflow.ReleaseMatching(ctx, wf, dag.ID(), workflow.BeforeStep("upgrade-node-*"))
	check(err)
	log.Printf("ReleaseMatching(before:step:upgrade-node-*): released %d", n)

	result, err := workflow.Await[string](ctx, wf, dag.ID(), verify)
	check(err)
	log.Printf("verify: %s", result)
	waitForStatus(ctx, wf, dag.ID(), workflow.DAGStatusDone)
	log.Printf("DAG status: done")
}

// waitStopped blocks until a step selected by m is stopped at m's position.
func waitStopped(ctx context.Context, wf *workflow.Workflow, dagID string, m workflow.BPMatcher) {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := workflow.WaitForBreakpoint(waitCtx, wf, dagID, m)
	if err != nil {
		log.Fatalf("waiting for %s: %v", m.Key(), err)
	}
	log.Printf("stopped: %s %s (armed by %v)", info.StepID, info.Position, info.Matchers)
}

func printBreakpoints(ctx context.Context, wf *workflow.Workflow, dagID string) {
	infos, err := workflow.ListBreakpoints(ctx, wf, dagID, workflow.OnlyArmed())
	check(err)
	for _, bp := range infos {
		state := string(bp.State)
		if state == "" {
			state = "not hit"
		}
		log.Printf("  bp: step=%s position=%s matchers=%v state=%s holding=%v",
			bp.StepID, bp.Position, bp.Matchers, state, bp.Holding)
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
