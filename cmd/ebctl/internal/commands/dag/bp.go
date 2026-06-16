package dag

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	"github.com/f1bonacc1/ebind/workflow"
)

func newBPCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bp",
		Short: "Inspect and resume step breakpoints",
	}
	cmd.AddCommand(newBPLsCmd(c))
	cmd.AddCommand(newBPResumeCmd(c))
	return cmd
}

func newBPLsCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "ls <dag-id>",
		Short: "List a DAG's breakpoints and their state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			infos, err := workflow.ListBreakpoints(ctx, wf, args[0])
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), infos)
			}
			if len(infos) == 0 {
				return c.Printer.Text(cmd.OutOrStdout(), "no breakpoints declared")
			}
			headers, rows := bpTable(infos)
			return c.Printer.Table(cmd.OutOrStdout(), headers, rows)
		},
	}
}

// bpTable maps breakpoint infos to the `dag bp ls` pretty table.
func bpTable(infos []workflow.BreakpointInfo) ([]string, [][]string) {
	headers := []string{"STEP", "POS", "LABELS", "ARMED", "STATE", "SINCE", "HOLDING"}
	rows := make([][]string, 0, len(infos))
	for _, bp := range infos {
		armed := "no"
		if bp.Armed {
			armed = "yes"
		}
		state := string(bp.State)
		if state == "" {
			state = "-"
		}
		since := "-"
		if bp.State == workflow.BPStateBlocked && !bp.BlockedSince.IsZero() {
			since = format.Age(bp.BlockedSince)
		}
		holding := "-"
		if len(bp.Holding) > 0 {
			holding = strings.Join(bp.Holding, ",")
		}
		rows = append(rows, []string{
			bp.StepID,
			string(bp.Position),
			strings.Join(bp.Labels, ","),
			armed,
			state,
			since,
			holding,
		})
	}
	return headers, rows
}

// bpStateSuffix renders a step record's persisted breakpoint state for the
// pretty `step get` view, e.g. "  [blocked 2m ago]" or "  [released]".
func bpStateSuffix(state workflow.BPState, blockedAt time.Time) string {
	switch state {
	case workflow.BPStateBlocked:
		if !blockedAt.IsZero() {
			return fmt.Sprintf("  [blocked %s ago]", format.Age(blockedAt))
		}
		return "  [blocked]"
	case workflow.BPStateReleased:
		return "  [released]"
	}
	return ""
}

func newBPResumeCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <dag-id> <label>",
		Short: "Release steps blocked at a breakpoint with this label (label stays armed)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			n, err := workflow.ResumeBreakpoint(ctx, wf, args[0], args[1])
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), map[string]any{
					"dag_id": args[0], "label": args[1], "released": n,
				})
			}
			return c.Printer.Text(cmd.OutOrStdout(),
				fmt.Sprintf("released %d breakpoint(s) for label %q in %s", n, args[1], args[0]))
		},
	}
}
