package dlq

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	ebinddlq "github.com/f1bonacc1/ebind/dlq"
)

func newShowCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "show <seq>",
		Short: "Show a DLQ entry in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			seq, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("parse seq: %w", err)
			}
			ctx, cancel := c.Ctx()
			defer cancel()
			entry, err := ebinddlq.Fetch(ctx, c.JS, seq)
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				return c.Printer.Value(cmd.OutOrStdout(), entry)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "DLQ seq %d\n", seq)
			fmt.Fprintf(w, "  fn:             %s\n", entry.Task.Name)
			fmt.Fprintf(w, "  task_id:        %s\n", entry.Task.ID)
			fmt.Fprintf(w, "  target:         %s\n", entry.Task.Target)
			fmt.Fprintf(w, "  dag/step:       %s/%s\n", entry.Task.DAGID, entry.Task.StepID)
			fmt.Fprintf(w, "  final_attempt:  %d\n", entry.FinalAttempt)
			fmt.Fprintf(w, "  dead_at:        %s\n", entry.DeadLetteredAt)
			fmt.Fprintf(w, "  error_kind:     %s\n", entry.Error.Kind)
			fmt.Fprintf(w, "  error_message:  %s\n", entry.Error.Message)
			fmt.Fprintf(w, "  retryable:      %v\n", entry.Error.Retryable)
			fmt.Fprintf(w, "  payload:        %s\n", string(entry.Task.Payload))
			return nil
		},
	}
}
