// Command ebctl is the operator CLI for ebind: inspect and manipulate DAGs,
// streams, consumers, and the DLQ of a running NATS JetStream deployment.
package main

import (
	"fmt"
	"os"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	cmddag "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/dag"
	cmddlq "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/dlq"
	cmdstream "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/stream"
	cmdversion "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/version"
)

func main() {
	c := cli.NewContext()
	root := cli.NewRootCommand(c)
	root.AddCommand(cmdversion.NewCmd(c))
	root.AddCommand(cmddag.NewCmd(c))
	root.AddCommand(cmdstream.NewStreamCmd(c))
	root.AddCommand(cmdstream.NewConsumerCmd(c))
	root.AddCommand(cmddlq.NewCmd(c))

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ebctl:", err)
		os.Exit(1)
	}
}
