// bpfman command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
)

type rootlessCommand interface {
	AllowRootless() bool
}

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
	Get        GetCmd        `cmd:"" hidden:"" help:"Verb-noun compatibility alias (Rust bpfman style)."`
}

// daemonMarkerFlag identifies the bpfman-operator's daemonset
// invocation shape. The operator passes args [--csi-support, ...]
// to a container whose entrypoint is "/bpfman"; we recognise that
// argv leading with --csi-support and inject the "serve" command so
// Kong dispatches to the daemon. Any other argv shape passes
// through unchanged, including kubectl-exec invocations like
// "bpfman get link 5" or "bpfman version".
const daemonMarkerFlag = "--csi-support"

// maybeInjectServe returns argv with the "serve" subcommand inserted
// before the flags when the post-program argv leads with the
// daemon marker flag. Other argv shapes are returned unchanged.
func maybeInjectServe(args []string) []string {
	if len(args) >= 2 && args[1] == daemonMarkerFlag {
		out := make([]string, 0, len(args)+1)
		out = append(out, args[0], "serve")
		out = append(out, args[1:]...)
		return out
	}
	return args
}

// NewCLI creates and initialises a CLI instance by parsing command-line arguments.
//
// When invoked as the bpfman-operator daemonset's bpfman container
// (entrypoint "/bpfman", args leading with --csi-support) the
// "serve" subcommand is inserted before the flags so the gRPC
// daemon starts; see maybeInjectServe.
//
// Note: Namespace helper mode (bpfman-ns) must be checked before calling NewCLI
// via RunNamespaceHelper(), as it uses a completely separate CLI structure.
func NewCLI() (*CLI, error) {
	os.Args = maybeInjectServe(os.Args)

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

	if err := c.enforceRootRequirement(); err != nil {
		_ = c.PrintErrf("bpfman: error: %v\n", err)
		return err
	}

	if err := c.kctx.Run(c); err != nil {
		// ErrSilent means the error was already communicated (e.g., via JSON)
		if !errors.Is(err, ErrSilent) {
			_ = c.PrintErrf("bpfman: error: %v\n", err)
		}
		return err
	}
	return nil
}

func (c *CLI) enforceRootRequirement() error {
	if os.Geteuid() == 0 || selectedCommandAllowsRootless(c.kctx) {
		return nil
	}
	return fmt.Errorf("must run as root")
}

func selectedCommandAllowsRootless(ctx *kong.Context) bool {
	node := ctx.Selected()
	if node == nil || !node.Target.IsValid() || !node.Target.CanAddr() {
		return false
	}
	cmd, ok := node.Target.Addr().Interface().(rootlessCommand)
	return ok && cmd.AllowRootless()
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
		kong.TypeMapper(reflect.TypeFor[bpfmancli.ProgramID](), programIDMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfmancli.LinkID](), linkIDMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfmancli.KeyValue](), keyValueMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfmancli.GlobalData](), globalDataMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfmancli.ObjectPath](), objectPathMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfmancli.ProgramSpec](), programSpecMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.ProgramType](), programTypeMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.LinkKind](), linkKindMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.Tracepoint](), tracepointMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.TCDirection](), tcDirectionMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.XDPAction](), xdpActionMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.TCAction](), tcActionMapper()),
		kong.TypeMapper(reflect.TypeFor[bpfman.ImagePullPolicy](), imagePullPolicyMapper()),
		kong.TypeMapper(reflect.TypeFor[dispatcher.DispatcherType](), dispatcherTypeMapper()),
		kong.TypeMapper(reflect.TypeFor[cliformat.OutputValue](), outputValueMapper()),
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
