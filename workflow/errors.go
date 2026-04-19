package workflow

import "errors"

// ErrStepFailed is returned by Await when the target step (or an upstream mandatory
// step whose failure cascaded) ended in a failed status.
var ErrStepFailed = errors.New("workflow: step failed")

// ErrStepSkipped is returned by Await when the target step was skipped because an
// upstream step this step depended on (via Ref, not RefOrDefault) failed or was skipped.
var ErrStepSkipped = errors.New("workflow: step skipped")

// ErrStepCanceled is returned by Await when the target step was canceled before it started.
var ErrStepCanceled = errors.New("workflow: step canceled")

// ErrDAGNotFound is returned when a DAG ID has no meta record in the store.
var ErrDAGNotFound = errors.New("workflow: DAG not found")

// ErrDAGCanceled is returned when a DAG no longer accepts new work because it was canceled.
var ErrDAGCanceled = errors.New("workflow: DAG canceled")

// ErrStepNotFound is returned when a step ID is not registered in the DAG.
var ErrStepNotFound = errors.New("workflow: step not found")

// ErrCycle is returned by DAG.Submit if the graph contains a cycle.
var ErrCycle = errors.New("workflow: cycle detected")

// ErrDuplicateStep is returned by DAG.Step when the step ID is already used.
var ErrDuplicateStep = errors.New("workflow: duplicate step ID")

// ErrStaleRevision is returned by stores when a CAS operation finds a newer revision.
var ErrStaleRevision = errors.New("workflow: stale revision")
