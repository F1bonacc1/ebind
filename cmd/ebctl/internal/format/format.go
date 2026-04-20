// Package format is the tiny output abstraction used by ebctl commands.
// Each command builds a "row set" or a single value, hands it to a Printer,
// and the Printer decides whether to render a pretty table or JSON.
package format

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// Printer is implemented by PrettyPrinter and JSONPrinter.
type Printer interface {
	// Table renders a headered table of rows. Ignored by JSON printer
	// (which uses Value instead).
	Table(w io.Writer, headers []string, rows [][]string) error
	// Value emits a single structured value. Pretty printer falls back to
	// fmt.Fprintf; JSON printer marshals.
	Value(w io.Writer, v any) error
	// Text emits a human-readable string. JSON printer wraps it as {"text": ...}.
	Text(w io.Writer, s string) error
	// Name returns the printer's canonical name ("pretty" or "json").
	Name() string
}

// New returns a Printer for the given format name. "pretty" and "" both yield
// the pretty printer.
func New(name string) (Printer, error) {
	switch strings.ToLower(name) {
	case "", "pretty", "table":
		return PrettyPrinter{}, nil
	case "json":
		return JSONPrinter{}, nil
	}
	return nil, fmt.Errorf("unknown output format %q (want pretty|json)", name)
}

// PrettyPrinter renders tab-aligned tables and human text.
type PrettyPrinter struct{}

func (PrettyPrinter) Name() string { return "pretty" }

func (PrettyPrinter) Table(w io.Writer, headers []string, rows [][]string) error {
	if w == nil {
		w = os.Stdout
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(headers) > 0 {
		fmt.Fprintln(tw, strings.Join(headers, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

func (PrettyPrinter) Value(w io.Writer, v any) error {
	if w == nil {
		w = os.Stdout
	}
	_, err := fmt.Fprintln(w, v)
	return err
}

func (PrettyPrinter) Text(w io.Writer, s string) error {
	if w == nil {
		w = os.Stdout
	}
	_, err := fmt.Fprintln(w, s)
	return err
}

// JSONPrinter emits newline-delimited JSON blocks.
type JSONPrinter struct{}

func (JSONPrinter) Name() string { return "json" }

func (JSONPrinter) Table(w io.Writer, headers []string, rows [][]string) error {
	if w == nil {
		w = os.Stdout
	}
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		m := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(r) {
				m[h] = r[i]
			}
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func (JSONPrinter) Value(w io.Writer, v any) error {
	if w == nil {
		w = os.Stdout
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (JSONPrinter) Text(w io.Writer, s string) error {
	if w == nil {
		w = os.Stdout
	}
	enc := json.NewEncoder(w)
	return enc.Encode(map[string]string{"text": s})
}

// HumanDuration is a compact rendering shared by table rows.
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
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
	}
	return d.Truncate(time.Second).String()
}

// Age renders the time since t, suitable for table rows. Zero times become "—".
func Age(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return HumanDuration(time.Since(t))
}
