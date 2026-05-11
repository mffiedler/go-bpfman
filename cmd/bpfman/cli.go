// bpfman command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/ns/nsenter"
)

// CLI is the root command structure for bpfman. It embeds the
// shared bpfmancli.CLI for global flags, output writers, and
// runtime services; the Kong-tagged subcommand fields here are
// the production verb set.
type CLI struct {
	bpfmancli.CLI

	kctx *kong.Context `kong:"-"`

	Program    ProgramCmd    `cmd:"" group:"resources" help:"Manage BPF programs."`
	Link       LinkCmd       `cmd:"" group:"resources" help:"Manage BPF links."`
	Dispatcher DispatcherCmd `cmd:"" group:"resources" help:"Manage dispatchers."`
	Image      ImageCmd      `cmd:"" group:"infra" help:"Image operations (verify signatures)."`
	Serve      ServeCmd      `cmd:"" group:"infra" help:"Start the gRPC daemon."`
	Version    VersionCmd    `cmd:"" group:"infra" help:"Print version information."`
}

// NewCLI creates and initialises a CLI instance by parsing command-line arguments.
//
// Mode detection for bpfman-rpc (daemon compatibility with bpfman-operator):
//   - BPFMAN_MODE=bpfman-rpc or argv[0] basename "bpfman-rpc" injects "serve" command
//
// Note: Namespace helper mode (bpfman-ns) must be checked before calling NewCLI
// via RunNamespaceHelper(), as it uses a completely separate CLI structure.
func NewCLI() (*CLI, error) {
	// Check for bpfman-rpc mode (daemon compatibility with bpfman-operator).
	modeEnv := os.Getenv(nsenter.ModeEnvVar)
	if modeEnv == "bpfman-rpc" || filepath.Base(os.Args[0]) == "bpfman-rpc" {
		os.Args = append([]string{os.Args[0], "serve"}, os.Args[1:]...)
	}

	// Rewrite "help [cmd...]" to "[cmd...] --help" so that
	// "bpfman help link attach xdp" works like most CLI tools.
	if len(os.Args) >= 2 && os.Args[1] == "help" {
		rest := os.Args[2:]
		os.Args = append(append([]string{os.Args[0]}, rest...), "--help")
	}

	var c CLI
	c.kctx = kong.Parse(&c, KongOptions()...)
	c.DefaultWriters()

	if err := c.InitLogger(); err != nil {
		return nil, fmt.Errorf("create logger: %w", err)
	}

	return &c, nil
}

// Execute runs the parsed command.
//
// Note: This method is deliberately not named "Run" because Kong looks for
// Run() methods on command structs. If CLI had a Run() method, kctx.Run(c)
// would call it recursively instead of dispatching to the matched subcommand.
func (c *CLI) Execute(ctx context.Context) error {
	c.kctx.BindTo(ctx, (*context.Context)(nil))
	c.kctx.Bind(&c.CLI)

	if err := c.kctx.Run(c); err != nil {
		// ErrSilent means the error was already communicated (e.g., via JSON)
		if !errors.Is(err, ErrSilent) {
			_ = c.PrintErrf("bpfman: error: %v\n", err)
		}
		return err
	}
	return nil
}

// KongOptions returns the Kong configuration options for the CLI.
func KongOptions() []kong.Option {
	return []kong.Option{
		kong.Name("bpfman"),
		kong.Description("BPF program manager with integrated CSI driver."),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.Groups{
			"global":    "Global Flags:",
			"resources": "BPF Resources:",
			"infra":     "Infrastructure:",
		},
		kong.Help(compactHelpPrinter),
		kong.PostBuild(func(k *kong.Kong) error {
			if k.Model.HelpFlag != nil {
				k.Model.HelpFlag.Value.Help = "Show help (-h for compact, --help for full)."
			}
			return nil
		}),
		kong.ShortUsageOnError(),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.ProgramID{}), programIDMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.LinkID{}), linkIDMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.KeyValue{}), keyValueMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.GlobalData{}), globalDataMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.ObjectPath{}), objectPathMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.ProgramSpec{}), programSpecMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.TracepointName{}), tracepointNameMapper()),
		kong.TypeMapper(reflect.TypeOf(bpfmancli.ImagePullPolicy{}), imagePullPolicyMapper()),
		kong.TypeMapper(reflect.TypeOf(cliformat.OutputValue{}), outputValueMapper()),
		kong.Vars{
			"default_runtime_dir":     fs.DefaultRoot,
			"default_image_cache_dir": "/var/cache/bpfman",
			"default_config_path":     "/etc/bpfman/bpfman.toml",
		},
	}
}

// compactHelpPrinter wraps Kong's default help printer. When invoked
// via -h it omits the global flags group for a more focused output.
// With --help the full output is shown. Command aliases are always
// suppressed from help output to keep it clean; the aliases still
// work for command resolution.
func compactHelpPrinter(options kong.HelpOptions, ctx *kong.Context) error {
	short := slices.Contains(os.Args[1:], "-h")

	// Temporarily strip aliases from all nodes so the default
	// printer does not append "(aliases)" after command names.
	type saved struct {
		node    *kong.Node
		aliases []string
	}
	var restored []saved
	var strip func(n *kong.Node)
	strip = func(n *kong.Node) {
		if len(n.Aliases) > 0 {
			restored = append(restored, saved{n, n.Aliases})
			n.Aliases = nil
		}
		for _, child := range n.Children {
			strip(child)
		}
	}
	strip(ctx.Model.Node)
	defer func() {
		for _, s := range restored {
			s.node.Aliases = s.aliases
		}
	}()

	if short {
		// Temporarily hide global-group flags so the default
		// printer skips them via AllFlags(hide=true).
		var hidden []*kong.Flag
		for _, flag := range ctx.Model.Node.Flags {
			if flag.Group != nil && flag.Group.Key == "global" && !flag.Hidden {
				flag.Hidden = true
				hidden = append(hidden, flag)
			}
		}
		err := kong.DefaultHelpPrinter(options, ctx)
		for _, flag := range hidden {
			flag.Hidden = false
		}
		return err
	}

	return kong.DefaultHelpPrinter(options, ctx)
}
