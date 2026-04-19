package main

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

type mutableClaims struct {
	mu     sync.Mutex
	claims []string
}

func (m *mutableClaims) Claims(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.claims))
	copy(out, m.claims)
	return out, nil
}

func (m *mutableClaims) Set(claims ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims = append([]string(nil), claims...)
}

var (
	holdStarted chan struct{}
	holdRelease chan struct{}
	holdOnce    sync.Once
)

func resetHold() {
	holdStarted = make(chan struct{})
	holdRelease = make(chan struct{})
	holdOnce = sync.Once{}
}

func CurrentWorker(ctx context.Context) (string, error) {
	return workflow.CurrentWorkerID(ctx), nil
}

func HoldPrimary(ctx context.Context) (string, error) {
	holdOnce.Do(func() { close(holdStarted) })
	<-holdRelease
	return workflow.CurrentWorkerID(ctx), nil
}

func AddColocatedChild(ctx context.Context) (string, error) {
	d := workflow.FromContext(ctx)
	if d == nil {
		return "", errors.New("missing workflow context")
	}
	if _, err := d.StepOpts("child-here", CurrentWorker, []workflow.StepOption{workflow.ColocateHere()}); err != nil {
		return "", err
	}
	return workflow.CurrentWorkerID(ctx), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-placement-*")
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

	primary := &mutableClaims{claims: []string{"primary"}}
	secondary := &mutableClaims{claims: []string{"secondary"}}

	startWorker(ctx, nc, wf, "worker-a", primary)
	startWorker(ctx, nc, wf, "worker-b", secondary)
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runColocateWithDemo(ctx, wf, primary, secondary)
	runFollowTargetDemo(ctx, wf, primary, secondary)
	runColocateHereDemo(ctx, wf, primary, secondary)
}

func runColocateWithDemo(ctx context.Context, wf *workflow.Workflow, primary, secondary *mutableClaims) {
	resetHold()
	primary.Set("primary")
	secondary.Set("secondary")

	dag := workflow.New()
	hold := dag.StepOpts("hold-primary", HoldPrimary, []workflow.StepOption{workflow.OnTarget("primary")})
	same := dag.StepOpts("same-worker", CurrentWorker, []workflow.StepOption{workflow.ColocateWith(hold)})
	check(dag.Submit(ctx, wf))

	waitForHold()
	primary.Set("secondary")
	secondary.Set("primary")
	time.Sleep(300 * time.Millisecond)
	close(holdRelease)

	waitCtx, wc := context.WithTimeout(ctx, 20*time.Second)
	defer wc()

	holdWorker, err := workflow.Await[string](waitCtx, wf, dag.ID(), hold)
	check(err)
	sameWorker, err := workflow.Await[string](waitCtx, wf, dag.ID(), same)
	check(err)

	log.Printf("ColocateWith: hold-primary=%s same-worker=%s", holdWorker, sameWorker)
	if sameWorker != holdWorker {
		log.Fatalf("ColocateWith mismatch: hold=%s same=%s", holdWorker, sameWorker)
	}
}

func runFollowTargetDemo(ctx context.Context, wf *workflow.Workflow, primary, secondary *mutableClaims) {
	resetHold()
	primary.Set("secondary")
	secondary.Set("primary")

	dag := workflow.New()
	hold := dag.StepOpts("hold-primary", HoldPrimary, []workflow.StepOption{workflow.OnTarget("primary")})
	follow := dag.StepOpts("follow-primary", CurrentWorker, []workflow.StepOption{workflow.FollowTargetOf(hold)})
	check(dag.Submit(ctx, wf))

	waitForHold()
	primary.Set("primary")
	secondary.Set("secondary")
	time.Sleep(300 * time.Millisecond)
	close(holdRelease)

	waitCtx, wc := context.WithTimeout(ctx, 20*time.Second)
	defer wc()

	holdWorker, err := workflow.Await[string](waitCtx, wf, dag.ID(), hold)
	check(err)
	followWorker, err := workflow.Await[string](waitCtx, wf, dag.ID(), follow)
	check(err)

	log.Printf("FollowTargetOf: hold-primary=%s follow-primary=%s", holdWorker, followWorker)
	if followWorker == holdWorker {
		log.Fatalf("FollowTargetOf did not move: hold=%s follow=%s", holdWorker, followWorker)
	}
}

func runColocateHereDemo(ctx context.Context, wf *workflow.Workflow, primary, secondary *mutableClaims) {
	primary.Set("primary")
	secondary.Set("secondary")

	dag := workflow.New()
	parent := dag.StepOpts("spawn-here", AddColocatedChild, []workflow.StepOption{workflow.OnTarget("primary")})
	check(dag.Submit(ctx, wf))

	waitCtx, wc := context.WithTimeout(ctx, 20*time.Second)
	defer wc()

	parentWorker, err := workflow.Await[string](waitCtx, wf, dag.ID(), parent)
	check(err)
	childWorker, err := workflow.AwaitByID[string](waitCtx, wf, dag.ID(), "child-here")
	check(err)

	log.Printf("ColocateHere: parent=%s child=%s", parentWorker, childWorker)
	if childWorker != parentWorker {
		log.Fatalf("ColocateHere mismatch: parent=%s child=%s", parentWorker, childWorker)
	}
}

func waitForHold() {
	select {
	case <-holdStarted:
	case <-time.After(10 * time.Second):
		log.Fatal("hold-primary did not start")
	}
}

func startWorker(ctx context.Context, nc *nats.Conn, wf *workflow.Workflow, workerID string, claims worker.ClaimProvider) {
	reg := task.NewRegistry()
	task.MustRegister(reg, CurrentWorker)
	task.MustRegister(reg, HoldPrimary)
	task.MustRegister(reg, AddColocatedChild)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency:          4,
		StepHook:             wf.Hook(),
		Middleware:           []worker.Middleware{wf.ContextMiddleware()},
		WorkerID:             workerID,
		Claims:               claims,
		ClaimRefreshInterval: 50 * time.Millisecond,
		ClaimRetryDelay:      25 * time.Millisecond,
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
