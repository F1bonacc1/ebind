package workflow

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

// DAGDebug is a structured snapshot of a DAG's state suitable for logs,
// dashboards, or debug endpoints. See Debug() for how to obtain one.
type DAGDebug struct {
	Meta          DAGMeta
	Steps         []StepDebug            // ordered by AddedAt (tie-breaker: StepID)
	Counts        map[StepStatus]int     // count of steps in each status
	TotalDuration time.Duration          // max(FinishedAt) - Meta.CreatedAt; 0 while running
	Blockers      []StepBlocker          // Pending steps still waiting on non-terminal deps
}

// StepDebug enriches StepRecord with computed per-step durations.
type StepDebug struct {
	StepRecord
	QueueDuration time.Duration // StartedAt - AddedAt (0 if step never started)
	ExecDuration  time.Duration // FinishedAt - StartedAt (0 if step never finished)
}

// StepBlocker names a Pending step and lists its non-terminal deps (required
// and optional combined). Useful for "why isn't this step running yet?".
type StepBlocker struct {
	StepID    string
	WaitingOn []string
}

// Debug returns a structured snapshot of the given DAG. Safe to call from any
// process that can reach the StateStore — it's a read-only operation.
// Returns ErrDAGNotFound if the DAG's meta key is missing.
func Debug(ctx context.Context, wf *Workflow, dagID string) (DAGDebug, error) {
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return DAGDebug{}, err
	}
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return DAGDebug{}, err
	}

	// Ordering: by AddedAt, tie-break by StepID for determinism.
	sort.Slice(steps, func(i, j int) bool {
		if !steps[i].AddedAt.Equal(steps[j].AddedAt) {
			return steps[i].AddedAt.Before(steps[j].AddedAt)
		}
		return steps[i].StepID < steps[j].StepID
	})

	stepByID := make(map[string]StepRecord, len(steps))
	for _, s := range steps {
		stepByID[s.StepID] = s
	}

	dbg := DAGDebug{
		Meta:   meta,
		Steps:  make([]StepDebug, 0, len(steps)),
		Counts: map[StepStatus]int{},
	}

	var maxFinished time.Time
	allTerminal := true
	for _, s := range steps {
		sd := StepDebug{StepRecord: s}
		if !s.StartedAt.IsZero() && !s.AddedAt.IsZero() {
			sd.QueueDuration = s.StartedAt.Sub(s.AddedAt)
		}
		if !s.FinishedAt.IsZero() && !s.StartedAt.IsZero() {
			sd.ExecDuration = s.FinishedAt.Sub(s.StartedAt)
		}
		dbg.Steps = append(dbg.Steps, sd)
		dbg.Counts[s.Status]++

		if s.FinishedAt.After(maxFinished) {
			maxFinished = s.FinishedAt
		}
		if !s.IsTerminal() {
			allTerminal = false
		}
	}

	if allTerminal && !maxFinished.IsZero() && !meta.CreatedAt.IsZero() {
		dbg.TotalDuration = maxFinished.Sub(meta.CreatedAt)
	}

	// Blockers: Pending steps whose deps (required + optional) are not all terminal.
	for _, s := range steps {
		if s.Status != StatusPending {
			continue
		}
		var waiting []string
		for _, dep := range s.Deps {
			d, ok := stepByID[dep]
			if !ok || !d.IsTerminal() {
				waiting = append(waiting, dep)
			}
		}
		for _, dep := range s.OptionalDeps {
			d, ok := stepByID[dep]
			if !ok || !d.IsTerminal() {
				waiting = append(waiting, dep)
			}
		}
		if len(waiting) > 0 {
			dbg.Blockers = append(dbg.Blockers, StepBlocker{StepID: s.StepID, WaitingOn: waiting})
		}
	}
	return dbg, nil
}

// DebugPrint writes a human-readable report of the DAG to w.
func DebugPrint(ctx context.Context, wf *Workflow, dagID string, w io.Writer) error {
	dbg, err := Debug(ctx, wf, dagID)
	if err != nil {
		return err
	}
	return WriteDebug(w, dbg)
}

// WriteDebug renders an already-loaded DAGDebug to w. Separated from DebugPrint
// so callers that have the snapshot can reuse the renderer without re-reading.
func WriteDebug(w io.Writer, dbg DAGDebug) error {
	ageStr := ""
	if !dbg.Meta.CreatedAt.IsZero() {
		ageStr = fmt.Sprintf("  created %s ago", humanDuration(time.Since(dbg.Meta.CreatedAt)))
	}
	totalStr := ""
	if dbg.TotalDuration > 0 {
		totalStr = fmt.Sprintf("  total %s", humanDuration(dbg.TotalDuration))
	}
	if _, err := fmt.Fprintf(w, "DAG %s  [%s]%s%s\n",
		dbg.Meta.ID, statusHeaderLabel(dbg.Meta.Status), ageStr, totalStr); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  steps: %d done, %d failed, %d skipped, %d pending, %d running\n\n",
		dbg.Counts[StatusDone], dbg.Counts[StatusFailed], dbg.Counts[StatusSkipped],
		dbg.Counts[StatusPending], dbg.Counts[StatusRunning]); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range dbg.Steps {
		glyph := statusGlyph(s.Status)
		queue := durationCol(s.QueueDuration, s.StartedAt.IsZero())
		exec := durationCol(s.ExecDuration, s.FinishedAt.IsZero())
		annotation := stepAnnotation(s)
		fmt.Fprintf(tw, "  %s\t%s\t%s\tqueue %s\texec %s\t%s\n",
			glyph, s.StepID, string(s.Status), queue, exec, annotation)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(dbg.Blockers) == 0 {
		_, err := fmt.Fprintln(w, "\n  blockers: none")
		return err
	}
	if _, err := fmt.Fprintln(w, "\n  blockers:"); err != nil {
		return err
	}
	for _, b := range dbg.Blockers {
		if _, err := fmt.Fprintf(w, "    %s waiting on: %v\n", b.StepID, b.WaitingOn); err != nil {
			return err
		}
	}
	return nil
}

// --- rendering helpers ---

func statusGlyph(s StepStatus) string {
	switch s {
	case StatusDone:
		return "✓"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "⊘"
	case StatusRunning:
		return "▶"
	case StatusPending:
		return "⋯"
	}
	return "?"
}

func statusHeaderLabel(s DAGStatus) string {
	switch s {
	case DAGStatusDone:
		return "DONE"
	case DAGStatusFailed:
		return "FAILED"
	case DAGStatusRunning:
		return "RUNNING"
	}
	return string(s)
}

// durationCol formats a duration column; returns "—" when not applicable.
func durationCol(d time.Duration, unknown bool) string {
	if unknown && d == 0 {
		return "—"
	}
	return humanDuration(d)
}

// humanDuration formats durations compactly: µs, ms, s, m.
func humanDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return d.Truncate(time.Second).String()
	}
}

func stepAnnotation(s StepDebug) string {
	var parts []string
	if s.Optional {
		parts = append(parts, "optional")
	}
	if s.Attempt > 1 {
		parts = append(parts, fmt.Sprintf("attempts=%d", s.Attempt))
	}
	if s.ErrorKind != "" {
		parts = append(parts, "kind="+s.ErrorKind)
	}
	switch s.Status {
	case StatusSkipped:
		if s.ErrorKind == "" {
			parts = append(parts, "(cascade)")
		}
	case StatusPending:
		// Intentionally leave empty — blockers section carries detail.
	}
	if len(parts) == 0 {
		return ""
	}
	return joinAnnotations(parts)
}

func joinAnnotations(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}
