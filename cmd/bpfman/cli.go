// bpfman command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/logging"
	"github.com/frobware/go-bpfman/ns/nsenter"
)

// CLI is the root command structure for bpfman.
type CLI struct {
	RuntimeDir    string        `name:"runtime-dir" group:"global" help:"Root directory for runtime files." default:"${default_runtime_dir}"`
	ImageCacheDir string        `name:"image-cache-dir" group:"global" help:"Root directory for OCI image cache." default:"${default_image_cache_dir}"`
	Config        string        `name:"config" group:"global" help:"Config file path." default:"${default_config_path}"`
	Log           string        `name:"log" group:"global" help:"Log spec (e.g., 'info,manager=debug')." env:"BPFMAN_LOG"`
	LockTimeout   time.Duration `name:"lock-timeout" group:"global" help:"Timeout for acquiring the global writer lock (0 for indefinite)." default:"30s"`

	// Out is the writer for command output. Defaults to os.Stdout.
	// Injected for testability.
	Out io.Writer `kong:"-"`
	// Err is the writer for error output. Defaults to os.Stderr.
	// Injected for testability.
	Err io.Writer `kong:"-"`

	// Cached config to avoid repeated file parsing per invocation.
	configOnce   sync.Once     `kong:"-"`
	cachedConfig config.Config `kong:"-"`
	configErr    error         `kong:"-"`

	// Logger is initialised eagerly by initLogger and never changes.
	logger *slog.Logger `kong:"-"`

	// kctx is the parsed Kong context, stored for Execute to dispatch.
	kctx *kong.Context `kong:"-"`

	Program    ProgramCmd    `cmd:"" aliases:"programs" group:"resources" help:"Manage BPF programs."`
	Link       LinkCmd       `cmd:"" aliases:"links" group:"resources" help:"Manage BPF links."`
	Dispatcher DispatcherCmd `cmd:"" aliases:"dispatchers" group:"resources" help:"Manage dispatchers."`
	Image      ImageCmd      `cmd:"" group:"infra" help:"Image operations (verify signatures)."`
	Serve      ServeCmd      `cmd:"" group:"infra" help:"Start the gRPC daemon."`
	GC         GCCmd         `cmd:"" group:"diag" help:"Garbage collect stale resources."`
	Doctor     DoctorCmd     `cmd:"" group:"diag" help:"Check coherency of database, kernel, and filesystem state."`
	Repl       ReplCmd       `cmd:"" group:"diag" help:"Start an interactive inspection shell."`
	Version    VersionCmd    `cmd:"" group:"infra" help:"Print version information."`
}

// Layout returns the filesystem layout for the configured runtime directory.
// Returns an error if RuntimeDir is empty or not an absolute path.
func (c *CLI) Layout() (fs.Layout, error) {
	return fs.New(c.RuntimeDir)
}

// ImageCache returns the image cache for the configured cache directory.
// Returns an error if ImageCacheDir is empty or not an absolute path.
func (c *CLI) ImageCache() (fs.ImageCache, error) {
	return fs.NewImageCache(c.ImageCacheDir)
}

// EnsuredImageCache returns an EnsuredImageCache capability token, creating
// the cache directory if needed. Use this when you need to pass the cache
// to functions that require proof the directory exists.
func (c *CLI) EnsuredImageCache() (fs.EnsuredImageCache, error) {
	cache, err := c.ImageCache()
	if err != nil {
		return fs.EnsuredImageCache{}, err
	}
	return fs.EnsureCache(cache)
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
// Formats in memory first to avoid partial writes on error.
func (c *CLI) PrintOutf(format string, args ...any) error {
	return c.PrintOut(fmt.Sprintf(format, args...))
}

// WriteErr writes bytes to Err, returning an error if the write fails or
// is short. Use this for error output to ensure I/O errors are propagated.
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
// Formats in memory first to avoid partial writes on error.
func (c *CLI) PrintErrf(format string, args ...any) error {
	return c.PrintErr(fmt.Sprintf(format, args...))
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

	// Default writers if not injected (e.g., by tests)
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}

	// Initialise logger eagerly so errors surface immediately
	if err := c.initLogger(); err != nil {
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
			"diag":      "Diagnostics:",
		},
		kong.Help(compactHelpPrinter),
		kong.PostBuild(func(k *kong.Kong) error {
			if k.Model.HelpFlag != nil {
				k.Model.HelpFlag.Value.Help = "Show help (-h for compact, --help for full)."
			}
			return nil
		}),
		kong.ShortUsageOnError(),
		kong.TypeMapper(reflect.TypeOf(ProgramID{}), programIDMapper()),
		kong.TypeMapper(reflect.TypeOf(LinkID{}), linkIDMapper()),
		kong.TypeMapper(reflect.TypeOf(KeyValue{}), keyValueMapper()),
		kong.TypeMapper(reflect.TypeOf(GlobalData{}), globalDataMapper()),
		kong.TypeMapper(reflect.TypeOf(ObjectPath{}), objectPathMapper()),
		kong.TypeMapper(reflect.TypeOf(ProgramSpec{}), programSpecMapper()),
		kong.TypeMapper(reflect.TypeOf(ImagePullPolicy{}), imagePullPolicyMapper()),
		kong.TypeMapper(reflect.TypeOf(OutputValue{}), outputValueMapper()),
		kong.Vars{
			"default_runtime_dir":     "/run/bpfman",
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

// LoadConfig loads the configuration from the config file path.
// Results are cached for the lifetime of the CLI instance.
func (c *CLI) LoadConfig() (config.Config, error) {
	c.configOnce.Do(func() {
		c.cachedConfig, c.configErr = config.Load(c.Config)
	})
	return c.cachedConfig, c.configErr
}

// initLogger initialises the CLI logger. Call this once during CLI setup,
// before any commands run. Returns an error if logger creation fails.
func (c *CLI) initLogger() error {
	cfg, err := c.LoadConfig()
	if err != nil {
		return err
	}

	format, err := logging.ParseFormat(cfg.Logging.Format)
	if err != nil {
		return err
	}

	// CLI commands default to warn unless --log is specified
	spec := c.Log
	if spec == "" {
		spec = "warn"
	}

	opts := logging.Options{
		CLISpec:    spec,
		ConfigSpec: cfg.Logging.ToSpec(),
		Format:     format,
		Output:     os.Stderr,
	}

	c.logger, err = logging.New(opts)
	return err
}

// Logger returns the CLI logger. The logger is initialised during CLI setup
// and never changes. CLI commands default to WARN level for quieter output.
// Use LoggerFromConfig for long-running services like serve.
func (c *CLI) Logger() *slog.Logger {
	return c.logger
}

// LoggerFromConfig creates a logger using config file settings.
// Used by long-running services (serve) where INFO level is appropriate.
// Output goes to stdout for daemon/container log collection.
func (c *CLI) LoggerFromConfig() (*slog.Logger, error) {
	cfg, err := c.LoadConfig()
	if err != nil {
		return nil, err
	}

	format, err := logging.ParseFormat(cfg.Logging.Format)
	if err != nil {
		return nil, err
	}

	opts := logging.Options{
		CLISpec:    c.Log,
		ConfigSpec: cfg.Logging.ToSpec(),
		Format:     format,
		Output:     os.Stdout,
	}

	return logging.New(opts)
}

// RunWithLock wraps mutating CLI operations with the global writer lock.
// The lock ensures serialised access to BPF state across concurrent CLI
// invocations.
func (c *CLI) RunWithLock(ctx context.Context, fn func(context.Context) error) error {
	_, err := RunWithLockValue(ctx, c, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// runBatchMutation executes mutate for each ID under the global
// writer lock, collects errors, and prints failures after releasing
// the lock. Returns a summary error if any mutations failed.
func runBatchMutation[ID ~uint32](
	ctx context.Context,
	cli *CLI,
	ids []ID,
	noun string,
	verb string,
	mutate func(context.Context, ID) error,
) error {
	type result struct {
		id  ID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := RunWithLock(ctx, cli, func(ctx context.Context) error {
		for _, id := range ids {
			err := mutate(ctx, id)
			results = append(results, result{id: id, err: err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("%s %d: %v\n", noun, r.id, r.err)
			failCount++
		}
	}
	if failCount > 0 {
		return fmt.Errorf("%d of %d %s(s) failed to %s", failCount, len(results), noun, verb)
	}
	return nil
}

// RunWithLock executes fn under the global writer lock. Use this pattern
// to perform mutations that don't return a value.
func RunWithLock(ctx context.Context, c *CLI, fn func(context.Context) error) error {
	_, err := RunWithLockValue(ctx, c, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// RunWithLockValue is like RunWithLock but returns a value from the locked
// section. Use this pattern to perform mutations under lock, then format and
// emit output outside the lock to minimise critical section duration.
func RunWithLockValue[T any](ctx context.Context, c *CLI, fn func(context.Context) (T, error)) (T, error) {
	var result T

	// Apply lock timeout if set (0 means indefinite)
	if c.LockTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.LockTimeout)
		defer cancel()
	}

	layout, err := c.Layout()
	if err != nil {
		return result, fmt.Errorf("invalid runtime directory: %w", err)
	}

	err = lock.RunWithTiming(ctx, layout.LockPath(), c.Logger(), func(ctx context.Context, _ lock.WriterScope) error {
		var fnErr error
		result, fnErr = fn(ctx)
		return fnErr
	})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("timed out waiting for lock %s (--lock-timeout=%v)", layout.LockPath(), c.LockTimeout)
		}
		return result, err
	}
	return result, nil
}

// RunWithLockValueAndScope is like RunWithLockValue but provides the
// WriterScope to the callback. This is needed for operations like uprobe
// attachment that may spawn subprocesses requiring lock inheritance.
func RunWithLockValueAndScope[T any](ctx context.Context, c *CLI, fn func(context.Context, lock.WriterScope) (T, error)) (T, error) {
	var result T

	// Apply lock timeout if set (0 means indefinite)
	if c.LockTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.LockTimeout)
		defer cancel()
	}

	layout, err := c.Layout()
	if err != nil {
		return result, fmt.Errorf("invalid runtime directory: %w", err)
	}

	err = lock.RunWithTiming(ctx, layout.LockPath(), c.Logger(), func(ctx context.Context, scope lock.WriterScope) error {
		var fnErr error
		result, fnErr = fn(ctx, scope)
		return fnErr
	})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("timed out waiting for lock %s (--lock-timeout=%v)", layout.LockPath(), c.LockTimeout)
		}
		return result, err
	}
	return result, nil
}
