package dlq

import (
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/stream"
)

func newPurgeCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "purge",
		Short: "Delete all DLQ entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, stream.DLQStream)
			if err != nil {
				return err
			}
			if err := c.Confirm("Purge the entire DLQ?"); err != nil {
				return err
			}
			if err := s.Purge(ctx); err != nil {
				return err
			}
			return c.Printer.Text(cmd.OutOrStdout(), "DLQ purged")
		},
	}
}
