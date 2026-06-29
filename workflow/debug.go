package workflow

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// DAGDebug is a structured snapshot of a DAG's state suitable for logs,
// dashboards, or debug endpoints. See Debug() for how to obtain one.
type DAGDebug struct {
	Meta          DAGMeta
	Steps         []StepDebug        // ordered by AddedAt (tie-breaker: StepID)
	Counts        map[StepStatus]int // count of steps in each status
	TotalDuration time.Duration      // max(FinishedAt) - Meta.CreatedAt; 0 while running
	Blockers      []StepBlocker      // Pending steps still waiting on non-terminal deps
	Breakpoints   []BreakpointInfo   // declared breakpoints with computed state
}

// StepDebug enriches StepRecord with computed per-step durations.
type StepDebug struct {
	StepRecord
	QueueDuration time.Duration // StartedAt - AddedAt (0 if step never started)
	ExecDuration  time.Duration // FinishedAt - StartedAt (0 if step never finished)
	BPNote        string        // computed breakpoint annotation, e.g. "bp:blocked[X]"
}

// StepBlocker names a Pending step and explains why it isn't running yet.
// WaitingOn lists non-terminal deps (required and optional combined); GatedBy
// lists deps that are done but whose armed after-breakpoint holds this step.
// Both carry pure step IDs.
type StepBlocker struct {
	StepID    string
	WaitingOn []string
	GatedBy   []string
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
		Meta:        meta,
		Steps:       make([]StepDebug, 0, len(steps)),
		Counts:      map[StepStatus]int{},
		Breakpoints: ComputeBreakpoints(meta, steps),
	}
	bpNotes := breakpointNotes(dbg.Breakpoints)

	var maxFinished time.Time
	allTerminal := true
	for _, s := range steps {
		sd := StepDebug{StepRecord: s, BPNote: bpNotes[s.StepID]}
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

	dagState := &DAGState{Meta: meta, Steps: stepByID}
	for _, s := range steps {
		if s.Status != StatusPending {
			continue
		}
		var waiting, gated []string
		classify := func(dep string) {
			d, ok := stepByID[dep]
			switch {
			case !ok || !d.IsTerminal():
				waiting = append(waiting, dep)
			case dagState.afterBPHolds(d):
				gated = append(gated, dep) // done, but its after-breakpoint gates this step
			}
		}
		for _, dep := range s.Deps {
			classify(dep)
		}
		for _, dep := range s.OptionalDeps {
			classify(dep)
		}
		if len(waiting) > 0 || len(gated) > 0 {
			dbg.Blockers = append(dbg.Blockers, StepBlocker{StepID: s.StepID, WaitingOn: waiting, GatedBy: gated})
		}
	}
	return dbg, nil
}

// breakpointNotes builds the per-step BPNote annotations from computed
// breakpoint infos (e.g. "bp:blocked[X]", "bp:holding[X]→{b,c}", "bp:released").
func breakpointNotes(infos []BreakpointInfo) map[string]string {
	notes := map[string]string{}
	for _, bp := range infos {
		var note string
		switch {
		case bp.State == BPStateReleased:
			note = "bp:released"
		case bp.State == BPStateBlocked && bp.Position == BPPositionBefore:
			note = "bp:blocked[" + strings.Join(bp.Labels, ",") + "]"
		case bp.State == BPStateBlocked && bp.Position == BPPositionAfter:
			note = "bp:holding[" + strings.Join(bp.Labels, ",") + "]→{" + strings.Join(bp.Holding, ",") + "}"
		default:
			continue // declared but not hit — keep step rows quiet
		}
		if prev := notes[bp.StepID]; prev != "" {
			note = prev + " " + note
		}
		notes[bp.StepID] = note
	}
	return notes
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
	pausedStr := ""
	if !dbg.Meta.PausedAt.IsZero() {
		pausedStr = fmt.Sprintf("  paused %s ago", humanDuration(time.Since(dbg.Meta.PausedAt)))
	}
	bpStr := ""
	if n := CountBlocked(dbg.Breakpoints); n > 0 {
		bpStr = fmt.Sprintf("  ⦿ %d at breakpoint", n)
	}
	if _, err := fmt.Fprintf(w, "DAG %s  [%s]%s%s%s%s\n",
		dbg.Meta.ID, statusHeaderLabel(dbg.Meta.Status), ageStr, pausedStr, totalStr, bpStr); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  steps: %d done, %d failed, %d skipped, %d canceled, %d pending, %d running\n",
		dbg.Counts[StatusDone], dbg.Counts[StatusFailed], dbg.Counts[StatusSkipped], dbg.Counts[StatusCanceled],
		dbg.Counts[StatusPending], dbg.Counts[StatusRunning]); err != nil {
		return err
	}
	if len(dbg.Meta.Labels) > 0 {
		if _, err := fmt.Fprintf(w, "  labels: %s\n", strings.Join(dbg.Meta.Labels, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
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
		if _, err := fmt.Fprintln(w, "\n  blockers: none"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "\n  blockers:"); err != nil {
			return err
		}
		for _, b := range dbg.Blockers {
			var parts []string
			if len(b.WaitingOn) > 0 {
				parts = append(parts, fmt.Sprintf("waiting on: %v", b.WaitingOn))
			}
			if len(b.GatedBy) > 0 {
				parts = append(parts, fmt.Sprintf("gated by after-bp: %v", b.GatedBy))
			}
			if _, err := fmt.Fprintf(w, "    %s %s\n", b.StepID, strings.Join(parts, "  ")); err != nil {
				return err
			}
		}
	}

	if len(dbg.Breakpoints) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\n  breakpoints:"); err != nil {
		return err
	}
	for _, bp := range dbg.Breakpoints {
		armed := "inactive"
		if bp.Armed {
			armed = "armed"
		}
		stateStr := ""
		switch bp.State {
		case BPStateBlocked:
			stateStr = "  ⦿ blocked"
			if !bp.BlockedSince.IsZero() {
				stateStr += fmt.Sprintf(" %s ago", humanDuration(time.Since(bp.BlockedSince)))
			}
			if len(bp.Holding) > 0 {
				stateStr += fmt.Sprintf("  holding %v", bp.Holding)
			}
		case BPStateReleased:
			stateStr = "  released"
		}
		if _, err := fmt.Fprintf(w, "    %s %s [%s]  %s%s\n",
			bp.StepID, bp.Position, strings.Join(bp.Labels, ","), armed, stateStr); err != nil {
			return err
		}
	}
	return nil
}

func statusGlyph(s StepStatus) string {
	switch s {
	case StatusDone:
		return "✓"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "⊘"
	case StatusCanceled:
		return "■"
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
	case DAGStatusCanceled:
		return "CANCELED"
	case DAGStatusRunning:
		return "RUNNING"
	case DAGStatusPausing:
		return "PAUSING"
	case DAGStatusPaused:
		return "PAUSED"
	}
	return string(s)
}

func durationCol(d time.Duration, unknown bool) string {
	if unknown && d == 0 {
		return "—"
	}
	return humanDuration(d)
}

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
	if s.Held {
		parts = append(parts, "held")
	}
	if s.Attempt > 1 {
		parts = append(parts, fmt.Sprintf("attempts=%d", s.Attempt))
	}
	if s.WorkerID != "" {
		parts = append(parts, "worker="+s.WorkerID)
	}
	if s.ErrorKind != "" {
		parts = append(parts, "kind="+s.ErrorKind)
	}
	if s.BPNote != "" {
		parts = append(parts, s.BPNote)
	}
	switch s.Status {
	case StatusSkipped:
		if s.ErrorKind == "" {
			parts = append(parts, "(cascade)")
		}
	case StatusCanceled:
		parts = append(parts, "(canceled)")
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
