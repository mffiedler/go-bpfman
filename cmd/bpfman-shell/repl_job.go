// Async job control: start / wait / kill builtins backing the
// 'let job <- start COMMAND ARGS' lifecycle described in
// REPL-REDESIGN section 8.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/frobware/go-bpfman/shell"
)

// replStart spawns a background subprocess and returns a Value
// wrapping a *shell.Job. The command runs as a process group
// leader so 'kill' can later signal the whole group, including
// any descendants the child fork-execs. stdout and stderr are
// captured into in-memory buffers; 'wait' reads them after the
// process exits.
//
// Adapter arguments (file:$var.path) are resolved to temp files
// before the spawn, the same way runExternal handles them, but
// the temp files outlive the start call: a wait or kill
// goroutine cleans them up when the job exits, so the script
// can use the captured paths until the job is reaped.
//
// Launch failure (command not found, permission denied) is
// reported as a Go error and produces no Job; this is
// 'structural failure' in the bind-result sense and propagates
// through ExecBind to halt the script.
func replStart(ctx context.Context, args []shell.Arg) (shell.Value, error) {
	if len(args) == 0 {
		return shell.Value{}, fmt.Errorf("start requires at least one argument")
	}

	tempFiles, resolved, err := resolveAdapterArgs("start", args)
	if err != nil {
		return shell.Value{}, err
	}

	argv := argTexts(resolved)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	// Process-group leader: 'kill -<pgid>' reaches every
	// descendant the child fork-execs (an 'ip netns exec ...
	// ping' wrapper, a shell-c spawned worker, etc.).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		removeTempFiles(tempFiles)
		return shell.Value{}, fmt.Errorf("start %s: %w", argv[0], err)
	}

	job := &shell.Job{
		PID:  cmd.Process.Pid,
		Done: make(chan struct{}),
		Args: argv,
	}

	// Reap the process in a goroutine. The goroutine is the sole
	// writer of Stdout/Stderr/ExitCode; close(Done) is the
	// happens-before barrier for any reader (typically wait).
	go func() {
		defer close(job.Done)
		defer removeTempFiles(tempFiles)
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				// Launch failure was caught at Start();
				// anything else here is unexpected. Use -1
				// as the conventional "abnormal" sentinel.
				exitCode = -1
			}
		}
		job.Mu.Lock()
		job.Stdout = stdout.String()
		job.Stderr = stderr.String()
		job.ExitCode = exitCode
		job.Mu.Unlock()
	}()

	return shell.ValueFromJob(job), nil
}

// resolveAdapterArgs walks args, resolving file: adapter values
// to temp files and rejecting structured-value args that cannot
// flatten into argv text. The temp files are returned to the
// caller so it can choose when to remove them; runExternal
// removes immediately after the command exits, replStart hands
// the cleanup to the wait goroutine.
func resolveAdapterArgs(name string, args []shell.Arg) ([]string, []shell.Arg, error) {
	var tempFiles []string
	resolved := make([]shell.Arg, len(args))
	for i, a := range args {
		switch aa := a.(type) {
		case shell.AdapterArg:
			if aa.Adapter != "file" {
				removeTempFiles(tempFiles)
				return nil, nil, fmt.Errorf("unknown adapter %q", aa.Adapter)
			}
			path, err := writeValueToTemp(aa.Value)
			if err != nil {
				removeTempFiles(tempFiles)
				return nil, nil, fmt.Errorf("adapter file: %w", err)
			}
			tempFiles = append(tempFiles, path)
			resolved[i] = shell.ScalarValueArg{Text: path}
		case shell.StructuredValueArg:
			removeTempFiles(tempFiles)
			return nil, nil, fmt.Errorf(
				"%s: argument %d is a %s value; use a scalar path (e.g. $name.field) or the file adapter (file:$name)",
				name, i+1, aa.Value.Kind())
		default:
			resolved[i] = a
		}
	}
	return tempFiles, resolved, nil
}

// removeTempFiles removes any temp files written by adapter
// resolution. Errors are ignored: removal failures are
// non-fatal and the OS reaps stale temp files on its own
// schedule anyway.
func removeTempFiles(paths []string) {
	for _, p := range paths {
		os.Remove(p)
	}
}

// replWait blocks until the given job's reaper goroutine has
// settled the captured streams and exit code, then builds the
// captured-result Envelope from those fields. If the job had
// already completed before wait was called the select returns
// immediately with the cached values; this is the
// future-shaped semantics the design calls out, so a job that
// exited between 'start' and 'wait' does not lose its result.
//
// The job is marked Managed regardless of outcome: the script
// has acknowledged the lifecycle, even if the result is a
// non-ok envelope. Scope-exit (commit 5) will use Managed to
// distinguish observed jobs from leaked ones.
//
// Killed jobs (commit 4) report ok: true in the envelope: a
// script that explicitly kills its own background work is
// performing a clean cleanup, not signalling failure. A
// non-zero exit on a job the script did not kill is a failure
// the consumer can act on through guard or by inspecting
// $rc.code.
func replWait(ctx context.Context, args []shell.Arg) (shell.Envelope, error) {
	if len(args) != 1 {
		return shell.Envelope{}, fmt.Errorf("wait requires exactly one argument: a $job")
	}
	job, err := jobFromArg(args[0])
	if err != nil {
		return shell.Envelope{}, err
	}
	select {
	case <-job.Done:
	case <-ctx.Done():
		return shell.Envelope{
			OK:     false,
			Code:   -1,
			Stderr: ctx.Err().Error(),
		}, nil
	}
	job.MarkManaged()

	job.Mu.Lock()
	stdout := job.Stdout
	stderr := job.Stderr
	exitCode := job.ExitCode
	killed := job.Killed
	job.Mu.Unlock()

	return shell.Envelope{
		OK:     killed || exitCode == 0,
		Code:   exitCode,
		Stdout: stdout,
		Stderr: stderr,
	}, nil
}

// jobFromArg unwraps the StructuredValueArg representing a
// $job reference and returns the underlying *shell.Job. Any
// other Arg shape, or a structured value whose origin is not a
// Job, fails with a message that names the offending kind so
// the user can correct the call site.
func jobFromArg(a shell.Arg) (*shell.Job, error) {
	sva, ok := a.(shell.StructuredValueArg)
	if !ok {
		return nil, fmt.Errorf("expected a $job argument, got %T", a)
	}
	if sva.Value.Kind() != shell.OriginJob {
		return nil, fmt.Errorf("expected a $job argument, got a %s value", sva.Value.Kind())
	}
	job, ok := sva.Value.Origin().(*shell.Job)
	if !ok {
		return nil, fmt.Errorf("$job has no underlying job handle (got %T)", sva.Value.Origin())
	}
	return job, nil
}
