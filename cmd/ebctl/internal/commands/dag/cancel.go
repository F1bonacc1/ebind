package dag

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newCancelCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <dag-id>",
		Short: "Cancel a running DAG (pending steps marked canceled)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			if err := c.Confirm(fmt.Sprintf("Cancel DAG %s?", args[0])); err != nil {
				return err
			}
			if err := workflow.Cancel(ctx, wf, args[0]); err != nil {
				return err
			}
			return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("canceled %s", args[0]))
		},
	}
}
