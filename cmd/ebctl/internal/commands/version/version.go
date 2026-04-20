// Package version prints the ebctl build metadata.
package version

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

// Populated via -ldflags "-X ...Version=..." at release time. Falls back to
// the module version stamped into the binary by the Go toolchain.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

func NewCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print ebctl version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, ok := debug.ReadBuildInfo()
			modVersion := ""
			if ok {
				modVersion = info.Main.Version
			}
			out := fmt.Sprintf("ebctl %s", Version)
			if Commit != "" {
				out += " (" + Commit + ")"
			}
			if Date != "" {
				out += " built " + Date
			}
			if modVersion != "" && modVersion != "(devel)" {
				out += " module " + modVersion
			}
			return c.Printer.Text(cmd.OutOrStdout(), out)
		},
	}
}
