package stream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

type peekRow struct {
	Seq      uint64            `json:"seq"`
	Subject  string            `json:"subject"`
	Time     time.Time         `json:"time"`
	Size     int               `json:"size"`
	Headers  map[string]string `json:"headers,omitempty"`
	DataUTF8 string            `json:"data,omitempty"`
}

func newPeekCmd(c *cli.Context) *cobra.Command {
	var seq uint64
	var last int
	var subject string

	cmd := &cobra.Command{
		Use:   "peek <stream>",
		Short: "Peek messages from a stream (read-only, no ack)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, args[0])
			if err != nil {
				return err
			}
			info, err := s.Info(ctx)
			if err != nil {
				return err
			}

			var rows []peekRow

			switch {
			case seq > 0:
				msg, err := getFiltered(ctx, s, seq, subject)
				if err != nil {
					return err
				}
				rows = append(rows, toPeekRow(msg))
			default:
				count := last
				if count <= 0 {
					count = 10
				}
				from := info.State.LastSeq
				collected := 0
				for sq := from; sq >= info.State.FirstSeq && collected < count; sq-- {
					msg, err := getFiltered(ctx, s, sq, subject)
					if err != nil {
						if errors.Is(err, jetstream.ErrMsgNotFound) {
							if sq == 0 {
								break
							}
							continue
						}
						return err
					}
					rows = append(rows, toPeekRow(msg))
					collected++
					if sq == 0 {
						break
					}
				}
			}

			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), rows)
			}
			headers := []string{"SEQ", "TIME", "SUBJECT", "SIZE", "MSG-ID"}
			out := make([][]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, []string{
					fmt.Sprintf("%d", r.Seq),
					r.Time.UTC().Format(time.RFC3339),
					r.Subject,
					fmt.Sprintf("%d", r.Size),
					r.Headers["Nats-Msg-Id"],
				})
			}
			return c.Printer.Table(cmd.OutOrStdout(), headers, out)
		},
	}
	cmd.Flags().Uint64Var(&seq, "seq", 0, "fetch a specific sequence number")
	cmd.Flags().IntVar(&last, "last", 10, "number of most-recent messages to show (ignored with --seq)")
	cmd.Flags().StringVar(&subject, "subject", "", "filter: only show messages on this subject")
	return cmd
}

func getFiltered(ctx context.Context, s jetstream.Stream, seq uint64, subject string) (*jetstream.RawStreamMsg, error) {
	msg, err := s.GetMsg(ctx, seq)
	if err != nil {
		return nil, err
	}
	if subject != "" && msg.Subject != subject {
		return nil, jetstream.ErrMsgNotFound
	}
	return msg, nil
}

func toPeekRow(m *jetstream.RawStreamMsg) peekRow {
	headers := make(map[string]string, len(m.Header))
	for k, v := range m.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	return peekRow{
		Seq:      m.Sequence,
		Subject:  m.Subject,
		Time:     m.Time,
		Size:     len(m.Data),
		Headers:  headers,
		DataUTF8: string(m.Data),
	}
}
