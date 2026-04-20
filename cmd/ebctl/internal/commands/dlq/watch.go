package dlq

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	ebinddlq "github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/stream"
)

func newWatchCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Follow new DLQ entries live",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := c.Background()
			s, err := c.JS.Stream(ctx, stream.DLQStream)
			if err != nil {
				return err
			}
			cons, err := s.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
				DeliverPolicy: jetstream.DeliverNewPolicy,
				FilterSubjects: []string{stream.DLQSubjectPrefix + ">"},
			})
			if err != nil {
				return err
			}
			cc, err := cons.Consume(func(m jetstream.Msg) {
				md, _ := m.Metadata()
				var entry ebinddlq.Entry
				if err := json.Unmarshal(m.Data(), &entry); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "decode:", err)
					_ = m.Ack()
					return
				}
				seq := uint64(0)
				if md != nil {
					seq = md.Sequence.Stream
				}
				line := fmt.Sprintf("%s  seq=%d fn=%s err=%s/%s attempts=%d",
					time.Now().UTC().Format(time.RFC3339), seq, entry.Task.Name,
					entry.Error.Kind, entry.Error.Message, entry.FinalAttempt)
				_ = c.Printer.Text(cmd.OutOrStdout(), line)
				_ = m.Ack()
			})
			if err != nil {
				return err
			}
			defer cc.Stop()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			<-sigCh
			return nil
		},
	}
}
