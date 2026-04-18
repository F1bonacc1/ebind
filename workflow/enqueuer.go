package workflow

import (
	"context"

	"github.com/f1bonacc1/ebind/task"
)

// Enqueuer is the narrow interface used by the scheduler to place a ready step
// on the TASKS stream. Keeps scheduler logic decoupled from NATS specifics —
// tests pass a capture fake, production wires to JetStream via Workflow.NewFromNATS.
type Enqueuer interface {
	Enqueue(ctx context.Context, envelope task.Task) error
}

// LeaderElector is the user-provided contract for gating scheduler processing.
// Every worker starts a scheduler loop; only the current leader drives state
// mutations to avoid races during failover. nil means "always leader."
type LeaderElector interface {
	IsLeader() bool
}

// alwaysLeader is the default elector (no gating — useful for single-node setups).
type alwaysLeader struct{}

func (alwaysLeader) IsLeader() bool { return true }
