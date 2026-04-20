package workflow

import "context"

// StateStore persists DAG + step + result records with CAS semantics.
// All PutX(..., expectedRev) operations return ErrStaleRevision if the stored
// record has a different revision. expectedRev == 0 means "create if absent".
//
// The interface is designed so the scheduler logic remains pure — all IO flows
// through StateStore, EventBus, and Enqueuer. Tests use store_mem.go fakes.
type StateStore interface {
	GetStep(ctx context.Context, dagID, stepID string) (StepRecord, uint64, error)
	PutStep(ctx context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) error
	ListSteps(ctx context.Context, dagID string) ([]StepRecord, error)

	GetResult(ctx context.Context, dagID, stepID string) ([]byte, error)
	PutResult(ctx context.Context, dagID, stepID string, data []byte) error

	GetMeta(ctx context.Context, dagID string) (DAGMeta, uint64, error)
	PutMeta(ctx context.Context, dagID string, meta DAGMeta, expectedRev uint64) error
	// ListDAGs enumerates all stored DAG meta records. Used by the scheduler's
	// sweep on leader acquisition to find stranded ready steps across all running DAGs.
	ListDAGs(ctx context.Context) ([]DAGMeta, error)

	// DeleteMeta, DeleteStep, DeleteResult remove records. Missing keys are not errors
	// (delete is idempotent). Callers coordinating a DAG-level delete should use
	// DeleteDAG which composes these in the correct order.
	DeleteMeta(ctx context.Context, dagID string) error
	DeleteStep(ctx context.Context, dagID, stepID string) error
	DeleteResult(ctx context.Context, dagID, stepID string) error

	// WatchResult streams a single message once a result is written at the given key.
	// Implementations should close the channel on cancel or immediately if the result
	// already exists (sending the existing value first).
	WatchResult(ctx context.Context, dagID, stepID string) (<-chan []byte, error)
}
