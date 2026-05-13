// REPL loop, dispatcher, source-file evaluator, and the Env-
// callback factories that bridge the shell evaluator to the
// chunk runtime. Pure mechanism: the embedding binary plugs in
// its handlers via RegisterBuiltin and its bpfman bridge via
// Config.Fallback / Config.BindFallback.

package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
)

// sourcingKey marks contexts inside a sourced file so Source can
// refuse nested invocations.
type sourcingKey struct{}

// SourceHooks bundles the per-chunk Env callbacks Source installs
// for each statement it evaluates from a sourced file. Source
// callers pass the same triple the outer Loop wires through
// Config.Fallback / BindFallback / MakeAssertStmt; the framework
// has no use for the hooks outside the source path, so they are
// not stored on the context.
type SourceHooks struct {
	Fallback       FallbackFunc
	BindFallback   BindFallbackFunc
	MakeAssertStmt MakeAssertStmtFunc
}

// Config bundles the call-site options Run needs. The embedding
// binary fills it from its Kong-parsed CLI struct (or equivalent)
// and passes it to Run; the repl package owns everything past
// that seam.
type Config struct {
	// CLI is the bpfmancli handle used for writers, manager
	// construction, and logger access.
	CLI *bpfmancli.CLI

	// Mgr is the constructed bpfman manager. Loop uses it for
	// the source builtin and assertion verbs; nil is fine if
	// the script only runs builtins that do not touch the
	// manager.
	Mgr *manager.Manager

	// LineReader is the input source the loop reads from.
	LineReader LineReader

	// Session is the shell session the loop evaluates against.
	Session *shell.Session

	// File is the diagnostic name for the input ("script.bpfman",
	// "<stdin>", ""). Loop uses it for source-location prefixes
	// and for the source builtin's containing-script context.
	File string

	// Interactive is the authoritative mode flag. Script mode
	// wraps the whole chunk loop in one defer scope; interactive
	// mode opens a fresh defer scope per chunk and a silent
	// job-leak handler instead of strict.
	Interactive bool

	// NoCheck disables the static pre-flight pass for script
	// mode. Used by tests that exercise runtime behaviour on
	// inputs the static checker would otherwise reject.
	NoCheck bool

	// Fallback is consulted when no registered builtin matches
	// the first token of a dispatched command. Embedders use
	// this to wire in a domain-command bridge (the bpfman
	// dispatcher). Return handled == false to let the loop
	// fall through to external-command execution.
	Fallback FallbackFunc

	// BindFallback is the equivalent of Fallback for the
	// `<- name args` bind path. Embedders use it to special-
	// case wait/net-exec (where the bind's Rc must reflect the
	// captured inner outcome) and to dispatch the bpfman
	// bridge. Return handled == false to let the loop fall
	// through to external-command execution.
	BindFallback BindFallbackFunc

	// MakeAssertStmt builds the assert-statement evaluator the
	// shell.Env wires into Env.ExecAssertStmt. The embedding
	// binary owns the actual verb dispatch (nil, not-empty, ok,
	// matches, ...) so this is a callback rather than a builtin.
	// nil disables the assert verb at evaluation time.
	MakeAssertStmt MakeAssertStmtFunc

	// PromptPrimary is the primary interactive prompt. Empty
	// falls back to "> "; the embedding binary supplies the
	// product-specific string (e.g. "bpfman> ").
	PromptPrimary string

	// PromptContinue is the continuation prompt shown while a
	// block stays open across newlines. Empty falls back to "... ".
	PromptContinue string
}

// FallbackFunc dispatches unhandled commands (statement
// position). Returning handled == false means "fall through to
// external command".
type FallbackFunc func(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, args []shell.Arg, loc SourceLoc, span shell.Span) (handled bool, val shell.Value, err error)

// BindFallbackFunc dispatches unhandled commands on the right
// of `<-`. Returning handled == false means "fall through to
// external command".
type BindFallbackFunc func(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, args []shell.Arg, loc SourceLoc, span shell.Span) (handled bool, br shell.BindResult, err error)

// MakeAssertStmtFunc builds the Env.ExecAssertStmt callback for
// one chunk. Returning nil disables assert evaluation in that
// chunk (assert statements then return a "no evaluator" error
// at parse-evaluate time).
type MakeAssertStmtFunc func(cli *bpfmancli.CLI, session *shell.Session, loc SourceLoc) func(*shell.AssertStmt, *shell.Env) error

// Run drives the chunk loop end-to-end and returns the session-
// aggregated outcome: ErrSilent for script-error / require-fail
// paths the caller has already cited, a wrapped error for
// assertion / defer / job-leak counters, or nil on clean exit.
//
// A single manager is held open for the session lifetime so
// repeated store open/close cost is paid once. The session
// counters drive the post-loop summary; assertion failures
// surface a non-zero exit even when individual chunks ran
// without aborting.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Session == nil {
		cfg.Session = shell.NewSession()
	}

	loopErr := Loop(ctx, cfg)

	if errors.Is(loopErr, ErrRequireFailed) || errors.Is(loopErr, ErrScriptError) {
		return ErrSilent
	}
	if loopErr != nil {
		return loopErr
	}

	if n := cfg.Session.AssertFailures(); n > 0 {
		_ = cfg.CLI.PrintErrf("%d assertion(s) failed\n", n)
		return fmt.Errorf("%d assertion(s) failed", n)
	}

	if n := cfg.Session.DeferFailures(); n > 0 {
		_ = cfg.CLI.PrintErrf("%d defer(s) failed\n", n)
		return fmt.Errorf("%d defer(s) failed", n)
	}

	if n := cfg.Session.JobLeaks(); n > 0 {
		_ = cfg.CLI.PrintErrf("%d job(s) leaked\n", n)
		return fmt.Errorf("%d job(s) leaked", n)
	}

	return nil
}

// Loop reads from cfg.LineReader and dispatches input until EOF
// or interrupt. Two modes with deliberately different policies:
//
// In script mode (cfg.Interactive == false) the chunk loop runs
// inside one outer WithJobScope and one outer WithDeferScope.
// `defer cleanup` fires at script end and the script-wide leak
// walk reports any unmanaged job as `[job] FAIL ...`, kills it,
// and pushes the exit code non-zero.
//
// In interactive mode (cfg.Interactive == true) the loop opens
// one outer WithJobScope around the whole readline session but a
// fresh WithDeferScope per chunk. Defers fire at end of prompt;
// jobs that no chunk waited or killed are cleaned up silently at
// session end (Ctrl+D) by a silent leak handler.
func Loop(ctx context.Context, cfg Config) error {
	ctx = EnsureInteractiveBaseDir(ctx)
	if cfg.Interactive {
		return interactiveLoop(ctx, cfg)
	}
	return scriptLoop(ctx, cfg)
}

// scriptLoop drives chunk-by-chunk evaluation in script mode but
// wraps the entire chunk loop in a single WithJobScope outside a
// single WithDeferScope. Each balanced statement is parsed and
// evaluated as its own program; defers registered along the way
// fire when the script-wide defer scope unwinds.
//
// SIGINT and SIGTERM cancel the script-wide context, matching
// the way a bash script aborts on ^C.
func scriptLoop(ctx context.Context, cfg Config) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cli := cfg.CLI
	lr := cfg.LineReader
	session := cfg.Session
	file := cfg.File

	src, slurpErr := SlurpReader(lr)
	if slurpErr != nil {
		_ = cli.PrintErrf("%s: %v\n", file, slurpErr)
		return ErrScriptError
	}
	if !cfg.NoCheck {
		if hadIssues := PreflightCheck(cli, file, src); hadIssues {
			return ErrScriptError
		}
	}
	lr = NewScannerReader(strings.NewReader(src), nil)

	env := &shell.Env{
		Session: session,
		PrintResult: func(v shell.Value) error {
			return WriteValue(cli, v)
		},
		RenderDeferFailure: func(stmtLoc shell.Pos, args []shell.Arg, rc shell.Envelope) {
			RenderEnvelopeFailure(cli, "defer", SourceLoc{File: file}, stmtLoc, args, rc)
		},
		HandleJobLeak: StrictJobLeakHandler(cli, session),
	}
	return shell.WithJobScope(env, func() error {
		return shell.WithDeferScope(env, func() error {
			var lineNo int
			var buf strings.Builder
			var startLine int
			var cs ContState
			for {
				input, err := lr.Readline()
				if err != nil {
					if err == io.EOF || err == ErrInterrupt {
						if buf.Len() > 0 {
							loc := SourceLoc{File: file, Line: startLine}
							_ = cli.PrintErrf("%serror: unterminated block at end of input\n", loc)
							return ErrScriptError
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
				cs.Advance(input)

				if cs.Open() {
					continue
				}

				accumulated := buf.String()
				buf.Reset()
				cs = ContState{}

				loc := SourceLoc{File: file, Line: startLine}
				env.ExecCommand = makeExecCommand(ctx, cli, cfg.Mgr, session, env, loc, cfg.Fallback)
				env.ExecBind = makeExecBind(ctx, cli, cfg.Mgr, session, env, loc, cfg.BindFallback)
				if cfg.MakeAssertStmt != nil {
					env.ExecAssertStmt = cfg.MakeAssertStmt(cli, session, loc)
				}
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
// by loc.Line so frames cite the absolute file line. When
// frameSrc is empty (interactive mode without a slurped buffer)
// the chunk input itself is used as the frame source.
func evalChunkInScope(cli *bpfmancli.CLI, env *shell.Env, input, frameSrc string, loc SourceLoc) error {
	emitFrame := func(span shell.Span, msg string) {
		src := frameSrc
		shift := loc.Line - 1
		if src == "" {
			src = input
			shift = 0
		}
		shifted := span
		if shift > 0 {
			shifted.Pos.Line += shift
			shifted.End.Line += shift
		}
		_ = cli.PrintErr(shell.RenderDiagnostic(src, loc.File, shell.Diagnostic{
			Span: shifted,
			Msg:  msg,
		}))
	}
	report := func(err error) error {
		var re *RuntimeError
		if errors.As(err, &re) {
			if loc.File != "" {
				shift := loc.Line - 1
				line := re.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s\n", loc.File, line, re.Msg)
			} else {
				_ = cli.PrintErrf("%s\n", re.Msg)
			}
			return ErrScriptError
		}
		var ae *ExecArgError
		if errors.As(err, &ae) {
			if loc.File != "" {
				shift := loc.Line - 1
				line := ae.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s\n", loc.File, line, ae.Msg)
			} else {
				_ = cli.PrintErrf("%s\n", ae.Msg)
			}
			return ErrScriptError
		}
		var cnf *CommandNotFound
		if errors.As(err, &cnf) {
			if loc.File != "" {
				shift := loc.Line - 1
				line := cnf.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s: command not found\n", loc.File, line, cnf.Name)
			} else {
				_ = cli.PrintErrf("%s: command not found\n", cnf.Name)
			}
			return ErrScriptError
		}
		var ef *ExecFailure
		if errors.As(err, &ef) {
			if loc.File != "" {
				shift := loc.Line - 1
				line := ef.Span.Pos.Line + shift
				_ = cli.PrintErrf("%s:%d: %s: exit %d\n", loc.File, line, strings.Join(ef.Argv, " "), ef.ExitCode)
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
			return ErrScriptError
		}
		var se *shell.SyntaxError
		if errors.As(err, &se) && se.Span.Pos.Line > 0 {
			emitFrame(se.Span, se.Msg)
			return ErrScriptError
		}
		_ = cli.PrintErrf("%serror: %v\n", loc, err)
		return ErrScriptError
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
		if errors.Is(err, ErrRequireFailed) {
			return err
		}
		if errors.Is(err, ErrScriptError) {
			return err
		}
		var gf *shell.GuardFailure
		if errors.As(err, &gf) {
			RenderEnvelopeFailure(cli, "guard", loc, gf.Pos, gf.Args, gf.Envelope)
			return ErrScriptError
		}
		return report(err)
	}
	return nil
}

// interactiveLoop runs the chunk-by-chunk loop suited to a
// readline prompt. The whole session runs inside one outer
// WithJobScope; each chunk opens its own WithDeferScope. The
// job-leak handler is silent: jobs the user never waited on are
// SIGKILLed at Ctrl+D with no diagnostic and no exit-code effect.
func interactiveLoop(ctx context.Context, cfg Config) error {
	cli := cfg.CLI
	lr := cfg.LineReader
	session := cfg.Session

	env := &shell.Env{
		Session: session,
		PrintResult: func(v shell.Value) error {
			return WriteValue(cli, v)
		},
		RenderDeferFailure: func(stmtLoc shell.Pos, args []shell.Arg, rc shell.Envelope) {
			RenderEnvelopeFailure(cli, "defer", SourceLoc{}, stmtLoc, args, rc)
		},
		HandleJobLeak: SilentJobLeakHandler(),
	}

	promptPrimary := cfg.PromptPrimary
	if promptPrimary == "" {
		promptPrimary = "> "
	}
	promptContinue := cfg.PromptContinue
	if promptContinue == "" {
		promptContinue = "... "
	}
	setPrompt := func(p string) {
		if ps, ok := lr.(PromptSetter); ok {
			ps.SetPrompt(p)
		}
	}

	return shell.WithJobScope(env, func() error {
		var lineNo int
		var buf strings.Builder
		var startLine int
		var cs ContState
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
			cs.Advance(input)

			if cs.Open() {
				setPrompt(promptContinue)
				continue
			}

			accumulated := buf.String()
			buf.Reset()
			cs = ContState{}
			setPrompt(promptPrimary)

			if hw, ok := lr.(HistoryWriter); ok {
				if entry := CanonicaliseHistory(accumulated); entry != "" {
					_ = hw.SaveHistory(entry)
				}
			}

			loc := SourceLoc{Line: startLine}

			chunkCtx, cancelChunk := signal.NotifyContext(ctx, syscall.SIGINT)

			env.ExecCommand = makeExecCommand(chunkCtx, cli, cfg.Mgr, session, env, loc, cfg.Fallback)
			env.ExecBind = makeExecBind(chunkCtx, cli, cfg.Mgr, session, env, loc, cfg.BindFallback)
			if cfg.MakeAssertStmt != nil {
				env.ExecAssertStmt = cfg.MakeAssertStmt(cli, session, loc)
			}
			env.Trace = makeTraceHook(cli, session, loc)

			chunkErr := shell.WithDeferScope(env, func() error {
				return evalChunkInScope(cli, env, accumulated, "", loc)
			})
			cancelChunk()

			if chunkErr == nil {
				continue
			}
			if errors.Is(chunkErr, ErrRequireFailed) {
				return chunkErr
			}
		}
	})
}

// Dispatch looks the first token of args up in the builtin
// registry and invokes its handler. Returns
// (true, value, err) when the registry has an entry; the value
// is the assignable primary for builtins that produce one,
// shell.Value{} for builtins that bind nothing. Returns
// (false, shell.Value{}, nil) when no builtin matches.
func Dispatch(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, args []shell.Arg, loc SourceLoc, span shell.Span) (bool, shell.Value, error) {
	if len(args) == 0 {
		return false, shell.Value{}, nil
	}
	cmd := ArgText(args[0])
	b, ok := LookupBuiltin(cmd)
	if !ok {
		return false, shell.Value{}, nil
	}
	c := Ctx{
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
	return true, val, shell.FrameAt(span, err)
}

// makeExecCommand bridges the evaluator's top-level CommandStmt
// dispatch into the loop pipeline. Output is visible on the CLI.
// Dispatch order: aliases expand first; registered builtins
// handle their own names; the embedder's Fallback handles
// domain commands; an unrecognised first word runs as an
// external subprocess.
func makeExecCommand(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, loc SourceLoc, fallback FallbackFunc) func([]shell.Arg, shell.Span) (shell.Value, error) {
	return func(args []shell.Arg, span shell.Span) (shell.Value, error) {
		if len(args) == 0 {
			return shell.Value{}, nil
		}
		args = ApplyAlias(session, args)
		handled, val, err := Dispatch(ctx, cli, mgr, session, env, args, loc, span)
		if err != nil {
			return shell.Value{}, err
		}
		if handled {
			return val, nil
		}
		if fallback != nil {
			handled, val, err = fallback(ctx, cli, mgr, args, loc, span)
			if handled {
				return val, err
			}
		}
		first := ArgText(args[0])
		if err := ResolveCommandPath(first, span); err != nil {
			return shell.Value{}, err
		}
		val, err = RunExecStatement(ctx, cli, args, span)
		return val, shell.FrameAt(span, err)
	}
}

// makeExecBind bridges the evaluator's BindStmt dispatch into the
// loop pipeline. Output is suppressed. Dispatch order:
// `exec NAME` always runs as a subprocess; the embedder's
// BindFallback handles special-case bind paths (wait, net exec,
// domain dispatch); registered builtins handle their own names;
// an unrecognised first word runs as an external subprocess.
func makeExecBind(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, env *shell.Env, loc SourceLoc, fallback BindFallbackFunc) func([]shell.Arg, shell.Span) (shell.BindResult, error) {
	return func(args []shell.Arg, span shell.Span) (shell.BindResult, error) {
		args = ApplyAlias(session, args)
		if len(args) == 0 {
			return shell.BindResult{}, shell.SpanErrorf(span, "empty command form on '<-' RHS")
		}

		if ArgText(args[0]) == "exec" {
			return runExternalAsBind(ctx, args[1:])
		}

		if fallback != nil {
			handled, br, err := fallback(ctx, cli, mgr, session, env, args, loc, span)
			if handled {
				return br, err
			}
		}

		quiet := cli.WithDiscardOutput()
		handled, val, err := Dispatch(ctx, quiet, mgr, session, env, args, loc, span)
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
		return runExternalAsBind(ctx, args)
	}
}

// runExternalAsBind runs args as a subprocess and packages the
// outcome as a BindResult. A launch failure returns a Go error;
// a non-zero exit is captured into the rc envelope so the bind
// caller can inspect it.
func runExternalAsBind(ctx context.Context, args []shell.Arg) (shell.BindResult, error) {
	cap, err := RunExternal(ctx, args)
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
// closure consults session.TraceEnabled() on every invocation
// so `trace on` / `trace off` can toggle tracing mid-script
// without rebuilding the Env.
func makeTraceHook(cli *bpfmancli.CLI, session *shell.Session, loc SourceLoc) func(int, string) {
	return func(line int, rendered string) {
		if !session.TraceEnabled() {
			return
		}
		shift := loc.Line - 1
		abs := line + shift
		file := loc.File
		if file == "" {
			file = "<repl>"
		}
		_ = cli.PrintErrf("+ %s:%d: %s\n", file, abs, rendered)
	}
}

// Source reads commands from a file and executes each line in
// the caller's session. Inherits the caller's session (vars,
// defs, aliases, jobs) and opens its own defer scope so
// `defer cleanup` near the top of a sourced file fires when
// source returns; does not open a new job scope so jobs started
// in the sourced file live in the caller's job scope. Nested
// source commands are rejected to prevent unbounded recursion.
//
// The hooks parameter carries the same Fallback / BindFallback /
// MakeAssertStmt triple the outer Loop received via Config; the
// embedding binary's `source` builtin handler passes them in so
// the sourced chunks dispatch identically to top-level chunks.
// A zero-valued SourceHooks is fine: non-builtin commands fall
// through to external execution.
func Source(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, env *shell.Env, hooks SourceHooks, args []string) error {
	if ctx.Value(sourcingKey{}) != nil {
		return fmt.Errorf("source cannot be used inside a sourced file")
	}
	if len(args) != 1 {
		return fmt.Errorf("source requires exactly one file argument")
	}

	lr, err := OpenScriptReader(args[0])
	if err != nil {
		return err
	}
	defer lr.Close()

	ctx = context.WithValue(ctx, sourcingKey{}, true)
	file := args[0]
	session := env.Session

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
		RenderEnvelopeFailure(cli, "defer", SourceLoc{File: file}, stmtLoc, args, rc)
	}

	return shell.WithDeferScope(env, func() error {
		var lineNo int
		var buf strings.Builder
		var startLine int
		var cs ContState
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
			cs.Advance(input)

			if cs.Open() {
				continue
			}

			accumulated := buf.String()
			buf.Reset()
			cs = ContState{}

			loc := SourceLoc{File: file, Line: startLine}
			env.ExecCommand = makeExecCommand(ctx, cli, mgr, session, env, loc, hooks.Fallback)
			env.ExecBind = makeExecBind(ctx, cli, mgr, session, env, loc, hooks.BindFallback)
			if hooks.MakeAssertStmt != nil {
				env.ExecAssertStmt = hooks.MakeAssertStmt(cli, session, loc)
			}
			env.Trace = makeTraceHook(cli, session, loc)
			if err := evalChunkInScope(cli, env, accumulated, "", loc); err != nil {
				return err
			}
		}
	})
}
