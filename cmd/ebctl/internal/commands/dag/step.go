package dag

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	"github.com/f1bonacc1/ebind/workflow"
)

func newStepCmd(c *cli.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "step",
		Short: "Inspect a step's record or result",
	}
	cmd.AddCommand(newStepGetCmd(c))
	cmd.AddCommand(newStepResultCmd(c))
	return cmd
}

func newStepGetCmd(c *cli.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "get <dag-id> <step-id>",
		Short: "Show a step's record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			rec, rev, err := wf.Store.GetStep(ctx, args[0], args[1])
			if err != nil {
				return err
			}
			if c.Printer.Name() == "json" {
				type view struct {
					workflow.StepRecord
					Revision uint64 `json:"revision"`
				}
				return c.Printer.Value(cmd.OutOrStdout(), view{rec, rev})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Step %s/%s  [%s]  attempt=%d  rev=%d\n", rec.DAGID, rec.StepID, rec.Status, rec.Attempt, rev)
			fmt.Fprintf(w, "  fn:           %s\n", rec.FnName)
			fmt.Fprintf(w, "  added_at:     %s\n", rec.AddedAt.Format("2006-01-02T15:04:05Z07:00"))
			if !rec.StartedAt.IsZero() {
				fmt.Fprintf(w, "  started_at:   %s\n", rec.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
			}
			if !rec.FinishedAt.IsZero() {
				fmt.Fprintf(w, "  finished_at:  %s\n", rec.FinishedAt.Format("2006-01-02T15:04:05Z07:00"))
			}
			if rec.WorkerID != "" {
				fmt.Fprintf(w, "  worker:       %s\n", rec.WorkerID)
			}
			if rec.ErrorKind != "" {
				fmt.Fprintf(w, "  error_kind:   %s\n", rec.ErrorKind)
			}
			if rec.ErrorMessage != "" {
				fmt.Fprintf(w, "  error_msg:    %s\n", rec.ErrorMessage)
			}
			if len(rec.Deps) > 0 {
				fmt.Fprintf(w, "  deps:         %v\n", rec.Deps)
			}
			if len(rec.OptionalDeps) > 0 {
				fmt.Fprintf(w, "  optional:     %v\n", rec.OptionalDeps)
			}
			if len(rec.ArgsJSON) > 0 {
				fmt.Fprintf(w, "  args:         %s\n", string(rec.ArgsJSON))
			}
			if rec.Policy != nil {
				fmt.Fprintf(w, "  retry_policy: %+v\n", rec.Policy)
			}
			return nil
		},
	}
}

func newStepResultCmd(c *cli.Context) *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "result <dag-id> <step-id>",
		Short: "Show a step's result payload",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := c.Ctx()
			defer cancel()
			wf, err := c.Workflow(ctx)
			if err != nil {
				return err
			}
			data, err := wf.Store.GetResult(ctx, args[0], args[1])
			if err != nil {
				if errors.Is(err, workflow.ErrStepNotFound) {
					return noResultError(ctx, wf, args[0], args[1])
				}
				return err
			}
			w := cmd.OutOrStdout()
			if raw {
				_, err := w.Write(data)
				return err
			}
			if c.Printer.Name() == "json" {
				_, err := fmt.Fprintln(w, string(data))
				return err
			}
			_, err = fmt.Fprintln(w, string(data))
			return err
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "write result bytes verbatim (no newline)")
	return cmd
}

// noResultError explains why a step has no result payload using the step's
// terminal status (and persisted error, if it failed) instead of a generic
// "may not be done yet".
func noResultError(ctx context.Context, wf *workflow.Workflow, dagID, stepID string) error {
	rec, _, err := wf.Store.GetStep(ctx, dagID, stepID)
	if err != nil {
		if errors.Is(err, workflow.ErrStepNotFound) {
			return fmt.Errorf("no result for %s/%s: step not found", dagID, stepID)
		}
		return err
	}
	switch rec.Status {
	case workflow.StatusFailed:
		reason := rec.ErrorKind
		switch {
		case reason != "" && rec.ErrorMessage != "":
			reason += ": " + rec.ErrorMessage
		case reason == "" && rec.ErrorMessage != "":
			reason = rec.ErrorMessage
		case reason == "":
			reason = "no error recorded"
		}
		return fmt.Errorf("no result for %s/%s: step failed (%s)", dagID, stepID, reason)
	case workflow.StatusSkipped:
		return fmt.Errorf("no result for %s/%s: step was skipped", dagID, stepID)
	case workflow.StatusCanceled:
		return fmt.Errorf("no result for %s/%s: step was canceled", dagID, stepID)
	default:
		return fmt.Errorf("no result for %s/%s: step is %s (not done yet)", dagID, stepID, rec.Status)
	}
}
