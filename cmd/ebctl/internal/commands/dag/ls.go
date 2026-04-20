package dag

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	"github.com/f1bonacc1/ebind/workflow"
)

func newLsCmd(c *cli.Context) *cobra.Command {
	var statusFilter string
	var since time.Duration
	var limit int

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List DAGs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			metas, err := wf.Store.ListDAGs(ctx)
			if err != nil {
				return fmt.Errorf("list dags: %w", err)
			}

			var filtered []workflow.DAGMeta
			var cutoff time.Time
			if since > 0 {
				cutoff = time.Now().Add(-since)
			}
			for _, m := range metas {
				if statusFilter != "" && !strings.EqualFold(string(m.Status), statusFilter) {
					continue
				}
				if !cutoff.IsZero() && m.CreatedAt.Before(cutoff) {
					continue
				}
				filtered = append(filtered, m)
			}
			sort.Slice(filtered, func(i, j int) bool {
				return filtered[i].CreatedAt.After(filtered[j].CreatedAt)
			})
			if limit > 0 && len(filtered) > limit {
				filtered = filtered[:limit]
			}

			if c.Printer.Name() == "json" {
				type row struct {
					ID        string            `json:"id"`
					Status    workflow.DAGStatus `json:"status"`
					CreatedAt time.Time         `json:"created_at"`
				}
				rows := make([]row, 0, len(filtered))
				for _, m := range filtered {
					rows = append(rows, row{m.ID, m.Status, m.CreatedAt})
				}
				return c.Printer.Value(cmd.OutOrStdout(), rows)
			}

			// Pretty: include step counts. One ListSteps call per DAG; acceptable
			// for ls (small N typical). Users who want raw, fast listing pass
			// `-o json`, which skips the extra round trips.
			headers := []string{"ID", "STATUS", "STEPS", "AGE", "CREATED"}
			rows := make([][]string, 0, len(filtered))
			for _, m := range filtered {
				steps, err := wf.Store.ListSteps(ctx, m.ID)
				stepSum := "?"
				if err == nil {
					stepSum = stepsSummary(steps)
				}
				rows = append(rows, []string{
					m.ID,
					string(m.Status),
					stepSum,
					format.Age(m.CreatedAt),
					m.CreatedAt.UTC().Format(time.RFC3339),
				})
			}
			return c.Printer.Table(cmd.OutOrStdout(), headers, rows)
		},
	}
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status: running|done|failed|canceled")
	cmd.Flags().DurationVar(&since, "since", 0, "only DAGs created within this duration (e.g. 1h)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	return cmd
}

func stepsSummary(steps []workflow.StepRecord) string {
	var done, failed, pending, running, canceled, skipped int
	for _, s := range steps {
		switch s.Status {
		case workflow.StatusDone:
			done++
		case workflow.StatusFailed:
			failed++
		case workflow.StatusPending:
			pending++
		case workflow.StatusRunning:
			running++
		case workflow.StatusCanceled:
			canceled++
		case workflow.StatusSkipped:
			skipped++
		}
	}
	return fmt.Sprintf("%d (✓%d ✗%d ▶%d ⋯%d ⊘%d ■%d)",
		len(steps), done, failed, running, pending, skipped, canceled)
}
