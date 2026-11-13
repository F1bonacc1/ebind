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
	// PutStep writes rec and returns the new revision. On a clustered KV the
	// returned revision lets a caller CAS-update the record again without a
	// read-back — a read-back can be served a stale value by a lagging replica
	// (NATS KV Get uses direct get), so avoiding it removes a read-your-writes
	// race. On error the returned revision is 0.
	PutStep(ctx context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) (uint64, error)
	ListSteps(ctx context.Context, dagID string) ([]StepRecord, error)

	GetResult(ctx context.Context, dagID, stepID string) ([]byte, error)
	PutResult(ctx context.Context, dagID, stepID string, data []byte) error

	GetMeta(ctx context.Context, dagID string) (DAGMeta, uint64, error)
	// GetMetaLatest is GetMeta with read-your-writes: it never returns a
	// meta older than the last committed write. On a clustered KV, GetMeta
	// is a direct get that a lagging replica may answer; GetMetaLatest asks
	// the stream leader, so it costs a leader round trip. Used where a stale
	// ArmedMatchers set could dispatch a step a runtime ArmBreakpoints has
	// already fenced (see ArmBreakpoints), and for read-modify-write of meta.
	GetMetaLatest(ctx context.Context, dagID string) (DAGMeta, uint64, error)
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

	// WatchDAG streams one DAG's meta and step records: first the current
	// value of each (deleted keys may appear as Deleted changes), then one
	// change with Synced set, then every later put or delete in commit order.
	// The channel closes when ctx ends, or when the watch fails or the
	// consumer falls too far behind; a caller that still needs updates
	// re-watches and gets a fresh snapshot. Results and signals are not
	// included.
	WatchDAG(ctx context.Context, dagID string) (<-chan DAGChange, error)

	// GetSignal returns the delivered signal record, or ErrSignalNotFound.
	GetSignal(ctx context.Context, dagID, name string) (SignalRecord, error)
	// PutSignal creates the record — first-wins: signal records are immutable
	// and there is no update path. Returns ErrStaleRevision if the signal was
	// already delivered.
	PutSignal(ctx context.Context, dagID string, rec SignalRecord) error
	ListSignals(ctx context.Context, dagID string) ([]SignalRecord, error)
	// DeleteSignal removes a record; missing keys are not errors.
	DeleteSignal(ctx context.Context, dagID, name string) error
}

// DAGChange is one item of a WatchDAG stream. Exactly one of Meta, Step,
// Deleted or Synced is set.
type DAGChange struct {
	// Meta is the DAG's meta record after a put.
	Meta *DAGMeta
	// Step is a step record after a put.
	Step *StepRecord
	// Deleted names a deleted key: DeletedMeta for the meta record,
	// otherwise the step ID.
	Deleted string
	// Synced is set once, after the initial snapshot has been delivered.
	Synced bool
}

// DeletedMeta is DAGChange.Deleted for a deleted meta record. It cannot
// collide with a step ID on NatsStore: '<' and '>' are outside the KV key
// charset, so no stored step can have this ID.
const DeletedMeta = "<meta>"
