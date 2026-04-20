package stream

import (
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func newInfoCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "info <stream>",
		Short: "Show full stream config + state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, args[0])
			if err != nil {
				return err
			}
			info, err := s.Info(ctx)
			if err != nil {
				return err
			}
			return c.Printer.Value(cmd.OutOrStdout(), info)
		},
	}
}
