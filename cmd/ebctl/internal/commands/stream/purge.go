package stream

import (
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func newPurgeCmd(c *cli.Context) *cobra.Command {
	var subject string
	var keep uint64

	cmd := &cobra.Command{
		Use:   "purge <stream>",
		Short: "Purge messages from a stream",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, args[0])
			if err != nil {
				return err
			}

			descr := fmt.Sprintf("Purge all messages from %s", args[0])
			var opts []jetstream.StreamPurgeOpt
			if subject != "" {
				opts = append(opts, jetstream.WithPurgeSubject(subject))
				descr = fmt.Sprintf("Purge subject %s from %s", subject, args[0])
			}
			if keep > 0 {
				opts = append(opts, jetstream.WithPurgeKeep(keep))
				descr += fmt.Sprintf(" (keep last %d)", keep)
			}
			if err := c.Confirm(descr + "?"); err != nil {
				return err
			}
			if err := s.Purge(ctx, opts...); err != nil {
				return err
			}
			return c.Printer.Text(cmd.OutOrStdout(), "purged")
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "purge only this subject")
	cmd.Flags().Uint64Var(&keep, "keep", 0, "keep the last N messages")
	return cmd
}
