package workflow

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// TestArm_RaceWithCompletion checks the ArmBreakpoints argument: an upstream
// completes (hook write + scheduler event) concurrently with a runtime arm
// on its dependent. Whatever the interleaving, the dependent may reach
// running only if the arm reported that instance Missed; otherwise it must
// be reported Stopped and be blocked. Half the runs give the scheduler a
// direct-get meta frozen before the arm (a lagging replica), so only the
// leader read after the step list, or a fence token, can stop the step.
func TestArm_RaceWithCompletion(t *testing.T) {
	n := 500
	if testing.Short() {
		n = 50
	}
	for i := 0; i < n; i++ {
		raceOnce(t, i)
		if t.Failed() {
			return
		}
	}
}

func raceOnce(t *testing.T, i int) {
	ctx := context.Background()
	store := &staleMetaStore{StateStore: NewMemStore()}
	enq := &captureEnq{}
	wf := NewWorkflow(store, NewMemBus(), enq)
	dag := New()
	u := dag.Step("u", noopA, 1)
	_ = dag.Step("d", noopA, u.Ref())
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	stale := i%2 == 0
	if stale {
		store.freeze(ctx, dag.ID())
	}
	m, inst := BeforeStep("d"), "d@before"
	if i%4 >= 2 {
		m, inst = AfterStep("u"), "u@after"
	}

	s := &Scheduler{wf: wf}
	var wg sync.WaitGroup
	start := make(chan struct{})
	var rep ArmReport
	var armErr, schedErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		rec, rev, err := store.GetStep(ctx, dag.ID(), "u")
		if err != nil {
			schedErr = err
			return
		}
		rec.Status = StatusDone
		if _, err := store.PutStep(ctx, dag.ID(), "u", rec, rev); err != nil {
			schedErr = err
			return
		}
		_ = store.PutResult(ctx, dag.ID(), "u", []byte(`2`))
		s.mu.Lock()
		schedErr = s.handleEvent(ctx, Event{Kind: EventCompleted, DAGID: dag.ID(), StepID: "u", Status: StatusDone})
		s.mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		<-start
		rep, armErr = ArmBreakpoints(ctx, wf, dag.ID(), m)
	}()
	close(start)
	wg.Wait()
	if armErr != nil || schedErr != nil {
		t.Fatalf("run %d: arm err %v, scheduler err %v", i, armErr, schedErr)
	}

	d, _, err := store.GetStep(ctx, dag.ID(), "d")
	if err != nil {
		t.Fatal(err)
	}
	running := d.Status == StatusRunning
	missed := slices.Contains(rep.Missed, inst)
	stopped := slices.Contains(rep.Stopped, inst)
	switch {
	case running && !missed:
		t.Fatalf("run %d (stale=%v %s): d started but the arm reported %+v", i, stale, inst, rep)
	case !running && (missed || !stopped):
		t.Fatalf("run %d (stale=%v %s): d held but the arm reported %+v", i, stale, inst, rep)
	case !running:
		blocked, err := ListBreakpoints(ctx, wf, dag.ID(), OnlyBlocked())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(blocked, func(bp BreakpointInfo) bool {
			return bp.StepID+"@"+string(bp.Position) == inst
		}) {
			t.Fatalf("run %d (stale=%v %s): d held but %s not blocked: %+v", i, stale, inst, inst, blocked)
		}
	}
	if running != (enq.count() == 2) {
		t.Fatalf("run %d: d running=%v but %d enqueues", i, running, enq.count())
	}
}
