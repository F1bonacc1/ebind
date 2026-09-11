//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/workflow"
)

// TestStoreListsCompleteUnderReplicaLag targets the window that stranded a
// pending step in nightly run 34570965794: Cancel listed the DAG's steps and
// silently missed one that a lagging replica had not applied yet. The same
// relaxed read backed ListSignals and ListDAGs.
//
// Each iteration creates a DAG's meta, steps and a signal through
// quorum-committed writes, then at once lists them through a client pinned to
// each node, whose local replica serves the direct gets. Background writes
// widen follower lag, and a mid-run follower restart reopens the widest window
// (a rejoined replica back in the direct-get pool but behind). A list may fail
// during failover; it must never come back incomplete with a nil error.
func TestStoreListsCompleteUnderReplicaLag(t *testing.T) {
	c := startClusterWithRetry(t)
	t.Cleanup(c.Shutdown)

	var urls []string
	for _, n := range c.Nodes {
		urls = append(urls, n.ClientURL())
	}
	openStore := func(desc, url string) *workflow.NatsStore {
		nc, err := nats.Connect(url, nats.ReconnectWait(100*time.Millisecond), nats.MaxReconnects(-1))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		var s *workflow.NatsStore
		setupRetry(t, desc, func(ctx context.Context) error {
			js, err := jetstream.New(nc)
			if err != nil {
				return err
			}
			s, err = workflow.NewNatsStore(ctx, js, replicas)
			return err
		})
		return s
	}
	writer := openStore("writer store", strings.Join(urls, ","))
	readers := make([]*workflow.NatsStore, len(urls))
	for i, u := range urls {
		readers[i] = openStore(fmt.Sprintf("reader store on node %d", i), u)
	}

	loadCtx, stopLoad := context.WithCancel(context.Background())
	var load sync.WaitGroup
	for g := range 4 {
		load.Add(1)
		go func() {
			defer load.Done()
			payload := []byte(strings.Repeat("x", 512))
			for n := 0; loadCtx.Err() == nil; n++ {
				_ = writer.PutResult(loadCtx, "load", fmt.Sprintf("g%d-%d", g, n%64), payload)
			}
		}()
	}
	defer func() { stopLoad(); load.Wait() }()

	const iterations, stepsPerDAG, victim, dagsEvery = 300, 8, 1, 25
	incomplete, errored := map[string]int{}, map[string]int{}
	var lists, failed, reported int
	firstErr, lastErr := -1, -1
	check := func(i, r int, kind string, got, want int, err error) {
		lists++
		switch {
		case err != nil:
			failed++
			errored[kind]++
			if firstErr < 0 {
				firstErr = i
			}
			lastErr = i
			if failed <= 5 {
				t.Logf("iteration %d, reader on node %d: %s failed: %v", i, r, kind, err)
			}
		case got != want:
			incomplete[kind]++
			if reported++; reported <= 10 {
				t.Errorf("iteration %d, reader on node %d: %s listed %d of %d with a nil error", i, r, kind, got, want)
			}
		}
	}
	for i := range iterations {
		if i == iterations/2 {
			c.ShutdownNode(victim)
			if err := c.RestartNode(victim); err != nil {
				t.Fatal(err)
			}
			if err := c.WaitNodeHealthy(victim, 60*time.Second); err != nil {
				t.Fatal(err)
			}
		}
		dagID := fmt.Sprintf("lag-%d", i)
		createWithRetry(t, dagID+" meta", func(ctx context.Context) error {
			return writer.PutMeta(ctx, dagID, workflow.DAGMeta{ID: dagID, Status: workflow.DAGStatusRunning}, 0)
		})
		for k := range stepsPerDAG {
			stepID := fmt.Sprintf("s%d", k)
			createWithRetry(t, dagID+"/"+stepID, func(ctx context.Context) error {
				_, err := writer.PutStep(ctx, dagID, stepID,
					workflow.StepRecord{DAGID: dagID, StepID: stepID, Status: workflow.StatusPending}, 0)
				return err
			})
		}
		createWithRetry(t, dagID+" signal", func(ctx context.Context) error {
			return writer.PutSignal(ctx, dagID, workflow.SignalRecord{DAGID: dagID, Name: "go", DeliveredAt: time.Now().UTC()})
		})
		for r, rs := range readers {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			steps, err := rs.ListSteps(ctx, dagID)
			check(i, r, "ListSteps", len(steps), stepsPerDAG, err)
			sigs, err := rs.ListSignals(ctx, dagID)
			check(i, r, "ListSignals", len(sigs), 1, err)
			if i%dagsEvery == 0 {
				metas, err := rs.ListDAGs(ctx)
				check(i, r, "ListDAGs", countLagDAGs(metas), i+1, err)
			}
			cancel()
		}
	}
	t.Logf("%d lists (restart at iteration %d); failed with an error: %v, in iterations %d..%d; incomplete with a nil error: %v",
		lists, iterations/2, errored, firstErr, lastErr, incomplete)
	// Errors are allowed around the restart, but a store whose lists mostly
	// fail would pass the completeness check vacuously.
	if failed > lists/10 {
		t.Errorf("%d of %d lists failed with an error", failed, lists)
	}
}

func countLagDAGs(metas []workflow.DAGMeta) int {
	n := 0
	for _, m := range metas {
		if strings.HasPrefix(m.ID, "lag-") {
			n++
		}
	}
	return n
}

// createWithRetry runs a create until it lands, retrying through failover
// windows. ErrStaleRevision means the key already exists: an earlier attempt
// landed even though its reply was lost.
func createWithRetry(t *testing.T, desc string, create func(ctx context.Context) error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := create(ctx)
		cancel()
		if err == nil || errors.Is(err, workflow.ErrStaleRevision) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("create %s: %v", desc, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
