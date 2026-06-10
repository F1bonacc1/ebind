package dag

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
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
			filter := "DAG.>"
			if len(args) == 1 {
				filter = fmt.Sprintf("DAG.%s.*.*", args[0])
			}

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			// Passive tap via core NATS subscription. JetStream publishes are
			// visible to plain subscribers on the same subjects, so this
			// observes events WITHOUT consuming from the work-queue events
			// stream. Using wf.Bus.Subscribe here would join (and rewrite) the
			// shared durable scheduler consumer and steal event deliveries
			// from production schedulers — including EventResumed.
			sub, err := c.NC.Subscribe(filter, func(msg *nats.Msg) {
				ev, err := workflow.UnmarshalEvent(msg.Data)
				if err != nil {
					return
				}
				line := fmt.Sprintf("%s  DAG %s  step %s  %s",
					time.Now().UTC().Format(time.RFC3339), ev.DAGID, ev.StepID, ev.Kind)
				if ev.Kind == workflow.EventCompleted {
					line += fmt.Sprintf("  status=%s", ev.Status)
					if ev.ErrorKind != "" {
						line += "  err=" + ev.ErrorKind
					}
				}
				_ = c.Printer.Text(cmd.OutOrStdout(), line)
			})
			if err != nil {
				return err
			}
			defer func() { _ = sub.Unsubscribe() }()

			<-sigCh
			return nil
		},
	}
}
