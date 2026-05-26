// bpfman-shell command-line interface using Kong for argument parsing.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/internal/builtins"
	bpfmanbuiltin "github.com/frobware/go-bpfman/cmd/bpfman-shell/internal/builtins/bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/version"
)

// CLI is the root command structure for bpfman-runtime. The binary is
// a script runner and inspection tool: with a positional Script it
// runs the named file; with no positional argument (or Script="-")
// it reads one whole program from stdin; with --check it parses
// without evaluating.
type CLI struct {
	bpfmancli.CLI

	kctx *kong.Context `kong:"-"`

	Script    string `arg:"" optional:"" name:"script" help:"Script file to run; '-' reads a whole program from stdin; omit to read a whole program from stdin."`
	Directory string `name:"directory" short:"C" help:"Change to this directory before doing anything else, like make -C dir. The script path, imported libraries, spawned subprocesses, and external commands all see the new working directory."`
	Check     bool   `name:"check" short:"c" help:"Parse input without evaluating; report syntax errors and exit."`
	NoCheck   bool   `name:"no-check" help:"Skip the static-analysis pre-flight before script evaluation. Default is to run Check first and refuse on errors."`
	AST       bool   `name:"ast" help:"Parse input and print the AST tree of the whole program to stdout; do not evaluate."`
	Lowered   bool   `name:"lowered" help:"Parse input, lower it to the canonical IR, and print the lowered form to stdout; do not evaluate."`
	Trace     bool   `name:"trace" short:"x" help:"Trace each statement to stderr with interpolations resolved, like bash -x. Equivalent to running 'trace on' at script start; toggle with 'trace on' / 'trace off' from within a session."`
	Version   bool   `name:"version" short:"V" help:"Print version information and exit."`
}

// NewCLI creates and initialises a CLI instance by parsing
// command-line arguments.
func NewCLI() (*CLI, error) {
	var c CLI
	c.kctx = kong.Parse(&c, KongOptions()...)
	c.DefaultWriters()

	// Initialise logger eagerly. Skip for --check, --ast,
	// --lowered, and --version, which do no I/O against the
	// manager and must be runnable without access to the system
	// config file.
	if !c.Check && !c.AST && !c.Lowered && !c.Version {
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
	// Apply -C / --directory before anything path-relative
	// runs: opening the script file, the static checker, the
	// manager's bytecode cache, every subprocess spawned at
	// runtime. Matches make -C / git -C semantics: change cwd
	// once, then proceed as if the user had cd'd there manually.
	if c.Directory != "" {
		if err := os.Chdir(c.Directory); err != nil {
			_ = c.PrintErrf("bpfman-shell: error: chdir %q: %v\n", c.Directory, err)
			return err
		}
	}
	if err := c.Run(ctx); err != nil {
		if !errors.Is(err, driver.ErrSilent) {
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
		kong.Description("Development / test / ops companion to bpfman: DSL runner, inspection tool, test scaffolding."),
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

// openInputReader chooses the input source for the CLI's script and
// parse-only modes: the positional script file, stdin via "-", or
// stdin when no positional argument was supplied.
func (c *CLI) openInputReader() (driver.LineReader, error) {
	if c.Script == "" {
		return driver.OpenScriptReader("-")
	}
	return driver.OpenScriptReader(c.Script)
}

// runCheck drives the --check pipeline: open the input source and
// hand it to driver.CheckInput. Returns driver.ErrSilent when any
// issue was emitted so the process exits non-zero without an extra
// message from Kong.
func (c *CLI) runCheck() error {
	reader, err := c.openInputReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	file := c.inputFileLabel()
	if driver.CheckInput(reader, c.Err, file) {
		return driver.ErrSilent
	}
	return nil
}

// runAST drives the --ast pipeline: slurp the whole input, parse it
// as one program, dump the resulting AST to stdout, and exit.
func (c *CLI) runAST() error {
	reader, err := c.openInputReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	file := c.inputFileLabel()
	if driver.ASTInput(reader, c.Out, c.Err, file) {
		return driver.ErrSilent
	}
	return nil
}

// runLowered drives the --lowered pipeline: slurp the whole input,
// parse it as one program, lower it to the canonical IR, dump the
// lowered form to stdout, and exit.
func (c *CLI) runLowered() error {
	reader, err := c.openInputReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	file := c.inputFileLabel()
	if driver.LoweredInput(reader, c.Out, c.Err, file) {
		return driver.ErrSilent
	}
	return nil
}

// Run is the CLI's top-level entry. With --check / --ast /
// --lowered it short-circuits to those parse-only pipelines;
// otherwise it opens the manager, builds a script-runner
// config, and delegates to driver.Run.
func (c *CLI) Run(ctx context.Context) error {
	if c.Check {
		return c.runCheck()
	}
	if c.AST {
		return c.runAST()
	}
	if c.Lowered {
		return c.runLowered()
	}
	mgr, cleanup, err := c.NewManagerWithPuller(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	session := runtime.NewSession()
	if c.Trace {
		session.SetTrace(true)
	}

	lr, err := c.openInputReader()
	if err != nil {
		return err
	}
	defer lr.Close()

	return driver.Run(ctx, driver.Config{
		CLI:            &c.CLI,
		Mgr:            mgr,
		LineReader:     lr,
		Session:        session,
		File:           c.inputFileLabel(),
		NoCheck:        c.NoCheck,
		Fallback:       commandFallback,
		BindFallback:   bindCommandFallback,
		MakeAssertStmt: makeExecAssertStmt,
		MakeAssertIR:   makeExecAssertIR,
	})
}

// commandFallback is the statement-position fallback the runner
// loop calls when no registered builtin matches the first
// token. It owns the "forgot the bpfman prefix" diagnostic;
// unknown first words fall through to external execution.
func commandFallback(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []runtime.Arg, loc driver.SourceLoc, span source.Span) (bool, runtime.Value, error) {
	if len(args) == 0 {
		return false, runtime.Value{}, nil
	}
	first := driver.ArgText(args[0])
	if bpfmanbuiltin.IsTopLevelNoun(first) {
		return true, runtime.Value{}, syntax.SpanErrorf(span, "domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(driver.ArgTexts(args), " "))
	}
	return false, runtime.Value{}, nil
}

// bindCommandFallback is the bind-position fallback the runner
// loop calls when no registered builtin matches. It owns the
// wait and net-exec fast paths (which need the bind's Rc to
// reflect the captured inner outcome) and the "forgot the
// bpfman prefix" diagnostic. Unknown first words fall through
// to external execution.
func bindCommandFallback(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *runtime.Session, env *runtime.Env, args []runtime.Arg, loc driver.SourceLoc, span source.Span) (bool, runtime.BindResult, error) {
	if len(args) == 0 {
		return false, runtime.BindResult{}, nil
	}
	first := driver.ArgText(args[0])

	// 'wait $job' is special-cased so the bind's Rc reflects
	// the JOB's outcome, not merely "wait succeeded".
	if first == "wait" {
		envEnv, err := builtins.WaitEnvelope(ctx, args[1:])
		if err != nil {
			return true, runtime.BindResult{}, err
		}
		return true, runtime.BindResult{Rc: envEnv, Primary: runtime.ValueFromEnvelope(envEnv)}, nil
	}

	// 'net exec $pair CMD...' captures into a real envelope
	// so the bind's Rc reflects the netns command's actual
	// outcome.
	if first == "net" && len(args) >= 2 && driver.ArgText(args[1]) == "exec" {
		envEnv, err := builtins.NetExecEnvelope(ctx, args[2:])
		if err != nil {
			return true, runtime.BindResult{}, err
		}
		return true, runtime.BindResult{Rc: envEnv, Primary: runtime.ValueFromEnvelope(envEnv)}, nil
	}

	if bpfmanbuiltin.IsTopLevelNoun(first) {
		rc := runtime.FailEnvelope()
		rc.Stderr = fmt.Sprintf("domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(driver.ArgTexts(args), " "))
		return true, runtime.BindResult{Rc: rc, Primary: runtime.ValueFromEnvelope(rc)}, nil
	}
	return false, runtime.BindResult{}, nil
}

// runScript is a thin test seam wrapping driver.Loop with the
// bpfman-side fallbacks pre-wired. The full bpfman-shell binary
// goes through Run; tests that want to drive one whole program
// directly over a string source call this wrapper.
func runScript(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr driver.LineReader, session *runtime.Session, file string, _ bool, noCheck bool) error {
	if file == "" || file == "-" {
		file = "<stdin>"
	}
	return driver.Loop(ctx, driver.Config{
		CLI:            cli,
		Mgr:            mgr,
		LineReader:     lr,
		Session:        session,
		File:           file,
		NoCheck:        noCheck,
		Fallback:       commandFallback,
		BindFallback:   bindCommandFallback,
		MakeAssertStmt: makeExecAssertStmt,
		MakeAssertIR:   makeExecAssertIR,
	})
}

func (c *CLI) inputFileLabel() string {
	if c.Script == "" || c.Script == "-" {
		return "<stdin>"
	}
	return c.Script
}
