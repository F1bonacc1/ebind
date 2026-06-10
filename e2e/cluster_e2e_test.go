//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// TestClusterE2E runs the whole library — task queue, futures, retries, DLQ,
// targeted placement, and the DAG workflow engine — on a 3-node embedded NATS
// cluster with everything replicated at R=3, while NATS nodes are killed and
// restarted mid-flight.
//
// The long-running DAG is gated on channels the test controls, so each failure
// injection happens in a deterministic window:
//
//	gate1 → kill a follower node, then run a 20-step chain on the 2-node cluster
//	gate2 → Pause/Resume the DAG while degraded
//	       → restart the killed follower, verify it rejoins and serves clients
//	gate3 → kill the JetStream meta-leader, then finish the DAG across the failover
//
// Phases run sequentially against one shared cluster; a phase failure skips the rest.
func TestClusterE2E(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	const longDAG = "e2e-long"
	// Step handles created in SubmitLongDAG, awaited in later phases.
	var colo, folo *workflow.Step

	ok := true
	phase := func(name string, fn func(t *testing.T)) {
		if !ok {
			t.Logf("skipping phase %s: an earlier phase failed", name)
			return
		}
		ok = t.Run(name, fn)
	}

	phase("00_BaselineTaskQueue", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Second)
		defer pcancel()

		// Enqueue + every Future flavor.
		fut, err := client.Enqueue(h.cli, eAdd, 2, 3)
		if err != nil {
			t.Fatal(err)
		}
		var sum int
		if err := fut.Get(pctx, &sum); err != nil {
			t.Fatal(err)
		}
		if sum != 5 {
			t.Fatalf("eAdd: got %d, want 5", sum)
		}
		fut2, err := client.Enqueue(h.cli, eAdd, 20, 22)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.Await[int](pctx, fut2); err != nil || got != 42 {
			t.Fatalf("Await[int]: got %d, %v; want 42", got, err)
		}
		fut3, err := client.Enqueue(h.cli, eAdd, 2, 3)
		if err != nil {
			t.Fatal(err)
		}
		if raw, err := fut3.GetRaw(pctx); err != nil || string(raw) != "5" {
			t.Fatalf("GetRaw: got %q, %v; want \"5\"", raw, err)
		}

		// Alias path: the envelope carries "e2e.LegacyAdd", workers resolve it to eAdd.
		futA, err := client.Enqueue(h.cli, LegacyAdd, 5, 6)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.Await[int](pctx, futA); err != nil || got != 11 {
			t.Fatalf("alias enqueue: got %d, %v; want 11", got, err)
		}

		// Targeted execution: concrete worker claim and logical claim.
		futW, err := client.EnqueueOpts(h.cli, WhoAmI, client.EnqueueOptions{Target: worker.ConcreteTarget("w2")})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.Await[string](pctx, futW); err != nil || got != "w2" {
			t.Fatalf("concrete target: got %q, %v; want w2", got, err)
		}
		futP, err := client.EnqueueOpts(h.cli, WhoAmI, client.EnqueueOptions{Target: "primary"})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.Await[string](pctx, futP); err != nil || got != "w1" {
			t.Fatalf("logical target: got %q, %v; want w1", got, err)
		}

		// Fire-and-forget: SkipResponse returns no future; EnqueueAsync returns an ID.
		futS, err := client.EnqueueOpts(h.cli, eSideEffect, client.EnqueueOptions{SkipResponse: true})
		if err != nil {
			t.Fatal(err)
		}
		if futS != nil {
			t.Fatal("SkipResponse: expected nil future")
		}
		id, err := client.EnqueueAsync(h.cli, eSideEffect)
		if err != nil || id == "" {
			t.Fatalf("EnqueueAsync: id=%q err=%v", id, err)
		}
		waitFor(t, 15*time.Second, "fire-and-forget handlers ran", func() bool {
			return sideEffectCount.Load() >= 2
		})

		// Per-task retry policy: first attempt fails, second succeeds.
		futF, err := client.EnqueueOpts(h.cli, eFlakyOnce, client.EnqueueOptions{RetryPolicy: &task.RetryPolicy{
			InitialInterval:    50 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumInterval:    50 * time.Millisecond,
			MaximumAttempts:    3,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.Await[string](pctx, futF); err != nil || got != "recovered" {
			t.Fatalf("flaky retry: got %q, %v; want recovered", got, err)
		}
		if n := flakyAttempts.Load(); n < 2 {
			t.Fatalf("flaky attempts: got %d, want >= 2", n)
		}

		// Expired deadline → terminal failure with kind "deadline" before dispatch.
		futD, err := client.EnqueueOpts(h.cli, eAdd, client.EnqueueOptions{Deadline: time.Now().Add(-time.Second)}, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskErrKind(t, futD.Get(pctx, new(int)), task.ErrKindDeadline)

		// Non-retryable TaskError → immediate terminal failure.
		futN, err := client.Enqueue(h.cli, eNonRetryable)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskErrKind(t, futN.Get(pctx, new(int)), "validation")

		// Panicking handler → Recover middleware converts to kind "panic".
		futPn, err := client.Enqueue(h.cli, ePanics)
		if err != nil {
			t.Fatal(err)
		}
		assertTaskErrKind(t, futPn.Get(pctx, nil), task.ErrKindPanic)

		// Cycle detection rejects the DAG before anything is persisted.
		cyc := workflow.New()
		a := cyc.Step("a", eDouble, workflow.Ref{StepID: "b", Mode: workflow.RefModeRequired})
		_ = cyc.Step("b", eDouble, a.Ref())
		if err := cyc.Submit(pctx, h.wf); !errors.Is(err, workflow.ErrCycle) {
			t.Fatalf("cycle submit: got %v, want ErrCycle", err)
		}

		// Every stream (and the workflow KV bucket) must be placed at R=3.
		for _, name := range ebindStreams() {
			s, err := h.js.Stream(pctx, name)
			if err != nil {
				t.Fatalf("stream %s: %v", name, err)
			}
			info, err := s.Info(pctx)
			if err != nil {
				t.Fatalf("stream %s info: %v", name, err)
			}
			if info.Cluster == nil || 1+len(info.Cluster.Replicas) != replicas {
				t.Fatalf("stream %s: not running %d replicas: %+v", name, replicas, info.Cluster)
			}
		}
	})

	phase("01_SubmitLongDAG", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Second)
		defer pcancel()

		retry := task.RetryPolicy{
			InitialInterval:    100 * time.Millisecond,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Second,
			MaximumAttempts:    4,
		}
		dag := workflow.New(workflow.WithDAGID(longDAG), workflow.WithRetry(retry))

		s1 := dag.Step("s1", eAdd, 2, 3)                                                       // = 5
		s2 := dag.StepOpts("s2", eDouble, []workflow.StepOption{workflow.OnTarget("primary")}, 10) // = 20 on w1
		g1 := dag.Step("g1", eGate1, s1.Ref())                                                 // = 5, kill-follower window
		prev := g1
		for i := 0; i < 20; i++ { // c00..c19: continuous traffic through the degraded window
			prev = dag.Step(fmt.Sprintf("c%02d", i), eInc, prev.Ref())
		}
		g2 := dag.Step("g2", eGate2, prev.Ref()) // = 25, pause/resume window
		g3 := dag.Step("g3", eGate3, g2.Ref())   // = 25, meta-leader-kill window

		// Optional failure + OrDefault substitution + temporal deps, all scheduled
		// after g1 so they execute while the cluster runs on 2 nodes.
		opt := dag.StepOpts("opt", eFailOptional, []workflow.StepOption{
			workflow.Optional(), workflow.WithStepRetry(task.NoRetryPolicy()), workflow.After(g1),
		})
		dflt := dag.Step("dflt", eDouble, opt.RefOrDefault(7)) // = 14 despite opt failing
		_ = dag.StepOpts("any", eWhere, []workflow.StepOption{workflow.AfterAny(opt)})
		_ = dag.StepOpts("ord", eWhere, []workflow.StepOption{workflow.After(dflt)})

		// Dynamic step addition behind the pause/resume window, pinned to w2.
		_ = dag.StepOpts("dynp", eDynamicParent, []workflow.StepOption{
			workflow.OnTarget("secondary"), workflow.After(g2),
		}, 3)

		// Placement chaining behind the meta-leader-kill window.
		colo = dag.StepOpts("colo", eWhere, []workflow.StepOption{workflow.ColocateWith(s2), workflow.After(g3)})
		folo = dag.StepOpts("folo", eWhere, []workflow.StepOption{workflow.FollowTargetOf(s2), workflow.After(g3)})

		final := dag.Step("final", eAdd3, g3.Ref(), s2.Ref(), dflt.Ref()) // = 25+20+14 = 59
		_ = final

		if err := dag.Submit(pctx, h.wf); err != nil {
			t.Fatal(err)
		}

		// Roots complete on the healthy cluster; the targeted root proves claim routing.
		if got, err := workflow.Await[int](pctx, h.wf, longDAG, s2); err != nil || got != 20 {
			t.Fatalf("s2: got %d, %v; want 20", got, err)
		}
		select {
		case <-gate1.Reached():
		case <-time.After(30 * time.Second):
			t.Fatal("gate1 handler never started")
		}
		if st := h.dagStatus(pctx, longDAG); st != workflow.DAGStatusRunning {
			t.Fatalf("long DAG status: got %s, want running", st)
		}
	})

	phase("02_KillFollowerNode", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 150*time.Second)
		defer pcancel()

		victim := h.followerIndex()
		if victim < 0 {
			t.Fatal("no follower to kill")
		}
		h.victim = victim
		h.victimURL = h.urls[victim]

		// The canary only knows the victim's URL — it proves, in phase 04, that
		// the restarted node accepts clients again.
		canary, err := nats.Connect(h.victimURL,
			nats.ReconnectWait(250*time.Millisecond),
			nats.MaxReconnects(-1),
			nats.RetryOnFailedConnect(true),
		)
		if err != nil {
			t.Fatal(err)
		}
		h.canary = canary

		t.Logf("killing follower node %d (%s)", victim, h.victimURL)
		h.cluster.ShutdownNode(victim)

		// The worker pinned to the victim fails over to a surviving node.
		pinned := h.workers[victim]
		waitFor(t, 15*time.Second, "pinned worker reconnects", func() bool {
			return pinned.nc.Status() == nats.CONNECTED && pinned.nc.ConnectedUrl() != h.victimURL
		})

		// Let stream/KV raft groups settle on the surviving quorum, then resume
		// the workflow: the 20-step chain runs entirely on the 2-node cluster.
		h.waitStreamLeadersSettled(t, 30*time.Second)
		gate1.Release()

		waitFor(t, 60*time.Second, "chain reaches c09 on the degraded cluster", func() bool {
			return h.stepStatus(ctx, longDAG, "c09") == workflow.StatusDone
		})

		// The plain task queue keeps serving while degraded.
		fut := h.enqueueWithRetry(t, 30*time.Second, eAdd, 40, 2)
		awaitCtx, awaitCancel := context.WithTimeout(pctx, 45*time.Second)
		defer awaitCancel()
		if got, err := client.Await[int](awaitCtx, fut); err != nil || got != 42 {
			t.Fatalf("degraded enqueue: got %d, %v; want 42", got, err)
		}

		select {
		case <-gate2.Reached():
		case <-time.After(60 * time.Second):
			t.Fatal("gate2 handler never started (chain did not complete)")
		}
	})

	phase("03_PauseResumeDegraded", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 120*time.Second)
		defer pcancel()

		// g2's handler is in-flight, so Pause must drain through `pausing`.
		if err := workflow.Pause(pctx, h.wf, longDAG); err != nil {
			t.Fatal(err)
		}
		if st := h.dagStatus(pctx, longDAG); st != workflow.DAGStatusPausing {
			t.Fatalf("after Pause: got %s, want pausing", st)
		}
		for _, id := range []string{"g3", "dynp", "final"} {
			rec, _, err := h.wf.Store.GetStep(pctx, longDAG, id)
			if err != nil {
				t.Fatal(err)
			}
			if !rec.Held || rec.Status != workflow.StatusPending {
				t.Fatalf("step %s: held=%v status=%s, want held pending", id, rec.Held, rec.Status)
			}
		}

		// Drain: g2 finishes, DAG auto-transitions pausing → paused.
		gate2.Release()
		waitFor(t, 30*time.Second, "DAG paused", func() bool {
			return h.dagStatus(ctx, longDAG) == workflow.DAGStatusPaused
		})

		// Nothing new may start while paused: g3 stays pending+held.
		time.Sleep(1500 * time.Millisecond)
		rec, _, err := h.wf.Store.GetStep(pctx, longDAG, "g3")
		if err != nil {
			t.Fatal(err)
		}
		if !rec.Held || rec.Status != workflow.StatusPending {
			t.Fatalf("paused g3: held=%v status=%s, want held pending", rec.Held, rec.Status)
		}

		if err := workflow.Resume(pctx, h.wf, longDAG); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 30*time.Second, "DAG running after Resume", func() bool {
			return h.dagStatus(ctx, longDAG) == workflow.DAGStatusRunning
		})

		// Resume re-enqueues the held ready steps: g3 dispatches...
		select {
		case <-gate3.Reached():
		case <-time.After(60 * time.Second):
			t.Fatal("gate3 handler never started after Resume")
		}
		// ...and the dynamic branch completes on w2 (still on 2 nodes).
		if got, err := workflow.AwaitByID[int](pctx, h.wf, longDAG, "dynp"); err != nil || got != 3 {
			t.Fatalf("dynp: got %d, %v; want 3", got, err)
		}
		if got, err := workflow.AwaitByID[int](pctx, h.wf, longDAG, "dyn-followup"); err != nil || got != 6 {
			t.Fatalf("dyn-followup: got %d, %v; want 6", got, err)
		}
		dyn, _, err := h.wf.Store.GetStep(pctx, longDAG, "dyn-followup")
		if err != nil {
			t.Fatal(err)
		}
		if dyn.WorkerID != "w2" {
			t.Fatalf("dyn-followup ran on %q, want w2 (ColocateHere with secondary parent)", dyn.WorkerID)
		}
	})

	phase("04_RestartFollower", func(t *testing.T) {
		if err := h.cluster.RestartNode(h.victim); err != nil {
			t.Fatal(err)
		}
		if err := h.cluster.WaitReady(30 * time.Second); err != nil {
			t.Fatal(err)
		}
		if err := h.cluster.WaitNodeHealthy(h.victim, 60*time.Second); err != nil {
			t.Fatal(err)
		}

		// The node serves clients again: the canary that only knows its URL reconnects.
		waitFor(t, 30*time.Second, "canary reconnects to restarted node", func() bool {
			return h.canary.Status() == nats.CONNECTED
		})

		// Replication converges: TASKS back to 3 current replicas.
		waitFor(t, 60*time.Second, "TASKS stream back to 3 current replicas", func() bool {
			return h.streamHasCurrentReplicas("EBIND_TASKS")
		})
	})

	phase("05_KillMetaLeaderAndFinish", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 180*time.Second)
		defer pcancel()

		leader := h.metaLeaderIndex()
		if leader < 0 {
			t.Fatal("no meta-leader found")
		}
		t.Logf("killing meta-leader node %d (%s)", leader, h.urls[leader])
		h.cluster.ShutdownNode(leader)

		// Quorum holds (the phase-02 victim is back): a new meta-leader is
		// elected and stream leaders settle on the survivors.
		h.waitMetaLeader(t, 60*time.Second)
		h.waitStreamLeadersSettled(t, 60*time.Second)

		// Finish the DAG across the failover. gate3 was held through the node
		// restart (deliberately past AckWait), so redelivered executions of g3
		// exercise the idempotent re-execution path here too.
		gate3.Release()

		if got, err := workflow.AwaitByID[int](pctx, h.wf, longDAG, "final"); err != nil || got != 59 {
			t.Fatalf("final: got %d, %v; want 59", got, err)
		}
		// Step-handle Await variant for the placement steps: both must have run
		// on w1 — colo on s2's concrete worker, folo on s2's re-resolved target.
		if got, err := workflow.Await[string](pctx, h.wf, longDAG, colo); err != nil || got != "w1" {
			t.Fatalf("colo: got %q, %v; want w1", got, err)
		}
		if got, err := workflow.Await[string](pctx, h.wf, longDAG, folo); err != nil || got != "w1" {
			t.Fatalf("folo: got %q, %v; want w1", got, err)
		}

		// Restore the full cluster: restart the killed meta-leader.
		if err := h.cluster.RestartNode(leader); err != nil {
			t.Fatal(err)
		}
		if err := h.cluster.WaitReady(30 * time.Second); err != nil {
			t.Fatal(err)
		}
		if err := h.cluster.WaitNodeHealthy(leader, 60*time.Second); err != nil {
			t.Fatal(err)
		}
	})

	phase("06_FailCancelDelete", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 120*time.Second)
		defer pcancel()

		// Retry exhaustion fails the step, cascade-skips the dependent, fails the DAG.
		fdag := workflow.New(workflow.WithDAGID("e2e-fail"))
		ex := fdag.StepOpts("exhaust", eRetryExhaust, []workflow.StepOption{workflow.WithStepRetry(task.RetryPolicy{
			InitialInterval:    50 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumInterval:    50 * time.Millisecond,
			MaximumAttempts:    3,
		})})
		down := fdag.Step("down", eDouble, ex.Ref())
		if err := fdag.Submit(pctx, h.wf); err != nil {
			t.Fatal(err)
		}
		if _, err := workflow.Await[int](pctx, h.wf, "e2e-fail", ex); !errors.Is(err, workflow.ErrStepFailed) {
			t.Fatalf("exhaust await: got %v, want ErrStepFailed", err)
		}
		if _, err := workflow.Await[int](pctx, h.wf, "e2e-fail", down); !errors.Is(err, workflow.ErrStepSkipped) {
			t.Fatalf("down await: got %v, want ErrStepSkipped", err)
		}
		waitFor(t, 30*time.Second, "fail DAG finalized", func() bool {
			return h.dagStatus(ctx, "e2e-fail") == workflow.DAGStatusFailed
		})
		if n := exhaustAttempts.Load(); n < 3 {
			t.Errorf("exhaust attempts: got %d, want >= 3", n)
		}
		rec, _, err := h.wf.Store.GetStep(pctx, "e2e-fail", "exhaust")
		if err != nil {
			t.Fatal(err)
		}
		if rec.ErrorKind != task.ErrKindHandler || !strings.Contains(rec.ErrorMessage, "always fails") {
			t.Errorf("exhaust record: kind=%q msg=%q, want handler/always fails", rec.ErrorKind, rec.ErrorMessage)
		}

		// Cancel mid-flight: the running root finishes, the pending dependent is canceled.
		cdag := workflow.New(workflow.WithDAGID("e2e-cancel"))
		root := cdag.Step("root", eGateCancel, 1)
		dep := cdag.StepOpts("dep", eWhere, []workflow.StepOption{workflow.After(root)})
		if err := cdag.Submit(pctx, h.wf); err != nil {
			t.Fatal(err)
		}
		select {
		case <-gateCancel.Reached():
		case <-time.After(30 * time.Second):
			t.Fatal("cancel-DAG root never started")
		}
		if err := workflow.Cancel(pctx, h.wf, "e2e-cancel"); err != nil {
			t.Fatal(err)
		}
		if st := h.dagStatus(pctx, "e2e-cancel"); st != workflow.DAGStatusCanceled {
			t.Fatalf("after Cancel: got %s, want canceled", st)
		}
		if _, err := workflow.Await[string](pctx, h.wf, "e2e-cancel", dep); !errors.Is(err, workflow.ErrStepCanceled) {
			t.Fatalf("dep await: got %v, want ErrStepCanceled", err)
		}
		gateCancel.Release()
		if got, err := workflow.Await[int](pctx, h.wf, "e2e-cancel", root); err != nil || got != 1 {
			t.Fatalf("canceled-DAG root: got %d, %v; want 1 (running steps finish)", got, err)
		}

		// Delete removes every record.
		ddag := workflow.New(workflow.WithDAGID("e2e-delete"))
		one := ddag.Step("one", eAdd, 1, 1)
		if err := ddag.Submit(pctx, h.wf); err != nil {
			t.Fatal(err)
		}
		if got, err := workflow.Await[int](pctx, h.wf, "e2e-delete", one); err != nil || got != 2 {
			t.Fatalf("delete-DAG step: got %d, %v; want 2", got, err)
		}
		waitFor(t, 30*time.Second, "delete DAG finalized", func() bool {
			return h.dagStatus(ctx, "e2e-delete") == workflow.DAGStatusDone
		})
		if err := workflow.DeleteDAG(pctx, h.wf, "e2e-delete"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.wf.Store.GetMeta(pctx, "e2e-delete"); !errors.Is(err, workflow.ErrDAGNotFound) {
			t.Fatalf("after DeleteDAG: got %v, want ErrDAGNotFound", err)
		}
	})

	phase("07_FinalVerification", func(t *testing.T) {
		pctx, pcancel := context.WithTimeout(ctx, 120*time.Second)
		defer pcancel()

		// The long DAG finalizes done: opt is the only failure and it's optional.
		waitFor(t, 60*time.Second, "long DAG done", func() bool {
			return h.dagStatus(ctx, longDAG) == workflow.DAGStatusDone
		})
		dbg, err := workflow.Debug(pctx, h.wf, longDAG)
		if err != nil {
			t.Fatal(err)
		}
		total := len(dbg.Steps)
		if total != 34 { // 33 static + 1 dynamic
			t.Errorf("long DAG steps: got %d, want 34", total)
		}
		if dbg.Counts[workflow.StatusDone] != total-1 || dbg.Counts[workflow.StatusFailed] != 1 {
			t.Errorf("long DAG counts: %v, want %d done + 1 failed", dbg.Counts, total-1)
		}
		if len(dbg.Blockers) != 0 {
			t.Errorf("long DAG blockers: %v, want none", dbg.Blockers)
		}

		// Data-flow spot checks beyond the fan-in.
		if got, err := workflow.AwaitByID[int](pctx, h.wf, longDAG, "dflt"); err != nil || got != 14 {
			t.Errorf("dflt: got %d, %v; want 14 (OrDefault substitution)", got, err)
		}
		if got, err := workflow.AwaitByID[string](pctx, h.wf, longDAG, "any"); err != nil || got == "" {
			t.Errorf("any: got %q, %v; want a worker ID (AfterAny survives upstream failure)", got, err)
		}

		// DLQ audit: at least one entry per induced failure with the right kind.
		// Counts are lower bounds — DLQ publishes are not deduped, so injection
		// windows can legitimately duplicate entries.
		counts := h.dlqEntries(t, pctx)
		for _, want := range []string{
			"e2e.eAdd/" + task.ErrKindDeadline,
			"e2e.eNonRetryable/validation",
			"e2e.ePanics/" + task.ErrKindPanic,
			"e2e.eFailOptional/" + task.ErrKindHandler,
			"e2e.eRetryExhaust/" + task.ErrKindHandler,
		} {
			if counts[want] < 1 {
				t.Errorf("DLQ missing entry %s (have %v)", want, counts)
			}
		}

		// The claim owners did targeted work; the pool dispatched everything.
		if n := h.workers[0].metrics.observed.Load(); n == 0 {
			t.Error("w1 observed no dispatches")
		}
		if n := h.workers[1].metrics.observed.Load(); n == 0 {
			t.Error("w2 observed no dispatches")
		}
		var totalObs int64
		for _, p := range h.workers {
			totalObs += p.metrics.observed.Load()
		}
		if totalObs < 40 {
			t.Errorf("total dispatches observed: %d, want >= 40", totalObs)
		}

		// WorkQueue semantics: every produced task was consumed and acked.
		waitFor(t, 60*time.Second, "TASKS stream drained", func() bool {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel2()
			s, err := h.js.Stream(ctx2, "EBIND_TASKS")
			if err != nil {
				return false
			}
			info, err := s.Info(ctx2)
			return err == nil && info.State.Msgs == 0
		})

		// The cluster ends healthy: all 3 nodes running after two kill/restart cycles.
		for i, n := range h.cluster.Nodes {
			if !n.Server().Running() {
				t.Errorf("node %d not running at end of test", i)
			}
		}
	})
}

// assertTaskErrKind asserts err is a *task.TaskError with the given kind.
func assertTaskErrKind(t *testing.T, err error, kind string) {
	t.Helper()
	var te *task.TaskError
	if !errors.As(err, &te) {
		t.Fatalf("got %v (%T), want *task.TaskError", err, err)
	}
	if te.Kind != kind {
		t.Fatalf("task error kind: got %q, want %q", te.Kind, kind)
	}
}
