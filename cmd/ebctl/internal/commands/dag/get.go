package dag

import (
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newGetCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "get <dag-id>",
		Short: "Show a DAG's full state (meta, steps, durations, blockers)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			dbg, err := workflow.Debug(ctx, wf, args[0])
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), dbg)
			}
			return workflow.WriteDebug(cmd.OutOrStdout(), dbg)
		},
	}
}
