package dag

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	"github.com/f1bonacc1/ebind/workflow"
)

func newSignalCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "signal <dag-id> <name> [json-payload]",
		Short: "Deliver an external signal to a DAG (one-shot, first-wins)",
		Long: `Deliver an external signal to a DAG. Steps gated on the name
(workflow.WaitForSignal / workflow.SignalRef) become eligible to run; a
SignalRef arg receives the payload. Delivery is one-shot and buffered: the
first payload wins, a repeat send is an idempotent no-op, and a signal sent
before any step waits on it is kept for the DAG's lifetime.`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			var payload any
			if len(args) == 3 {
				if !json.Valid([]byte(args[2])) {
					return fmt.Errorf("payload is not valid JSON: %q", args[2])
				}
				payload = json.RawMessage(args[2])
			}
			delivered, err := workflow.Signal(ctx, wf, args[0], args[1], payload)
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), map[string]any{
					"dag_id": args[0], "name": args[1], "delivered": delivered,
				})
			}
			if delivered {
				return c.Printer.Text(cmd.OutOrStdout(),
					fmt.Sprintf("delivered signal %q to %s", args[1], args[0]))
			}
			// (false, nil) is either a duplicate or a terminal DAG — read the
			// meta to tell the operator which.
			meta, _, metaErr := wf.Store.GetMeta(ctx, args[0])
			if metaErr == nil {
				switch meta.Status {
				case workflow.DAGStatusDone, workflow.DAGStatusFailed, workflow.DAGStatusCanceled:
					return c.Printer.Text(cmd.OutOrStdout(),
						fmt.Sprintf("DAG is %s; signal %q not delivered", meta.Status, args[1]))
				}
			}
			return c.Printer.Text(cmd.OutOrStdout(),
				fmt.Sprintf("signal %q already delivered to %s (first payload kept)", args[1], args[0]))
		},
	}
	cmd.AddCommand(newSignalLsCmd(c))
	return cmd
}

func newSignalLsCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "ls <dag-id>",
		Short: "List a DAG's signals: delivered and still-awaited",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			infos, err := workflow.ListSignalInfo(ctx, wf, args[0])
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), infos)
			}
			if len(infos) == 0 {
				return c.Printer.Text(cmd.OutOrStdout(), "no signals referenced or delivered")
			}
			headers, rows := signalTable(infos)
			return c.Printer.Table(cmd.OutOrStdout(), headers, rows)
		},
	}
}

// signalTable maps signal infos to the `dag signal ls` pretty table.
func signalTable(infos []workflow.SignalInfo) ([]string, [][]string) {
	headers := []string{"NAME", "DELIVERED", "AT", "WAITING", "PAYLOAD"}
	rows := make([][]string, 0, len(infos))
	for _, si := range infos {
		delivered := "no"
		at := "-"
		if si.Delivered {
			delivered = "yes"
			if !si.DeliveredAt.IsZero() {
				at = format.Age(si.DeliveredAt) + " ago"
			}
		}
		waiting := "-"
		if len(si.Waiting) > 0 {
			waiting = strings.Join(si.Waiting, ",")
		}
		rows = append(rows, []string{si.Name, delivered, at, waiting, truncatePayload(si.Payload, 40)})
	}
	return headers, rows
}

func truncatePayload(p []byte, max int) string {
	if len(p) == 0 {
		return "-"
	}
	r := []rune(string(p))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max-1]) + "…"
}
