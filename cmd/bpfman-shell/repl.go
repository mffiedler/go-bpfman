package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/shell"
	"github.com/frobware/go-bpfman/version"
)

// errRequireFailed is the sentinel error used to halt script execution
// when a require assertion fails.
var errRequireFailed = errors.New("require failed")

// errScriptError is the sentinel error used to halt script execution
// when a command error occurs in file mode. The error message has
// already been printed with a source location prefix.
var errScriptError = errors.New("script error")

// Run starts the read-eval-print loop. A single manager is held
// open for the session lifetime to avoid repeated store open/close.
// When --check is set, Run short-circuits to a parse-only mode
// that reads the same input, reports syntax errors, and exits
// without touching the manager, session, or evaluator.
func (c *CLI) Run(ctx context.Context) error {
	if c.Check {
		return c.runCheck()
	}
	mgr, cleanup, err := c.NewManagerWithPuller(ctx)
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

	file := c.Script
	if file == "-" || (file == "" && !term.IsTerminal(int(os.Stdin.Fd()))) {
		file = "<stdin>"
	}
	loopErr := replLoop(ctx, &c.CLI, mgr, lr, session, file)

	if errors.Is(loopErr, errRequireFailed) || errors.Is(loopErr, errScriptError) {
		return ErrSilent
	}
	if loopErr != nil {
		return loopErr
	}

	if n := session.AssertFailures(); n > 0 {
		_ = c.PrintErrf("%d assertion(s) failed\n", n)
		return fmt.Errorf("%d assertion(s) failed", n)
	}

	if n := session.DeferFailures(); n > 0 {
		_ = c.PrintErrf("%d defer(s) failed\n", n)
		return fmt.Errorf("%d defer(s) failed", n)
	}

	return nil
}

// newReader selects the appropriate LineReader: positional script
// file, piped stdin, or interactive readline.
func (c *CLI) newReader(ctx context.Context, mgr *manager.Manager, session *shell.Session) (LineReader, error) {
	if c.Script != "" {
		return openScriptReader(c.Script)
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

// replLoop reads from lr and dispatches input until EOF or
// interrupt. Two modes:
//
// In script mode (file != ""), the whole input is slurped, then
// evaluated as one program. This makes the script a single
// defer scope, matching design section 7.2: 'defer cleanup'
// near the top of a script fires at script exit, not at the
// end of its own physical line.
//
// In interactive mode (file == ""), input is dispatched
// chunk-by-chunk: each balanced statement (or block) is its
// own EvalProgram call, so defers register and fire at the
// prompt boundary. That is the right interactive behaviour --
// a defer at the prompt should not pile up across the
// session.
//
// Variable assignment and expansion use the shell.Session,
// which is shared across modes; only the defer-scope
// granularity differs.
func replLoop(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr LineReader, session *shell.Session, file string) error {
	if file != "" {
		return replScript(ctx, cli, mgr, lr, session, file)
	}
	return replInteractive(ctx, cli, mgr, lr, session)
}

// replScript drives chunk-by-chunk evaluation in script mode but
// wraps the entire chunk loop in a single shell.WithDeferScope
// call. Each balanced statement is parsed and evaluated as its
// own program, so existing line-tracking (loc.line tied to each
// chunk's startLine) keeps error diagnostics pointed at the
// right source line, but every defer registered along the way
// fires when the wrapping scope unwinds at script exit -- not
// at the end of the chunk that registered it.
func replScript(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr LineReader, session *shell.Session, file string) error {
	env := &shell.Env{
		Session: session,
		PrintResult: func(v shell.Value) error {
			return writeValue(cli, v)
		},
		RenderDeferFailure: func(stmtLoc shell.Loc, args []shell.Arg, rc shell.Envelope) {
			renderEnvelopeFailure(cli, "defer", sourceLoc{file: file}, stmtLoc, args, rc)
		},
	}
	return shell.WithDeferScope(env, func() error {
		var lineNo int
		var buf strings.Builder
		var startLine int
		var cs contState
		for {
			input, err := lr.Readline()
			if err != nil {
				if err == io.EOF || err == ErrInterrupt {
					if buf.Len() > 0 {
						loc := sourceLoc{file: file, line: startLine}
						_ = cli.PrintErrf("%s[repl] error: unterminated block at end of input\n", loc)
						return errScriptError
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

			loc := sourceLoc{file: file, line: startLine}
			env.ExecCommand = makeExecCommand(ctx, cli, mgr, session, loc)
			env.ExecBind = makeExecBind(ctx, cli, mgr, session, loc)
			env.ExecAssertStmt = makeExecAssertStmt(cli, session, loc)
			if err := evalChunkInScope(cli, env, accumulated, loc); err != nil {
				return err
			}
		}
	})
}

// evalChunkInScope tokenises, parses, and evaluates one chunk
// against an env whose defer scope was already opened by the
// caller. Errors are rendered in the same shape as replEval:
// guard halts trigger renderEnvelopeFailure; everything else
// is printed with the chunk's source-loc prefix.
func evalChunkInScope(cli *bpfmancli.CLI, env *shell.Env, input string, loc sourceLoc) error {
	report := func(err error) error {
		errLoc := loc
		msg := err.Error()
		if line, rest, ok := splitLineColPrefix(msg); ok && loc.file != "" {
			// chunk-relative line from the parser/evaluator
			// composes with the chunk's startLine to yield the
			// absolute file line; matches the convention in
			// repl_assert.go.
			errLoc.line = loc.line + line - 1
			msg = rest
		}
		_ = cli.PrintErrf("%s[repl] error: %s\n", errLoc, msg)
		return errScriptError
	}
	tokens, err := shell.Tokenise(input)
	if err != nil {
		return report(err)
	}
	if len(tokens) == 0 {
		return nil
	}
	prog, err := shell.Parse(tokens)
	if err != nil {
		return report(err)
	}
	if err := shell.EvalProgramInScope(prog, env); err != nil {
		if errors.Is(err, errRequireFailed) {
			return err
		}
		var gf *shell.GuardFailure
		if errors.As(err, &gf) {
			renderEnvelopeFailure(cli, "guard", loc, gf.Loc, gf.Args, gf.Envelope)
			return errScriptError
		}
		return report(err)
	}
	return nil
}

// replInteractive runs the chunk-by-chunk loop suited to a
// readline prompt: each balanced statement is dispatched as
// its own EvalProgram call so the user sees their input
// take effect immediately and defers fire at the prompt
// boundary.
func replInteractive(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr LineReader, session *shell.Session) error {
	var lineNo int
	var buf strings.Builder
	var startLine int
	var cs contState
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == ErrInterrupt || err == io.EOF {
				if buf.Len() > 0 {
					_ = cli.PrintErrf("[repl] error: unterminated block at end of input\n")
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

		if hw, ok := lr.(HistoryWriter); ok {
			if entry := canonicaliseHistory(accumulated); entry != "" {
				_ = hw.SaveHistory(entry)
			}
		}

		// Interactive mode has no source file; loc stays
		// zero-valued. startLine is still tracked above for
		// possible future use.
		_ = startLine
		if err := replEval(ctx, cli, mgr, session, accumulated, sourceLoc{}); err != nil {
			return err
		}
	}
}

// canonicaliseHistory collapses a multi-line REPL submission into a
// single line suitable for a one-entry history record. Backslash
// continuations and bare newlines outside quoted strings become a
// single space, leading whitespace on continuation lines is dropped,
// and `#` comments outside quoted strings are stripped to the end of
// their line. Newlines inside quoted strings are preserved verbatim.
func canonicaliseHistory(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var inSingle, inDouble bool
	emitSpace := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inSingle && !inDouble && ch == '#' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i >= len(s) {
				break
			}
			ch = s[i]
		}
		if !inSingle && !inDouble && ch == '\\' && i+1 < len(s) && s[i+1] == '\n' {
			i++
			emitSpace = true
			continue
		}
		if !inSingle && !inDouble && ch == '\n' {
			emitSpace = true
			continue
		}
		if emitSpace {
			if ch == ' ' || ch == '\t' || ch == '\r' {
				continue
			}
			out := b.String()
			out = strings.TrimRight(out, " \t")
			b.Reset()
			b.WriteString(out)
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			emitSpace = false
		}
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}
		b.WriteByte(ch)
	}
	return strings.TrimSpace(b.String())
}

// contState tracks brace and parenthesis depth across accumulated
// input lines so the REPL knows when a multi-line if-block or
// parenthesised expression is complete. Quote state persists
// across lines so multi-line quoted strings are treated as a
// single literal span; unterminated strings themselves are
// surfaced by the tokeniser when the accumulated chunk is
// eventually parsed. lineCont records whether the line just
// consumed ended with an unescaped backslash outside quotes,
// which the tokeniser treats as a line continuation.
type contState struct {
	braces, parens     int
	inSingle, inDouble bool
	lineCont           bool
}

// advance walks one line of input, updating the brace and paren
// counters. Comments (`#` to end of line) outside a quoted string
// are ignored; quoted content is skipped so braces and parens
// inside strings do not count. The in-string flags are fields on
// the struct so they survive across line boundaries, matching how
// the tokeniser actually treats multi-line quoted literals.
func (c *contState) advance(line string) {
	c.lineCont = false
	lastNonSpace := -1
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
		case ch == '(':
			c.parens++
		case ch == ')':
			if c.parens > 0 {
				c.parens--
			}
		}
		if !c.inSingle && !c.inDouble && ch != ' ' && ch != '\t' && ch != '\r' {
			lastNonSpace = i
		}
	}
	if lastNonSpace >= 0 && line[lastNonSpace] == '\\' {
		c.lineCont = true
	}
}

// open reports whether the accumulated input is still inside an
// open brace or parenthesised group, or the line just consumed
// ended with a backslash continuation.
func (c *contState) open() bool {
	return c.braces > 0 || c.parens > 0 || c.lineCont
}

// shellCommands is the set of commands that are shell-language or
// session concerns rather than bpfman domain commands. These are
// handled directly by replEval and never reach the domain command
// dispatcher.
var shellCommands = map[string]bool{
	"alias":   true,
	"aliases": true,
	"assert":  true,
	"defs":    true,
	"exec":    true,
	"file":    true,
	"jq":      true,
	"kill":    true,
	"require": true,
	"print":   true,
	"help":    true,
	"source":  true,
	"start":   true,
	"unalias": true,
	"undef":   true,
	"unset":   true,
	"vars":    true,
	"version": true,
	"wait":    true,
}

// replShellCmd handles shell-language and session commands. It returns
// (true, value, err) if the command was handled, where value is
// non-nil for commands that produce an assignable result (e.g. exec).
// Returns (false, Value{}, nil) if the command is not a shell command
// and should be dispatched to the domain layer.
func replShellCmd(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, loc sourceLoc) (bool, shell.Value, error) {
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
	case "defs":
		return true, shell.Value{}, replDefs(cli, session)
	case "undef":
		return true, shell.Value{}, replUndef(session, argTexts(args[1:]))
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
	case "kill":
		env, err := replKill(args[1:])
		if err != nil {
			return true, shell.Value{}, err
		}
		return true, shell.ValueFromEnvelope(env), nil
	case "require":
		return true, shell.Value{}, replAssertRequire(ctx, cli, mgr, session, args[1:], true, loc)
	case "print":
		return true, shell.Value{}, replPrint(cli, args[1:])
	case "help":
		return true, shell.Value{}, replHelp(cli)
	case "source":
		return true, shell.Value{}, replSource(ctx, cli, mgr, session, argTexts(args[1:]))
	case "start":
		val, err := replStart(ctx, args[1:])
		return true, val, err
	case "unalias":
		return true, shell.Value{}, replUnalias(cli, session, argTexts(args[1:]))
	case "unset":
		return true, shell.Value{}, replUnset(cli, session, argTexts(args[1:]))
	case "vars":
		return true, shell.Value{}, replVars(cli, session)
	case "version":
		return true, shell.Value{}, replVersion(cli)
	case "wait":
		env, err := replWait(ctx, args[1:])
		if err != nil {
			return true, shell.Value{}, err
		}
		return true, shell.ValueFromEnvelope(env), nil
	default:
		return false, shell.Value{}, nil
	}
}

// replEval processes a single input line or block: tokenise, parse
// to an AST, and evaluate against the session. Shell-language
// commands (assert, require, print, help, source, unset, vars,
// version) flow through ExecCommand on the evaluator's Env; domain
// commands are dispatched via replDispatch from the same hook. In
// interactive mode (loc has no file), non-fatal errors are printed
// and replEval returns nil so the session continues. In script mode
// (loc has a file), errors return errScriptError to halt execution.
func replEval(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, input string, loc sourceLoc) error {
	reportErr := func(err error) error {
		errLoc := loc
		msg := err.Error()
		if loc.file != "" {
			if line, rest, ok := splitLineColPrefix(msg); ok {
				errLoc.line = loc.line + line - 1
				msg = rest
			}
		}
		_ = cli.PrintErrf("%s[repl] error: %s\n", errLoc, msg)
		if loc.file != "" {
			return errScriptError
		}
		return nil
	}

	tokens, err := shell.Tokenise(input)
	if err != nil {
		return reportErr(err)
	}
	if len(tokens) == 0 {
		return nil
	}

	prog, err := shell.Parse(tokens)
	if err != nil {
		return reportErr(err)
	}

	env := &shell.Env{
		Session:        session,
		ExecCommand:    makeExecCommand(ctx, cli, mgr, session, loc),
		ExecBind:       makeExecBind(ctx, cli, mgr, session, loc),
		ExecAssertStmt: makeExecAssertStmt(cli, session, loc),
		PrintResult: func(v shell.Value) error {
			return writeValue(cli, v)
		},
		RenderDeferFailure: func(stmtLoc shell.Loc, args []shell.Arg, rc shell.Envelope) {
			renderEnvelopeFailure(cli, "defer", loc, stmtLoc, args, rc)
		},
	}

	if err := shell.EvalProgram(prog, env); err != nil {
		if errors.Is(err, errRequireFailed) {
			return err
		}
		var gf *shell.GuardFailure
		if errors.As(err, &gf) {
			renderEnvelopeFailure(cli, "guard", loc, gf.Loc, gf.Args, gf.Envelope)
			if loc.file != "" {
				return errScriptError
			}
			return nil
		}
		return reportErr(err)
	}
	return nil
}

// splitLineColPrefix recognises the "LINE:COL: REST" diagnostic
// prefix produced by shell.locErrorf. When present, it returns
// the line number and the remainder of the message; the caller
// uses the line to replace the script-level prefix's line
// (which would otherwise always be 1 in script mode after the
// whole-script slurp).
func splitLineColPrefix(msg string) (int, string, bool) {
	colon1 := strings.IndexByte(msg, ':')
	if colon1 <= 0 {
		return 0, "", false
	}
	line, err := strconv.Atoi(msg[:colon1])
	if err != nil || line <= 0 {
		return 0, "", false
	}
	rest := msg[colon1+1:]
	colon2 := strings.IndexByte(rest, ':')
	if colon2 <= 0 {
		return 0, "", false
	}
	if _, err := strconv.Atoi(rest[:colon2]); err != nil {
		return 0, "", false
	}
	rest = rest[colon2+1:]
	rest = strings.TrimPrefix(rest, " ")
	return line, rest, true
}

// renderEnvelopeFailure prints a captured-result failure as a
// labelled block: the verb header (guard, require, assert, defer),
// the source position of the failing statement, the resolved
// command line, the exit code, and any captured stdout and stderr.
// Empty stdout/stderr emit just the label; multi-line text is
// indented two spaces per line. The format matches the shape
// described in the REPL design's section on the shared failure
// renderer.
func renderEnvelopeFailure(cli *bpfmancli.CLI, verb string, scriptLoc sourceLoc, stmtLoc shell.Loc, args []shell.Arg, env shell.Envelope) {
	file := scriptLoc.file
	if file == "" {
		file = "<repl>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] FAIL at %s:%d\n", verb, file, stmtLoc.Line)
	b.WriteString("command:\n")
	if argv := argTexts(args); len(argv) > 0 {
		fmt.Fprintf(&b, "  %s\n", strings.Join(argv, " "))
	}
	fmt.Fprintf(&b, "exit:\n  %d\n", env.Code)
	b.WriteString("stdout:\n")
	writeIndented(&b, env.Stdout)
	b.WriteString("stderr:\n")
	writeIndented(&b, env.Stderr)
	_ = cli.PrintErrf("%s", b.String())
}

// writeIndented appends s to b with each line prefixed by two
// spaces. A trailing newline on s is dropped before splitting so a
// captured stdout that already ended in '\n' does not produce a
// blank indented line at the end.
func writeIndented(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// makeExecCommand bridges the evaluator's top-level CommandStmt
// dispatch into the REPL pipeline. Output is visible on the CLI.
// Alias expansion applies to the first argument before dispatch.
// The returned Value is ignored by the evaluator for top-level
// commands; it is still produced so shell builtins can compute
// values that callers happen to observe in tests.
//
// Dispatch order: aliases expand first; registered shell builtins
// (replShellCmd) handle their own names; the bpfman domain
// dispatcher handles "bpfman ..."; an unrecognised first word
// falls through to runExternal so 'ip link add ...' spawns the
// system 'ip' without an explicit 'exec' prefix.
func makeExecCommand(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, loc sourceLoc) func([]shell.Arg) (shell.Value, error) {
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
		first := argText(args[0])
		if first == "bpfman" {
			return replDispatch(ctx, cli, mgr, args)
		}
		if domainNouns[first] {
			return shell.Value{}, fmt.Errorf("domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(argTexts(args), " "))
		}
		// Fallthrough: unknown first word runs as a subprocess.
		return replExec(ctx, cli, args)
	}
}

// makeExecBind bridges the evaluator's BindStmt dispatch into the
// REPL pipeline. The hook returns a BindResult carrying the result
// envelope (Rc) and the provider's primary result. Non-zero exit
// and runtime errors map to Rc.OK: false; the script decides
// whether to halt (guard) or inspect (let). Output is suppressed.
//
// Dispatch order on the right of '<-':
//
//   - 'exec NAME ARGS' is the explicit force-external escape
//     hatch: NAME runs as a subprocess, primary is the rc
//     envelope.
//   - registered shell builtins (jq, file, ...) handle their own
//     names; primary is the builtin's typed Value, or the rc
//     envelope when the builtin produces no value.
//   - 'bpfman ...' dispatches in-process; primary is the typed
//     payload on success, zero Value on failure.
//   - any other first word is treated as an unknown name and
//     runs as a subprocess (the registry's implicit fallthrough).
//     'ip link del foo', 'bpftool map dump id 5', etc. work
//     without an explicit 'exec' prefix.
func makeExecBind(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, loc sourceLoc) func([]shell.Arg) (shell.BindResult, error) {
	return func(args []shell.Arg) (shell.BindResult, error) {
		args = applyAlias(session, args)
		if len(args) == 0 {
			return shell.BindResult{}, fmt.Errorf("empty command form on '<-' RHS")
		}

		if argText(args[0]) == "exec" {
			return runExternalAsBind(ctx, args[1:])
		}

		// 'wait $job' is special-cased so the bind's Rc
		// reflects the JOB's outcome, not merely "wait
		// succeeded". 'guard r <- wait $job' must halt when
		// the background process exited non-zero (or fail
		// to launch); a not-ok job is the kind of failure
		// the script wanted to gate on.
		if argText(args[0]) == "wait" {
			env, err := replWait(ctx, args[1:])
			if err != nil {
				return shell.BindResult{}, err
			}
			return shell.BindResult{Rc: env, Primary: shell.ValueFromEnvelope(env)}, nil
		}

		quiet := cli.WithDiscardOutput()
		handled, val, err := replShellCmd(ctx, quiet, mgr, session, args, loc)
		if handled {
			if err != nil {
				rc := shell.Envelope{OK: false, Code: 1, Stderr: err.Error()}
				return shell.BindResult{Rc: rc, Primary: shell.ValueFromEnvelope(rc)}, nil
			}
			rc := shell.Envelope{OK: true, Code: 0}
			primary := val
			if primary.IsNil() {
				primary = shell.ValueFromEnvelope(rc)
			}
			return shell.BindResult{Rc: rc, Primary: primary}, nil
		}

		first := argText(args[0])
		if first == "bpfman" {
			val, err := replDispatch(ctx, quiet, mgr, args)
			if err != nil {
				rc := shell.Envelope{OK: false, Code: 1, Stderr: err.Error()}
				return shell.BindResult{Rc: rc, Primary: shell.Value{}}, nil
			}
			rc := shell.Envelope{OK: true, Code: 0}
			return shell.BindResult{Rc: rc, Primary: val}, nil
		}
		if domainNouns[first] {
			rc := shell.Envelope{
				OK:     false,
				Code:   1,
				Stderr: fmt.Sprintf("domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(argTexts(args), " ")),
			}
			return shell.BindResult{Rc: rc, Primary: shell.ValueFromEnvelope(rc)}, nil
		}
		// Fallthrough: unknown first word runs as a subprocess.
		return runExternalAsBind(ctx, args)
	}
}

// runExternalAsBind runs args as a subprocess and packages the
// outcome as a BindResult. A launch failure (command not found,
// permission denied) returns a Go error; a non-zero exit is
// captured into the rc envelope so '<-' callers can inspect it.
func runExternalAsBind(ctx context.Context, args []shell.Arg) (shell.BindResult, error) {
	cap, err := runExternal(ctx, args)
	if err != nil {
		return shell.BindResult{}, err
	}
	rc := shell.Envelope{
		OK:     cap.ExitCode == 0,
		Code:   cap.ExitCode,
		Stdout: cap.Stdout,
		Stderr: cap.Stderr,
	}
	return shell.BindResult{Rc: rc, Primary: shell.ValueFromEnvelope(rc)}, nil
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
func replSource(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []string) error {
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

	// Accumulate physical lines into logical statements, mirroring the
	// continuation logic that replLoop uses for the interactive REPL
	// and for `bpfman-shell -f`. Without this, multi-line forms in a
	// sourced file (def / if / foreach / retry blocks, command
	// substitutions that span lines) would each fail to parse on
	// their first line because the open brace or bracket has not yet
	// been closed.
	var lineNo int
	var buf strings.Builder
	var startLine int
	var cs contState
	for {
		input, err := lr.Readline()
		if err != nil {
			if err == io.EOF {
				if buf.Len() > 0 {
					return fmt.Errorf("source %q: unterminated block at end of file (started at line %d)", args[0], startLine)
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

		loc := sourceLoc{file: args[0], line: startLine}
		if err := replEval(ctx, cli, mgr, session, accumulated, loc); err != nil {
			return err
		}
	}
}

// domainNouns is the set of top-level words that parseCommand
// recognises after a leading "bpfman". It exists so replDispatch can
// distinguish "forgot the bpfman prefix" (suggest prefixing) from
// "this word is not a command at all" (just say unknown).
var domainNouns = map[string]bool{
	"program":    true,
	"show":       true,
	"link":       true,
	"dispatcher": true,
	"audit":      true,
}

// replDispatch dispatches a "bpfman ..." command into the in-process
// domain pipeline. The caller has already verified that the first
// argument is "bpfman" (registered-name routing happens at the
// makeExecCommand / makeExecBind level, before this function is
// reached). parseCommand routes arguments to the per-command parser
// and returns a typed Command node; execCommand dispatches via a
// type-switch.
func replDispatch(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 || argText(args[0]) != "bpfman" {
		return shell.Value{}, fmt.Errorf("replDispatch: expected leading \"bpfman\", got %v", argTexts(args))
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

func replHelp(cli *bpfmancli.CLI) error {
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
	b.WriteString("    bpfman audit [rule...]                          Audit coherency (read-only; use CLI for --repair)\n")
	b.WriteString("    bpfman audit explain [rule]                     Explain a coherency rule\n")
	b.WriteString("\n")
	b.WriteString("Shell commands:\n")
	b.WriteString("\n")
	b.WriteString("  exec <command> [args|file:$var...]        Run a host command\n")
	b.WriteString("  file temp $var[.path]                     Write value to temp file (assignable)\n")
	b.WriteString("  jq <filter> <value>                       Apply a jq filter to a value (assignable)\n")
	b.WriteString("  print <value>...                          Print one or more values (one pretty, many compact space-joined)\n")
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
	b.WriteString("  let X = EXPR                      Bind an expression result\n")
	b.WriteString("  let X <- COMMAND                  Bind a command's primary result\n")
	b.WriteString("  guard X <- COMMAND                Bind primary; halt on failure\n")
	b.WriteString("  let (rc, X) <- COMMAND            Bind result envelope and primary\n")
	b.WriteString("  guard (rc, X) <- COMMAND          Same; halt on failure\n")
	b.WriteString("  bpfman show program $prog         Variable reference (auto-extracts program ID)\n")
	b.WriteString("\n")
	b.WriteString("Assertions:\n")
	b.WriteString("  assert <verb> [args...]       Check condition, continue on failure\n")
	b.WriteString("  require <verb> [args...]      Check condition, stop on failure\n")
	b.WriteString("  assert not <verb> [args...]   Negate condition\n")
	b.WriteString("\n")
	b.WriteString("  Operators (infix): == != < <= > >=  (semantics chosen by operand type)\n")
	b.WriteString("  Verbs: nil, not-empty, ok, fail, path exists, contains\n")
	b.WriteString("\n")
	b.WriteString("Single-arg form: assert <bool-expr>      e.g. assert $flag, assert true\n")
	b.WriteString("\n")
	b.WriteString("Coerce stringy numeric input via [$x |> jq tonumber] before comparing.\n")
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

// replVersion prints version information.
func replVersion(cli *bpfmancli.CLI) error {
	return cli.PrintOut(version.Get().Long())
}
