package dag

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newWatchCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "watch [dag-id]",
		Short: "Follow DAG events live (all DAGs if no id)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			setupCtx, setupCancel := c.Ctx()
			wf, err := c.Workflow(setupCtx)
			setupCancel()
			if err != nil {
				return err
			}

			filter := "DAG.>"
			if len(args) == 1 {
				filter = fmt.Sprintf("DAG.%s.*.*", args[0])
			}

			ctx := c.Background()
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			sub, err := wf.Bus.Subscribe(ctx, filter, func(ev workflow.Event) {
				line := fmt.Sprintf("%s  DAG %s  step %s  %s",
					time.Now().UTC().Format(time.RFC3339), ev.DAGID, ev.StepID, ev.Kind)
				if ev.Kind == workflow.EventCompleted {
					line += fmt.Sprintf("  status=%s", ev.Status)
					if ev.ErrorKind != "" {
						line += "  err=" + ev.ErrorKind
					}
				}
				_ = c.Printer.Text(cmd.OutOrStdout(), line)
				_ = ev.Ack()
			})
			if err != nil {
				return err
			}
			defer sub.Stop()

			<-sigCh
			return nil
		},
	}
}
