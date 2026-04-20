package dlq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	ebinddlq "github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/stream"
)

func newRequeueCmd(c *cli.Context) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "requeue [seq]",
		Short: "Re-publish a DLQ entry to EBIND_TASKS and remove it from DLQ",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			if all {
				if len(args) != 0 {
					return fmt.Errorf("--all and <seq> are mutually exclusive")
				}
				return requeueAll(ctx, c, cmd)
			}
			if len(args) != 1 {
				return fmt.Errorf("requeue requires a seq (or --all)")
			}
			seq, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("parse seq: %w", err)
			}
			if err := c.Confirm(fmt.Sprintf("Requeue DLQ seq %d?", seq)); err != nil {
				return err
			}
			if err := ebinddlq.Requeue(ctx, c.JS, seq); err != nil {
				return err
			}
			return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("requeued seq %d", seq))
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "requeue every entry currently in the DLQ")
	return cmd
}

func requeueAll(ctx context.Context, c *cli.Context, cmd *cobra.Command) error {
	s, err := c.JS.Stream(ctx, stream.DLQStream)
	if err != nil {
		return err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return err
	}
	if info.State.Msgs == 0 {
		return c.Printer.Text(cmd.OutOrStdout(), "DLQ is empty — nothing to requeue")
	}
	if err := c.Confirm(fmt.Sprintf("Requeue all %d DLQ entries?", info.State.Msgs)); err != nil {
		return err
	}
	var seqs []uint64
	for sq := info.State.FirstSeq; sq <= info.State.LastSeq; sq++ {
		msg, err := s.GetMsg(ctx, sq)
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				continue
			}
			return err
		}
		var entry ebinddlq.Entry
		if err := json.Unmarshal(msg.Data, &entry); err != nil {
			continue
		}
		seqs = append(seqs, sq)
	}
	var ok, failed int
	for _, sq := range seqs {
		if err := ebinddlq.Requeue(ctx, c.JS, sq); err != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "requeue %d: %v\n", sq, err)
			continue
		}
		ok++
	}
	return c.Printer.Text(cmd.OutOrStdout(),
		fmt.Sprintf("requeued %d entries (%d failed)", ok, failed))
}
