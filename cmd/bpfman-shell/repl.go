package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

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
// without touching the manager, session, or evaluator. When
// --ast is set, Run short-circuits to a dump-only mode that
// reads the same input, parses each chunk, and prints the AST
// tree to stdout; nothing is evaluated.
func (c *CLI) Run(ctx context.Context) error {
	if c.Check {
		return c.runCheck()
	}
	if c.AST {
		return c.runAST()
	}
	mgr, cleanup, err := c.NewManagerWithPuller(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	session := shell.NewSession()
	if c.Trace {
		session.SetTrace(true)
	}

	lr, err := c.newReader(ctx, mgr, session)
	if err != nil {
		return err
	}
	defer lr.Close()

	// Three input shapes:
	//   --script <FILE>      file != "" (the named script).
	//   stdin pipe / -       file = "<stdin>" (still a script
	//                        contract, just from stdin).
	//   bare TTY invocation  file = "" and interactive = true.
	// The string is for diagnostics; the boolean is the
	// authoritative mode flag downstream code consults.
	file := c.Script
	interactive := false
	switch {
	case file == "-":
		file = "<stdin>"
	case file == "" && !term.IsTerminal(int(os.Stdin.Fd())):
		file = "<stdin>"
	case file == "":
		interactive = true
	}
	loopErr := replLoop(ctx, &c.CLI, mgr, lr, session, file, interactive, c.NoCheck)

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

	if n := session.JobLeaks(); n > 0 {
		_ = c.PrintErrf("%d job(s) leaked\n", n)
		return fmt.Errorf("%d job(s) leaked", n)
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
// interrupt. Two modes with deliberately different policies:
//
// In script mode (file != "") the chunk loop runs inside one
// outer WithJobScope and one outer WithDeferScope. 'defer
// cleanup' fires at script end and the script-wide leak walk
// reports any unmanaged job as '[job] FAIL ...', kills it,
// and pushes the exit code non-zero. Scripts are a
// reproducible test contract; leaking a job is a bug.
//
// In interactive mode (file == "") the loop opens one outer
// WithJobScope around the whole readline session but a fresh
// WithDeferScope per chunk. Defers fire at end of prompt
// (typing 'defer kill $p' as part of the same chunk that
// started the job is the canonical self-contained idiom);
// jobs that no chunk waited or killed are cleaned up silently
// at session end (Ctrl+D) by a leak handler that just
// SIGKILLs the process group, prints nothing, and does not
// affect the exit code. The REPL is exploratory scratch
// space; starting something and then exiting is normal use,
// not a failure to punish.
//
// Variable assignment and expansion use the shell.Session,
// which is shared across modes.
func replLoop(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr LineReader, session *shell.Session, file string, interactive, noCheck bool) error {
	if interactive {
		return replInteractive(ctx, cli, mgr, lr, session)
	}
	return replScript(ctx, cli, mgr, lr, session, file, noCheck)
}

// replScript drives chunk-by-chunk evaluation in script mode
// but wraps the entire chunk loop in a single shell.WithJobScope
// outside a single shell.WithDeferScope. Each balanced statement
// is parsed and evaluated as its own program, so existing
// line-tracking (loc.line tied to each chunk's startLine) keeps
// error diagnostics pointed at the right source line, but every
// defer registered along the way fires when the script-wide
// defer scope unwinds at script exit -- not at the end of the
// chunk that registered it -- and the unmanaged-job walk runs
// once over the whole script. Defers nest inside jobs so
// 'defer kill $job' marks a job Managed before the leak walk
// sees it.
//
// SIGINT and SIGTERM cancel the script-wide context, matching
// the way a bash script aborts on ^C. Children spawned via
// the bind path (capture exec) observe the same context and
// shut down with the script; children spawned via the
// foreground inherit path receive the signal directly through
// the TTY's foreground process group.
func replScript(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr LineReader, session *shell.Session, file string, noCheck bool) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Slurp the whole input up-front so we can run the
	// static checker as a pre-flight before any side
	// effects fire. The buffered content is then re-read
	// through a fresh scanner-backed LineReader for the
	// existing chunk-by-chunk evaluator, preserving the
	// per-chunk loc machinery the runtime error path relies
	// on. Static issues from Check are reported with
	// 'file:line: error: ...' and returned as errScriptError
	// so the caller exits non-zero without evaluating any
	// statement.
	src, slurpErr := slurpReader(lr)
	if slurpErr != nil {
		_ = cli.PrintErrf("%s: %v\n", file, slurpErr)
		return errScriptError
	}
	if !noCheck {
		if hadIssues := preflightCheck(cli, file, src); hadIssues {
			return errScriptError
		}
	}
	lr = NewScannerReader(strings.NewReader(src), nil)

	env := &shell.Env{
		Session: session,
		PrintResult: func(v shell.Value) error {
			return writeValue(cli, v)
		},
		RenderDeferFailure: func(stmtLoc shell.Pos, args []shell.Arg, rc shell.Envelope) {
			renderEnvelopeFailure(cli, "defer", sourceLoc{file: file}, stmtLoc, args, rc)
		},
		HandleJobLeak: strictJobLeakHandler(cli, session),
	}
	return shell.WithJobScope(env, func() error {
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
							_ = cli.PrintErrf("%serror: unterminated block at end of input\n", loc)
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
				env.ExecCommand = makeExecCommand(ctx, cli, mgr, session, env, loc)
				env.ExecBind = makeExecBind(ctx, cli, mgr, session, env, loc)
				env.ExecAssertStmt = makeExecAssertStmt(cli, session, loc)
				env.Trace = makeTraceHook(cli, session, loc)
				if err := evalChunkInScope(cli, env, accumulated, src, loc); err != nil {
					return err
				}
			}
		})
	})
}

// evalChunkInScope tokenises, parses, and evaluates one chunk
// against an env whose defer scope was already opened by the
// caller. Typed errors with a Span are rendered as rust-style
// frames against frameSrc; the chunk-relative Span is shifted
// by loc.line so frames cite the absolute file line. When
// frameSrc is empty (interactive mode without a slurped buffer)
// the chunk input itself is used as the frame source. Errors
// without typed Span info fall back to the plain single-line
// "file:line:col: error: msg" shape.
func evalChunkInScope(cli *bpfmancli.CLI, env *shell.Env, input, frameSrc string, loc sourceLoc) error {
	emitFrame := func(span shell.Span, msg string) {
		src := frameSrc
		shift := loc.line - 1
		if src == "" {
			src = input
			shift = 0
		}
		shifted := span
		if shift > 0 {
			shifted.Pos.Line += shift
			shifted.End.Line += shift
		}
		_ = cli.PrintErr(shell.RenderDiagnostic(src, loc.file, shell.Diagnostic{
			Span: shifted,
			Msg:  msg,
		}))
	}
	report := func(err error) error {
		var re *RuntimeError
		if errors.As(err, &re) {
			// Runtime-outcome failure (bpfman dispatch
			// returned an error, or a builtin that opted in).
			// Same citation shape as the exec-failure family:
			// `file:line: msg` in batch, `msg` only in
			// interactive. The construct itself is fine; the
			// runtime fact is what the user needs to see.
			if loc.file != "" {
				shift := loc.line - 1
				line := re.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s\n", loc.file, line, re.Msg)
			} else {
				_ = cli.PrintErrf("%s\n", re.Msg)
			}
			return errScriptError
		}
		var ae *ExecArgError
		if errors.As(err, &ae) {
			// An argument cannot flatten into argv text -- a
			// structured value passed where a scalar was
			// needed, or an unknown adapter form. The source
			// is well-formed; the runtime value just does not
			// compose. Cite, do not frame.
			if loc.file != "" {
				shift := loc.line - 1
				line := ae.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s\n", loc.file, line, ae.Msg)
			} else {
				_ = cli.PrintErrf("%s\n", ae.Msg)
			}
			return errScriptError
		}
		var cnf *CommandNotFound
		if errors.As(err, &cnf) {
			// Subprocess fallthrough hit a name that does not
			// resolve on $PATH. The source is well-formed; the
			// name just is not a command. Cite the line in
			// batch, emit a single line in interactive. No
			// frame, no carets -- the construct is fine.
			if loc.file != "" {
				shift := loc.line - 1
				line := cnf.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s: command not found\n", loc.file, line, cnf.Name)
			} else {
				_ = cli.PrintErrf("%s: command not found\n", cnf.Name)
			}
			return errScriptError
		}
		var ef *ExecFailure
		if errors.As(err, &ef) {
			// Subprocess exited non-zero. The child wrote its
			// own diagnostics to the inherited stdout/stderr,
			// so we do not repeat them; we just say where the
			// script tripped.
			//
			//   Interactive (loc.file == ""): silent. The
			//     prompt returning is the signal.
			//   Batch: one citation line, optionally
			//     followed by indented stdout/stderr blocks
			//     if the executor captured any.
			if loc.file != "" {
				shift := loc.line - 1
				line := ef.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s: exit %d\n", loc.file, line, strings.Join(ef.Argv, " "), ef.ExitCode)
				if ef.Stdout != "" {
					_ = cli.PrintErrf("stdout:\n")
					for _, l := range strings.Split(strings.TrimRight(ef.Stdout, "\n"), "\n") {
						_ = cli.PrintErrf("  %s\n", l)
					}
				}
				if ef.Stderr != "" {
					_ = cli.PrintErrf("stderr:\n")
					for _, l := range strings.Split(strings.TrimRight(ef.Stderr, "\n"), "\n") {
						_ = cli.PrintErrf("  %s\n", l)
					}
				}
			}
			return errScriptError
		}
		var se *shell.SyntaxError
		if errors.As(err, &se) && se.Span.Pos.Line > 0 {
			emitFrame(se.Span, se.Msg)
			return errScriptError
		}
		// Defensive: an error reached here without a Span.
		// After G1 every parser/runtime path is typed, so
		// this should be unreachable; if it fires, print a
		// flat line so something still surfaces.
		_ = cli.PrintErrf("%serror: %v\n", loc, err)
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
		if errors.Is(err, errScriptError) {
			// A nested evaluation (a sourced file, a def
			// body, ...) already rendered its own
			// diagnostic and returned the script-error
			// sentinel to ask the caller to halt. Propagate
			// the sentinel unchanged: re-rendering would
			// frame the same failure a second time at the
			// outer call site (e.g. underlining 'source
			// foo.bpfman' with 'error: script error'),
			// hiding the real cause that the inner level
			// already emitted.
			return err
		}
		var gf *shell.GuardFailure
		if errors.As(err, &gf) {
			renderEnvelopeFailure(cli, "guard", loc, gf.Pos, gf.Args, gf.Envelope)
			return errScriptError
		}
		return report(err)
	}
	return nil
}

// replInteractive runs the chunk-by-chunk loop suited to a
// readline prompt. The whole session runs inside one outer
// WithJobScope so 'start' / 'wait' / 'kill' work across prompts
// and unmanaged jobs surface only at session end. Each chunk
// opens its own WithDeferScope so 'defer cleanup' fires at end
// of prompt; the single-prompt idiom
//
//	bpfman> guard p <- start sleep 60; defer kill $p
//
// works because the chunk's defer runs before any leak check
// would: kill marks the job Managed and the session-end walk
// sees nothing to clean up.
//
// The job-leak handler is silent in interactive: jobs the user
// never waited on are SIGKILLed at Ctrl+D with no diagnostic
// and no exit-code effect. The REPL is exploratory scratch
// space; starting something and exiting is normal use, not a
// failure to punish. A future --strict-jobs flag (or a
// 'session' opt-in for explicit lifetime) can give power
// users the script-mode policy at the prompt.
//
// def bodies continue to open inner defer scopes via callDef so
// a def's own defers fire at def return.
func replInteractive(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, lr LineReader, session *shell.Session) error {
	env := &shell.Env{
		Session: session,
		PrintResult: func(v shell.Value) error {
			return writeValue(cli, v)
		},
		RenderDeferFailure: func(stmtLoc shell.Pos, args []shell.Arg, rc shell.Envelope) {
			renderEnvelopeFailure(cli, "defer", sourceLoc{}, stmtLoc, args, rc)
		},
		HandleJobLeak: silentJobLeakHandler(),
	}

	// Continuation-prompt support: when the accumulated chunk
	// is mid-form (unclosed brace, paren, quote, or backslash
	// line-continuation), swap the primary prompt for a
	// continuation prompt so the user sees they are still
	// inside an unfinished form. Without this the silent
	// 'bpfman> ' looked identical to a fresh prompt and a
	// stray '#' that swallowed a closing delimiter would
	// silently buffer every subsequent line. Backends without
	// a visible prompt (scanner-based readers used by tests
	// and pipes) implement nothing and the prompt swap is a
	// no-op.
	const promptPrimary = "bpfman> "
	const promptContinue = "... "
	setPrompt := func(p string) {
		if ps, ok := lr.(PromptSetter); ok {
			ps.SetPrompt(p)
		}
	}

	return shell.WithJobScope(env, func() error {
		var lineNo int
		var buf strings.Builder
		var startLine int
		var cs contState
		for {
			input, err := lr.Readline()
			if err != nil {
				if err == ErrInterrupt || err == io.EOF {
					if buf.Len() > 0 {
						_ = cli.PrintErrf("error: unterminated block at end of input\n")
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
				setPrompt(promptContinue)
				continue
			}

			accumulated := buf.String()
			buf.Reset()
			cs = contState{}
			setPrompt(promptPrimary)

			if hw, ok := lr.(HistoryWriter); ok {
				if entry := canonicaliseHistory(accumulated); entry != "" {
					_ = hw.SaveHistory(entry)
				}
			}

			// Interactive mode has no source file but does
			// track absolute session line numbers so the
			// trace hook can cite '<repl>:N'. loc.line carries
			// startLine through unchanged; evalChunkInScope's
			// report helper still keys batch-vs-interactive
			// rendering off loc.file == "".
			loc := sourceLoc{line: startLine}

			// Per-chunk SIGINT: a ^C at the prompt or
			// during a long-running builtin cancels just
			// this chunk's context, never the loop's.
			// signal.NotifyContext installs a watcher
			// scoped to chunkCtx; cancel() removes it
			// before the next prompt so SIGINT delivered
			// between prompts is observed by main.go's
			// hard-exit watcher (a second ^C still kills
			// the process). Foreground externals do not
			// observe chunkCtx for cancellation (the
			// inherit path uses exec.Command, not
			// CommandContext) because ^C reaches the
			// child directly through the TTY foreground
			// group and the child handles it.
			chunkCtx, cancelChunk := signal.NotifyContext(ctx, syscall.SIGINT)

			env.ExecCommand = makeExecCommand(chunkCtx, cli, mgr, session, env, loc)
			env.ExecBind = makeExecBind(chunkCtx, cli, mgr, session, env, loc)
			env.ExecAssertStmt = makeExecAssertStmt(cli, session, loc)
			env.Trace = makeTraceHook(cli, session, loc)

			chunkErr := shell.WithDeferScope(env, func() error {
				return evalChunkInScope(cli, env, accumulated, "", loc)
			})
			cancelChunk()

			if chunkErr == nil {
				continue
			}
			if errors.Is(chunkErr, errRequireFailed) {
				return chunkErr
			}
			// Chunk errors (parse, runtime, guard halt) are
			// already rendered by evalChunkInScope; swallow
			// the errScriptError sentinel so the next
			// prompt is reached rather than tearing the
			// session down. Context-cancellation errors
			// from a ^C-interrupted builtin are similarly
			// swallowed -- the user asked for the chunk
			// to stop, and the next prompt is the answer.
		}
	})
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

// replShellCmd handles shell-language and session commands by
// looking up the first token in builtinRegistry and invoking
// the matching handler. Returns (true, value, err) when the
// registry has an entry for the command; (false, Value{}, nil)
// means "not a shell builtin -- try the domain layer". The
// value is the assignable primary for builtins that produce
// one (exec, file, jq, kill, start, wait); it is the zero
// Value for builtins that do not bind anything (alias, vars,
// help, ...).
func replShellCmd(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, args []shell.Arg, loc sourceLoc, span shell.Span) (bool, shell.Value, error) {
	if len(args) == 0 {
		return false, shell.Value{}, nil
	}
	cmd := argText(args[0])
	b, ok := builtinRegistry[cmd]
	if !ok {
		return false, shell.Value{}, nil
	}
	c := builtinCtx{
		Ctx:  ctx,
		CLI:  cli,
		Mgr:  mgr,
		Env:  env,
		Cmd:  cmd,
		Args: args[1:],
		Pos:  loc,
		Span: span,
	}
	val, err := b.Handler(c)
	// Frame any non-typed error at the originating command's Span
	// so the renderer can draw a frame regardless of which handler
	// emitted it. Handlers that pinpoint a tighter Span themselves
	// keep theirs (FrameAt short-circuits on existing *SyntaxError).
	return true, val, shell.FrameAt(span, err)
}

// renderEnvelopeFailure prints a captured-result failure as a
// labelled block: the verb header (guard, require, assert, defer),
// the source position of the failing statement, the resolved
// command line, the exit code, and any captured stdout and stderr.
// Empty stdout/stderr emit just the label; multi-line text is
// indented two spaces per line. The format matches the shape
// described in the REPL design's section on the shared failure
// renderer.
func renderEnvelopeFailure(cli *bpfmancli.CLI, verb string, scriptLoc sourceLoc, stmtLoc shell.Pos, args []shell.Arg, env shell.Envelope) {
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
func makeExecCommand(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, loc sourceLoc) func([]shell.Arg, shell.Span) (shell.Value, error) {
	return func(args []shell.Arg, span shell.Span) (shell.Value, error) {
		if len(args) == 0 {
			return shell.Value{}, nil
		}
		args = applyAlias(session, args)
		handled, val, err := replShellCmd(ctx, cli, mgr, session, env, args, loc, span)
		if err != nil {
			return shell.Value{}, err
		}
		if handled {
			return val, nil
		}
		first := argText(args[0])
		// Frame errors from the bpfman domain dispatcher and
		// the subprocess fallthrough at the originating
		// command's Span. The dispatcher and parser sites
		// inside command.go return plain fmt.Errorf strings
		// today; wrapping at the boundary covers all of them
		// in one place. FrameAt short-circuits on existing
		// *SyntaxError values so any tighter Span set
		// downstream (e.g. via SpanErrorf) is preserved.
		if first == "bpfman" {
			val, err := replDispatch(ctx, cli, mgr, args)
			if err != nil {
				// bpfman dispatch failures are runtime
				// outcomes on a well-formed construct: the
				// statement parsed and reached the manager,
				// the manager returned a fact. Cite, do
				// not frame -- mirrors the policy applied
				// to bare subprocess exit failures.
				return val, &RuntimeError{Msg: err.Error(), Span: span}
			}
			return val, nil
		}
		if domainNouns[first] {
			return shell.Value{}, shell.SpanErrorf(span, "domain commands require a \"bpfman\" prefix: try %q", "bpfman "+strings.Join(argTexts(args), " "))
		}
		// Fallthrough: unknown first word runs as a subprocess.
		// Resolve the executable on $PATH first so an unknown
		// command reports as "name: command not found" rather
		// than producing a downstream argument-flatten failure
		// when one of the remaining arguments cannot be turned
		// into a shell scalar.
		if err := resolveCommandPath(first, span); err != nil {
			return shell.Value{}, err
		}
		val, err = runExecStatement(ctx, cli, args, span)
		return val, shell.FrameAt(span, err)
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
func makeExecBind(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, loc sourceLoc) func([]shell.Arg, shell.Span) (shell.BindResult, error) {
	return func(args []shell.Arg, span shell.Span) (shell.BindResult, error) {
		args = applyAlias(session, args)
		if len(args) == 0 {
			return shell.BindResult{}, shell.SpanErrorf(span, "empty command form on '<-' RHS")
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

		// 'net exec $pair CMD...' captures into a real
		// envelope so the bind's Rc reflects the netns
		// command's actual outcome, not merely "the handler
		// returned without an error". Mirrors the wait
		// special case so 'guard _ <- net exec $pair ping'
		// halts on a non-zero ping exactly the way 'guard _
		// <- ip netns exec NS ping' does in the
		// pre-migration scripts.
		if argText(args[0]) == "net" && len(args) >= 2 && argText(args[1]) == "exec" {
			env, err := replNetExec(ctx, args[2:])
			if err != nil {
				return shell.BindResult{}, err
			}
			return shell.BindResult{Rc: env, Primary: shell.ValueFromEnvelope(env)}, nil
		}

		quiet := cli.WithDiscardOutput()
		handled, val, err := replShellCmd(ctx, quiet, mgr, session, env, args, loc, span)
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

// makeTraceHook builds the Env.Trace closure for one chunk. The
// closure consults session.TraceEnabled() on every invocation so
// `trace on` / `trace off` can toggle tracing mid-script without
// rebuilding the Env. Output goes to the cli's stderr with a `+ `
// prefix and a `file:line:` citation; interactive sessions (file
// unset) cite as `<repl>:N` so a long session's trace still
// resolves to a specific input chunk.
func makeTraceHook(cli *bpfmancli.CLI, session *shell.Session, loc sourceLoc) func(int, string) {
	return func(line int, rendered string) {
		if !session.TraceEnabled() {
			return
		}
		shift := loc.line - 1
		abs := line + shift
		file := loc.file
		if file == "" {
			file = "<repl>"
		}
		_ = cli.PrintErrf("+ %s:%d: %s\n", file, abs, rendered)
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

// replSource reads commands from a file and executes each line in
// the caller's session. Source is shaped like a def body, not a
// fresh script: it inherits the caller's session (vars, defs,
// aliases, jobs) and opens its own defer scope so 'defer
// cleanup' near the top of a sourced file fires when source
// returns, but it does not open a new job scope. Jobs started
// in the sourced file therefore live in the caller's job
// scope: 'jobs' at the prompt sees them, 'wait $p' / 'kill $p'
// work on $p that the file bound, and the caller's leak policy
// applies on the caller's scope-exit. That matches the bash
// mental model the user reaches for when they type 'source'
// and the principle of least astonishment that anything bound
// in the file is observable in the caller after the call
// returns.
//
// Defers stay file-scoped because they are statement-level
// cleanup ('when this completes'), and the natural completion
// boundary for a sourced file is 'when source returns'. If
// defers also inherited the caller's scope, a library file's
// 'defer cleanup' would silently accumulate in the caller and
// fire at some unknowable later moment; treating the file as
// the cleanup boundary is the predictable answer.
//
// Nested source commands are rejected to prevent unbounded
// recursion.
func replSource(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, env *shell.Env, args []string) error {
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
	file := args[0]
	session := env.Session

	// Borrow the caller's env. Save the per-chunk dispatch
	// fields and the defer-failure renderer so we can restore
	// them when source returns; everything else (Session,
	// PrintResult, HandleJobLeak, jobs registry) flows through
	// unchanged so the caller's policy applies.
	savedExecCommand := env.ExecCommand
	savedExecBind := env.ExecBind
	savedExecAssert := env.ExecAssertStmt
	savedRenderDefer := env.RenderDeferFailure
	savedTrace := env.Trace
	defer func() {
		env.ExecCommand = savedExecCommand
		env.ExecBind = savedExecBind
		env.ExecAssertStmt = savedExecAssert
		env.RenderDeferFailure = savedRenderDefer
		env.Trace = savedTrace
	}()
	env.RenderDeferFailure = func(stmtLoc shell.Pos, args []shell.Arg, rc shell.Envelope) {
		renderEnvelopeFailure(cli, "defer", sourceLoc{file: file}, stmtLoc, args, rc)
	}

	return shell.WithDeferScope(env, func() error {
		// Accumulate physical lines into logical statements,
		// mirroring the continuation logic that replLoop uses
		// for the interactive REPL and for 'bpfman-shell
		// FILE'. Without this, multi-line forms in a sourced
		// file (def / if / foreach / retry blocks, command
		// substitutions that span lines) would each fail to
		// parse on their first line because the open brace
		// or bracket has not yet been closed.
		var lineNo int
		var buf strings.Builder
		var startLine int
		var cs contState
		for {
			input, err := lr.Readline()
			if err != nil {
				if err == io.EOF {
					if buf.Len() > 0 {
						return fmt.Errorf("source %q: unterminated block at end of file (started at line %d)", file, startLine)
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
			env.ExecCommand = makeExecCommand(ctx, cli, mgr, session, env, loc)
			env.ExecBind = makeExecBind(ctx, cli, mgr, session, env, loc)
			env.ExecAssertStmt = makeExecAssertStmt(cli, session, loc)
			env.Trace = makeTraceHook(cli, session, loc)
			if err := evalChunkInScope(cli, env, accumulated, "", loc); err != nil {
				return err
			}
		}
	})
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
		return shell.Value{}, fmt.Errorf("expected a command starting with \"bpfman\", got %v", argTexts(args))
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

// handleHelp renders the overview (no args) or the detail for
// one named builtin or keyword (one arg). The overview composes
// a hand-curated 'Domain commands' section with auto-rendered
// sections derived from builtinRegistry (grouped by Category,
// alphabetised within group) and from keywordRegistry. Adding
// a builtin or keyword updates the help automatically.
func handleHelp(c builtinCtx) (shell.Value, error) {
	args := argTexts(c.Args)
	switch len(args) {
	case 0:
		return shell.Value{}, c.CLI.PrintOut(renderHelpOverview())
	case 1:
		out, ok := renderHelpDetail(args[0])
		if !ok {
			return shell.Value{}, fmt.Errorf("no help for %q (try 'help' for the overview)", args[0])
		}
		return shell.Value{}, c.CLI.PrintOut(out)
	default:
		return shell.Value{}, fmt.Errorf("help takes at most one argument")
	}
}

// renderHelpOverview composes the no-arg help. The 'Domain
// commands' block stays hand-curated because the bpfman
// subcommand grammar is multi-level and does not fit a flat
// registry; everything else is registry-derived.
func renderHelpOverview() string {
	var b strings.Builder
	b.WriteString("Available commands:\n\n")
	b.WriteString(domainCommandsBlock)
	for _, cat := range categoryOrder {
		writeBuiltinCategory(&b, cat)
	}
	writeKeywordSection(&b)
	b.WriteString("'help <name>' shows the long-form help for a builtin or keyword.\n")
	return b.String()
}

// domainCommandsBlock is the hand-curated top section of the
// help overview. Phrased so multi-id arguments use plural
// forms (<ids>) instead of trailing ellipsis to keep the
// column-aligned table readable as a single scan.
const domainCommandsBlock = `Domain commands (require "bpfman" prefix):

  Program management:
    bpfman program list [flags]                     List managed BPF programs
    bpfman program get <id>                         Get program details (assignable)
    bpfman program load file [flags]                Load from a local object file (assignable)
    bpfman program load image [flags]               Load from an OCI image (assignable)
    bpfman program unload <ids>                     Unload programs
    bpfman program delete (<ids> | --all) [-r]      Delete with cascading cleanup
    bpfman show program <id> [view] [-o]            Inspect (views: links, maps, paths)

  Link management:
    bpfman link attach <type> [flags] <id>          Attach a program (assignable)
    bpfman link detach <link-ids>                   Detach links
    bpfman link get <link-id>                       Get link details (assignable)
    bpfman link list [flags]                        List managed links
    bpfman link delete <link-ids> [-r]              Delete with cascading cleanup

  Dispatcher management:
    bpfman dispatcher list [--type <type>]           List dispatchers
    bpfman dispatcher get <type> <nsid> <ifindex>    Get dispatcher details
    bpfman dispatcher delete <type> <nsid> <ifindex> Delete a dispatcher

  Diagnostics:
    bpfman audit [rules]                            Audit coherency (read-only)
    bpfman audit explain [rule]                     Explain a coherency rule

`

// helpUsageWrapWidth is the soft cap on Usage length before we
// stop trying to share a column with the Summary and wrap the
// Summary onto its own indented line. Multi-form Usages like
// 'let X = EXPR  |  let X <- COMMAND  |  let (rc, X) <- COMMAND'
// would otherwise force a column that wastes space on every
// short row in the section.
const helpUsageWrapWidth = 48

// helpRowIndent is the left margin for every help row, applied
// uniformly so the section bodies share a vertical alignment
// regardless of how wide their Usage column ends up being.
const helpRowIndent = "  "

// writeAlignedRows emits Usage / Summary rows with column-
// aligned spacing computed dynamically by text/tabwriter so
// adding a new entry never requires re-tuning a hand-picked
// column width. Rows whose Usage exceeds helpUsageWrapWidth
// wrap the Summary onto an indented continuation line so they
// do not stretch the column for the rest of the section. The
// tabwriter is flushed and re-created around each wrapped row
// so subsequent narrow rows realign to their own widest
// neighbour rather than the wide outlier.
func writeAlignedRows(b *strings.Builder, rows [][2]string) {
	tw := newHelpTabwriter(b)
	for _, row := range rows {
		usage, summary := row[0], row[1]
		if len(usage) > helpUsageWrapWidth {
			_ = tw.Flush()
			fmt.Fprintf(b, "%s%s\n%s    %s\n", helpRowIndent, usage, helpRowIndent, summary)
			tw = newHelpTabwriter(b)
			continue
		}
		fmt.Fprintf(tw, "%s%s\t%s\n", helpRowIndent, usage, summary)
	}
	_ = tw.Flush()
}

// newHelpTabwriter constructs the tabwriter used by every help
// section: zero minwidth (the column floats to the longest
// Usage), two-space padding between Usage and Summary, no
// special flags. Keeping this in one place means the help
// sections render consistently and a future tweak (e.g. a
// minimum gap) lands in one spot.
func newHelpTabwriter(b *strings.Builder) *tabwriter.Writer {
	return tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
}

// writeBuiltinCategory emits one section of the overview. The
// section header comes from categoryLabels; the body lists
// every builtin with that Category, alphabetised by name. A
// category with no entries is skipped silently so an unused
// constant does not produce an empty header.
func writeBuiltinCategory(b *strings.Builder, cat string) {
	var entries []builtin
	for _, bi := range builtinRegistry {
		if bi.Category == cat {
			entries = append(entries, bi)
		}
	}
	if len(entries) == 0 {
		return
	}
	slices.SortFunc(entries, func(a, b builtin) int { return cmp.Compare(a.Name, b.Name) })
	fmt.Fprintf(b, "%s:\n", categoryLabels[cat])
	rows := make([][2]string, 0, len(entries))
	for _, bi := range entries {
		rows = append(rows, [2]string{bi.Usage, bi.Summary})
	}
	writeAlignedRows(b, rows)
	b.WriteString("\n")
}

// writeKeywordSection emits the parser-level keyword section.
// Keywords are listed in alphabetical order; their Usage often
// shows multiple variants separated by '|' so the wrap path
// fires regularly here.
func writeKeywordSection(b *strings.Builder) {
	if len(keywordRegistry) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(keywordRegistry))
	b.WriteString("Keywords:\n")
	rows := make([][2]string, 0, len(names))
	for _, n := range names {
		k := keywordRegistry[n]
		rows = append(rows, [2]string{k.Usage, k.Summary})
	}
	writeAlignedRows(b, rows)
	b.WriteString("\n")
}

// renderHelpDetail looks name up in the builtin registry, then
// the keyword registry, and renders Usage / Summary / Detail.
// Returns ok=false when the name is in neither registry so the
// caller can produce a useful error.
func renderHelpDetail(name string) (string, bool) {
	if bi, ok := builtinRegistry[name]; ok {
		return formatDetail(bi.Name, bi.Usage, bi.Summary, bi.Detail), true
	}
	if kw, ok := keywordRegistry[name]; ok {
		return formatDetail(kw.Name, kw.Usage, kw.Summary, kw.Detail), true
	}
	return "", false
}

// formatDetail renders the detail block for one entry. Empty
// fields drop their lines so a builtin without Detail just
// shows Usage and Summary; a keyword without Summary shows
// only Usage and Detail.
func formatDetail(name, usage, summary, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", name)
	if usage != "" {
		fmt.Fprintf(&b, "  usage: %s\n", usage)
	}
	if summary != "" {
		fmt.Fprintf(&b, "  %s\n", summary)
	}
	if detail != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
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

// handleVersion prints version information.
func handleVersion(c builtinCtx) (shell.Value, error) {
	return shell.Value{}, c.CLI.PrintOut(version.Get().Long())
}
