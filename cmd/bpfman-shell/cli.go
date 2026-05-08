// bpfman-shell command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/alecthomas/kong"
)

// ErrSilent indicates an error has already been reported (e.g. via
// JSON output) and the top-level dispatcher should not print it again.
var ErrSilent = errors.New("silent")

// CLI is the root command structure for bpfman-shell.
type CLI struct {
	Out io.Writer `kong:"-"`
	Err io.Writer `kong:"-"`

	kctx *kong.Context `kong:"-"`

	Version VersionCmd `cmd:"" help:"Print version information."`
}

// WriteOut writes bytes to Out, returning an error if the write fails or
// is short. Use this for all command output to ensure I/O errors are
// propagated.
func (c *CLI) WriteOut(p []byte) error {
	n, err := c.Out.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// PrintOut writes a string to Out, returning an error on failure.
func (c *CLI) PrintOut(s string) error {
	return c.WriteOut([]byte(s))
}

// PrintOutf formats and writes to Out, returning an error on failure.
func (c *CLI) PrintOutf(format string, args ...any) error {
	return c.PrintOut(fmt.Sprintf(format, args...))
}

// WriteErr writes bytes to Err, returning an error if the write fails or
// is short.
func (c *CLI) WriteErr(p []byte) error {
	n, err := c.Err.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// PrintErr writes a string to Err, returning an error on failure.
func (c *CLI) PrintErr(s string) error {
	return c.WriteErr([]byte(s))
}

// PrintErrf formats and writes to Err, returning an error on failure.
func (c *CLI) PrintErrf(format string, args ...any) error {
	return c.PrintErr(fmt.Sprintf(format, args...))
}

// NewCLI creates and initialises a CLI instance by parsing command-line arguments.
func NewCLI() (*CLI, error) {
	if len(os.Args) >= 2 && os.Args[1] == "help" {
		rest := os.Args[2:]
		os.Args = append(append([]string{os.Args[0]}, rest...), "--help")
	}

	var c CLI
	c.kctx = kong.Parse(&c, KongOptions()...)

	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}

	return &c, nil
}

// Execute runs the parsed command.
func (c *CLI) Execute(ctx context.Context) error {
	c.kctx.BindTo(ctx, (*context.Context)(nil))

	if err := c.kctx.Run(c); err != nil {
		if !errors.Is(err, ErrSilent) {
			_ = c.PrintErrf("bpfman-shell: error: %v\n", err)
		}
		return err
	}
	return nil
}

// KongOptions returns the Kong configuration options for bpfman-shell.
func KongOptions() []kong.Option {
	return []kong.Option{
		kong.Name("bpfman-shell"),
		kong.Description("Development / test / ops companion to bpfman: REPL, DSL runner, test scaffolding."),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.ShortUsageOnError(),
	}
}
