package dag

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newRmCmd(c *cli.Context) *cobra.Command {
	var finishedOnly bool
	cmd := &cobra.Command{
		Use:   "rm <dag-id>",
		Short: "Delete all KV records for a DAG",
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
			if finishedOnly {
				switch meta.Status {
				case workflow.DAGStatusDone, workflow.DAGStatusFailed, workflow.DAGStatusCanceled:
				default:
					return fmt.Errorf("refusing to rm %s (status=%s); pass without --finished-only to force", args[0], meta.Status)
				}
			}
			if err := c.Confirm(fmt.Sprintf("Delete DAG %s (status=%s) and all its steps/results?", args[0], meta.Status)); err != nil {
				return err
			}
			if err := workflow.DeleteDAG(ctx, wf, args[0]); err != nil {
				return err
			}
			return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("deleted %s", args[0]))
		},
	}
	cmd.Flags().BoolVar(&finishedOnly, "finished-only", false, "refuse unless status is done|failed|canceled")
	return cmd
}
