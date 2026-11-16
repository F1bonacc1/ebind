// Example 20: runtime breakpoints — stop a DAG that is already running.
//
// Examples 15 and 18 arm breakpoints when the DAG is submitted. Here the DAG
// is submitted with no breakpoint options at all, and an operator arms them
// while it runs:
//
//	ArmBreakpoints(BeforeStep("build"), AfterStep("test"), BeforeFn("main.Package"))
//
// The ArmReport says what that did: build is already running, so it is
// missed (arming never interrupts work); test will hold its dependents when
// it completes; the package steps do not exist yet. build's handler then
// adds them dynamically, and they stop anyway, because the matcher is in
// the DAG's armed set, not stamped on the steps that existed at the time.
//
// DisarmBreakpoints removes a matcher and lets what it alone stopped
// continue. It never marks anything released, so disarming and re-arming a
// matcher before a step arrives still stops that step. ReleaseStep is the
// other tool: it lets one stopped step continue and leaves the matcher
// armed.
//
// The same session from the CLI:
//
//	ebctl dag bp arm <dag-id> --before --step build
//	ebctl dag bp arm <dag-id> --after --step test
//	ebctl dag bp arm <dag-id> --before --fn main.Package
//	ebctl dag bp wait <dag-id> --before --fn main.Package
//	ebctl dag bp disarm <dag-id> --before --fn main.Package
//	ebctl dag bp release <dag-id> test --position after
//
// Rolling upgrades: every worker must run ebind v0.10.0 or later before you
// arm at runtime. An older scheduler ignores runtime breakpoints.
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

// buildStarted and armed sequence the walkthrough: build signals that it is
// running and waits until main has armed the breakpoints, so the arm is
// guaranteed to land mid-run. A real build would simply take a while.
var (
	buildStarted = make(chan struct{})
	armed        = make(chan struct{})
)

var platforms = []string{"linux", "darwin"}

// Build compiles, then adds one package step per platform to the running DAG.
func Build(ctx context.Context, version string) (string, error) {
	close(buildStarted)
	select {
	case <-armed:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	d := workflow.FromContext(ctx)
	if d == nil {
		return "", fmt.Errorf("not running inside a DAG")
	}
	for _, p := range platforms {
		// Each package step implicitly depends on build.
		if _, err := d.Step("package-"+p, Package, p); err != nil {
			return "", err
		}
	}
	return "app-" + version, nil
}

func Package(_ context.Context, platform string) (string, error) {
	return "app-" + platform + ".tar.gz", nil
}

func Test(_ context.Context, artifact string) (string, error) {
	return artifact + ": 128 tests passed", nil
}

func Deploy(_ context.Context, report string) (string, error) {
	return "deployed after " + report, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-runtime-bp-*")
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
	task.MustRegister(reg, Package)
	task.MustRegister(reg, Test)
	task.MustRegister(reg, Deploy)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// build → test → deploy, plus the package steps build adds at runtime.
	dag := workflow.New()
	build := dag.Step("build", Build, "2.4.1")
	test := dag.Step("test", Test, build.Ref())
	deploy := dag.Step("deploy", Deploy, test.Ref())
	check(dag.Submit(ctx, wf)) // no breakpoint options
	id := dag.ID()
	log.Printf("submitted %s with nothing armed", id)

	// Part 1: arm while build is running.
	<-buildStarted
	rep, err := workflow.ArmBreakpoints(ctx, wf, id,
		workflow.BeforeStep("build"),
		workflow.AfterStep("test"),
		workflow.BeforeFn("main.Package"),
	)
	check(err)
	log.Printf("armed %s", strings.Join(rep.Added, ", "))
	log.Printf("  stopped: %v", rep.Stopped)
	log.Printf("  missed:  %v (already running; not interrupted)", rep.Missed)
	close(armed)

	// Part 2: the package steps did not exist at arm time, and they stop.
	for _, p := range platforms {
		waitStopped(ctx, wf, id, workflow.BeforeStep("package-"+p))
	}
	n, err := workflow.DisarmBreakpoints(ctx, wf, id, workflow.BeforeFn("main.Package"))
	check(err)
	log.Printf("disarmed before:fn:main.Package: released %d", n)
	for _, p := range platforms {
		out, err := workflow.AwaitByID[string](ctx, wf, id, "package-"+p)
		check(err)
		log.Printf("  package-%s: %s", p, out)
	}

	// Part 3: test ran and holds deploy. Arm a stop before deploy, disarm it,
	// arm it again: disarm released nothing, so deploy still stops when it
	// arrives.
	waitStopped(ctx, wf, id, workflow.AfterStep("test"))
	report, err := workflow.Await[string](ctx, wf, id, test)
	check(err)
	log.Printf("test result: %q; holding deploy", report)
	for i := 0; i < 2; i++ {
		rep, err = workflow.ArmBreakpoints(ctx, wf, id, workflow.BeforeStep("deploy"))
		check(err)
		log.Printf("armed before:step:deploy: stopped %v", rep.Stopped)
		if i == 0 {
			n, err = workflow.DisarmBreakpoints(ctx, wf, id, workflow.BeforeStep("deploy"))
			check(err)
			log.Printf("disarmed before:step:deploy: released %d (deploy had not arrived)", n)
		}
	}
	_, err = workflow.ReleaseStep(ctx, wf, id, "test", workflow.BPPositionAfter)
	check(err)
	waitStopped(ctx, wf, id, workflow.BeforeStep("deploy"))
	printBreakpoints(ctx, wf, id)

	_, err = workflow.ReleaseStep(ctx, wf, id, "deploy", workflow.BPPositionBefore)
	check(err)
	result, err := workflow.Await[string](ctx, wf, id, deploy)
	check(err)
	log.Printf("deploy: %s", result)
	waitForStatus(ctx, wf, id, workflow.DAGStatusDone)
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
