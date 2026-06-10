package dag

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newPauseCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "pause <dag-id>",
		Short: "Pause a running DAG (in-flight steps finish, pending stay pending)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			if err := workflow.Pause(ctx, wf, args[0]); err != nil {
				return err
			}
			// Report the actual resulting state: pausing (draining in-flight
			// steps) or paused (no in-flight work).
			status := workflow.DAGStatusPaused
			if meta, _, err := wf.Store.GetMeta(ctx, args[0]); err == nil {
				status = meta.Status
			}
			return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("%s %s", status, args[0]))
		},
	}
}
