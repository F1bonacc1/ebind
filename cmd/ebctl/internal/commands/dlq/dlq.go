// Package dlq implements `ebctl dlq ...` subcommands.
package dlq

import (
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func NewCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dlq",
		Short: "Inspect and manage the dead-letter queue",
	}
	cmd.AddCommand(newLsCmd(c))
	cmd.AddCommand(newShowCmd(c))
	cmd.AddCommand(newWatchCmd(c))
	cmd.AddCommand(newRequeueCmd(c))
	cmd.AddCommand(newPurgeCmd(c))
	return cmd
}
