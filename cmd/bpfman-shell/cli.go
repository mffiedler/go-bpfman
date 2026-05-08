// bpfman-shell command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/logging"
)

// CLI is the root command structure for bpfman-shell.
type CLI struct {
	RuntimeDir    string        `name:"runtime-dir" group:"global" help:"Root directory for runtime files." default:"${default_runtime_dir}"`
	ImageCacheDir string        `name:"image-cache-dir" group:"global" help:"Root directory for OCI image cache." default:"${default_image_cache_dir}"`
	Config        string        `name:"config" group:"global" help:"Config file path." default:"${default_config_path}"`
	Log           string        `name:"log" group:"global" help:"Log spec (e.g., 'info,manager=debug')." env:"BPFMAN_LOG"`
	LockTimeout   time.Duration `name:"lock-timeout" group:"global" help:"Timeout for acquiring the global writer lock (0 for indefinite)." default:"30s"`

	Out io.Writer `kong:"-"`
	Err io.Writer `kong:"-"`

	configOnce   sync.Once     `kong:"-"`
	cachedConfig config.Config `kong:"-"`
	configErr    error         `kong:"-"`

	logger *slog.Logger `kong:"-"`

	kctx *kong.Context `kong:"-"`

	Repl    ReplCmd    `cmd:"" group:"diag" help:"Start an interactive inspection shell."`
	Version VersionCmd `cmd:"" group:"infra" help:"Print version information."`
}

// WithDiscardOutput returns a new CLI with the same
// execution-relevant settings but with output discarded.
func (c *CLI) WithDiscardOutput() *CLI {
	return &CLI{
		RuntimeDir:    c.RuntimeDir,
		ImageCacheDir: c.ImageCacheDir,
		Config:        c.Config,
		Log:           c.Log,
		LockTimeout:   c.LockTimeout,
		Out:           io.Discard,
		Err:           io.Discard,
		logger:        c.logger,
	}
}

// Layout returns the filesystem layout for the configured runtime directory.
func (c *CLI) Layout() (fs.Layout, error) {
	return fs.New(c.RuntimeDir)
}

// ImageCache returns the image cache for the configured cache directory.
func (c *CLI) ImageCache() (fs.ImageCache, error) {
	return fs.NewImageCache(c.ImageCacheDir)
}

// EnsuredImageCache returns an EnsuredImageCache capability token, creating
// the cache directory if needed.
func (c *CLI) EnsuredImageCache() (fs.EnsuredImageCache, error) {
	cache, err := c.ImageCache()
	if err != nil {
		return fs.EnsuredImageCache{}, err
	}
	return fs.EnsureCache(cache)
}

// WriteOut writes bytes to Out, returning an error if the write fails or is short.
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

// WriteErr writes bytes to Err, returning an error if the write fails or is short.
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
	if len(os.Args) == 1 {
		os.Args = append(os.Args, "repl")
	}

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

	// Initialise logger eagerly. Skip for repl --check, which does no I/O
	// and must be runnable without access to the system config file.
	if !c.Repl.Check {
		if err := c.initLogger(); err != nil {
			return nil, fmt.Errorf("create logger: %w", err)
		}
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

// LoadConfig loads the configuration from the config file path.
func (c *CLI) LoadConfig() (config.Config, error) {
	c.configOnce.Do(func() {
		c.cachedConfig, c.configErr = config.Load(c.Config)
	})
	return c.cachedConfig, c.configErr
}

// initLogger initialises the CLI logger.
func (c *CLI) initLogger() error {
	cfg, err := c.LoadConfig()
	if err != nil {
		return err
	}

	format, err := logging.ParseFormat(cfg.Logging.Format)
	if err != nil {
		return err
	}

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

// Logger returns the CLI logger.
func (c *CLI) Logger() *slog.Logger {
	return c.logger
}

// RunWithLock wraps mutating CLI operations with the global writer lock.
func (c *CLI) RunWithLock(ctx context.Context, fn func(context.Context, lock.WriterScope) error) error {
	return RunWithLock(ctx, c, fn)
}

// RunWithLock executes fn under the global writer lock.
func RunWithLock(ctx context.Context, c *CLI, fn func(context.Context, lock.WriterScope) error) error {
	_, err := RunWithLockValue(ctx, c, func(ctx context.Context, writeLock lock.WriterScope) (struct{}, error) {
		return struct{}{}, fn(ctx, writeLock)
	})
	return err
}

// RunWithLockValue is like RunWithLock but returns a value from the locked section.
func RunWithLockValue[T any](ctx context.Context, c *CLI, fn func(context.Context, lock.WriterScope) (T, error)) (T, error) {
	var result T

	if c.LockTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.LockTimeout)
		defer cancel()
	}

	layout, err := c.Layout()
	if err != nil {
		return result, fmt.Errorf("invalid runtime directory: %w", err)
	}

	err = lock.RunWithTiming(ctx, layout.LockPath(), c.Logger(), func(ctx context.Context, writeLock lock.WriterScope) error {
		var fnErr error
		result, fnErr = fn(ctx, writeLock)
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

// runBatchMutation executes mutate for each ID under the global writer lock.
func runBatchMutation[ID ~uint32](
	ctx context.Context,
	cli *CLI,
	ids []ID,
	noun string,
	verb string,
	mutate func(context.Context, lock.WriterScope, ID) error,
) error {
	type result struct {
		id  ID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		for _, id := range ids {
			err := mutate(ctx, writeLock, id)
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
