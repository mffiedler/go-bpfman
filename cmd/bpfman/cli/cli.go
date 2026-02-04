// Package cli provides the command-line interface for bpfman.
// It uses Kong for argument parsing and calls the manager directly
// for BPF operations.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/logging"
	"github.com/frobware/go-bpfman/nsenter"
)

// CLI is the root command structure for bpfman.
type CLI struct {
	RuntimeDir  string        `name:"runtime-dir" help:"Runtime directory base path." default:"${default_runtime_dir}"`
	Config      string        `name:"config" help:"Config file path." default:"${default_config_path}"`
	Log         string        `name:"log" help:"Log spec (e.g., 'info,manager=debug')." env:"BPFMAN_LOG"`
	LockTimeout time.Duration `name:"lock-timeout" help:"Timeout for acquiring the global writer lock (0 for indefinite)." default:"30s"`

	// Out is the writer for command output. Defaults to os.Stdout.
	// Injected for testability.
	Out io.Writer `kong:"-"`
	// Err is the writer for error output. Defaults to os.Stderr.
	// Injected for testability.
	Err io.Writer `kong:"-"`

	// Cached config and logger to avoid repeated file parsing per invocation.
	configOnce   sync.Once     `kong:"-"`
	cachedConfig config.Config `kong:"-"`
	configErr    error         `kong:"-"`
	loggerOnce   sync.Once     `kong:"-"`
	cachedLogger *slog.Logger  `kong:"-"`
	loggerErr    error         `kong:"-"`

	Serve  ServeCmd  `cmd:"" help:"Start the gRPC daemon."`
	Load   LoadCmd   `cmd:"" help:"Load a BPF program from an object file."`
	Unload UnloadCmd `cmd:"" help:"Unload a managed BPF program."`
	Attach AttachCmd `cmd:"" help:"Attach a loaded program to a hook."`
	Detach DetachCmd `cmd:"" help:"Detach a link."`
	List   ListCmd   `cmd:"" help:"List managed programs or links."`
	Get    GetCmd    `cmd:"" help:"Get a loaded eBPF program or program attachment link."`
	GC     GCCmd     `cmd:"" help:"Garbage collect stale resources."`
	Doctor DoctorCmd `cmd:"" help:"Check coherency of database, kernel, and filesystem state."`
	Image  ImageCmd  `cmd:"" help:"Image operations (verify signatures)."`
}

// RuntimeDirs returns the runtime directories configuration.
// Returns an error if RuntimeDir is empty or not an absolute path.
func (c *CLI) RuntimeDirs() (config.RuntimeDirs, error) {
	return config.NewRuntimeDirs(c.RuntimeDir)
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

// Run parses command-line arguments and executes the selected command.
//
// Mode detection:
//   - BPFMAN_MODE env var is authoritative when set (handled by helper detection)
//   - argv[0] basename provides symlink/binary name compatibility
//
// Modes:
//   - "bpfman-ns": namespace helper for container uprobes (early exit)
//   - "bpfman-rpc": serve command (for bpfman-operator compatibility)
//   - otherwise: normal CLI parsing
func Run(ctx context.Context) {
	// Check for namespace helper subprocess mode (used for container uprobes).
	// This needs early handling before the main CLI is set up because the
	// subprocess runs in a different mount namespace.
	// Helper detection owns BPFMAN_MODE interpretation and returns errors for
	// unknown values.
	modeEnv := os.Getenv(nsenter.ModeEnvVar)
	handled, err := HandleNamespaceHelperInvocation(os.Args, modeEnv, runNamespaceHelper)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bpfman: error: %v\n", err)
		os.Exit(1)
	}
	if handled {
		return
	}

	// Check for bpfman-rpc mode (daemon compatibility with bpfman-operator).
	// BPFMAN_MODE takes precedence over argv[0] for explicit configuration.
	// By this point, if BPFMAN_MODE is set, it's "bpfman-rpc" (helper detection
	// handles "bpfman-ns" and rejects unknown values).
	if modeEnv == "bpfman-rpc" || filepath.Base(os.Args[0]) == "bpfman-rpc" {
		os.Args = append([]string{os.Args[0], "serve"}, os.Args[1:]...)
	}

	var c CLI
	kctx := kong.Parse(&c, KongOptions()...)

	// Default writers if not injected (e.g., by tests)
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}

	kctx.BindTo(ctx, (*context.Context)(nil))

	// Handle errors ourselves to use injected writers instead of Kong's
	// default os.Stderr. This ensures I/O error propagation is consistent.
	if err := kctx.Run(&c); err != nil {
		// ErrSilent means the error was already communicated (e.g., via JSON)
		// so we exit non-zero without printing anything additional.
		if !errors.Is(err, ErrSilent) {
			// Attempt to write error to stderr. If stderr write fails, we still
			// exit with code 1 - there's nothing more useful we can do.
			_ = c.PrintErrf("bpfman: error: %v\n", err)
		}
		os.Exit(1)
	}
}

// KongOptions returns the Kong configuration options for the CLI.
func KongOptions() []kong.Option {
	return []kong.Option{
		kong.Name("bpfman"),
		kong.Description("BPF program manager with integrated CSI driver."),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.ShortUsageOnError(),
		kong.TypeMapper(reflect.TypeOf(ProgramID{}), programIDMapper()),
		kong.TypeMapper(reflect.TypeOf(LinkID{}), linkIDMapper()),
		kong.TypeMapper(reflect.TypeOf(KeyValue{}), keyValueMapper()),
		kong.TypeMapper(reflect.TypeOf(GlobalData{}), globalDataMapper()),
		kong.TypeMapper(reflect.TypeOf(ObjectPath{}), objectPathMapper()),
		kong.TypeMapper(reflect.TypeOf(ProgramSpec{}), programSpecMapper()),
		kong.TypeMapper(reflect.TypeOf(ImagePullPolicy{}), imagePullPolicyMapper()),
		kong.Vars{
			"default_runtime_dir": "/run/bpfman",
			"default_config_path": "/etc/bpfman/bpfman.toml",
		},
	}
}

// LoadConfig loads the configuration from the config file path.
// Results are cached for the lifetime of the CLI instance.
func (c *CLI) LoadConfig() (config.Config, error) {
	c.configOnce.Do(func() {
		c.cachedConfig, c.configErr = config.Load(c.Config)
	})
	return c.cachedConfig, c.configErr
}

// Logger creates a logger for CLI commands.
// CLI commands default to WARN level for quieter output.
// Results are cached for the lifetime of the CLI instance.
// Use LoggerFromConfig for long-running services like serve.
func (c *CLI) Logger() (*slog.Logger, error) {
	c.loggerOnce.Do(func() {
		cfg, err := c.LoadConfig()
		if err != nil {
			c.loggerErr = err
			return
		}

		format, err := logging.ParseFormat(cfg.Logging.Format)
		if err != nil {
			c.loggerErr = err
			return
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

		c.cachedLogger, c.loggerErr = logging.New(opts)
	})
	return c.cachedLogger, c.loggerErr
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

	dirs, err := c.RuntimeDirs()
	if err != nil {
		return result, fmt.Errorf("invalid runtime directory: %w", err)
	}
	logger, logErr := c.Logger()
	if logErr != nil {
		// Fall back to default logger if config parsing fails.
		// This allows operations to proceed even with invalid logging config.
		logger = slog.Default()
	}

	err = lock.RunWithTiming(ctx, dirs.Lock(), logger, func(ctx context.Context, _ lock.WriterScope) error {
		var fnErr error
		result, fnErr = fn(ctx)
		return fnErr
	})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("timed out waiting for lock %s (--lock-timeout=%v)", dirs.Lock(), c.LockTimeout)
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

	dirs, err := c.RuntimeDirs()
	if err != nil {
		return result, fmt.Errorf("invalid runtime directory: %w", err)
	}
	logger, logErr := c.Logger()
	if logErr != nil {
		logger = slog.Default()
	}

	err = lock.RunWithTiming(ctx, dirs.Lock(), logger, func(ctx context.Context, scope lock.WriterScope) error {
		var fnErr error
		result, fnErr = fn(ctx, scope)
		return fnErr
	})

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("timed out waiting for lock %s (--lock-timeout=%v)", dirs.Lock(), c.LockTimeout)
		}
		return result, err
	}
	return result, nil
}
