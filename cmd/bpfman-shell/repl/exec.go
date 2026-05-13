// External-process primitives: spawn a command and capture its
// output, or hand stdio to the parent so interactive programs
// work. Two paths share a single argv-resolution helper that
// flattens shell.Arg values (including file:$var adapters) into
// argv strings.
//
// These are pure mechanism: no knowledge of the builtin registry,
// no knowledge of bpfman. The `exec` builtin and the loop's
// fall-through to external commands both call into them.

package repl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

// ExecFailure is the typed error returned by RunExecStatement when
// an external subprocess at top-level statement position exits
// non-zero. It deliberately is not a *shell.SyntaxError: nothing in
// the source is malformed, the child simply reported a non-zero exit.
// SourceSpan satisfies shell.SpanCarrier so frameAtSpan leaves these
// values untouched; the renderer routes them to a citation shape
// (file:line: argv: exit N) instead of the rust-compiler frame
// reserved for parser/checker diagnostics.
type ExecFailure struct {
	Argv     []string
	ExitCode int
	Span     shell.Span
	Stdout   string
	Stderr   string
}

func (e *ExecFailure) Error() string {
	return fmt.Sprintf("%s: exit status %d", strings.Join(e.Argv, " "), e.ExitCode)
}

// SourceSpan implements shell.SpanCarrier.
func (e *ExecFailure) SourceSpan() shell.Span { return e.Span }

// CommandNotFound is the typed error returned at the subprocess
// fallthrough when the first word resolves to no executable on
// $PATH. Detected before argument resolution so a script that
// names a non-existent command reports the missing-command failure
// first, rather than a downstream argument-flatten error caused by
// the later arguments. SpanCarrier keeps it out of the syntax-error
// frame: the source is well-formed, the name just does not resolve.
type CommandNotFound struct {
	Name string
	Span shell.Span
}

func (e *CommandNotFound) Error() string {
	return fmt.Sprintf("%s: command not found", e.Name)
}

// SourceSpan implements shell.SpanCarrier.
func (e *CommandNotFound) SourceSpan() shell.Span { return e.Span }

// ExecArgError is the typed error returned by ResolveExternalArgs
// when an argument cannot be flattened into argv text -- a
// structured value passed where the spawned process expects a
// scalar, or an unrecognised adapter form. The source construct is
// well-formed; the runtime value just does not compose with what
// the executor needs.
type ExecArgError struct {
	Msg  string
	Span shell.Span
}

func (e *ExecArgError) Error() string { return e.Msg }

// SourceSpan implements shell.SpanCarrier.
func (e *ExecArgError) SourceSpan() shell.Span { return e.Span }

// RuntimeError is the typed error a handler returns when the
// failure is a runtime outcome on a syntactically well-formed
// construct, not a malformed region of source. The in-process
// bpfman dispatcher wraps its returned errors in this type;
// individual shell builtins can opt in when their failure is
// genuinely runtime-outcome rather than usage-error.
type RuntimeError struct {
	Msg  string
	Span shell.Span
}

func (e *RuntimeError) Error() string { return e.Msg }

// SourceSpan implements shell.SpanCarrier.
func (e *RuntimeError) SourceSpan() shell.Span { return e.Span }

// ResolveCommandPath returns nil if name names an executable
// reachable from the current process: an absolute path, a relative
// path containing a slash, or a bare name that exec.LookPath finds
// on $PATH. Otherwise it returns a *CommandNotFound carrying the
// originating Span so the renderer can cite the source line. No
// caching: exec.LookPath rescans $PATH on every call.
func ResolveCommandPath(name string, span shell.Span) error {
	if _, err := exec.LookPath(name); err != nil {
		return &CommandNotFound{Name: name, Span: span}
	}
	return nil
}

// RunExecStatement is the shared exec-as-statement implementation
// used by the `exec` builtin handler and by the loop's fallthrough
// for unknown first words. span identifies the originating statement
// so failures cite the right source location without a
// syntax-error frame.
func RunExecStatement(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg, span shell.Span) (shell.Value, error) {
	argv, exitCode, err := RunExternalInherit(ctx, cli, args)
	if err != nil {
		return shell.Value{}, err
	}
	if exitCode != 0 {
		return shell.Value{}, &ExecFailure{
			Argv:     argv,
			ExitCode: exitCode,
			Span:     span,
		}
	}
	return shell.Value{}, nil
}

// ExecCapture is the result of running an external command
// without any policy applied: argv as constructed, captured stdout
// and stderr, and the actual exit code. Launch failure is reported
// as a Go error from RunExternal and never appears as an
// ExecCapture.
type ExecCapture struct {
	Argv     []string
	Stdout   string
	Stderr   string
	ExitCode int
}

// ResolveExternalArgs walks args, resolving file: adapter values
// to temp files and rejecting structured-value args that cannot
// flatten into argv text. Returned tempFiles are the caller's to
// remove (typically via defer); they outlive the resolve call so
// the spawned process can read them. Shared between RunExternal
// and RunExternalInherit.
func ResolveExternalArgs(args []shell.Arg) (argv []string, tempFiles []string, err error) {
	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		switch aa := a.(type) {
		case shell.AdapterArg:
			if aa.Adapter != "file" {
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, &ExecArgError{
					Msg:  fmt.Sprintf("argument %d: unknown adapter %q", i+1, aa.Adapter),
					Span: aa.Span,
				}
			}
			path, terr := WriteValueToTemp(aa.Value)
			if terr != nil {
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, &ExecArgError{
					Msg:  fmt.Sprintf("argument %d: file adapter: %v", i+1, terr),
					Span: aa.Span,
				}
			}
			tempFiles = append(tempFiles, path)
			resolved[i] = shell.ScalarValueArg{Text: path, Span: aa.Span}
		case shell.StructuredValueArg:
			for _, f := range tempFiles {
				os.Remove(f)
			}
			return nil, nil, &ExecArgError{
				Msg: fmt.Sprintf(
					"argument %d: cannot pass a %s value to an external command; use a scalar field (e.g. $name.field) or the file adapter (file:$name)",
					i+1, aa.Value.Kind()),
				Span: aa.Span,
			}
		default:
			resolved[i] = a
		}
	}
	return ArgTexts(resolved), tempFiles, nil
}

// RunExternal runs an external command and captures its output.
// Inline adapter arguments (e.g. file:$var.path) are resolved to
// temporary files before the command runs and removed
// unconditionally after. Structured-value arguments are rejected
// because they cannot be flattened into argv text. Non-zero exit
// is reported via ExecCapture.ExitCode, not as an error: callers
// decide whether non-zero is fatal.
func RunExternal(ctx context.Context, args []shell.Arg) (ExecCapture, error) {
	if len(args) == 0 {
		return ExecCapture{}, fmt.Errorf("exec requires at least one argument")
	}
	argv, tempFiles, err := ResolveExternalArgs(args)
	if err != nil {
		return ExecCapture{}, err
	}
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	cap := ExecCapture{Argv: argv}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return ExecCapture{}, fmt.Errorf("exec %s: %w", argv[0], err)
		}
		cap.ExitCode = exitErr.ExitCode()
	}
	cap.Stdout = stdout.String()
	cap.Stderr = stderr.String()
	return cap, nil
}

// RunExternalInherit runs an external command with stdio
// connected to the parent: stdin from os.Stdin, stdout/stderr to
// the cli's writers, and (when stdin is a TTY) full foreground
// job control so the child owns the terminal for the duration of
// the call. Interactive programs (vi, less, htop) get a real TTY.
//
// The ctx parameter is intentionally not threaded into
// exec.CommandContext for the spawn: a cancellation of the
// shell's root ctx (a ^C the user intended for the foreground
// program) must not SIGKILL the child via cmd.Cancel. With job
// control in place the child receives SIGINT directly through
// the TTY and handles it itself.
//
// Off-TTY callers (script mode, stdin pipe, CI) skip the
// foreground-group dance via FgJob's disabled zero value;
// behaviour there matches the no-job-control case.
func RunExternalInherit(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg) (argv []string, exitCode int, err error) {
	_ = ctx
	if len(args) == 0 {
		return nil, 0, fmt.Errorf("exec requires at least one argument")
	}
	argv, tempFiles, err := ResolveExternalArgs(args)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	fg := NewFgJob()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = fg.SysProcAttr()
	cmd.Stdin = os.Stdin
	cmd.Stdout = cli.Out
	cmd.Stderr = cli.Err

	if rerr := cmd.Start(); rerr != nil {
		return argv, 0, fmt.Errorf("exec %s: %w", argv[0], rerr)
	}

	// Hand the terminal to the child. SIGTTOU is masked at process
	// startup so this ioctl from a now-background process does not
	// stop us. A failure here means the child runs without owning
	// the foreground group; the user may see ^C affect the shell
	// rather than the child, but the run still completes.
	_ = fg.Grant(cmd.Process.Pid)
	defer func() { _ = fg.Reclaim() }()

	if rerr := cmd.Wait(); rerr != nil {
		var exitErr *exec.ExitError
		if !errors.As(rerr, &exitErr) {
			return argv, 0, fmt.Errorf("exec %s: %w", argv[0], rerr)
		}
		return argv, exitErr.ExitCode(), nil
	}
	return argv, 0, nil
}
