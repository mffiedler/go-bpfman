// exec builtin: spawn an external command. Two execution paths
// live here:
//
//   - runExternal: capture stdout/stderr/exit-code into an
//     execCapture, used on the bind path (`let r <- ls`) and by
//     net's `net exec $pair ...` wrapper.
//   - runExternalInherit: hand stdin/stdout/stderr to the parent
//     and grant the child the controlling TTY, used on the
//     statement-position path (`exec vi foo`) so interactive
//     programs work.
//
// Inline `file:$var` adapter arguments are resolved to temp files
// before either path runs and removed unconditionally after. The
// shared resolveExternalArgs returns paths plus the temp-file list
// so callers can defer the cleanup.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/shell"
)

// ExecFailure is the typed error returned by runExecStatement when
// an external subprocess at top-level statement position exits
// non-zero. It deliberately is not a *shell.SyntaxError: nothing in
// the source is malformed, the child simply reported a non-zero exit.
// SourceSpan satisfies shell.SpanCarrier so frameAtSpan leaves these
// values untouched; the REPL's diagnostic renderer routes them to a
// citation shape (file:line: argv: exit N) instead of the
// rust-compiler frame reserved for parser/checker diagnostics.
//
// Stdout and Stderr exist on the struct so a future tee path can
// surface captured output alongside the citation. The bare-statement
// exec path uses inherited fds (runExternalInherit), so the child
// has already streamed to the user's terminal and both fields are
// empty in that case.
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

// handleExec runs an external command at top-level statement
// position with stdio inherited from the parent: stdin from the
// terminal, stdout/stderr streamed live to the user's writers.
// Interactive programs (vi, less, ssh) get a real TTY; long-
// running programs (make, build) stream progress instead of
// buffering it. Non-zero exit becomes a returned *ExecFailure so
// the chunk is reported as failed; launch failures (command not
// found, permission denied) propagate as plain errors. The bind
// path uses runExternal to capture into a BindResult.
func handleExec(c builtinCtx) (shell.Value, error) {
	return runExecStatement(c.Ctx, c.CLI, c.Args, c.Span)
}

// runExecStatement is the shared exec-as-statement implementation
// used by handleExec and by repl.go's fallthrough for unknown
// first words (where any non-builtin, non-domain command runs as
// an external subprocess at statement position). span identifies
// the originating statement so the failure is cited at the right
// source location without a syntax-error frame.
func runExecStatement(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg, span shell.Span) (shell.Value, error) {
	argv, exitCode, err := runExternalInherit(ctx, cli, args)
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

// execCapture is the result of running an external command
// without any policy applied: argv as constructed, captured
// stdout and stderr, and the actual exit code. Launch failure
// (command not found, permission denied) is reported as a Go
// error from runExternal and never appears as an execCapture.
type execCapture struct {
	Argv     []string
	Stdout   string
	Stderr   string
	ExitCode int
}

// resolveExternalArgs walks args, resolving file: adapter values
// to temp files and rejecting structured-value args that cannot
// flatten into argv text. Returned tempFiles are the caller's to
// remove (typically via defer); they outlive the resolve call so
// the spawned process can read them. Shared between runExternal
// (capture path) and runExternalInherit (top-level pass-through
// path).
func resolveExternalArgs(args []shell.Arg) (argv []string, tempFiles []string, err error) {
	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		switch aa := a.(type) {
		case shell.AdapterArg:
			if aa.Adapter != "file" {
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, fmt.Errorf("unknown adapter %q", aa.Adapter)
			}
			path, terr := writeValueToTemp(aa.Value)
			if terr != nil {
				for _, f := range tempFiles {
					os.Remove(f)
				}
				return nil, nil, fmt.Errorf("adapter file: %w", terr)
			}
			tempFiles = append(tempFiles, path)
			resolved[i] = shell.ScalarValueArg{Text: path, Span: aa.Span}
		case shell.StructuredValueArg:
			for _, f := range tempFiles {
				os.Remove(f)
			}
			return nil, nil, fmt.Errorf(
				"exec: argument %d is a %s value; use a scalar path (e.g. $name.field) or the file adapter (file:$name)",
				i+1, aa.Value.Kind())
		default:
			resolved[i] = a
		}
	}
	return argTexts(resolved), tempFiles, nil
}

// runExternal runs an external command and captures its output.
// Inline adapter arguments (e.g. file:$var.path) are resolved to
// temporary files before the command runs and removed
// unconditionally after. Structured-value arguments are rejected
// because they cannot be flattened into argv text. Non-zero exit
// is reported via execCapture.ExitCode, not as an error: callers
// decide whether non-zero is fatal. Use this on the bind path
// ('let r <- ls') where the script needs the captured output;
// for top-level statement position use runExternalInherit so
// TTY-needing programs (vi, less, htop) work.
func runExternal(ctx context.Context, args []shell.Arg) (execCapture, error) {
	if len(args) == 0 {
		return execCapture{}, fmt.Errorf("exec requires at least one argument")
	}
	argv, tempFiles, err := resolveExternalArgs(args)
	if err != nil {
		return execCapture{}, err
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

	cap := execCapture{Argv: argv}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return execCapture{}, fmt.Errorf("exec %s: %w", argv[0], err)
		}
		cap.ExitCode = exitErr.ExitCode()
	}
	cap.Stdout = stdout.String()
	cap.Stderr = stderr.String()
	return cap, nil
}

// runExternalInherit runs an external command with stdio
// connected to the parent: stdin from os.Stdin, stdout/stderr to
// the cli's writers, and (when stdin is a TTY) full foreground
// job control so the child owns the terminal for the duration of
// the call. Interactive programs (vi, less, htop, ssh, top) get
// a real TTY, their output streams to the user as it happens,
// and ^C reaches only the child, not the shell. When the child
// exits the shell reclaims the terminal's foreground group and
// the prompt resumes.
//
// The ctx parameter is intentionally not threaded into
// exec.CommandContext for the spawn: a cancellation of the
// shell's root ctx (a ^C the user intended for the foreground
// program, not for the shell) must not SIGKILL the child via
// cmd.Cancel. With job control in place the child receives
// SIGINT directly through the TTY and handles it itself; the
// shell does not even see the signal while the child holds the
// foreground group. ctx stays on the signature so callers do not
// need to know which exec path they are taking.
//
// Off-TTY callers (script mode, stdin pipe, CI) skip the
// foreground-group dance via fgJob's disabled zero value;
// behaviour there matches the no-job-control case.
func runExternalInherit(ctx context.Context, cli *bpfmancli.CLI, args []shell.Arg) (argv []string, exitCode int, err error) {
	_ = ctx
	if len(args) == 0 {
		return nil, 0, fmt.Errorf("exec requires at least one argument")
	}
	argv, tempFiles, err := resolveExternalArgs(args)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for _, f := range tempFiles {
			os.Remove(f)
		}
	}()

	fg := newFgJob()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = fg.SysProcAttr()
	cmd.Stdin = os.Stdin
	cmd.Stdout = cli.Out
	cmd.Stderr = cli.Err

	if rerr := cmd.Start(); rerr != nil {
		return argv, 0, fmt.Errorf("exec %s: %w", argv[0], rerr)
	}

	// Hand the terminal to the child. SIGTTOU is masked at
	// process startup (see init in jobctl_signal.go) so this
	// ioctl from a now-background process does not stop us. A
	// failure here means the child runs without owning the
	// foreground group; the user may see ^C affect the shell
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
