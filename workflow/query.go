package workflow

import (
	"context"
	"sort"
)

// MatchesLabels reports whether m carries every label in want (AND semantics).
// An empty want matches any DAG. Used by ListDAGsByLabels and `ebctl dag ls
// --label` so both share one matching definition.
func MatchesLabels(m DAGMeta, want []string) bool {
	if len(want) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(m.Labels))
	for _, l := range m.Labels {
		have[l] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			return false
		}
	}
	return true
}

// ListDAGsByLabels returns every DAG whose labels contain all of the given
// labels (AND semantics), newest-first by CreatedAt. With no labels it returns
// all DAGs, equivalent to a sorted Store.ListDAGs.
//
// This is a client-side filter over the full DAG history — the same scan
// `ebctl dag ls` performs — so it carries no extra index and needs no cleanup.
func ListDAGsByLabels(ctx context.Context, wf *Workflow, labels ...string) ([]DAGMeta, error) {
	metas, err := wf.Store.ListDAGs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DAGMeta, 0, len(metas))
	for _, m := range metas {
		if MatchesLabels(m, labels) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}
