package stream

import (
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
)

func newConsumerLsCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "ls <stream>",
		Short: "List consumers on a stream",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, args[0])
			if err != nil {
				return err
			}

			type row struct {
				Name        string `json:"name"`
				Durable     string `json:"durable,omitempty"`
				Pending     uint64 `json:"pending"`
				Delivered   uint64 `json:"delivered"`
				AckFloor    uint64 `json:"ack_floor"`
				Redelivered int    `json:"redelivered"`
				Waiting     int    `json:"waiting"`
				Filter      string `json:"filter,omitempty"`
			}
			var rows []row

			infos := s.ListConsumers(ctx)
			for info := range infos.Info() {
				rows = append(rows, row{
					Name:        info.Name,
					Durable:     info.Config.Durable,
					Pending:     info.NumPending,
					Delivered:   info.Delivered.Stream,
					AckFloor:    info.AckFloor.Stream,
					Redelivered: info.NumRedelivered,
					Waiting:     info.NumWaiting,
					Filter:      info.Config.FilterSubject,
				})
			}
			if err := infos.Err(); err != nil {
				return err
			}

			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), rows)
			}
			headers := []string{"NAME", "DURABLE", "PENDING", "DELIVERED", "ACK-FLOOR", "REDELIVERED", "FILTER"}
			out := make([][]string, 0, len(rows))
			for _, r := range rows {
				out = append(out, []string{
					r.Name, r.Durable,
					fmt.Sprintf("%d", r.Pending),
					fmt.Sprintf("%d", r.Delivered),
					fmt.Sprintf("%d", r.AckFloor),
					fmt.Sprintf("%d", r.Redelivered),
					r.Filter,
				})
			}
			return c.Printer.Table(cmd.OutOrStdout(), headers, out)
		},
	}
}

func newConsumerInfoCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "info <stream> <consumer>",
		Short: "Show full consumer config + state",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			s, err := c.JS.Stream(ctx, args[0])
			if err != nil {
				return err
			}
			cons, err := s.Consumer(ctx, args[1])
			if err != nil {
				return err
			}
			info, err := cons.Info(ctx)
			if err != nil {
				return err
			}
			_ = jetstream.ConsumerInfo{} // keep import in case future summarization needs it
			return c.Printer.Value(cmd.OutOrStdout(), info)
		},
	}
}
