package dag

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newTreeCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "tree <dag-id>",
		Short: "Render the DAG as a dependency tree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			meta, _, err := wf.Store.GetMeta(ctx, args[0])
			if err != nil {
				return err
			}
			steps, err := wf.Store.ListSteps(ctx, args[0])
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				type node struct {
					ID       string             `json:"id"`
					Status   workflow.StepStatus `json:"status"`
					Deps     []string           `json:"deps,omitempty"`
					Optional []string           `json:"optional_deps,omitempty"`
				}
				nodes := make([]node, 0, len(steps))
				for _, s := range steps {
					nodes = append(nodes, node{s.StepID, s.Status, s.Deps, s.OptionalDeps})
				}
				return c.Printer.Value(cmd.OutOrStdout(), map[string]any{
					"dag": meta, "steps": nodes,
				})
			}
			return renderTree(cmd.OutOrStdout(), meta, steps)
		},
	}
}

func renderTree(w io.Writer, meta workflow.DAGMeta, steps []workflow.StepRecord) error {
	byID := make(map[string]workflow.StepRecord, len(steps))
	children := make(map[string][]string, len(steps))
	hasParent := make(map[string]bool, len(steps))
	for _, s := range steps {
		byID[s.StepID] = s
	}
	for _, s := range steps {
		for _, dep := range s.Deps {
			children[dep] = append(children[dep], s.StepID)
			hasParent[s.StepID] = true
		}
		for _, dep := range s.OptionalDeps {
			children[dep] = append(children[dep], s.StepID)
			hasParent[s.StepID] = true
		}
	}
	for _, list := range children {
		sort.Strings(list)
	}

	var roots []string
	for id := range byID {
		if !hasParent[id] {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)

	fmt.Fprintf(w, "DAG %s [%s]\n", meta.ID, meta.Status)
	seen := map[string]bool{}
	for _, r := range roots {
		writeNode(w, r, byID, children, seen, "", true)
	}
	// Surface any nodes missed by cycles / orphans.
	for _, s := range steps {
		if !seen[s.StepID] {
			writeNode(w, s.StepID, byID, children, seen, "", true)
		}
	}
	return nil
}

func writeNode(w io.Writer, id string, byID map[string]workflow.StepRecord, children map[string][]string, seen map[string]bool, prefix string, last bool) {
	if seen[id] {
		return
	}
	seen[id] = true
	connector := "├─"
	if last {
		connector = "└─"
	}
	rec := byID[id]
	fmt.Fprintf(w, "%s%s %s %s [%s]\n", prefix, connector, glyph(rec.Status), id, rec.Status)
	childPrefix := prefix
	if last {
		childPrefix += "   "
	} else {
		childPrefix += "│  "
	}
	kids := children[id]
	for i, k := range kids {
		writeNode(w, k, byID, children, seen, childPrefix, i == len(kids)-1)
	}
}

func glyph(s workflow.StepStatus) string {
	switch s {
	case workflow.StatusDone:
		return "✓"
	case workflow.StatusFailed:
		return "✗"
	case workflow.StatusSkipped:
		return "⊘"
	case workflow.StatusCanceled:
		return "■"
	case workflow.StatusRunning:
		return "▶"
	case workflow.StatusPending:
		return "⋯"
	}
	return "?"
}
