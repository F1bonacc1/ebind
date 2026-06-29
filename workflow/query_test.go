package workflow

import (
	"context"
	"testing"
	"time"
)

func TestMatchesLabels(t *testing.T) {
	cases := []struct {
		name string
		have []string
		want []string
		ok   bool
	}{
		{"empty want matches all", []string{"a", "b"}, nil, true},
		{"empty want matches labelless", nil, nil, true},
		{"single match", []string{"billing"}, []string{"billing"}, true},
		{"single miss", []string{"billing"}, []string{"reports"}, false},
		{"subset of have (AND)", []string{"billing", "nightly", "x"}, []string{"billing", "nightly"}, true},
		{"want superset of have", []string{"billing"}, []string{"billing", "nightly"}, false},
		{"disjoint", []string{"a"}, []string{"b"}, false},
		{"labelless meta, non-empty want", nil, []string{"billing"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesLabels(DAGMeta{Labels: tc.have}, tc.want); got != tc.ok {
				t.Errorf("MatchesLabels(have=%v, want=%v) = %v, want %v", tc.have, tc.want, got, tc.ok)
			}
		})
	}
}

func TestListDAGsByLabels(t *testing.T) {
	ctx := context.Background()
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})

	base := time.Now().UTC()
	seed := []DAGMeta{
		{ID: "d1", Status: DAGStatusDone, CreatedAt: base.Add(1 * time.Minute), Labels: []string{"billing", "nightly"}},
		{ID: "d2", Status: DAGStatusRunning, CreatedAt: base.Add(2 * time.Minute), Labels: []string{"billing"}},
		{ID: "d3", Status: DAGStatusDone, CreatedAt: base.Add(3 * time.Minute), Labels: []string{"reports"}},
		{ID: "d4", Status: DAGStatusDone, CreatedAt: base.Add(4 * time.Minute)}, // no labels
	}
	for _, m := range seed {
		if err := wf.Store.PutMeta(ctx, m.ID, m, 0); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}

	ids := func(metas []DAGMeta) []string {
		out := make([]string, len(metas))
		for i, m := range metas {
			out[i] = m.ID
		}
		return out
	}
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("single label matches all carriers", func(t *testing.T) {
		got, err := ListDAGsByLabels(ctx, wf, "billing")
		if err != nil {
			t.Fatal(err)
		}
		// newest-first: d2 (t+2) before d1 (t+1)
		eq(t, ids(got), []string{"d2", "d1"})
	})

	t.Run("multi-label AND", func(t *testing.T) {
		got, err := ListDAGsByLabels(ctx, wf, "billing", "nightly")
		if err != nil {
			t.Fatal(err)
		}
		eq(t, ids(got), []string{"d1"})
	})

	t.Run("no match returns empty", func(t *testing.T) {
		got, err := ListDAGsByLabels(ctx, wf, "does-not-exist")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", ids(got))
		}
	})

	t.Run("no labels returns all, newest-first", func(t *testing.T) {
		got, err := ListDAGsByLabels(ctx, wf)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, ids(got), []string{"d4", "d3", "d2", "d1"})
	})
}
