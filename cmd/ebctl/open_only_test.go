package main_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	"github.com/f1bonacc1/ebind/embed"
	"github.com/f1bonacc1/ebind/internal/testutil"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

const (
	kvStream     = "KV_ebind-dags"
	kvBucket     = "ebind-dags"
	eventsStream = workflow.DAGEventsStream
)

// bareCtx is a cli.Context on nc that, unlike newTestCtx, provisions nothing.
func bareCtx(t *testing.T, nc *nats.Conn, js jetstream.JetStream) *cli.Context {
	t.Helper()
	p, err := format.New("json")
	if err != nil {
		t.Fatal(err)
	}
	c := cli.NewContext()
	c.NC = nc
	c.JS = js
	c.Printer = p
	c.Timeout = 5 * time.Second
	return c
}

func streamConfig(t *testing.T, ctx context.Context, js jetstream.JetStream, name string) jetstream.StreamConfig {
	t.Helper()
	s, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatalf("stream %s: %v", name, err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("stream %s info: %v", name, err)
	}
	return info.Config
}

// TestWorkflowOpen_ClusterKeepsReplicas is the regression test for ebctl
// scaling an R=3 deployment down to R=1: opening the workflow from ebctl used
// create-or-update with ebctl's default --replicas 1.
func TestWorkflowOpen_ClusterKeepsReplicas(t *testing.T) {
	cl, err := embed.StartCluster(embed.ClusterConfig{Size: 3, Name: "ebctl-open-only"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Shutdown)
	if err := cl.WaitReady(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(cl.ClientURLs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The worker side owns the deployment and provisions it at R=3.
	if _, err := workflow.NewFromNATS(ctx, nc, 3); err != nil {
		t.Fatalf("provision workflow at R=3: %v", err)
	}

	c := bareCtx(t, nc, js)
	if _, err := c.Workflow(ctx); err != nil {
		t.Fatalf("ebctl open workflow: %v", err)
	}

	for _, name := range []string{kvStream, eventsStream} {
		if got := streamConfig(t, ctx, js, name).Replicas; got != 3 {
			t.Errorf("%s replicas after ebctl open = %d, want 3", name, got)
		}
	}
}

// TestWorkflowOpen_PreservesConfig checks the same property on one node, for
// settings other than replicas: an open must not rewrite anything.
func TestWorkflowOpen_PreservesConfig(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := workflow.NewFromNATS(ctx, h.Conn, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.JS.UpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: kvBucket, Description: "owned by the deployment", History: 7,
	}); err != nil {
		t.Fatalf("customize bucket: %v", err)
	}
	evCfg := streamConfig(t, ctx, h.JS, eventsStream)
	evCfg.Description = "owned by the deployment"
	if _, err := h.JS.UpdateStream(ctx, evCfg); err != nil {
		t.Fatalf("customize events stream: %v", err)
	}

	c := bareCtx(t, h.Conn, h.JS)
	if _, err := c.Workflow(ctx); err != nil {
		t.Fatalf("ebctl open workflow: %v", err)
	}

	kvCfg := streamConfig(t, ctx, h.JS, kvStream)
	if kvCfg.Description != "owned by the deployment" || kvCfg.MaxMsgsPerSubject != 7 {
		t.Errorf("bucket config rewritten: description=%q history=%d", kvCfg.Description, kvCfg.MaxMsgsPerSubject)
	}
	if got := streamConfig(t, ctx, h.JS, eventsStream).Description; got != "owned by the deployment" {
		t.Errorf("events stream config rewritten: description=%q", got)
	}
}

// TestWorkflowOpen_MissingBucket checks that ebctl reports an unprovisioned
// deployment instead of creating the bucket itself.
func TestWorkflowOpen_MissingBucket(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c := bareCtx(t, h.Conn, h.JS)
	if _, err := c.Workflow(ctx); !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Fatalf("open on empty server: err = %v, want ErrBucketNotFound", err)
	}
	if _, err := h.JS.KeyValue(ctx, kvBucket); !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Errorf("ebctl created the bucket: err = %v", err)
	}
	if _, err := h.JS.Stream(ctx, eventsStream); !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("ebctl created the events stream: err = %v", err)
	}
}
