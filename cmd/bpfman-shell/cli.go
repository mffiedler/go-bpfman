// bpfman-shell command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/version"
)

// CLI is the root command structure for bpfman-shell. The binary is
// a single-purpose REPL/script runner: with no positional argument
// it starts an interactive prompt; with a positional Script it runs
// the named file (or stdin when Script is "-"); with --check it
// parses without evaluating.
type CLI struct {
	bpfmancli.CLI

	kctx *kong.Context `kong:"-"`

	Script  string `arg:"" optional:"" name:"script" help:"Script file to run; '-' reads from stdin; omit for an interactive prompt."`
	Check   bool   `name:"check" short:"c" help:"Parse input without evaluating; report syntax errors and exit."`
	AST     bool   `name:"ast" help:"Parse input and print the AST tree of each chunk to stdout; do not evaluate."`
	Version bool   `name:"version" short:"V" help:"Print version information and exit."`
}

// NewCLI creates and initialises a CLI instance by parsing
// command-line arguments.
func NewCLI() (*CLI, error) {
	var c CLI
	c.kctx = kong.Parse(&c, KongOptions()...)
	c.DefaultWriters()

	// Initialise logger eagerly. Skip for --check, --ast, and
	// --version, which do no I/O against the manager and must
	// be runnable without access to the system config file.
	if !c.Check && !c.AST && !c.Version {
		if err := c.InitLogger(); err != nil {
			return nil, fmt.Errorf("create logger: %w", err)
		}
	}

	return &c, nil
}

// Execute runs the parsed command.
func (c *CLI) Execute(ctx context.Context) error {
	if c.Version {
		return c.PrintOut(version.Get().Long())
	}
	if err := c.Run(ctx); err != nil {
		if !errors.Is(err, ErrSilent) {
			_ = c.PrintErrf("bpfman-shell: error: %v\n", err)
		}
		return err
	}
	return nil
}

// KongOptions returns the Kong configuration options for the CLI.
func KongOptions() []kong.Option {
	return []kong.Option{
		kong.Name("bpfman-shell"),
		kong.Description("Development / test / ops companion to bpfman: REPL, DSL runner, test scaffolding."),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.ShortUsageOnError(),
		kong.Vars{
			"default_runtime_dir":     fs.DefaultRoot,
			"default_image_cache_dir": "/var/cache/bpfman",
			"default_config_path":     "/etc/bpfman/bpfman.toml",
		},
	}
}
