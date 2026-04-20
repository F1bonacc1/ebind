// Package cli holds the shared CLI context, global flags, and wiring helpers
// used by every ebctl subcommand.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	"github.com/f1bonacc1/ebind/workflow"
)

// Context carries shared state across the command tree. Populated by root's
// PersistentPreRun and torn down by PersistentPostRun.
type Context struct {
	Server       string
	OutputFormat string
	Timeout      time.Duration
	Yes          bool
	Verbose      bool
	Replicas     int

	NC *nats.Conn
	JS jetstream.JetStream

	Printer format.Printer

	wf *workflow.Workflow
}

// NewContext returns a Context with defaults baked in.
func NewContext() *Context {
	return &Context{
		Server:       envOr("EBIND_NATS_URL", nats.DefaultURL),
		OutputFormat: "pretty",
		Timeout:      10 * time.Second,
		Replicas:     1,
	}
}

// Ctx returns a timeout-bounded context for per-operation RPCs.
func (c *Context) Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.Timeout)
}

// Background returns a cancel-free context for long-running streams (watch).
func (c *Context) Background() context.Context {
	return context.Background()
}

// Workflow returns a *workflow.Workflow, lazily constructing it on first use
// so that non-DAG commands don't create the KV bucket.
func (c *Context) Workflow(ctx context.Context) (*workflow.Workflow, error) {
	if c.wf != nil {
		return c.wf, nil
	}
	wf, err := workflow.NewFromNATS(ctx, c.NC, c.Replicas)
	if err != nil {
		return nil, fmt.Errorf("open workflow: %w", err)
	}
	c.wf = wf
	return wf, nil
}

// Confirm prompts the user for y/N confirmation. Returns nil on approval,
// a non-nil error on decline, or immediately succeeds when --yes is set.
// On a non-TTY stdin we refuse destructive ops without --yes rather than hang.
func (c *Context) Confirm(prompt string) error {
	if c.Yes {
		return nil
	}
	if !stdinIsTerminal() {
		return errors.New("refusing destructive operation on non-TTY without --yes")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	var resp string
	_, _ = fmt.Fscanln(os.Stdin, &resp)
	switch resp {
	case "y", "Y", "yes", "YES":
		return nil
	}
	return errors.New("aborted")
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// NewRootCommand constructs the root cobra command with global flags,
// dial setup, and cleanup hooks.
func NewRootCommand(c *Context) *cobra.Command {
	root := &cobra.Command{
		Use:           "ebctl",
		Short:         "Operator CLI for ebind (NATS-backed task queue + DAG workflows)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&c.Server, "server", "s", c.Server, "NATS URL (env EBIND_NATS_URL)")
	pf.StringVarP(&c.OutputFormat, "output", "o", c.OutputFormat, "output format: pretty|json")
	pf.DurationVar(&c.Timeout, "timeout", c.Timeout, "per-request timeout")
	pf.BoolVar(&c.Yes, "yes", c.Yes, "skip confirmation on destructive ops")
	pf.BoolVarP(&c.Verbose, "verbose", "v", c.Verbose, "verbose output")
	pf.IntVar(&c.Replicas, "replicas", c.Replicas, "KV bucket replicas when opening the workflow (dev=1, HA=3)")

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		p, err := format.New(c.OutputFormat)
		if err != nil {
			return err
		}
		c.Printer = p

		// Skip dial for `help`, `version`, or `completion` invocations.
		if skipDial(cmd) {
			return nil
		}
		nc, err := nats.Connect(c.Server,
			nats.Name("ebctl"),
			nats.Timeout(c.Timeout),
			nats.MaxReconnects(3),
		)
		if err != nil {
			return fmt.Errorf("connect %s: %w", c.Server, err)
		}
		c.NC = nc
		js, err := jetstream.New(nc)
		if err != nil {
			nc.Close()
			return fmt.Errorf("jetstream: %w", err)
		}
		c.JS = js
		return nil
	}
	root.PersistentPostRun = func(_ *cobra.Command, _ []string) {
		if c.NC != nil {
			c.NC.Close()
		}
	}
	return root
}

func skipDial(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "help", "version", "completion", "ebctl":
		return true
	}
	for p := cmd.Parent(); p != nil; p = p.Parent() {
		if p.Name() == "completion" {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
