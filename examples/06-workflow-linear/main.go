// Linear DAG workflow: A → B → C. Each step's output feeds the next.
// State is durable in a NATS KV bucket, so the DAG survives producer restarts.
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

type User struct {
	ID    string
	Name  string
	Email string
}

type Profile struct {
	User    User
	Summary string
}

func FetchUser(ctx context.Context, id string) (User, error) {
	log.Printf("fetching user %s", id)
	return User{ID: id, Name: "Alice", Email: "alice@example.com"}, nil
}

func BuildProfile(ctx context.Context, u User) (Profile, error) {
	log.Printf("building profile for %s", u.Name)
	return Profile{User: u, Summary: fmt.Sprintf("%s <%s>", u.Name, u.Email)}, nil
}

func Format(ctx context.Context, p Profile) (string, error) {
	return fmt.Sprintf("PROFILE: %s", p.Summary), nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	storeDir, _ := os.MkdirTemp("", "ebind-linear-*")
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
	task.MustRegister(reg, FetchUser)
	task.MustRegister(reg, BuildProfile)
	task.MustRegister(reg, Format)

	w, err := worker.New(nc, reg, worker.Options{
		Concurrency: 4,
		StepHook:    wf.Hook(),
		Middleware:  []worker.Middleware{wf.ContextMiddleware()},
	})
	check(err)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = wf.RunScheduler(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// Build the DAG. Each step's Ref() feeds into the next step's args.
	dag := workflow.New()
	a := dag.Step("fetch", FetchUser, "user-42")
	b := dag.Step("build", BuildProfile, a.Ref())
	c := dag.Step("format", Format, b.Ref())

	check(dag.Submit(ctx, wf))

	waitCtx, wc := context.WithTimeout(ctx, 15*time.Second)
	defer wc()
	result, err := workflow.Await[string](waitCtx, wf, dag.ID(), c)
	check(err)

	log.Printf("final: %s", result)

	// Brief poll so DAG meta is finalized before we read the snapshot.
	for i := 0; i < 50; i++ {
		if m, _, _ := wf.Store.GetMeta(ctx, dag.ID()); m.Status != workflow.DAGStatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Structured debug snapshot + human-readable report.
	log.Println("--- DAG debug snapshot ---")
	if err := workflow.DebugPrint(ctx, wf, dag.ID(), os.Stdout); err != nil {
		log.Printf("DebugPrint: %v", err)
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
