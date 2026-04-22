package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/itchyny/gojq"
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

// replCommandNames lists the top-level command tokens for completion.
// Domain commands live behind the "bpfman" prefix; shell-language
// commands are bare.
var replCommandNames = []string{"alias", "aliases", "assert", "bpfman", "dump", "exec", "file", "help", "jq", "let", "require", "source", "unalias", "unset", "vars", "version"}

// replAssertVerbs lists the valid assertion verbs for completion.
var replAssertVerbs = []string{"contains", "fail", "false", "nil", "not", "not-empty", "ok", "path", "true"}

// replSubcommands maps a top-level token to its valid subcommands for completion.
var replSubcommands = map[string][]string{
	"assert":  replAssertVerbs,
	"bpfman":  {"dispatcher", "doctor", "gc", "link", "list", "load", "program", "programs", "show"},
	"exec":    {"status"},
	"file":    {"temp"},
	"require": replAssertVerbs,
}

// bpfmanSubcommands maps a bpfman domain token to its valid
// subcommands for completion.
var bpfmanSubcommands = map[string][]string{
	"dispatcher": {"delete", "get", "list"},
	"doctor":     {"checkup", "explain"},
	"link":       {"attach", "delete", "detach", "get", "list"},
	"list":       {"programs"},
	"load":       {"file", "image"},
	"program":    {"delete", "get", "list", "load", "unload"},
	"programs":   {"list"},
	"show":       {"program"},
}

// replAttachTypes lists the valid attach types for "link attach <type>".
var replAttachTypes = []string{"fentry", "fexit", "kprobe", "tc", "tcx", "tracepoint", "uprobe", "xdp"}

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
		val, err := replFile(cli, session, args[1:])
		return true, val, err
	case "jq":
		val, err := replJQ(args[1:])
		return true, val, err
	case "require":
		return true, shell.Value{}, replAssertRequire(ctx, cli, mgr, session, args[1:], true, loc)
	case "dump":
		return true, shell.Value{}, replDump(cli, session, argTexts(args[1:]))
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

// replCompleter returns a CompleteFunc that has access to the manager
// and session for dynamic completions such as program IDs and
// variable names.
func replCompleter(ctx context.Context, mgr *manager.Manager, session *shell.Session) CompleteFunc {
	return func(line string, pos int) (replace int, candidates []string) {
		return replComplete(ctx, mgr, session, line, pos)
	}
}

func replComplete(ctx context.Context, mgr *manager.Manager, session *shell.Session, line string, pos int) (replace int, candidates []string) {
	head := line[:pos]

	tokens := strings.Fields(head)
	trailingSpace := len(head) > 0 && head[len(head)-1] == ' '

	// Detect --path / -p flag completion: if the previous token is
	// "--path" or "-p", offer filesystem completions for the
	// current partial token.
	isPathFlag := func(tok string) bool {
		return tok == "--path" || tok == "-p"
	}
	if len(tokens) >= 2 && trailingSpace && isPathFlag(tokens[len(tokens)-1]) {
		// Cursor is right after "--path " or "-p ", complete filesystem paths.
		candidates = replFileCompletions("")
		return
	}
	if len(tokens) >= 2 && !trailingSpace {
		prevIdx := len(tokens) - 2
		if isPathFlag(tokens[prevIdx]) {
			prefix := tokens[len(tokens)-1]
			candidates = replFileCompletions(prefix)
			replace = len(prefix)
			return
		}
	}

	// When the first token is "bpfman", delegate to domain
	// completion with the prefix stripped.
	if len(tokens) >= 1 && tokens[0] == "bpfman" {
		return replCompleteBpfman(ctx, mgr, session, tokens[1:], trailingSpace)
	}

	switch {
	case len(tokens) == 0 || (len(tokens) == 1 && !trailingSpace):
		// Completing the first token.
		prefix := ""
		if len(tokens) == 1 {
			prefix = tokens[0]
		}
		for _, cmd := range replCommandNames {
			if strings.HasPrefix(cmd, prefix) {
				candidates = append(candidates, cmd+" ")
			}
		}
		replace = len(prefix)
	case (len(tokens) == 1 && trailingSpace) || (len(tokens) == 2 && !trailingSpace):
		// "source" takes a file path as its argument.
		if tokens[0] == "source" {
			if len(tokens) == 2 {
				candidates = replFileCompletions(tokens[1])
				replace = len(tokens[1])
			} else {
				candidates = replFileCompletions("")
			}
			return
		}
		// "dump" takes a variable path (bare, no $ prefix).
		if tokens[0] == "dump" {
			prefix := ""
			if len(tokens) == 2 {
				prefix = tokens[1]
			}
			candidates, replace = replCompleteVarPath(session, prefix, false)
			return
		}
		// "unset" takes bare variable names.
		if tokens[0] == "unset" {
			prefix := ""
			if len(tokens) == 2 {
				prefix = tokens[1]
			}
			candidates, replace = replCompleteVarNames(session, prefix)
			return
		}
		// Completing the second token (subcommand).
		subs := replSubcommands[tokens[0]]
		prefix := ""
		if len(tokens) == 2 {
			prefix = tokens[1]
		}
		for _, sub := range subs {
			if strings.HasPrefix(sub, prefix) {
				candidates = append(candidates, sub+" ")
			}
		}
		replace = len(prefix)
	default:
		// Third token onwards: context-sensitive completion.
		candidates, replace = replCompleteArgs(ctx, mgr, session, tokens, trailingSpace)
	}

	return
}

// replCompleteBpfman handles completion for tokens after the leading
// "bpfman" prefix. The args slice has the prefix already stripped.
func replCompleteBpfman(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (replace int, candidates []string) {
	// "bpfman " or "bpfman dis..." -- complete domain command names.
	if len(args) == 0 || (len(args) == 1 && !trailingSpace) {
		prefix := ""
		if len(args) == 1 {
			prefix = args[0]
		}
		for _, sub := range replSubcommands["bpfman"] {
			if strings.HasPrefix(sub, prefix) {
				candidates = append(candidates, sub+" ")
			}
		}
		replace = len(prefix)
		return
	}

	// "bpfman program " -- complete second-level domain subcommand.
	if (len(args) == 1 && trailingSpace) || (len(args) == 2 && !trailingSpace) {
		subs := bpfmanSubcommands[args[0]]
		prefix := ""
		if len(args) == 2 {
			prefix = args[1]
		}
		for _, sub := range subs {
			if strings.HasPrefix(sub, prefix) {
				candidates = append(candidates, sub+" ")
			}
		}
		replace = len(prefix)
		return
	}

	// Third token onwards: delegate to the same arg completer,
	// using the domain tokens (without the "bpfman" prefix).
	candidates, replace = replCompleteArgs(ctx, mgr, session, args, trailingSpace)
	return
}

// replCompleteArgs handles completion for the third token onwards,
// dispatching based on the command prefix.
func replCompleteArgs(ctx context.Context, mgr *manager.Manager, session *shell.Session, tokens []string, trailingSpace bool) (candidates []string, replace int) {
	if len(tokens) < 2 {
		return
	}
	if tokens[0] == "unset" {
		prefix := ""
		if !trailingSpace {
			prefix = tokens[len(tokens)-1]
		}
		return replCompleteVarNames(session, prefix)
	}

	// program subcommands
	if tokens[0] == "program" {
		switch tokens[1] {
		case "delete":
			return replCompleteProgramDelete(ctx, mgr, session, tokens[2:], trailingSpace)
		case "get":
			return replCompleteProgramIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "unload":
			return replCompleteProgramIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "load":
			// "program load file" or "program load image" -- third-level subcommand
			if len(tokens) == 2 && trailingSpace {
				return []string{"file ", "image "}, 0
			}
			if len(tokens) == 3 && !trailingSpace {
				prefix := tokens[2]
				for _, sub := range []string{"file", "image"} {
					if strings.HasPrefix(sub, prefix) {
						candidates = append(candidates, sub+" ")
					}
				}
				return candidates, len(prefix)
			}
			return
		}
	}

	if tokens[0] == "show" && tokens[1] == "program" {
		return replCompleteShowProgram(ctx, mgr, session, tokens[2:], trailingSpace)
	}

	// link subcommands
	if tokens[0] == "link" {
		switch tokens[1] {
		case "attach":
			return replCompleteLinkAttach(ctx, mgr, session, tokens[2:], trailingSpace)
		case "detach":
			return replCompleteLinkIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "get":
			return replCompleteLinkIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		case "delete":
			return replCompleteLinkIDs(ctx, mgr, session, tokens[2:], trailingSpace)
		}
	}

	return
}

// replCompleteLinkAttach handles completion for "link attach ...".
// First arg is attach type, remaining args get program ID completion.
func replCompleteLinkAttach(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	switch {
	case len(args) == 0 && trailingSpace:
		// "link attach " -- complete attach types
		for _, t := range replAttachTypes {
			candidates = append(candidates, t+" ")
		}
		return
	case len(args) == 1 && !trailingSpace:
		// "link attach xd" -- partial attach type
		prefix := args[0]
		for _, t := range replAttachTypes {
			if strings.HasPrefix(t, prefix) {
				candidates = append(candidates, t+" ")
			}
		}
		return candidates, len(prefix)
	default:
		// After the attach type, offer program ID completion for
		// tokens that look like they could be a program ID argument.
		// We only complete the last token if it starts with $ or is numeric.
		if !trailingSpace && len(args) > 1 {
			last := args[len(args)-1]
			if strings.HasPrefix(last, "$") {
				return replCompleteVarPath(session, last, true)
			}
		}
		if trailingSpace {
			// Offer program IDs and $variables.
			return replCompleteProgramIDs(ctx, mgr, session, nil, true)
		}
		return
	}
}

// showProgramViews lists the valid sub-view names for "show program <id>".
var showProgramViews = []string{"links", "maps", "paths"}

// replCompleteShowProgram handles completion for "show program ..."
// arguments. The first argument is a program ID; the second is a
// sub-view name.
func replCompleteShowProgram(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	switch {
	case len(args) == 0 && trailingSpace:
		// "show program " -- complete program IDs
		return replCompleteProgramIDs(ctx, mgr, session, args, trailingSpace)
	case len(args) == 1 && !trailingSpace:
		// "show program 12" -- partial program ID
		return replCompleteProgramIDs(ctx, mgr, session, args, trailingSpace)
	case len(args) == 1 && trailingSpace:
		// "show program 123 " -- complete sub-view
		for _, v := range showProgramViews {
			candidates = append(candidates, v+" ")
		}
		return
	case len(args) == 2 && !trailingSpace:
		// "show program 123 li" -- partial sub-view
		prefix := args[1]
		for _, v := range showProgramViews {
			if strings.HasPrefix(v, prefix) {
				candidates = append(candidates, v+" ")
			}
		}
		replace = len(prefix)
		return
	}
	return
}

// replCompleteProgramDelete handles completion for "program delete",
// offering --all in addition to program IDs.
func replCompleteProgramDelete(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	candidates, replace = replCompleteProgramIDs(ctx, mgr, session, args, trailingSpace)

	// Determine the current prefix.
	prefix := ""
	if !trailingSpace && len(args) > 0 {
		prefix = args[len(args)-1]
	}

	// Offer --all when it matches the prefix and hasn't been specified.
	if strings.HasPrefix("--all", prefix) {
		already := false
		for _, a := range args {
			if a == "--all" {
				already = true
				break
			}
		}
		if !already {
			candidates = append(candidates, "--all ")
		}
	}

	return
}

// replCompleteProgramIDs offers program ID completions, excluding IDs
// that have already been specified on the command line. When the
// prefix starts with '$', completion is delegated to
// replCompleteVarPath for dotted path support. Otherwise, numeric IDs
// and top-level $variable names are offered.
func replCompleteProgramIDs(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	// Collect IDs already on the line so we don't offer them again.
	specified := make(map[string]struct{}, len(args))
	for _, a := range args {
		specified[a] = struct{}{}
	}

	prefix := ""
	if !trailingSpace && len(args) > 0 {
		// The last token is a partial ID being typed.
		prefix = args[len(args)-1]
		delete(specified, prefix)
	}

	// When the prefix starts with '$', delegate to path completion.
	if strings.HasPrefix(prefix, "$") {
		candidates, replace = replCompleteVarPath(session, prefix, true)
		return
	}

	if mgr != nil {
		result, err := mgr.ListPrograms(ctx)
		if err == nil {
			for _, prog := range result.Programs {
				id := fmt.Sprintf("%d", prog.Record.ProgramID)
				if _, already := specified[id]; already {
					continue
				}
				if strings.HasPrefix(id, prefix) {
					candidates = append(candidates, id+" ")
				}
			}
		}
	}

	// Offer $variable completions from the session when no prefix
	// or a non-$ prefix is being typed.
	if session != nil {
		for _, name := range session.Names() {
			candidate := "$" + name
			if _, already := specified[candidate]; already {
				continue
			}
			if !strings.HasPrefix(candidate, prefix) {
				continue
			}
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			if v.IsStructured() {
				// Only offer if it has .record.program_id
				if _, err := v.LookupValue(name, "record.program_id"); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			} else if v.IsScalar() {
				s, err := v.Scalar()
				if err != nil {
					continue
				}
				if _, err := ParseProgramID(s); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			}
		}
	}

	replace = len(prefix)
	return
}

// replCompleteVarPath completes dotted variable paths. The token is
// the partial text (e.g. "$prog.rec" or "prog.record."). When sigil
// is true, variable names carry a '$' prefix (program ID contexts);
// when false they are bare (dump context). Returns candidates and the
// number of characters to replace (the full token length).
func replCompleteVarPath(session *shell.Session, token string, sigil bool) (candidates []string, replace int) {
	if session == nil {
		return
	}

	replace = len(token)

	// Strip the '$' prefix when present.
	stripped := token
	if sigil && strings.HasPrefix(stripped, "$") {
		stripped = stripped[1:]
	}

	// Empty remainder: list all variable names.
	if stripped == "" {
		for _, name := range session.Names() {
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			candidate := name
			if sigil {
				candidate = "$" + name
			}
			// Append trailing character based on value type.
			candidate += varPathSuffix(v)
			candidates = append(candidates, candidate)
		}
		return
	}

	// Find the split point: first '.' or '['.
	sepIdx := strings.IndexAny(stripped, ".[")

	// No separator and token does not end with '.' -- partial variable name.
	if sepIdx < 0 {
		for _, name := range session.Names() {
			if !strings.HasPrefix(name, stripped) {
				continue
			}
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			candidate := name
			if sigil {
				candidate = "$" + name
			}
			candidate += varPathSuffix(v)
			candidates = append(candidates, candidate)
		}
		return
	}

	varName := stripped[:sepIdx]
	v, ok := session.Get(varName)
	if !ok {
		return
	}

	pathPart := stripped[sepIdx:]

	// Determine the resolved prefix (complete segments) and the
	// tail (partial last segment after the final '.' or '[').
	var resolvedPath, tail string
	if strings.HasSuffix(pathPart, ".") {
		// e.g. "record." -- walk to "record", enumerate keys
		resolvedPath = strings.TrimPrefix(pathPart, ".")
		resolvedPath = strings.TrimSuffix(resolvedPath, ".")
		tail = ""
	} else if strings.HasSuffix(pathPart, "[") {
		// e.g. "maps[" -- walk to "maps", enumerate indices
		resolvedPath = strings.TrimPrefix(pathPart, ".")
		resolvedPath = strings.TrimSuffix(resolvedPath, "[")
		tail = "["
	} else {
		// e.g. "record.prog" -- walk to "record", match "prog"
		lastDot := strings.LastIndex(pathPart, ".")
		lastBracket := strings.LastIndex(pathPart, "[")
		if lastDot >= lastBracket {
			resolvedPath = strings.TrimPrefix(pathPart[:lastDot], ".")
			tail = pathPart[lastDot+1:]
		} else {
			// Partial array index like "maps[1" -- not useful to complete
			return
		}
	}

	// Walk to the resolved prefix.
	target, err := v.LookupValue(varName, resolvedPath)
	if err != nil {
		return
	}

	// Build the candidate prefix: everything before the tail.
	var candidatePrefix string
	if sigil {
		candidatePrefix = "$"
	}
	candidatePrefix += varName
	if resolvedPath != "" {
		candidatePrefix += "." + resolvedPath
	}

	keys := target.Keys()
	if keys == nil {
		return
	}

	if tail == "[" {
		// Array index completion.
		for _, key := range keys {
			if !strings.HasPrefix(key, "[") {
				continue
			}
			// Walk to the element to determine its trailing character.
			elemPath := resolvedPath
			if elemPath != "" {
				elemPath += key
			} else {
				elemPath = key
			}
			elem, err := v.LookupValue(varName, elemPath)
			if err != nil {
				continue
			}
			candidate := candidatePrefix + key + varPathSuffix(elem)
			candidates = append(candidates, candidate)
		}
		return
	}

	// Map field completion: match keys against tail prefix.
	for _, key := range keys {
		if !strings.HasPrefix(key, tail) {
			continue
		}
		// Walk to the field to determine its trailing character.
		fieldPath := resolvedPath
		if fieldPath != "" {
			fieldPath += "." + key
		} else {
			fieldPath = key
		}
		child, err := v.LookupValue(varName, fieldPath)
		if err != nil {
			continue
		}
		candidate := candidatePrefix + "." + key + varPathSuffix(child)
		candidates = append(candidates, candidate)
	}
	return
}

// replCompleteVarNames offers bare variable name completions with a
// trailing space. Used by commands like unset that take whole variable
// names rather than dotted paths.
func replCompleteVarNames(session *shell.Session, prefix string) (candidates []string, replace int) {
	if session == nil {
		return
	}
	replace = len(prefix)
	for _, name := range session.Names() {
		if strings.HasPrefix(name, prefix) {
			candidates = append(candidates, name+" ")
		}
	}
	return
}

// varPathSuffix returns the trailing character for a completion
// candidate based on the value type: "." for maps (invites deeper
// traversal), "[" for arrays (invites indexing), " " for scalars
// and nil (terminal).
func varPathSuffix(v shell.Value) string {
	switch v.Raw().(type) {
	case map[string]any:
		return "."
	case []any:
		return "["
	default:
		return " "
	}
}

// replFileCompletions returns filesystem path completions for the
// given prefix. Directories get a trailing slash. When the prefix
// starts with "./" the dot-slash is preserved in completions because
// filepath.Glob normalises it away.
func replFileCompletions(prefix string) []string {
	// When no prefix is given, list the current directory with an
	// explicit "./" so completions read as relative paths.
	dotSlash := false
	globPrefix := prefix
	if globPrefix == "" {
		globPrefix = "./"
		dotSlash = true
	} else if strings.HasPrefix(globPrefix, "./") {
		dotSlash = true
	}

	pattern := globPrefix + "*"
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return nil
	}
	var completions []string
	for _, m := range matches {
		// filepath.Glob strips the "./" prefix; restore it
		// when the user typed one.
		if dotSlash && !strings.HasPrefix(m, "./") {
			m = "./" + m
		}
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.IsDir() {
			completions = append(completions, m+"/")
		} else {
			completions = append(completions, m)
		}
	}
	return completions
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
	b.WriteString("  file temp <variable>[.path]               Write value to temp file (assignable)\n")
	b.WriteString("  jq <filter> <value>                       Apply a jq filter to a value (assignable)\n")
	b.WriteString("  dump <variable>[.path]                    Display variable contents\n")
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

// replJQ runs a jq filter against a Value using an embedded gojq
// interpreter.  It is the DSL's "higher-order ops over JSON-shaped
// data" primitive.  Shape: jq <filter> <value>.
//
// The filter is a scalar (Word/Quoted/ScalarValue); the value may
// be scalar or structured.  Multiple jq results are collected into
// a list Value; zero results yield a nil Value; a single result is
// returned directly.  Integer outputs from gojq are normalised to
// json.Number so downstream Scalar() and path access treat them
// like any other numeric value in the pipeline.  Bool results get
// OriginBool so AsBool works on them for assertions.
func replJQ(args []shell.Arg) (shell.Value, error) {
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("usage: jq <filter> <value>")
	}
	filterText := argText(args[0])
	query, err := gojq.Parse(filterText)
	if err != nil {
		return shell.Value{}, fmt.Errorf("jq: parse filter: %w", err)
	}
	input, err := argToJQInput(args[1])
	if err != nil {
		return shell.Value{}, fmt.Errorf("jq: %w", err)
	}

	iter := query.Run(input)
	var results []any
	for {
		v, hasMore := iter.Next()
		if !hasMore {
			break
		}
		if iterErr, ok := v.(error); ok {
			return shell.Value{}, fmt.Errorf("jq: %w", iterErr)
		}
		results = append(results, normaliseJQValue(v))
	}
	switch len(results) {
	case 0:
		return shell.Value{}, nil
	case 1:
		return wrapJQResult(results[0]), nil
	default:
		return wrapJQResult(results), nil
	}
}

// argToJQInput extracts a JSON-compatible any from a shell.Arg.
// Structured args pass through as their Raw representation;
// scalar args are parsed as JSON text, matching the default
// behaviour of the standalone jq CLI (which reads stdin as
// JSON).  A scalar that isn't valid JSON is an error — users who
// want to pass a literal string wrap it in JSON quotes
// ('"hello"' rather than 'hello').
func argToJQInput(a shell.Arg) (any, error) {
	switch v := a.(type) {
	case shell.WordArg:
		return decodeJQScalar(v.Text)
	case shell.QuotedArg:
		return decodeJQScalar(v.Text)
	case shell.ScalarValueArg:
		return decodeJQScalar(v.Text)
	case shell.StructuredValueArg:
		return v.Value.Raw(), nil
	case shell.AdapterArg:
		return v.Value.Raw(), nil
	default:
		return nil, fmt.Errorf("unsupported input type %T", a)
	}
}

// decodeJQScalar parses a scalar as a single JSON value.  Numbers
// come back as json.Number so Value.Scalar() renders them
// losslessly; trailing data after the value is rejected so
// sloppy inputs fail fast.
func decodeJQScalar(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("input is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("input is not valid JSON: trailing data after value")
	}
	return v, nil
}

// normaliseJQValue walks a jq output and converts Go-native
// integer types to json.Number so the result lines up with the
// rest of the pipeline, which carries numbers as json.Number
// throughout.  float64 is left alone; nested maps and slices are
// rewritten recursively.
func normaliseJQValue(x any) any {
	switch v := x.(type) {
	case int:
		return json.Number(strconv.Itoa(v))
	case int64:
		return json.Number(strconv.FormatInt(v, 10))
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = normaliseJQValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = normaliseJQValue(e)
		}
		return out
	default:
		return v
	}
}

// wrapJQResult turns a single jq output into a shell.Value,
// preferring the most specific origin kind so assertions and
// other origin-aware consumers see the shape they expect: bool →
// OriginBool, scalar → OriginScalar, structured → OriginUnknown.
func wrapJQResult(x any) shell.Value {
	if x == nil {
		return shell.Value{}
	}
	if b, ok := x.(bool); ok {
		return shell.BoolValue(b)
	}
	v := shell.ValueFromAny(x)
	if v.IsScalar() {
		return v.WithKind(shell.OriginScalar)
	}
	return v
}

// replFile implements the file shell command. The only subcommand is
// "temp", which writes a REPL value to a private temporary file and
// returns the path as a scalar string. The argument is a bare
// variable name with optional field path (no $ prefix), the same
// form accepted by dump.
func replFile(cli *CLI, session *shell.Session, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 || argText(args[0]) != "temp" {
		return shell.Value{}, fmt.Errorf("usage: file temp <variable>[.path]")
	}
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("file temp requires exactly one argument")
	}
	v, err := lookupBareVar(session, argText(args[1]))
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	path, err := writeValueToTemp(v)
	if err != nil {
		return shell.Value{}, fmt.Errorf("file temp: %w", err)
	}
	if err := cli.PrintOut(path + "\n"); err != nil {
		return shell.Value{}, err
	}
	return shell.StringValue(path), nil
}

// writeValueToTemp renders a shell.Value to a private temporary file
// and returns the absolute path. The file is created with mode 0600
// in the OS default temp directory with a recognisable prefix.
func writeValueToTemp(v shell.Value) (string, error) {
	data, err := shell.RenderValue(v)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "bpfman-repl-")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write temp file: %w", err)
	}
	return f.Name(), nil
}

// execResult holds the captured output of a subprocess run by the
// exec shell command. The JSON tags produce the field names visible
// in the REPL's structured-value model.
type execResult struct {
	Argv     []string `json:"argv"`
	Stdout   string   `json:"stdout"`
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit_code"`
}

// replExec runs an external command and returns a structured result.
//
// In strict mode (the default), exit 0 returns a structured result
// and non-zero exit returns an error. This keeps the common case
// clean for require ok exec and assert ok exec.
//
// In status mode (exec status ...), non-zero exit is not an error.
// The structured result is returned for all exit codes, with
// exit_code reflecting the actual status. Only genuine launch
// failures (command not found, permission denied) produce errors.
// This mode is for commands like diff, grep -q, and cmp where
// non-zero exit is a domain result rather than an execution failure.
//
// Inline adapter arguments (e.g. file:$var.path) are resolved to
// temporary files before the command runs. All adapter-created temp
// files are removed unconditionally after the command completes.
func replExec(ctx context.Context, cli *CLI, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("exec requires at least one argument")
	}

	// Detect status mode.
	statusMode := false
	if argText(args[0]) == "status" {
		statusMode = true
		args = args[1:]
		if len(args) == 0 {
			return shell.Value{}, fmt.Errorf("exec status requires at least one argument")
		}
	}

	// Resolve adapter args to temp files.
	var tempFiles []string
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		switch aa := a.(type) {
		case shell.AdapterArg:
			if aa.Adapter != "file" {
				return shell.Value{}, fmt.Errorf("unknown adapter %q", aa.Adapter)
			}
			path, err := writeValueToTemp(aa.Value)
			if err != nil {
				return shell.Value{}, fmt.Errorf("adapter file: %w", err)
			}
			tempFiles = append(tempFiles, path)
			resolved[i] = shell.ScalarValueArg{Text: path}
		case shell.StructuredValueArg:
			// A structured value (program, link, exec.result, ...)
			// cannot be flattened into argv text. This most often
			// arises when a nested command substitution returned a
			// structured result; the fix is to access a scalar
			// field (e.g. $result.stdout) or use the file adapter
			// (file:$result).
			return shell.Value{}, fmt.Errorf(
				"exec: argument %d is a %s value; use a scalar path (e.g. $name.field) or the file adapter (file:$name)",
				i+1, aa.Value.Kind())
		default:
			resolved[i] = a
		}
	}

	argv := argTexts(resolved)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// Not an exit error — command not found or similar.
			return shell.Value{}, fmt.Errorf("exec %s: %w", argv[0], err)
		}
		if !statusMode {
			msg := fmt.Sprintf("exec %s: exit status %d", strings.Join(argv, " "), exitErr.ExitCode())
			if stderr.Len() > 0 {
				msg += ": " + strings.TrimRight(stderr.String(), "\n")
			}
			return shell.Value{}, errors.New(msg)
		}
		exitCode = exitErr.ExitCode()
	}

	result := execResult{
		Argv:     argv,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}
	val, err := shell.ValueFromStruct(result)
	if err != nil {
		return shell.Value{}, fmt.Errorf("exec: build result: %w", err)
	}

	if stdout.Len() > 0 {
		if err := cli.PrintOut(stdout.String()); err != nil {
			return shell.Value{}, err
		}
	}

	return val.WithKind(shell.OriginExecResult), nil
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

// replDump displays the contents of a session variable. The argument
// is a bare variable name with an optional dotted/indexed path (no $
// prefix). Scalars are printed as plain text, structured values as
// indented JSON, and nil as "null".
func replDump(cli *CLI, session *shell.Session, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("dump requires exactly one argument: dump <variable>[.path]")
	}

	v, err := lookupBareVar(session, args[0])
	if err != nil {
		return err
	}

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

// replCompleteLinkIDs offers link ID completions, analogous to
// replCompleteProgramIDs.
func replCompleteLinkIDs(ctx context.Context, mgr *manager.Manager, session *shell.Session, args []string, trailingSpace bool) (candidates []string, replace int) {
	specified := make(map[string]struct{}, len(args))
	for _, a := range args {
		specified[a] = struct{}{}
	}

	prefix := ""
	if !trailingSpace && len(args) > 0 {
		prefix = args[len(args)-1]
		delete(specified, prefix)
	}

	if strings.HasPrefix(prefix, "$") {
		candidates, replace = replCompleteVarPath(session, prefix, true)
		return
	}

	if mgr != nil {
		links, err := mgr.ListLinks(ctx)
		if err == nil {
			for _, l := range links {
				id := fmt.Sprintf("%d", l.ID)
				if _, already := specified[id]; already {
					continue
				}
				if strings.HasPrefix(id, prefix) {
					candidates = append(candidates, id+" ")
				}
			}
		}
	}

	if session != nil {
		for _, name := range session.Names() {
			candidate := "$" + name
			if _, already := specified[candidate]; already {
				continue
			}
			if !strings.HasPrefix(candidate, prefix) {
				continue
			}
			v, ok := session.Get(name)
			if !ok {
				continue
			}
			if v.IsStructured() {
				if _, err := v.LookupValue(name, "record.id"); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			} else if v.IsScalar() {
				s, err := v.Scalar()
				if err != nil {
					continue
				}
				if _, err := ParseLinkID(s); err != nil {
					continue
				}
				candidates = append(candidates, candidate+" ")
			}
		}
	}

	replace = len(prefix)
	return
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
