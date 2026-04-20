// Package dag implements `ebctl dag ...` subcommands.
package dag

import (
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func NewCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dag",
		Short: "Inspect and manage DAG workflows",
	}
	cmd.AddCommand(newLsCmd(c))
	cmd.AddCommand(newGetCmd(c))
	cmd.AddCommand(newTreeCmd(c))
	cmd.AddCommand(newCancelCmd(c))
	cmd.AddCommand(newRmCmd(c))
	cmd.AddCommand(newWatchCmd(c))
	cmd.AddCommand(newStepCmd(c))
	return cmd
}
