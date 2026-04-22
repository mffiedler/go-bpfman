package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/coherency"
	"github.com/frobware/go-bpfman/shell"
	"github.com/frobware/go-bpfman/version"
)

// ReplCmd starts an interactive shell for inspecting BPF state.
// When --file is given, commands are read from the named file. When
// stdin is not a terminal, commands are read from stdin. Otherwise an
// interactive readline prompt is started.
type ReplCmd struct {
	File  string `name:"file" short:"f" help:"Read commands from a file (use '-' for stdin)."`
	Check bool   `name:"check" short:"c" help:"Parse input without evaluating; report syntax errors and exit."`
}

// errRequireFailed is the sentinel error used to halt script execution
// when a require assertion fails.
var errRequireFailed = errors.New("require failed")

// errScriptError is the sentinel error used to halt script execution
// when a command error occurs in file mode. The error message has
// already been printed with a source location prefix.
var errScriptError = errors.New("script error")

// assertResult holds the outcome of evaluating an assertion verb.
type assertResult struct {
	pass    bool
	message string
}

// Run starts the read-eval-print loop. A single manager is held open
// for the session lifetime to avoid repeated store open/close. When
// --check is set, Run short-circuits to a parse-only mode that reads
// the same input, reports syntax errors, and exits without touching
// the manager, session, or evaluator.
func (c *ReplCmd) Run(cli *CLI, ctx context.Context) error {
	if c.Check {
		return c.runCheck(cli)
	}
	mgr, cleanup, err := cli.NewManagerWithPuller(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	session := shell.NewSession()

	lr, err := c.newReader(ctx, mgr, session)
	if err != nil {
		return err
	}
	defer lr.Close()

	file := c.File
	if file == "-" || (file == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		file = "<stdin>"
	}
	loopErr := replLoop(ctx, cli, mgr, lr, session, file)

	if errors.Is(loopErr, errRequireFailed) || errors.Is(loopErr, errScriptError) {
		return ErrSilent
	}
	if loopErr != nil {
		return loopErr
	}

	if n := session.AssertFailures(); n > 0 {
		_ = cli.PrintErrf("%d assertion(s) failed\n", n)
		return fmt.Errorf("%d assertion(s) failed", n)
	}

	return nil
}

// newReader selects the appropriate LineReader: file, pipe, or
// interactive readline.
func (c *ReplCmd) newReader(ctx context.Context, mgr *manager.Manager, session *shell.Session) (LineReader, error) {
	if c.File != "" {
		return openScriptReader(c.File)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return NewScannerReader(os.Stdin, nil), nil
	}
	historyPath, err := replHistoryPath()
	if err != nil {
		return nil, fmt.Errorf("history path: %w", err)
	}
	return NewLineReader("bpfman> ", historyPath, replCompleter(ctx, mgr, session))
}

// runCheck drives the --check pipeline: read chunks of input, feed
// each completed chunk through Tokenise and Parse, and report the
// first error from each stage with a file:line: prefix. No Session,
// Manager, or evaluator is involved. Returns ErrSilent when any
// error was reported so the process exits non-zero without an extra
// message from Kong.
func (c *ReplCmd) runCheck(cli *CLI) error {
	reader, err := c.checkReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	file := c.File
	if file == "-" || (file == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		file = "<stdin>"
	}
	if replCheckInput(reader, cli.Err, file) {
		return ErrSilent
	}
	return nil
}

// checkReader chooses the input source for --check: the named file,
// or stdin. Unlike Run's newReader it never falls back to an
// interactive line editor because --check is a batch operation.
func (c *ReplCmd) checkReader() (LineReader, error) {
	if c.File != "" {
		return openScriptReader(c.File)
	}
	return NewScannerReader(os.Stdin, nil), nil
}

// replCheckInput reads from r, accumulates lines until brace and
// bracket depth balances (mirroring replLoop), and checks each
// accumulated chunk via shell.Tokenise and shell.Parse. Errors are
// written to errOut with a file:line: prefix. Returns true when any
// error was emitted so the caller can signal a non-zero exit.
func replCheckInput(r LineReader, errOut io.Writer, file string) bool {
	var lineNo int
	var buf strings.Builder
	var startLine int
	var cs contState
	hadErrors := false

	reportErr := func(line int, err error) {
		hadErrors = true
		loc := sourceLoc{file: file, line: line}
		fmt.Fprintf(errOut, "%s[check] error: %v\n", loc, err)
	}

	for {
		input, err := r.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				if buf.Len() > 0 {
					reportErr(startLine, fmt.Errorf("unterminated block at end of input"))
				}
				break
			}
			reportErr(lineNo, err)
			break
		}
		lineNo++

		if buf.Len() == 0 {
			startLine = lineNo
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(input)
		cs.advance(input)
		if cs.open() {
			continue
		}

		accumulated := buf.String()
		buf.Reset()
		cs = contState{}

		tokens, tokErr := shell.Tokenise(accumulated)
		if tokErr != nil {
			reportErr(startLine, tokErr)
			continue
		}
		if len(tokens) == 0 {
			continue
		}
		if _, parseErr := shell.Parse(tokens); parseErr != nil {
			reportErr(startLine, parseErr)
		}
	}
	return hadErrors
}

// openScriptReader opens a file for reading commands. Use "-" to
// read from stdin.
func openScriptReader(path string) (LineReader, error) {
	if path == "-" {
		return NewScannerReader(os.Stdin, nil), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open script: %w", err)
	}
	return NewScannerReader(f, f), nil
}

// sourceLoc identifies a position in a script file. The zero value
// means "no location" and formats as the empty string, so interactive
// and piped-stdin modes are unaffected.
type sourceLoc struct {
	file string
	line int
}

func (l sourceLoc) String() string {
	if l.file == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d: ", l.file, l.line)
}

// replLoop reads lines from lr and dispatches them until EOF or
// interrupt. Blank lines and comments are handled by
// shell.Tokenise. Variable assignment and expansion use the
// shell.Session. When file is non-empty, error messages include a
// file:line: prefix for compiler-style diagnostics.
func replLoop(ctx context.Context, cli *CLI, mgr *manager.Manager, lr LineReader, session *shell.Session, file string) error {
	var lineNo int
	var buf strings.Builder
	var startLine int
	var cs contState
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				if buf.Len() > 0 {
					loc := sourceLoc{}
					if file != "" {
						loc = sourceLoc{file: file, line: startLine}
					}
					_ = cli.PrintErrf("%s[repl] error: unterminated block at end of input\n", loc)
				}
				return nil
			}
			return err
		}
		lineNo++

		if buf.Len() == 0 {
			startLine = lineNo
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(input)
		cs.advance(input)

		if cs.open() {
			continue
		}

		accumulated := buf.String()
		buf.Reset()
		cs = contState{}

		var loc sourceLoc
		if file != "" {
			loc = sourceLoc{file: file, line: startLine}
		}

		if err := replEval(ctx, cli, mgr, session, accumulated, loc); err != nil {
			return err
		}
	}
}

// contState tracks brace and bracket depth across accumulated input
// lines so the REPL knows when a multi-line if-block or command
// substitution is complete. Quote state persists across lines so
// multi-line quoted strings are treated as a single literal span;
// unterminated strings themselves are surfaced by the tokeniser
// when the accumulated chunk is eventually parsed.
type contState struct {
	braces, brackets, parens int
	inSingle, inDouble       bool
}

// advance walks one line of input, updating the brace and bracket
// counters. Comments (`#` to end of line) outside a quoted string
// are ignored; quoted content is skipped so braces and brackets
// inside strings do not count. The in-string flags are fields on
// the struct so they survive across line boundaries, matching how
// the tokeniser actually treats multi-line quoted literals.
func (c *contState) advance(line string) {
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\'' && !c.inDouble:
			c.inSingle = !c.inSingle
		case ch == '"' && !c.inSingle:
			c.inDouble = !c.inDouble
		case c.inSingle || c.inDouble:
			// ignore content inside strings
		case ch == '#':
			return
		case ch == '{':
			c.braces++
		case ch == '}':
			if c.braces > 0 {
				c.braces--
			}
		case ch == '[':
			c.brackets++
		case ch == ']':
			if c.brackets > 0 {
				c.brackets--
			}
		case ch == '(':
			c.parens++
		case ch == ')':
			if c.parens > 0 {
				c.parens--
			}
		}
	}
}

// open reports whether the accumulated input is still inside an
// open brace, bracket, or parenthesised group.
func (c *contState) open() bool {
	return c.braces > 0 || c.brackets > 0 || c.parens > 0
}

// shellCommands is the set of commands that are shell-language or
// session concerns rather than bpfman domain commands. These are
// handled directly by replEval and never reach the domain command
// dispatcher.
var shellCommands = map[string]bool{
	"alias":   true,
	"aliases": true,
	"assert":  true,
	"exec":    true,
	"file":    true,
	"jq":      true,
	"require": true,
	"dump":    true,
	"help":    true,
	"source":  true,
	"unalias": true,
	"unset":   true,
	"vars":    true,
	"version": true,
}

// replShellCmd handles shell-language and session commands. It returns
// (true, value, err) if the command was handled, where value is
// non-nil for commands that produce an assignable result (e.g. exec).
// Returns (false, Value{}, nil) if the command is not a shell command
// and should be dispatched to the domain layer.
func replShellCmd(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, loc sourceLoc) (bool, shell.Value, error) {
	if len(args) == 0 {
		return false, shell.Value{}, nil
	}
	cmd := argText(args[0])
	if !shellCommands[cmd] {
		return false, shell.Value{}, nil
	}

	switch cmd {
	case "alias":
		return true, shell.Value{}, replAlias(cli, session, argTexts(args[1:]))
	case "aliases":
		return true, shell.Value{}, replAliases(cli, session)
	case "assert":
		return true, shell.Value{}, replAssertRequire(ctx, cli, mgr, session, args[1:], false, loc)
	case "exec":
		val, err := replExec(ctx, cli, args[1:])
		return true, val, err
	case "file":
		val, err := replFile(cli, args[1:])
		return true, val, err
	case "jq":
		val, err := replJQ(args[1:])
		return true, val, err
	case "require":
		return true, shell.Value{}, replAssertRequire(ctx, cli, mgr, session, args[1:], true, loc)
	case "dump":
		return true, shell.Value{}, replDump(cli, args[1:])
	case "help":
		return true, shell.Value{}, replHelp(cli)
	case "source":
		return true, shell.Value{}, replSource(ctx, cli, mgr, session, argTexts(args[1:]))
	case "unalias":
		return true, shell.Value{}, replUnalias(cli, session, argTexts(args[1:]))
	case "unset":
		return true, shell.Value{}, replUnset(cli, session, argTexts(args[1:]))
	case "vars":
		return true, shell.Value{}, replVars(cli, session)
	case "version":
		return true, shell.Value{}, replVersion(cli)
	default:
		return false, shell.Value{}, nil
	}
}

// replEval processes a single input line or block: tokenise, parse
// to an AST, and evaluate against the session. Shell-language
// commands (assert, require, dump, help, source, unset, vars,
// version) flow through ExecCommand on the evaluator's Env; domain
// commands are dispatched via replDispatch from the same hook. In
// interactive mode (loc has no file), non-fatal errors are printed
// and replEval returns nil so the session continues. In script mode
// (loc has a file), errors return errScriptError to halt execution.
func replEval(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, input string, loc sourceLoc) error {
	scriptErr := func(format string, args ...any) error {
		_ = cli.PrintErrf(format, args...)
		if loc.file != "" {
			return errScriptError
		}
		return nil
	}

	tokens, err := shell.Tokenise(input)
	if err != nil {
		return scriptErr("%s[repl] error: %v\n", loc, err)
	}
	if len(tokens) == 0 {
		return nil
	}

	prog, err := shell.Parse(tokens)
	if err != nil {
		return scriptErr("%s[repl] error: %v\n", loc, err)
	}

	env := &shell.Env{
		Session:          session,
		ExecCommand:      makeExecCommand(ctx, cli, mgr, session, loc),
		ExecSubstitution: makeExecSubstitution(ctx, cli, mgr, session, loc),
		PrintResult: func(v shell.Value) error {
			return writeValue(cli, v)
		},
	}

	if err := shell.EvalProgram(prog, env); err != nil {
		if errors.Is(err, errRequireFailed) {
			return err
		}
		return scriptErr("%s[repl] error: %v\n", loc, err)
	}
	return nil
}

// makeExecCommand bridges the evaluator's top-level CommandStmt
// dispatch into the REPL pipeline. Output is visible on the CLI.
// Alias expansion applies to the first argument before dispatch.
// The returned Value is ignored by the evaluator for top-level
// commands; it is still produced so shell builtins can compute
// values that callers happen to observe in tests.
func makeExecCommand(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, loc sourceLoc) func([]shell.Arg) (shell.Value, error) {
	return func(args []shell.Arg) (shell.Value, error) {
		if len(args) == 0 {
			return shell.Value{}, nil
		}
		args = applyAlias(session, args)
		handled, val, err := replShellCmd(ctx, cli, mgr, session, args, loc)
		if err != nil {
			return shell.Value{}, err
		}
		if handled {
			return val, nil
		}
		return replDispatch(ctx, cli, mgr, args)
	}
}

// makeExecSubstitution bridges the evaluator's CmdSubExpr dispatch.
// Output is suppressed so bindings do not clutter the terminal; the
// returned Value must be non-nil or the caller reports an error.
// Alias expansion applies to the first argument before dispatch.
func makeExecSubstitution(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, loc sourceLoc) func([]shell.Arg) (shell.Value, error) {
	return func(args []shell.Arg) (shell.Value, error) {
		args = applyAlias(session, args)
		if len(args) == 0 {
			return shell.Value{}, fmt.Errorf("empty command substitution")
		}
		quiet := cli.WithDiscardOutput()
		handled, val, err := replShellCmd(ctx, quiet, mgr, session, args, loc)
		if err != nil {
			return shell.Value{}, err
		}
		if handled {
			if val.IsNil() {
				return shell.Value{}, fmt.Errorf("command %q produces no assignable value", argText(args[0]))
			}
			return val, nil
		}
		val, err = replDispatch(ctx, quiet, mgr, args)
		if err != nil {
			return shell.Value{}, err
		}
		if val.IsNil() {
			return shell.Value{}, fmt.Errorf("command %q produces no assignable value", argText(args[0]))
		}
		return val, nil
	}
}

// argText extracts the text from a single Arg. For text-bearing
// variants (WordArg, QuotedArg, ScalarValueArg) this returns the
// text directly. For StructuredValueArg this returns "$name" as a
// display form suitable for error messages.
func argText(a shell.Arg) string {
	switch v := a.(type) {
	case shell.WordArg:
		return v.Text
	case shell.QuotedArg:
		return v.Text
	case shell.ScalarValueArg:
		return v.Text
	case shell.StructuredValueArg:
		return "$" + v.Name
	case shell.AdapterArg:
		if v.Path != "" {
			return fmt.Sprintf("%s:$%s.%s", v.Adapter, v.Name, v.Path)
		}
		return fmt.Sprintf("%s:$%s", v.Adapter, v.Name)
	default:
		return ""
	}
}

// argTexts extracts plain strings from all Args. This is the
// conversion boundary for passing expanded arguments to Kong parsers
// and handlers that operate on resolved string values. Structured
// values should already have been extracted by typed helpers before
// this point; any remaining StructuredValueArg is rendered as
// "$name" for display.
func argTexts(args []shell.Arg) []string {
	ss := make([]string, len(args))
	for i, a := range args {
		ss[i] = argText(a)
	}
	return ss
}

type replContextKey int

const replSourcingKey replContextKey = iota

// replSource reads commands from a file and executes each line in the
// current session. The sourced file shares all variable bindings with
// the caller. Nested source commands are rejected to prevent
// unbounded recursion.
func replSource(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []string) error {
	if ctx.Value(replSourcingKey) != nil {
		return fmt.Errorf("source cannot be used inside a sourced file")
	}
	if len(args) != 1 {
		return fmt.Errorf("source requires exactly one file argument")
	}

	lr, err := openScriptReader(args[0])
	if err != nil {
		return err
	}
	defer lr.Close()

	ctx = context.WithValue(ctx, replSourcingKey, true)

	var lineNo int
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		lineNo++

		loc := sourceLoc{file: args[0], line: lineNo}
		if err := replEval(ctx, cli, mgr, session, input, loc); err != nil {
			return err
		}
	}
}

// domainNouns is the set of top-level words that parseCommand
// recognises after a leading "bpfman". It exists so replDispatch can
// distinguish "forgot the bpfman prefix" (suggest prefixing) from
// "this word is not a command at all" (just say unknown).
var domainNouns = map[string]bool{
	"list":       true,
	"program":    true,
	"programs":   true,
	"load":       true,
	"show":       true,
	"link":       true,
	"dispatcher": true,
	"gc":         true,
	"doctor":     true,
}

// replDispatch routes expanded domain command arguments to the
// appropriate bpfman command handler. Shell-language commands (assert,
// require, dump, help, source, unset, vars, version) are handled by
// replShellCmd before reaching this function.
//
// Parsing and execution are fully decoupled: parseCommand routes
// arguments to the per-command parser and returns a typed Command
// node, then execCommand dispatches execution via a type-switch.
func replDispatch(ctx context.Context, cli *CLI, mgr *manager.Manager, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 {
		return shell.Value{}, nil
	}
	first := argText(args[0])
	if first != "bpfman" {
		if domainNouns[first] {
			return shell.Value{}, fmt.Errorf("domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(argTexts(args), " "))
		}
		return shell.Value{}, fmt.Errorf("unknown command: %s", first)
	}
	cmd, err := parseCommand(args[1:])
	if err != nil {
		return shell.Value{}, err
	}
	if cmd == nil {
		return shell.Value{}, fmt.Errorf("missing command after \"bpfman\"; try \"bpfman program list\"")
	}
	return execCommand(ctx, cli, mgr, cmd)
}

func replHelp(cli *CLI) error {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	b.WriteString("\n")
	b.WriteString("Domain commands (require \"bpfman\" prefix):\n")
	b.WriteString("\n")
	b.WriteString("  Program management:\n")
	b.WriteString("    bpfman program list [flags]                     List managed BPF programs\n")
	b.WriteString("    bpfman program get <id>                         Get program details (assignable)\n")
	b.WriteString("    bpfman program load file [flags]                Load from a local object file (assignable)\n")
	b.WriteString("    bpfman program load image [flags]               Load from an OCI image (assignable)\n")
	b.WriteString("    bpfman program unload <id>...                   Unload programs\n")
	b.WriteString("    bpfman program delete (<id>... | --all) [-r]    Delete with cascading cleanup\n")
	b.WriteString("    bpfman show program <id> [view] [-o]            Inspect (views: links, maps, paths)\n")
	b.WriteString("\n")
	b.WriteString("  Link management:\n")
	b.WriteString("    bpfman link attach <type> [flags] <id>          Attach a program (assignable)\n")
	b.WriteString("    bpfman link detach <link-id>...                 Detach links\n")
	b.WriteString("    bpfman link get <link-id>                       Get link details (assignable)\n")
	b.WriteString("    bpfman link list [flags]                        List managed links\n")
	b.WriteString("    bpfman link delete <link-id>... [-r]            Delete with cascading cleanup\n")
	b.WriteString("\n")
	b.WriteString("  Dispatcher management:\n")
	b.WriteString("    bpfman dispatcher list [--type <type>]           List dispatchers\n")
	b.WriteString("    bpfman dispatcher get <type> <nsid> <ifindex>    Get dispatcher details\n")
	b.WriteString("    bpfman dispatcher delete <type> <nsid> <ifindex> Delete a dispatcher\n")
	b.WriteString("\n")
	b.WriteString("  Diagnostics:\n")
	b.WriteString("    bpfman gc [--dry-run] [--prune] [rule...]       Garbage collect stale resources\n")
	b.WriteString("    bpfman doctor [checkup]                         Run coherency checks\n")
	b.WriteString("    bpfman doctor explain [rule]                    Explain a coherency rule\n")
	b.WriteString("\n")
	b.WriteString("Shell commands:\n")
	b.WriteString("\n")
	b.WriteString("  exec <command> [args|file:$var...]        Run a host command (assignable)\n")
	b.WriteString("  exec status <command> [args...]           Run, capture all exit codes (assignable)\n")
	b.WriteString("  file temp $var[.path] | [expr]            Write value to temp file (assignable)\n")
	b.WriteString("  jq <filter> <value>                       Apply a jq filter to a value (assignable)\n")
	b.WriteString("  dump $var[.path] | [expr]                 Print a resolved value\n")
	b.WriteString("  source <file>                            Execute commands from a file\n")
	b.WriteString("  unset <var>...                           Remove variable bindings\n")
	b.WriteString("  vars                                     List session variables\n")
	b.WriteString("  version                                  Print version information\n")
	b.WriteString("  help                                     Show this help\n")
	b.WriteString("\n")
	b.WriteString("Aliases:\n")
	b.WriteString("  alias <name> = <expansion>               Define a first-token alias\n")
	b.WriteString("  unalias <name>...                        Remove alias bindings\n")
	b.WriteString("  aliases                                  List defined aliases\n")
	b.WriteString("\n")
	b.WriteString("Variables:\n")
	b.WriteString("  let prog = bpfman load file ...   Assign command result to a variable\n")
	b.WriteString("  set <name> = <value>              Bind scalar value to variable\n")
	b.WriteString("  bpfman show program $prog         Variable reference (auto-extracts program ID)\n")
	b.WriteString("  bpfman link attach xdp -i eth0 $prog  Use $variable as program ID argument\n")
	b.WriteString("\n")
	b.WriteString("Assertions:\n")
	b.WriteString("  assert <verb> [args...]       Check condition, continue on failure\n")
	b.WriteString("  require <verb> [args...]      Check condition, stop on failure\n")
	b.WriteString("  assert not <verb> [args...]   Negate condition\n")
	b.WriteString("\n")
	b.WriteString("  Verbs: eq, ne, nil, not-empty, ok, fail, path exists,\n")
	b.WriteString("         contains, true, false, lt, le, gt, ge\n")
	return cli.PrintOut(b.String())
}

// replHistoryPath returns the path to the REPL history file,
// following the XDG Base Directory specification. The file is stored
// at $XDG_STATE_HOME/bpfman/repl-history, defaulting to
// $HOME/.local/state/bpfman/repl-history when XDG_STATE_HOME is
// unset. The parent directory is created if it does not exist.
func replHistoryPath() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determine home directory: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(stateHome, "bpfman")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	return filepath.Join(dir, "repl-history"), nil
}

// replVars lists all session variables and their types.
func replVars(cli *CLI, session *shell.Session) error {
	names := session.Names()
	if len(names) == 0 {
		return cli.PrintOut("No variables defined.\n")
	}
	var b strings.Builder
	for _, name := range names {
		v, _ := session.Get(name)
		kind := "scalar"
		if v.IsStructured() {
			kind = "structured"
		}
		fmt.Fprintf(&b, "  %s (%s)\n", name, kind)
	}
	return cli.PrintOut(b.String())
}

// applyAlias rewrites the first token of an expanded arg slice if it
// matches a session alias. Expansion is non-recursive: only one
// rewrite is performed.
func applyAlias(session *shell.Session, args []shell.Arg) []shell.Arg {
	if len(args) == 0 {
		return args
	}
	w, ok := args[0].(shell.WordArg)
	if !ok {
		return args
	}
	expansion, found := session.GetAlias(w.Text)
	if !found {
		return args
	}
	rewritten := make([]shell.Arg, len(args))
	copy(rewritten, args)
	rewritten[0] = shell.WordArg{Text: expansion}
	return rewritten
}

// replAlias defines a first-token alias. Syntax: alias <name> = <expansion>.
// The name must not collide with shell commands or "bpfman".
func replAlias(cli *CLI, session *shell.Session, args []string) error {
	if len(args) != 3 || args[1] != "=" {
		return fmt.Errorf("usage: alias <name> = <expansion>")
	}
	name, expansion := args[0], args[2]
	if shellCommands[name] {
		return fmt.Errorf("cannot alias %q: it is a shell command", name)
	}
	if name == "bpfman" {
		return fmt.Errorf("cannot alias %q: it is the domain prefix", name)
	}
	if name == "let" || name == "set" {
		return fmt.Errorf("cannot alias %q: it is a shell keyword", name)
	}
	session.SetAlias(name, expansion)
	return nil
}

// replUnalias removes one or more alias bindings.
func replUnalias(cli *CLI, session *shell.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("unalias requires at least one alias name")
	}
	for _, name := range args {
		if _, ok := session.GetAlias(name); !ok {
			return fmt.Errorf("undefined alias %q", name)
		}
		session.DeleteAlias(name)
	}
	return nil
}

// replAliases lists all defined aliases.
func replAliases(cli *CLI, session *shell.Session) error {
	names := session.AliasNames()
	if len(names) == 0 {
		return cli.PrintOut("No aliases defined\n")
	}
	var b strings.Builder
	for _, name := range names {
		expansion, _ := session.GetAlias(name)
		fmt.Fprintf(&b, "  %s = %s\n", name, expansion)
	}
	return cli.PrintOut(b.String())
}

// replUnset removes one or more variable bindings from the session.
func replUnset(cli *CLI, session *shell.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("unset requires at least one variable name")
	}
	for _, name := range args {
		if _, ok := session.Get(name); !ok {
			return fmt.Errorf("undefined variable %q", name)
		}
		session.Delete(name)
	}
	return nil
}

// replDump prints a value to stdout. Its single argument is any
// expression that evaluates to a value: a variable reference
// ($var, $var.path), a command substitution ([...]), a quoted
// string, or a bare word (which is a literal string — "dump foo"
// prints the word "foo", not the variable foo).  To print a
// variable, write "dump $foo".  Scalars print as plain text,
// structured values as indented JSON, and nil as "null".
func replDump(cli *CLI, args []shell.Arg) error {
	if len(args) != 1 {
		return fmt.Errorf("dump requires exactly one argument: dump $var[.path] | [expr] | \"literal\"")
	}

	v, err := dumpValue(args[0])
	if err != nil {
		return err
	}
	return writeValue(cli, v)
}

// dumpValue resolves a single dump argument into a shell.Value.
// Every arg kind is treated as a value: WordArg and QuotedArg are
// literal strings; ScalarValueArg and StructuredValueArg are
// already-resolved values from variable expansion or command
// substitution; AdapterArg carries its resolved Value.  Bare
// identifiers are NOT looked up in the session -- callers must
// write $name to dereference a variable.
func dumpValue(arg shell.Arg) (shell.Value, error) {
	switch a := arg.(type) {
	case shell.WordArg:
		return shell.StringValue(a.Text), nil
	case shell.QuotedArg:
		return shell.StringValue(a.Text), nil
	case shell.ScalarValueArg:
		return shell.StringValue(a.Text), nil
	case shell.StructuredValueArg:
		return a.Value, nil
	case shell.AdapterArg:
		return a.Value, nil
	default:
		return shell.Value{}, fmt.Errorf("dump: unsupported argument kind %T", arg)
	}
}

// writeValue renders a shell.Value onto cli: nil as "null", scalars
// as plain text, structured values as indented JSON. Shared between
// dump and any other "print me this value" caller.
func writeValue(cli *CLI, v shell.Value) error {
	if v.IsNil() {
		return cli.PrintOut("null\n")
	}
	if v.IsScalar() {
		s, err := v.Scalar()
		if err != nil {
			return err
		}
		return cli.PrintOut(s + "\n")
	}
	b, err := json.MarshalIndent(v.Raw(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	return cli.PrintOut(string(b) + "\n")
}

// lookupBareVar resolves a bare variable name (no $ prefix) with an
// optional dotted path into a shell.Value. This is the shared logic
// used by dump and assert nil.
func lookupBareVar(session *shell.Session, arg string) (shell.Value, error) {
	varName := arg
	path := ""
	if i := strings.IndexAny(arg, ".["); i >= 0 {
		varName = arg[:i]
		path = arg[i:]
		path = strings.TrimPrefix(path, ".")
	}

	v, ok := session.Get(varName)
	if !ok {
		return shell.Value{}, fmt.Errorf("undefined variable %q", varName)
	}

	if path != "" {
		return v.LookupValue(varName, path)
	}
	return v, nil
}

// replAssertRequire handles both "assert" and "require" commands.
// When isRequire is true, failure halts execution immediately via
// errRequireFailed. When false, failure is recorded in the session
// counter and execution continues.
func replAssertRequire(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, isRequire bool, loc sourceLoc) error {
	if len(args) == 0 {
		return fmt.Errorf("expected an assertion (e.g. \"$a eq $b\", \"true $flag\", \"ok exec ...\")")
	}

	label := "assert"
	if isRequire {
		label = "require"
	}

	// Check for "not" negation.
	negate := false
	if argText(args[0]) == "not" {
		negate = true
		args = args[1:]
		if len(args) == 0 {
			return fmt.Errorf("expected a verb after \"not\"")
		}
	}

	// Value-based assertion: binary comparison or unary predicate.
	// These route through the expression grammar. "not" is legal
	// before unary predicates but banned before binary comparisons
	// (use the complementary operator instead).
	if isExprAssertion(args) {
		if negate && len(args) == 3 {
			return fmt.Errorf("\"not\" is not supported with infix comparisons; use the complementary operator (ne, le, ge, !=, <=, >=)")
		}
		result, err := evalExprAssertion(session, args)
		if err != nil {
			return err
		}
		if negate {
			result.pass = !result.pass
			result.message = negateMessage(result.message)
		}
		if result.pass {
			return nil
		}
		_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, result.message)
		if isRequire {
			return errRequireFailed
		}
		session.RecordAssertFailure()
		return nil
	}

	// Prefix verb dispatch (command assertions and remaining special
	// verbs: ok, fail, path, contains).
	verb := argText(args[0])
	verbArgs := args[1:]

	result, err := evalAssertVerb(ctx, cli, mgr, session, verb, verbArgs)
	if err != nil {
		return err
	}

	if negate {
		result.pass = !result.pass
		result.message = negateMessage(result.message)
	}

	if result.pass {
		return nil
	}

	// Failure path.
	_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, result.message)

	if isRequire {
		return errRequireFailed
	}

	session.RecordAssertFailure()
	return nil
}

// isExprAssertion reports whether args matches the shape of a
// value-based assertion that should be routed through the expression
// grammar: either [lhs op rhs] with a binary operator, or [pred
// operand] with a unary predicate.
func isExprAssertion(args []shell.Arg) bool {
	switch len(args) {
	case 2:
		return shell.IsUnaryPred(argText(args[0]))
	case 3:
		return shell.IsBinaryOp(argText(args[1]))
	}
	return false
}

// evalExprAssertion rebuilds an expression from the evaluated args,
// evaluates it, and wraps the boolean result with an
// assertion-appropriate failure message that describes the operands
// involved.
func evalExprAssertion(session *shell.Session, args []shell.Arg) (assertResult, error) {
	expr, err := shell.ExprFromArgs(args)
	if err != nil {
		return assertResult{}, err
	}
	env := &shell.Env{Session: session}
	v, err := shell.EvalExpr(expr, env)
	if err != nil {
		return assertResult{}, err
	}
	pass, err := shell.AsBool(v)
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    pass,
		message: formatExprFailure(expr, session),
	}, nil
}

// formatExprFailure produces an assertion failure message describing
// the expression and its operand values. Evaluation errors in the
// operands surface as-is; they should not occur here because Eval
// already succeeded on the top-level expression.
func formatExprFailure(e shell.Expr, session *shell.Session) string {
	switch x := e.(type) {
	case *shell.BinaryExpr:
		left := exprScalar(x.Left, session)
		right := exprScalar(x.Right, session)
		switch x.Op {
		case "eq":
			return fmt.Sprintf("expected %q to equal %q", left, right)
		case "ne":
			return fmt.Sprintf("expected %q to not equal %q", left, right)
		case "lt", "le", "gt", "ge":
			return fmt.Sprintf("expected %q %s %q (lexicographic)", left, x.Op, right)
		default:
			return fmt.Sprintf("expected %s %s %s", left, x.Op, right)
		}
	case *shell.UnaryExpr:
		operand := exprScalar(x.Operand, session)
		switch x.Pred {
		case "nil":
			return fmt.Sprintf("expected %s to be nil", operand)
		case "not-empty":
			return fmt.Sprintf("expected non-empty string, got %q", operand)
		case "true":
			return fmt.Sprintf("expected %s to be true", operand)
		case "false":
			return fmt.Sprintf("expected %s to be false", operand)
		default:
			return fmt.Sprintf("expected predicate %s to hold on %s", x.Pred, operand)
		}
	}
	return "assertion failed"
}

// exprScalar is a best-effort scalar stringification of an expression
// for inclusion in error messages. Non-scalar values render as their
// kind; evaluation errors render as "<err>". The Env has no
// substitution runner, so any CmdSubExpr reached here would error —
// this helper is only called on operand sub-expressions that have
// already been evaluated once via EvalExpr at the top level.
func exprScalar(e shell.Expr, session *shell.Session) string {
	v, err := shell.EvalExpr(e, &shell.Env{Session: session})
	if err != nil {
		return "<err>"
	}
	s, err := v.Scalar()
	if err != nil {
		return "<" + v.Kind().String() + ">"
	}
	return s
}

// evalAssertVerb dispatches to the prefix verb evaluators that are
// not part of the expression grammar: command status checks (ok,
// fail), filesystem checks (path), and string containment
// (contains). Value-based comparisons and unary predicates go
// through the expression path (see evalExprAssertion).
func evalAssertVerb(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, verb string, args []shell.Arg) (assertResult, error) {
	ss := argTexts(args)
	switch verb {
	case "ok":
		return assertOk(ctx, cli, mgr, session, args)
	case "fail":
		return assertFail(ctx, cli, mgr, session, args)
	case "path":
		return assertPath(ss)
	case "contains":
		return assertContains(ss)
	case "nil":
		return assertNil(session, ss)
	case "eq", "ne", "lt", "le", "gt", "ge":
		return assertResult{}, fmt.Errorf("%q is not a prefix verb; use infix form: assert <left> %s <right>", verb, verb)
	case "true", "false", "not-empty":
		return assertResult{}, fmt.Errorf("%q requires exactly one operand: assert %s <operand>", verb, verb)
	default:
		return assertResult{}, fmt.Errorf("unknown assertion verb %q", verb)
	}
}

// assertNil checks whether a variable holds a nil Value. The operand
// is a bare variable name, not a value expression: the runtime
// Session can hold nil values but variable expansion refuses to
// carry them through, so the only way to inspect nil-ness is by
// name.
func assertNil(session *shell.Session, args []string) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, fmt.Errorf("nil requires exactly 1 argument (bare variable name, no $)")
	}
	v, err := lookupBareVar(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    v.IsNil(),
		message: fmt.Sprintf("expected %s to be nil", args[0]),
	}, nil
}

// runCommand executes a command through both the shell command layer
// and the domain dispatch layer. It is used by assertion verbs (ok,
// fail) to test whether a sub-command succeeds or fails.
func runCommand(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) error {
	handled, _, err := replShellCmd(ctx, cli, mgr, session, args, sourceLoc{})
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	_, err = replDispatch(ctx, cli, mgr, args)
	return err
}

func assertOk(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, fmt.Errorf("ok requires a command")
	}
	err := runCommand(ctx, cli.WithDiscardOutput(), mgr, session, args)
	if err != nil {
		return assertResult{
			pass:    false,
			message: fmt.Sprintf("expected command to succeed, but got: %v", err),
		}, nil
	}
	return assertResult{
		pass:    true,
		message: "expected command to succeed",
	}, nil
}

func assertFail(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, fmt.Errorf("fail requires a command")
	}
	err := runCommand(ctx, cli.WithDiscardOutput(), mgr, session, args)
	if err != nil {
		return assertResult{
			pass:    true,
			message: "expected command to fail",
		}, nil
	}
	return assertResult{
		pass:    false,
		message: "expected command to fail, but it succeeded",
	}, nil
}

func assertPath(args []string) (assertResult, error) {
	if len(args) != 2 || args[0] != "exists" {
		return assertResult{}, fmt.Errorf("path requires: path exists <filepath>")
	}
	_, err := os.Stat(args[1])
	pass := err == nil
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected path %q to exist", args[1]),
	}, nil
}

func assertContains(args []string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, fmt.Errorf("contains requires exactly 2 arguments: <haystack> <needle>")
	}
	pass := strings.Contains(args[0], args[1])
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %q to contain %q", args[0], args[1]),
	}, nil
}

// negateMessage transforms an assertion message for negated assertions.
// It inserts "not" into the message: "expected X to equal Y" becomes
// "expected X not to equal Y", "expected X to be Y" becomes
// "expected X not to be Y".
func negateMessage(msg string) string {
	// Try "to equal", "to not equal", "to be", "to contain", "to exist", "to succeed", "to fail".
	if i := strings.Index(msg, " to "); i >= 0 {
		return msg[:i] + " not to " + msg[i+4:]
	}
	// Try "expected command to" patterns.
	if strings.HasPrefix(msg, "expected") {
		return "expected not: " + msg[9:]
	}
	return "not: " + msg
}

// replVersion prints version information.
func replVersion(cli *CLI) error {
	return cli.PrintOut(version.Get().Long())
}

func replDoctorCheckup(ctx context.Context, cli *CLI, mgr *manager.Manager) error {
	report, err := mgr.Doctor(ctx)
	if err != nil {
		return fmt.Errorf("doctor failed: %w", err)
	}

	if len(report.Findings) == 0 {
		return cli.PrintOut("All checks passed. Database, kernel, and filesystem are coherent.\n")
	}

	ruleCounts := make(map[string]int)
	for _, f := range report.Findings {
		ruleCounts[f.RuleName]++
	}

	var out strings.Builder
	var errorCount, warningCount int
	lastCategory := ""
	lastRule := ""

	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)

	for _, f := range report.Findings {
		category := categoryHeading(f.Category)
		if category != lastCategory {
			w.Flush()
			if lastCategory != "" {
				out.WriteString("\n")
			}
			out.WriteString(category)
			out.WriteString("\n")
			lastCategory = category
			lastRule = ""
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		if f.RuleName != lastRule {
			w.Flush()
			fmt.Fprintf(&out, "  [%s] (%d)\n", f.RuleName, ruleCounts[f.RuleName])
			lastRule = f.RuleName
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		fmt.Fprintf(w, "    %s\t%s\n", f.Severity, f.Description)
		switch f.Severity {
		case coherency.SeverityError:
			errorCount++
		case coherency.SeverityWarning:
			warningCount++
		}
	}
	w.Flush()

	fmt.Fprintf(&out, "\nSummary: %d error(s), %d warning(s)\n", errorCount, warningCount)
	return cli.PrintOut(out.String())
}

func replDoctorExplain(cli *CLI, args []string) error {
	if len(args) == 0 {
		// List all rules.
		var out strings.Builder
		out.WriteString("Available coherency rules:\n\n")
		names := coherency.RuleNames()
		sort.Strings(names)
		for _, name := range names {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString("\n")
		}
		out.WriteString("\nUse 'doctor explain <rule>' for details.\n")
		return cli.PrintOut(out.String())
	}

	ruleName := args[0]
	rule := coherency.FindRule(ruleName)
	if rule == nil {
		return fmt.Errorf("unknown rule: %s\n\nUse 'doctor explain' to list all rules", ruleName)
	}

	var out strings.Builder
	out.WriteString(rule.Name)
	out.WriteString("\n")
	out.WriteString(strings.Repeat("=", len(rule.Name)))
	out.WriteString("\n\n")
	if rule.Description != "" {
		out.WriteString(rule.Description)
	} else {
		out.WriteString("(No description available)")
	}
	out.WriteString("\n")
	return cli.PrintOut(out.String())
}
