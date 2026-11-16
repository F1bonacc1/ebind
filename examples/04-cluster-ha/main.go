// HA cluster: start 3 NATS JetStream nodes in-process with replicated streams.
// The same pattern (with one node per machine) gives you production HA — losing
// one node leaves a majority, streams are replicated (R=3), tasks keep flowing.
//
// Two timing facts of a real cluster show up here, and the example handles
// both the way a production client should:
//   - Right after the cluster forms, a reply can be dropped while a new
//     client's subscription interest spreads across the routes. Stream
//     creation is idempotent, so EnsureStreams is simply retried; a dropped
//     pull delivery heals when the consumer's 30s pull request expires, so
//     the first round trip gets a budget longer than that.
//   - Losing a node is not instant: if it led a stream, the survivors elect a
//     new leader first, and a publish in that window times out. enqueueRetry
//     retries through it.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

func Echo(ctx context.Context, s string) (string, error) {
	return "echo:" + s, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3-node cluster on loopback. In production, each node runs on a different
	// machine, routes point at the other machines' cluster ports.
	cluster, err := embed.StartCluster(embed.ClusterConfig{
		Size: 3,
		Name: "ebind-ha-demo",
	})
	check(err)
	defer cluster.Shutdown()

	check(cluster.WaitReady(15 * time.Second))
	log.Printf("cluster URLs: %s", cluster.ClientURLs())

	nc, err := nats.Connect(cluster.ClientURLs())
	check(err)
	defer nc.Close()

	js, _ := jetstream.New(nc)
	// Replicas: 3 — streams are replicated across all nodes for HA.
	check(retry(30*time.Second, "create streams", func() error {
		attemptCtx, ac := context.WithTimeout(ctx, 5*time.Second)
		defer ac()
		return stream.EnsureStreams(attemptCtx, js, stream.Config{Replicas: 3})
	}))

	reg := task.NewRegistry()
	task.MustRegister(reg, Echo)

	w, err := worker.New(nc, reg, worker.Options{Concurrency: 4})
	check(err)
	go func() { _ = w.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)

	c, err := client.New(ctx, nc, client.Options{})
	check(err)
	defer c.Close()

	// Round-trip some tasks.
	for i := 0; i < 3; i++ {
		fut, err := client.Enqueue(c, Echo, fmt.Sprintf("msg-%d", i))
		check(err)
		waitCtx, wc := context.WithTimeout(ctx, 40*time.Second)
		result, err := client.Await[string](waitCtx, fut)
		wc()
		check(err)
		log.Printf("task %d result: %s", i, result)
	}

	// Now kill a node that is not the JetStream meta leader and verify the
	// cluster keeps serving. It may still lead one of the streams.
	for i, n := range cluster.Nodes {
		if !n.Server().JetStreamIsLeader() {
			log.Printf("killing non-leader node %d for failover demo", i)
			cluster.ShutdownNode(i)
			break
		}
	}

	fut := enqueueRetry(c, "after-failover")
	waitCtx, wc := context.WithTimeout(ctx, 30*time.Second)
	result, err := client.Await[string](waitCtx, fut)
	wc()
	check(err)
	log.Printf("post-failover: %s", result)
}

// enqueueRetry enqueues, retrying while the cluster re-elects the leader of
// a stream the killed node led. Each Enqueue publish has its own 5s budget.
func enqueueRetry(c *client.Client, msg string) *client.Future {
	var fut *client.Future
	check(retry(30*time.Second, "enqueue during failover", func() error {
		var err error
		fut, err = client.Enqueue(c, Echo, msg)
		return err
	}))
	return fut
}

// retry calls fn until it succeeds or budget runs out.
func retry(budget time.Duration, what string, fn func() error) error {
	deadline := time.Now().Add(budget)
	for {
		err := fn()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		log.Printf("%s: %v; retrying", what, err)
		time.Sleep(500 * time.Millisecond)
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
