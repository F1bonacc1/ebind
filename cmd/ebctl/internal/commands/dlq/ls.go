package dlq

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	ebinddlq "github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/stream"
)

func newLsCmd(c *cli.Context) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List DLQ entries (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, stream.DLQStream)
			if err != nil {
				return err
			}
			info, err := s.Info(ctx)
			if err != nil {
				return err
			}
			if info.State.Msgs == 0 {
				return c.Printer.Text(cmd.OutOrStdout(), "DLQ is empty")
			}

			type row struct {
				Seq          uint64    `json:"seq"`
				Fn           string    `json:"fn"`
				TaskID       string    `json:"task_id"`
				ErrorKind    string    `json:"error_kind"`
				ErrorMessage string    `json:"error_message"`
				FinalAttempt int       `json:"final_attempt"`
				DeadAt       time.Time `json:"dead_at"`
			}
			var rows []row

			collected := 0
			for sq := info.State.LastSeq; sq >= info.State.FirstSeq && (limit <= 0 || collected < limit); sq-- {
				msg, err := s.GetMsg(ctx, sq)
				if err != nil {
					if errors.Is(err, jetstream.ErrMsgNotFound) {
						if sq == 0 {
							break
						}
						continue
					}
					return err
				}
				var entry ebinddlq.Entry
				if err := json.Unmarshal(msg.Data, &entry); err != nil {
					continue
				}
				rows = append(rows, row{
					Seq:          msg.Sequence,
					Fn:           entry.Task.Name,
					TaskID:       entry.Task.ID,
					ErrorKind:    entry.Error.Kind,
					ErrorMessage: entry.Error.Message,
					FinalAttempt: entry.FinalAttempt,
					DeadAt:       entry.DeadLetteredAt,
				})
				collected++
				if sq == 0 {
					break
				}
			}

			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), rows)
			}
			headers := []string{"SEQ", "FN", "ERROR", "ATTEMPT", "AGE", "TASK-ID"}
			out := make([][]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, []string{
					fmt.Sprintf("%d", r.Seq),
					r.Fn,
					truncate(r.ErrorKind+": "+r.ErrorMessage, 60),
					fmt.Sprintf("%d", r.FinalAttempt),
					format.Age(r.DeadAt),
					r.TaskID,
				})
			}
			return c.Printer.Table(cmd.OutOrStdout(), headers, out)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "max entries (0 = unlimited)")
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
