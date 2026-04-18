// HA cluster: start 3 NATS JetStream nodes in-process with replicated streams.
// The same pattern (with one node per machine) gives you production HA — losing
// one node leaves a majority, streams are replicated (R=3), tasks keep flowing.
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
	check(stream.EnsureStreams(ctx, js, stream.Config{Replicas: 3}))

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
		waitCtx, wc := context.WithTimeout(ctx, 5*time.Second)
		result, err := client.Await[string](waitCtx, fut)
		wc()
		check(err)
		log.Printf("task %d result: %s", i, result)
	}

	// Now kill a non-leader node and verify the cluster keeps serving.
	for i, n := range cluster.Nodes {
		if !n.Server().JetStreamIsLeader() {
			log.Printf("killing non-leader node %d for failover demo", i)
			cluster.ShutdownNode(i)
			break
		}
	}

	fut, err := client.Enqueue(c, Echo, "after-failover")
	check(err)
	waitCtx, wc := context.WithTimeout(ctx, 10*time.Second)
	result, err := client.Await[string](waitCtx, fut)
	wc()
	check(err)
	log.Printf("post-failover: %s", result)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
