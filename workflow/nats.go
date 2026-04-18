package workflow

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NewFromNATS constructs a Workflow backed by the given NATS connection:
// - NatsStore for step/result persistence (KV bucket ebind-dags)
// - NatsBus for scheduler events (stream EBIND_DAG_EVENTS)
// - NatsEnqueuer for publishing tasks on the existing TASKS stream
// Replicas applies to the KV bucket and events stream; use 1 for single-node dev,
// 3 for HA cluster.
func NewFromNATS(ctx context.Context, nc *nats.Conn, replicas int) (*Workflow, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("workflow: jetstream: %w", err)
	}
	store, err := NewNatsStore(ctx, js, replicas)
	if err != nil {
		return nil, err
	}
	bus, err := NewNatsBus(ctx, js, replicas)
	if err != nil {
		return nil, err
	}
	enq := NewNatsEnqueuer(js)
	return NewWorkflow(store, bus, enq), nil
}
