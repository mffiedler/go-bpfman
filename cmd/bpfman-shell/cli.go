// bpfman-shell command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

// CLI is the root command structure for bpfman-shell. It embeds the
// shared bpfmancli.CLI for global flags, output writers, and runtime
// services; the Kong-tagged subcommand fields here are the
// dev/test/ops verb set.
type CLI struct {
	bpfmancli.CLI

	kctx *kong.Context `kong:"-"`

	Repl    ReplCmd    `cmd:"" group:"diag" help:"Start an interactive inspection shell."`
	Version VersionCmd `cmd:"" group:"infra" help:"Print version information."`
}

// NewCLI creates and initialises a CLI instance by parsing command-line arguments.
func NewCLI() (*CLI, error) {
	if len(os.Args) == 1 {
		os.Args = append(os.Args, "repl")
	}

	if len(os.Args) >= 2 && os.Args[1] == "help" {
		rest := os.Args[2:]
		os.Args = append(append([]string{os.Args[0]}, rest...), "--help")
	}

	var c CLI
	c.kctx = kong.Parse(&c, KongOptions()...)
	c.DefaultWriters()

	// Initialise logger eagerly. Skip for repl --check, which does no I/O
	// and must be runnable without access to the system config file.
	if !c.Repl.Check {
		if err := c.InitLogger(); err != nil {
			return nil, fmt.Errorf("create logger: %w", err)
		}
	}

	return &c, nil
}

// Execute runs the parsed command.
func (c *CLI) Execute(ctx context.Context) error {
	c.kctx.BindTo(ctx, (*context.Context)(nil))
	c.kctx.Bind(&c.CLI)

	if err := c.kctx.Run(c); err != nil {
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
		kong.Groups{
			"global": "Global Flags:",
			"infra":  "Infrastructure:",
			"diag":   "Diagnostics:",
		},
		kong.ShortUsageOnError(),
		kong.Vars{
			"default_runtime_dir":     fs.DefaultRoot,
			"default_image_cache_dir": "/var/cache/bpfman",
			"default_config_path":     "/etc/bpfman/bpfman.toml",
		},
	}
}
