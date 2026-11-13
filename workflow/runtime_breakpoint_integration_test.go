package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/workflow"
)

// TestRuntimeBreakpoints_E2E: StartPaused, arm at runtime, Resume, wait for
// the stop, disarm, and the DAG completes. Real NATS KV: arm and disarm
// read meta from the stream leader.
func TestRuntimeBreakpoints_E2E(t *testing.T) {
	h := setup(t)
	task.MustRegister(h.reg, hAdd)
	task.MustRegister(h.reg, hDouble)

	dag := workflow.New()
	a := dag.Step("a", hAdd, 2, 3)
	b := dag.Step("b", hDouble, a.Ref())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := dag.Submit(ctx, h.wf, workflow.StartPaused()); err != nil {
		t.Fatal(err)
	}
	rep, err := workflow.ArmBreakpoints(ctx, h.wf, dag.ID(), workflow.AfterStep("a"), workflow.BeforeStep("b"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rep.Stopped, ","); got != "a@after,b@before" {
		t.Fatalf("Stopped = %s, report %+v", got, rep)
	}
	if err := workflow.Resume(ctx, h.wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	info, err := workflow.WaitForBreakpoint(ctx, h.wf, dag.ID(), workflow.AfterStep("a"))
	if err != nil || info.StepID != "a" {
		t.Fatalf("after a: %+v, %v", info, err)
	}
	if n, err := workflow.DisarmBreakpoints(ctx, h.wf, dag.ID(), workflow.AfterStep("a")); err != nil || n != 1 {
		t.Fatalf("disarm after a: n=%d err=%v", n, err)
	}
	if _, err := workflow.WaitForBreakpoint(ctx, h.wf, dag.ID(), workflow.BeforeStep("b")); err != nil {
		t.Fatalf("before b: %v", err)
	}
	if n, err := workflow.DisarmBreakpoints(ctx, h.wf, dag.ID(), workflow.BeforeStep("b")); err != nil || n != 1 {
		t.Fatalf("disarm before b: n=%d err=%v", n, err)
	}
	got, err := workflow.Await[int](ctx, h.wf, dag.ID(), b)
	if err != nil || got != 10 {
		t.Fatalf("b = %d, %v; want 10", got, err)
	}
	meta, steps, err := workflow.DAGInfo(ctx, h.wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.ArmedMatchers) != 0 {
		t.Errorf("ArmedMatchers = %+v, want none", meta.ArmedMatchers)
	}
	for _, s := range steps {
		if len(s.BPFence) != 0 || s.BPBefore == workflow.BPStateReleased || s.BPAfter == workflow.BPStateReleased {
			t.Errorf("step %s: fence %v, before %q, after %q", s.StepID, s.BPFence, s.BPBefore, s.BPAfter)
		}
	}
}
