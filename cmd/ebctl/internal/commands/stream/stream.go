// Package stream implements `ebctl stream ...` and `ebctl consumer ...`.
package stream

import (
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

// NewStreamCmd returns the `stream` subcommand tree.
func NewStreamCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stream",
		Short: "Inspect and manage ebind JetStream streams",
	}
	cmd.AddCommand(newLsCmd(c))
	cmd.AddCommand(newInfoCmd(c))
	cmd.AddCommand(newPeekCmd(c))
	cmd.AddCommand(newPurgeCmd(c))
	cmd.AddCommand(newRmMsgCmd(c))
	return cmd
}

// NewConsumerCmd returns the `consumer` subcommand tree.
func NewConsumerCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "consumer",
		Short: "Inspect JetStream consumers",
	}
	cmd.AddCommand(newConsumerLsCmd(c))
	cmd.AddCommand(newConsumerInfoCmd(c))
	return cmd
}

// ebindStreams is the set of streams ebind creates, used by `stream ls`.
// EBIND_DAG_EVENTS is not always present (only when workflow is used).
var ebindStreams = []string{
	"EBIND_TASKS",
	"EBIND_RESP",
	"EBIND_DLQ",
	"EBIND_DAG_EVENTS",
}
