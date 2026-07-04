package dag

import (
	"context"
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
	var bpBlockedOnly bool
	var sigWaitingOnly bool
	var labelFilter []string

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
			// blockedCounts / waitingCounts are filled only when the respective
			// filter needs them; the pretty path reuses them to avoid second
			// round trips.
			blockedCounts := map[string]int{}
			waitingCounts := map[string]int{}
			for _, m := range metas {
				if statusFilter != "" && !strings.EqualFold(string(m.Status), statusFilter) {
					continue
				}
				if !cutoff.IsZero() && m.CreatedAt.Before(cutoff) {
					continue
				}
				if len(labelFilter) > 0 && !workflow.MatchesLabels(m, labelFilter) {
					continue
				}
				if bpBlockedOnly || sigWaitingOnly {
					steps, err := wf.Store.ListSteps(ctx, m.ID)
					if err != nil {
						continue
					}
					if bpBlockedOnly {
						n := blockedBPCount(m, steps)
						if n == 0 {
							continue
						}
						blockedCounts[m.ID] = n
					}
					if sigWaitingOnly {
						n := sigWaitingCount(ctx, wf, m, steps)
						if n == 0 {
							continue
						}
						waitingCounts[m.ID] = n
					}
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
					ID        string             `json:"id"`
					Status    workflow.DAGStatus `json:"status"`
					CreatedAt time.Time          `json:"created_at"`
					Labels    []string           `json:"labels,omitempty"`
				}
				rows := make([]row, 0, len(filtered))
				for _, m := range filtered {
					rows = append(rows, row{m.ID, m.Status, m.CreatedAt, m.Labels})
				}
				return c.Printer.Value(cmd.OutOrStdout(), rows)
			}

			// Pretty: include step counts. One ListSteps call per DAG; acceptable
			// for ls (small N typical). Users who want raw, fast listing pass
			// `-o json`, which skips the extra round trips.
			headers := []string{"ID", "STATUS", "STEPS", "LABELS", "AGE", "CREATED"}
			rows := make([][]string, 0, len(filtered))
			for _, m := range filtered {
				steps, err := wf.Store.ListSteps(ctx, m.ID)
				stepSum := "?"
				if err == nil {
					stepSum = stepsSummary(steps)
					n, counted := blockedCounts[m.ID]
					if !counted {
						n = blockedBPCount(m, steps)
					}
					if n > 0 {
						stepSum += fmt.Sprintf(" ⦿%d", n)
					}
					w, counted := waitingCounts[m.ID]
					if !counted {
						w = sigWaitingCount(ctx, wf, m, steps)
					}
					if w > 0 {
						stepSum += fmt.Sprintf(" ⚑%d", w)
					}
				}
				labels := "-"
				if len(m.Labels) > 0 {
					labels = strings.Join(m.Labels, ",")
				}
				rows = append(rows, []string{
					m.ID,
					string(m.Status),
					stepSum,
					labels,
					format.Age(m.CreatedAt),
					m.CreatedAt.UTC().Format(time.RFC3339),
				})
			}
			return c.Printer.Table(cmd.OutOrStdout(), headers, rows)
		},
	}
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status: running|done|failed|canceled|pausing|paused")
	cmd.Flags().DurationVar(&since, "since", 0, "only DAGs created within this duration (e.g. 1h)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = unlimited)")
	cmd.Flags().BoolVar(&bpBlockedOnly, "bp-blocked", false, "only DAGs with at least one step blocked at a breakpoint")
	cmd.Flags().BoolVar(&sigWaitingOnly, "sig-waiting", false, "only DAGs with at least one step waiting on an undelivered signal")
	cmd.Flags().StringArrayVar(&labelFilter, "label", nil, "only DAGs carrying all of these labels (repeatable)")
	return cmd
}

// blockedBPCount counts breakpoints currently stopping work in a DAG.
func blockedBPCount(meta workflow.DAGMeta, steps []workflow.StepRecord) int {
	return workflow.CountBlocked(workflow.ComputeBreakpoints(meta, steps))
}

// sigWaitingCount counts steps currently waiting on undelivered signals.
// Skips the ListSignals round trip when no pending step declares wait-signals
// — the common case for DAGs without a signal flow.
func sigWaitingCount(ctx context.Context, wf *workflow.Workflow, meta workflow.DAGMeta, steps []workflow.StepRecord) int {
	declared := false
	for _, s := range steps {
		if s.Status == workflow.StatusPending && len(s.WaitSignals) > 0 {
			declared = true
			break
		}
	}
	if !declared {
		return 0
	}
	sigs, err := wf.Store.ListSignals(ctx, meta.ID)
	if err != nil {
		return 0
	}
	return workflow.CountWaitingSteps(workflow.ComputeSignals(meta, steps, sigs))
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
