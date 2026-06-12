// Command ebctl-playground runs a long-lived ebind deployment that exercises
// every code path ebctl cares about: parallel DAG branches, retries, DLQ
// entries, cascade-skips, cancellations, and step breakpoints. A new DAG is
// submitted every -interval. Point ebctl at the advertised NATS URL to poke
// around.
//
// Breakpoints: every -bp-every'th DAG (default: every 2nd, starting with the
// first) is submitted with three breakpoints armed:
//
//	validate  BreakAfter("AfterValidate", "Checkpoint") — validate completes,
//	          then holds all three transform branches; resumable via either label
//	publish   BreakBefore("BeforePublish") — aggregate result inspectable
//	          before the side-effecting publish step runs
//	notify    BreakBefore("BeforeNotify") — blocks the independent metrics
//	          line while the main line keeps flowing
//
// Typical session:
//
//	# terminal 1
//	go run ./cmd/ebctl-playground -interval 20s
//
//	# terminal 2
//	./bin/ebctl dag ls
//	./bin/ebctl dag ls --bp-blocked
//	./bin/ebctl dag get <dag-id>
//	./bin/ebctl dag bp ls <dag-id>
//	./bin/ebctl dag bp resume <dag-id> Checkpoint
//	./bin/ebctl dag bp resume <dag-id> BeforePublish
//	./bin/ebctl dag bp resume <dag-id> BeforeNotify
//	./bin/ebctl dag watch
//	./bin/ebctl dlq ls
//	./bin/ebctl dlq show <seq>
//	./bin/ebctl stream ls
//	./bin/ebctl consumer ls EBIND_TASKS
//
// Each DAG has a friendly prefix and a UUID suffix; pretty-print in logs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// -----------------------------------------------------------------------------
// Handlers — each sleeps for realism; some fail deterministically.

// Ingest: slow root. Produces a batch "id" downstream steps can pass around.
func Ingest(ctx context.Context, source string) (string, error) {
	log.Printf("  [ingest] pulling from %s ...", source)
	sleep(ctx, 3*time.Second)
	return fmt.Sprintf("batch-%s-%d", source, time.Now().Unix()%1000), nil
}

// Validate: lightweight gate after ingest.
func Validate(ctx context.Context, batch string) (string, error) {
	log.Printf("  [validate] %s", batch)
	sleep(ctx, 2*time.Second)
	return batch, nil
}

// TransformA: successful parallel branch.
func TransformA(ctx context.Context, batch string) (string, error) {
	log.Printf("  [transform-a] %s", batch)
	sleep(ctx, 8*time.Second)
	return "A(" + batch + ")", nil
}

// TransformB: fails deterministically — drives retries and DLQ entries.
// Always returns a retryable error so the worker's RetryPolicy decides when to
// give up. Aggregate depends on transform-b via RefOrDefault so the DAG stays
// alive after B lands in the DLQ.
func TransformB(ctx context.Context, batch string) (string, error) {
	log.Printf("  [transform-b] %s — will fail (drives DLQ)", batch)
	sleep(ctx, 2*time.Second)
	return "", errors.New("transform-b: downstream API returned 503")
}

// TransformC: the slowest branch — gives the operator lots of time to
// `ebctl dag watch` the running DAG.
func TransformC(ctx context.Context, batch string) (string, error) {
	log.Printf("  [transform-c] %s", batch)
	sleep(ctx, 10*time.Second)
	return "C(" + batch + ")", nil
}

// Aggregate: fan-in over the three transforms. B might be the fallback string.
func Aggregate(ctx context.Context, a, b, c string) (string, error) {
	log.Printf("  [aggregate] a=%q b=%q c=%q", a, b, c)
	sleep(ctx, 4*time.Second)
	return fmt.Sprintf("%s | %s | %s", a, b, c), nil
}

// Publish: terminal step.
func Publish(ctx context.Context, payload string) (string, error) {
	log.Printf("  [publish] %s", payload)
	sleep(ctx, 2*time.Second)
	return "published:" + payload, nil
}

// Metrics + Notify: independent parallel chain that always succeeds; gives
// `dag tree` a second root to render.
func Metrics(ctx context.Context) (int, error) {
	log.Printf("  [metrics] computing ...")
	sleep(ctx, 5*time.Second)
	return rand.Intn(1000), nil
}

func Notify(ctx context.Context, metric int) (string, error) {
	log.Printf("  [notify] metric=%d", metric)
	sleep(ctx, 3*time.Second)
	return fmt.Sprintf("notified %d", metric), nil
}

// Flaky + Dependent: a chain that fails WITHOUT a RefOrDefault fallback, so
// Dependent gets cascade-skipped. Good for showing "skipped" rows in
// `ebctl dag get`.
func Flaky(ctx context.Context) (string, error) {
	log.Printf("  [flaky] failing (no retry, no fallback)")
	sleep(ctx, 2*time.Second)
	return "", errors.New("flaky: permanent failure")
}

func Dependent(ctx context.Context, upstream string) (string, error) {
	log.Printf("  [dependent] got %s (never actually runs)", upstream)
	return upstream, nil
}

// sleep respects ctx cancellation — important for the cancel story.
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// -----------------------------------------------------------------------------
// Runner

type config struct {
	port       int
	interval   time.Duration
	submitOnce bool
	storeDir   string
	bpEvery    int
}

func main() {
	cfg := config{}
	flag.IntVar(&cfg.port, "port", 4222, "NATS port to listen on (ebctl -s nats://127.0.0.1:<port>)")
	flag.DurationVar(&cfg.interval, "interval", 30*time.Second, "interval between DAG submissions")
	flag.BoolVar(&cfg.submitOnce, "once", false, "submit a single DAG and keep the server running")
	flag.StringVar(&cfg.storeDir, "store-dir", "", "persistent JetStream store (default: tempdir, wiped on exit)")
	flag.IntVar(&cfg.bpEvery, "bp-every", 2, "arm breakpoints on every Nth DAG, starting with the first (0 = never)")
	flag.Parse()

	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func run(cfg config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("shutting down ...")
		cancel()
	}()

	storeDir := cfg.storeDir
	cleanup := func() {}
	if storeDir == "" {
		d, err := os.MkdirTemp("", "ebctl-playground-*")
		if err != nil {
			return err
		}
		storeDir = d
		cleanup = func() { _ = os.RemoveAll(d) }
	}
	defer cleanup()

	node, err := embed.StartNode(embed.NodeConfig{
		ServerName: "ebctl-playground",
		Port:       cfg.port,
		StoreDir:   storeDir,
	})
	if err != nil {
		return fmt.Errorf("start NATS: %w", err)
	}
	defer node.Shutdown()
	log.Printf("NATS listening at %s", node.ClientURL())
	log.Printf("store dir: %s", storeDir)
	log.Printf("→ run `ebctl -s %s dag ls` in another terminal", node.ClientURL())

	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		return err
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 15*time.Second)
	if err := stream.EnsureStreams(setupCtx, js, stream.Config{Replicas: 1}); err != nil {
		setupCancel()
		return fmt.Errorf("ensure streams: %w", err)
	}
	setupCancel()

	wf, err := workflow.NewFromNATS(ctx, nc, 1)
	if err != nil {
		return err
	}

	reg := task.NewRegistry()
	for _, fn := range []any{
		Ingest, Validate,
		TransformA, TransformB, TransformC,
		Aggregate, Publish,
		Metrics, Notify,
		Flaky, Dependent,
	} {
		if err := task.Register(reg, fn); err != nil {
			return err
		}
	}

	// Worker: leave MaxDeliver=-1 so the task-level RetryPolicy decides when
	// to dead-letter. Keep concurrency high enough that parallel branches
	// actually run in parallel.
	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 8,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	if err != nil {
		return err
	}
	workerErr := make(chan error, 1)
	go func() { workerErr <- w.Run(ctx) }()

	schedErr := make(chan error, 1)
	go func() { schedErr <- wf.RunScheduler(ctx) }()

	time.Sleep(300 * time.Millisecond) // let consumer bind

	var submitted atomic.Uint64
	submit := func() {
		n := submitted.Add(1)
		armBPs := cfg.bpEvery > 0 && (n-1)%uint64(cfg.bpEvery) == 0
		id, err := submitDAG(ctx, wf, armBPs)
		if err != nil {
			log.Printf("submit: %v", err)
			return
		}
		if armBPs {
			log.Printf("submitted DAG #%d id=%s  ⦿ breakpoints armed: %s", n, id, strings.Join(armedBPLabels, ", "))
			log.Printf("  watch:  ebctl -s %s dag bp ls %s", node.ClientURL(), id)
			log.Printf("  resume: ebctl -s %s dag bp resume %s Checkpoint", node.ClientURL(), id)
		} else {
			log.Printf("submitted DAG #%d id=%s", n, id)
			log.Printf("  try:  ebctl -s %s dag get %s", node.ClientURL(), id)
		}
	}

	submit()
	if cfg.submitOnce {
		<-ctx.Done()
	} else {
		t := time.NewTicker(cfg.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				goto shutdown
			case <-t.C:
				submit()
			}
		}
	}
shutdown:
	log.Printf("waiting for worker / scheduler to drain ...")
	<-workerErr
	<-schedErr
	return nil
}

// armedBPLabels is the active set passed at Submit when a DAG is chosen to
// stop at its breakpoints. Note "Checkpoint" arms validate's after-BP via its
// SECOND label — resuming works through either one ("any label" semantics).
var armedBPLabels = []string{"Checkpoint", "BeforePublish", "BeforeNotify"}

// submitDAG builds and submits the complex DAG described in the package doc.
// Returns the DAG's generated ID so the caller can surface it in logs.
// Breakpoint labels are always declared on the steps (inert by default);
// armBPs decides at submit time whether this execution stops at them.
func submitDAG(ctx context.Context, wf *workflow.Workflow, armBPs bool) (string, error) {
	// Fast-giving-up retry policy — transform-b goes to DLQ after 3 attempts
	// so the user sees DLQ entries within ~10s rather than after a minute.
	quickRetry := task.RetryPolicy{
		InitialInterval:    500 * time.Millisecond,
		BackoffCoefficient: 2.0,
		MaximumInterval:    2 * time.Second,
		MaximumAttempts:    3,
	}

	dagID := "pipeline-" + uuid.NewString()[:8]
	dag := workflow.New(workflow.WithDAGID(dagID), workflow.WithRetry(quickRetry))

	ingest := dag.Step("ingest", Ingest, "api.example.com")
	// After-BP with two labels: when armed, validate completes (its result is
	// readable via `dag step result`) but all three transforms are held until
	// `dag bp resume <id> Checkpoint` (or AfterValidate — any label works).
	validate := dag.StepOpts("validate", Validate,
		[]workflow.StepOption{workflow.BreakAfter("AfterValidate", "Checkpoint")},
		ingest.Ref())

	// Fan out: three parallel transforms.
	tA := dag.Step("transform-a", TransformA, validate.Ref())
	tB := dag.Step("transform-b", TransformB, validate.Ref())
	tC := dag.Step("transform-c", TransformC, validate.Ref())

	// Fan in. RefOrDefault on tB so the DAG survives tB's DLQ outcome.
	agg := dag.Step("aggregate", Aggregate,
		tA.Ref(),
		tB.RefOrDefault("[transform-b unavailable]"),
		tC.Ref(),
	)

	// Before-BP: when armed, the line stops with publish pending — inspect
	// aggregate's output before the "side-effecting" terminal step runs.
	_ = dag.StepOpts("publish", Publish,
		[]workflow.StepOption{workflow.BreakBefore("BeforePublish")},
		agg.Ref())

	// Independent parallel chain. Nice second root for `ebctl dag tree`.
	// Its before-BP shows one line blocked while the other keeps running.
	metrics := dag.Step("metrics", Metrics)
	_ = dag.StepOpts("notify", Notify,
		[]workflow.StepOption{workflow.BreakBefore("BeforeNotify")},
		metrics.Ref())

	// Cascade-skip chain — flaky fails with no fallback, so dependent is
	// skipped (visible as ⊘ in dag get). Uses NoRetryPolicy so the demo
	// doesn't waste time retrying.
	flaky := dag.StepOpts("flaky", Flaky,
		[]workflow.StepOption{workflow.WithStepRetry(task.NoRetryPolicy())},
	)
	_ = dag.Step("dependent-skip", Dependent, flaky.Ref())

	var opts []workflow.SubmitOption
	if armBPs {
		opts = append(opts, workflow.WithActiveBreakpoints(armedBPLabels...))
	}
	if err := dag.Submit(ctx, wf, opts...); err != nil {
		return "", err
	}
	return dag.ID(), nil
}
