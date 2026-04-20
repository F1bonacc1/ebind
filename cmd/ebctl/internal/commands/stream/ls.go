package stream

import (
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func newLsCmd(c *cli.Context) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List ebind streams with stats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()

			var names []string
			if all {
				iter := c.JS.StreamNames(ctx)
				for n := range iter.Name() {
					names = append(names, n)
				}
				if err := iter.Err(); err != nil {
					return err
				}
			} else {
				names = append(names, ebindStreams...)
			}

			type row struct {
				Name      string `json:"name"`
				Messages  uint64 `json:"messages"`
				Bytes     uint64 `json:"bytes"`
				Consumers int    `json:"consumers"`
				FirstSeq  uint64 `json:"first_seq"`
				LastSeq   uint64 `json:"last_seq"`
				Subjects  string `json:"subjects"`
				Retention string `json:"retention"`
			}
			var rows []row
			for _, name := range names {
				s, err := c.JS.Stream(ctx, name)
				if err != nil {
					if errors.Is(err, jetstream.ErrStreamNotFound) {
						continue
					}
					return fmt.Errorf("stream %s: %w", name, err)
				}
				info, err := s.Info(ctx)
				if err != nil {
					return err
				}
				rows = append(rows, row{
					Name:      info.Config.Name,
					Messages:  info.State.Msgs,
					Bytes:     info.State.Bytes,
					Consumers: info.State.Consumers,
					FirstSeq:  info.State.FirstSeq,
					LastSeq:   info.State.LastSeq,
					Subjects:  joinShort(info.Config.Subjects),
					Retention: retentionName(info.Config.Retention),
				})
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), rows)
			}
			headers := []string{"NAME", "MSGS", "BYTES", "CONS", "FIRST", "LAST", "SUBJECTS", "RETENTION"}
			out := make([][]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, []string{
					r.Name,
					fmt.Sprintf("%d", r.Messages),
					humanBytes(r.Bytes),
					fmt.Sprintf("%d", r.Consumers),
					fmt.Sprintf("%d", r.FirstSeq),
					fmt.Sprintf("%d", r.LastSeq),
					r.Subjects,
					r.Retention,
				})
			}
			return c.Printer.Table(cmd.OutOrStdout(), headers, out)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "list every stream on the server, not just ebind's")
	return cmd
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func joinShort(ss []string) string {
	switch len(ss) {
	case 0:
		return ""
	case 1:
		return ss[0]
	default:
		return fmt.Sprintf("%s (+%d)", ss[0], len(ss)-1)
	}
}

func retentionName(r jetstream.RetentionPolicy) string {
	switch r {
	case jetstream.LimitsPolicy:
		return "limits"
	case jetstream.InterestPolicy:
		return "interest"
	case jetstream.WorkQueuePolicy:
		return "workq"
	}
	return "?"
}
