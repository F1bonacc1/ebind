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

var (
	longStarted chan struct{}
	longRelease chan struct{}
	longOnce    sync.Once
)

func resetLong() {
	longStarted = make(chan struct{})
	longRelease = make(chan struct{})
	longOnce = sync.Once{}
}

func LongStep(context.Context) (string, error) {
	longOnce.Do(func() { close(longStarted) })
	<-longRelease
	return "long-finished", nil
}

func AfterStep(context.Context) (string, error) {
	return "after-finished", nil
}

func main() {
	resetLong()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-cancel-*")
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
	task.MustRegister(reg, LongStep)
	task.MustRegister(reg, AfterStep)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	dag := workflow.New()
	long := dag.Step("long", LongStep)
	after := dag.StepOpts("after", AfterStep, []workflow.StepOption{workflow.After(long)})

	check(dag.Submit(ctx, wf))

	select {
	case <-longStarted:
	case <-time.After(10 * time.Second):
		log.Fatal("long step did not start")
	}

	check(workflow.Cancel(ctx, wf, dag.ID()))
	log.Printf("requested cancel for DAG %s", dag.ID())

	close(longRelease)

	waitCtx, wc := context.WithTimeout(ctx, 20*time.Second)
	defer wc()

	longResult, err := workflow.Await[string](waitCtx, wf, dag.ID(), long)
	check(err)
	log.Printf("long step result: %s", longResult)

	_, err = workflow.Await[string](waitCtx, wf, dag.ID(), after)
	if !errors.Is(err, workflow.ErrStepCanceled) {
		log.Fatalf("after step: got %v, want %v", err, workflow.ErrStepCanceled)
	}
	log.Printf("after step was canceled")

	metaDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(metaDeadline) {
		meta, _, err := workflow.DAGInfo(ctx, wf, dag.ID())
		if err == nil && meta.Status == workflow.DAGStatusCanceled {
			log.Printf("DAG status: %s", meta.Status)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatal("DAG did not finalize to canceled")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
