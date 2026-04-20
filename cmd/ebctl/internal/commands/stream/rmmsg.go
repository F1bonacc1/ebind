package stream

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func newRmMsgCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "rm-msg <stream> <seq>",
		Short: "Delete a single message by sequence number",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			seq, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("parse seq: %w", err)
			}
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, args[0])
			if err != nil {
				return err
			}
			if err := c.Confirm(fmt.Sprintf("Delete seq %d from %s?", seq, args[0])); err != nil {
				return err
			}
			if err := s.DeleteMsg(ctx, seq); err != nil {
				return err
			}
			return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("deleted %s seq %d", args[0], seq))
		},
	}
}
