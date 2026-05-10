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
	"strings"
	"syscall"
	"time"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
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
func replStart(ctx context.Context, env *shell.Env, origin string, args []shell.Arg) (shell.Value, error) {
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
		PID:     cmd.Process.Pid,
		Done:    make(chan struct{}),
		Args:    argv,
		Origin:  origin,
		Started: time.Now(),
	}
	if env != nil {
		env.RegisterJob(job)
	}

	// Reap the process in a goroutine. The goroutine is the sole
	// writer of Stdout/Stderr/ExitCode/Signal; close(Done) is
	// the happens-before barrier for any reader (typically
	// wait). Signal is set when the process ended via a signal
	// (whether from our kill builtin, an external sender, or
	// a parent shutdown); the kill builtin also records its
	// requested signal up-front, but the reaper's value is
	// what actually ended the process.
	go func() {
		defer close(job.Done)
		defer removeTempFiles(tempFiles)
		err := cmd.Wait()
		exitCode := 0
		var sigName string
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					sig := ws.Signal()
					sigName = signalShortName(sig)
					// Shell convention: signal-killed
					// processes report code 128+signum.
					exitCode = 128 + int(sig)
				} else {
					exitCode = exitErr.ExitCode()
				}
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
		if sigName != "" && job.Signal == "" {
			// Don't overwrite the signal recorded by the
			// kill builtin -- it is more authoritative
			// (kill's intent vs. whatever signal the
			// kernel ultimately delivered).
			job.Signal = sigName
		}
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
			resolved[i] = shell.ScalarValueArg{Text: path, Span: aa.Span}
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
	signal := job.Signal
	job.Mu.Unlock()

	// ok stays tied to "exit code 0" so the field reads
	// consistently across synchronous and asynchronous
	// commands. A killed job is typically !ok with code=143
	// (SIGTERM convention) plus killed=true and signal="TERM";
	// the script distinguishes "expected termination" from
	// "real failure" via $r.killed, not by overloading $r.ok.
	return shell.Envelope{
		OK:     exitCode == 0,
		Code:   exitCode,
		Stdout: stdout,
		Stderr: stderr,
		Killed: killed,
		Signal: signal,
	}, nil
}

// defaultKillGrace is the window kill waits between SIGTERM
// and SIGKILL on the default-termination path. Long enough for
// a cooperative SIGTERM handler to flush state; short enough
// that a hung process does not stall a script's teardown for
// an irritating amount of time. systemd defaults to 90s,
// container runtimes to 10s, but in this shell most jobs are
// ephemeral test fixtures and 2s is the right ergonomic
// default. Adjust per call with --grace=DURATION.
const defaultKillGrace = 2 * time.Second

// replKill signals the process group of the given job and, on
// the default-termination path, escalates to SIGKILL if the
// process does not exit within the grace period. Three
// behaviours, chosen by flags:
//
//	kill $job                  default termination: SIGTERM,
//	                           wait up to --grace (default 2s),
//	                           SIGKILL if still alive, block
//	                           until reaped. Returns only when
//	                           the job is no longer alive.
//	kill --grace=DUR $job      same, with a custom grace
//	                           window. --grace=0 sends SIGTERM
//	                           immediately followed by SIGKILL
//	                           with no wait.
//	kill --signal=NAME $job    custom signal: deliver and return.
//	                           No escalation. NAME accepts both
//	                           the 'SIGTERM' and 'TERM'
//	                           spellings. Used for control-flow
//	                           signals (USR1, HUP) where the
//	                           script wants to nudge a daemon,
//	                           not terminate it.
//
// The kill targets the process group (-pgid) so descendants
// the child fork-execs (an 'ip netns exec ...' wrapper, a
// sh -c spawn) receive the signal too. ESRCH (the process
// already exited) is treated as success because the desired
// state (job not running) is true.
//
// Concurrency: Killed and Signal are written before the
// initial signal goes out, so a concurrent 'wait' that races
// the kill sees the requested termination. On escalation,
// Signal is rewritten to "KILL" before SIGKILL is sent so the
// final wait envelope reflects what actually ended the
// process; the reaper's "don't overwrite a non-empty Signal"
// rule preserves whichever value the kill builtin set last.
func replKill(ctx context.Context, args []shell.Arg) (shell.Envelope, error) {
	sig := syscall.SIGTERM
	sigName := "TERM"
	grace := defaultKillGrace
	explicitSignal := false
	var jobArg shell.Arg
	for _, a := range args {
		text := argText(a)
		switch {
		case strings.HasPrefix(text, "--signal="):
			name := strings.TrimPrefix(text, "--signal=")
			s, err := signalFromName(name)
			if err != nil {
				return shell.Envelope{}, err
			}
			sig = s
			sigName = signalShortName(s)
			explicitSignal = true
		case strings.HasPrefix(text, "--grace="):
			d, err := time.ParseDuration(strings.TrimPrefix(text, "--grace="))
			if err != nil {
				return shell.Envelope{}, fmt.Errorf("--grace: %w", err)
			}
			if d < 0 {
				return shell.Envelope{}, fmt.Errorf("--grace must not be negative (got %s)", d)
			}
			grace = d
		default:
			if jobArg != nil {
				return shell.Envelope{}, fmt.Errorf("kill takes one $job argument; got more than one")
			}
			jobArg = a
		}
	}
	if jobArg == nil {
		return shell.Envelope{}, fmt.Errorf("kill requires a $job argument")
	}
	job, err := jobFromArg(jobArg)
	if err != nil {
		return shell.Envelope{}, err
	}

	// Mark up-front so a concurrent wait reads "killed"
	// regardless of whether the signal has been delivered yet.
	job.Mu.Lock()
	job.Killed = true
	job.Signal = sigName
	job.Mu.Unlock()

	if err := syscall.Kill(-job.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return shell.Envelope{
			OK:     false,
			Code:   1,
			Stderr: fmt.Sprintf("kill -%d -%d: %v", int(sig), job.PID, err),
		}, nil
	}
	job.MarkManaged()

	// Custom signals are not termination paths: deliver and
	// return. Escalation applies only when the user accepted
	// the default (no --signal flag).
	if explicitSignal {
		return shell.Envelope{OK: true, Code: 0}, nil
	}

	// Default path: wait up to grace for the process to exit,
	// escalate to SIGKILL if needed, block until the reaper
	// closes Done. 'kill' returns only after the job is
	// genuinely gone, so 'defer kill $p' is a real cleanup
	// primitive rather than a hopeful suggestion.
	if waitForDone(ctx, job, grace) {
		return shell.Envelope{OK: true, Code: 0}, nil
	}
	// Race: the process might have exited at the boundary
	// of the grace window. Re-check before escalating.
	select {
	case <-job.Done:
		return shell.Envelope{OK: true, Code: 0}, nil
	default:
	}
	job.Mu.Lock()
	job.Signal = "KILL"
	job.Mu.Unlock()
	if err := syscall.Kill(-job.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return shell.Envelope{
			OK:     false,
			Code:   1,
			Stderr: fmt.Sprintf("kill -KILL -%d: %v", job.PID, err),
		}, nil
	}
	// SIGKILL is uncatchable; the reaper will close Done
	// almost immediately. Block on it (respecting ctx) so we
	// return only after the kernel has reaped the process.
	waitForDoneIndefinitely(ctx, job)
	return shell.Envelope{OK: true, Code: 0}, nil
}

// waitForDone blocks until job.Done closes, ctx is cancelled,
// or timeout elapses. timeout == 0 means "do not wait" --
// returns true only if Done is already closed. Returns true
// when Done observed, false on timeout or ctx cancellation.
func waitForDone(ctx context.Context, job *shell.Job, timeout time.Duration) bool {
	if timeout == 0 {
		select {
		case <-job.Done:
			return true
		default:
			return false
		}
	}
	select {
	case <-job.Done:
		return true
	case <-time.After(timeout):
		return false
	case <-ctx.Done():
		return false
	}
}

// waitForDoneIndefinitely blocks until job.Done closes or ctx
// is cancelled. Used after sending SIGKILL where the kernel
// will reap the process imminently and we just need to wait
// for the reaper goroutine to settle the captured fields.
func waitForDoneIndefinitely(ctx context.Context, job *shell.Job) {
	select {
	case <-job.Done:
	case <-ctx.Done():
	}
}

// Two leak-handling policies, chosen at the call site:
//
//   strictJobLeakHandler   FAIL render, kill, bump counter, exit non-zero
//   silentJobLeakHandler   kill, nothing else
//
// Each is a factory returning a func(*shell.Job) so call sites
// uniformly assign 'env.HandleJobLeak = <name>(args...)'. A
// nil handler is also valid -- the shell layer treats it as
// silent-no-kill -- but driver call sites always pick one of
// the two explicitly so the policy choice is visible.
//
// Strict is for contracts: 'bpfman-shell FILE' and stdin
// scripts. Silent is for exploration: the interactive prompt
// itself, and 'source FILE' invoked from a prompt. A user
// who wants the strict policy on a particular file runs
// 'bpfman-shell file.bpfman'; we deliberately do not offer a
// middle 'warn' policy because in practice the user iterating
// on a broken script wants noise to drop, not accumulate.

// strictJobLeakHandler is the script-mode policy: an unmanaged
// job is a contract violation. Renders '[job] FAIL <origin>:
// never waited or killed: argv', SIGKILLs the process group so
// 'bpfman-shell script.bpfman' never leaves stray processes,
// and bumps the session leak counter so c.Run surfaces a
// non-zero exit. Scripts are a reproducible test contract;
// leaking a job is a bug worth failing the run for.
//
// ESRCH (the process exited on its own between leak detection
// and signal delivery) is silently fine; permission errors
// fall through and would print, but in practice we sent the
// job's own SIGTERM-able signal earlier or could not have
// spawned it in the first place.
func strictJobLeakHandler(cli *bpfmancli.CLI, session *shell.Session) func(*shell.Job) {
	return func(j *shell.Job) {
		origin := j.Origin
		if origin == "" {
			origin = "<stdin>"
		}
		argv := strings.Join(j.Args, " ")
		_ = cli.PrintErrf("[job] FAIL %s: never waited or killed: %s\n", origin, argv)
		if j.PID > 0 {
			_ = syscall.Kill(-j.PID, syscall.SIGKILL)
		}
		session.RecordJobLeak()
	}
}

// silentJobLeakHandler is the policy for the interactive
// prompt and for 'source' invoked from a prompt. It SIGKILLs
// the leaked process group so nothing outlives the shell, then
// returns: no diagnostic, no counter bump, no exit-code
// effect. The REPL is exploratory scratch space; starting
// something and walking away is normal use, and a 'source'
// from a prompt is part of that exploration -- the user is
// often iterating on a broken file, and warnings would only
// become noise. The shell quietly cleans up; if a user wants
// strict feedback on a specific file, 'bpfman-shell FILE' is
// the explicit gate.
func silentJobLeakHandler() func(*shell.Job) {
	return func(j *shell.Job) {
		if j.PID > 0 {
			_ = syscall.Kill(-j.PID, syscall.SIGKILL)
		}
	}
}

// replJobs lists the jobs registered in the active job scope.
// Read-only: peeking at status does not mark any job Managed
// and does not move it out of the registry, so a 'jobs' call
// after a kill still shows the killed entry until the prompt
// chunk's WithDeferScope (or the session's WithJobScope) clears
// it. Output is column-aligned text so a long argv does not
// shift the earlier columns; each row is one job in
// registration order.
func replJobs(cli *bpfmancli.CLI, env *shell.Env) error {
	jobs := env.ActiveJobs()
	if len(jobs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-7s %-8s %-12s %-22s %s\n", "PID", "START", "STATUS", "ORIGIN", "ARGV")
	for _, j := range jobs {
		origin := j.Origin
		if origin == "" {
			origin = "<interactive>"
		}
		argv := strings.Join(j.Args, " ")
		fmt.Fprintf(&b, "%-7d %-8s %-12s %-22s %s\n",
			j.PID, j.Started.Format("15:04:05"), jobStatus(j), origin, argv)
	}
	return cli.PrintOut(b.String())
}

// replReap drops every job from the active job scope's
// registry whose Done channel has closed. Always explicit:
// the user invokes 'reap' when they want the listing
// trimmed; nothing happens automatically when a wait or kill
// returns, because the script may still want to inspect
// $job after observing its outcome. Running jobs are left
// alone. Returns no output: success is silent (Unix
// contract) and 'jobs' afterwards reflects the trimmed
// registry.
func replReap(env *shell.Env) error {
	if env == nil {
		return fmt.Errorf("reap requires an active shell environment")
	}
	env.ReapJobs(func(j *shell.Job) bool {
		select {
		case <-j.Done:
			return true
		default:
			return false
		}
	})
	return nil
}

// jobStatus reports the lifecycle stage of a Job for the 'jobs'
// listing. Four buckets:
//
//	running   - process still alive, no kill issued.
//	killing   - kill builtin has signalled but the reaper has
//	            not yet observed exit (Done open, Killed true).
//	exited N  - process exited with the given status code.
//	killed S  - process ended on signal S (whether from our
//	            kill builtin or an external sender).
//
// jobStatus does not block: it peeks Done with a non-blocking
// select so listing is O(jobs) and never waits for an
// in-flight process to die.
func jobStatus(j *shell.Job) string {
	select {
	case <-j.Done:
		// Process has been reaped; fields below are stable.
	default:
		j.Mu.Lock()
		killed := j.Killed
		j.Mu.Unlock()
		if killed {
			return "killing"
		}
		return "running"
	}
	j.Mu.Lock()
	signal := j.Signal
	exitCode := j.ExitCode
	j.Mu.Unlock()
	if signal != "" {
		return fmt.Sprintf("killed %s", signal)
	}
	return fmt.Sprintf("exited %d", exitCode)
}

// signalShortName is the inverse of signalFromName: it maps a
// syscall.Signal to the bare 'TERM' / 'USR1' / ... spelling so
// the envelope's Signal field reads naturally regardless of
// which signal the process actually ended on. An unrecognised
// signal falls back to the numeric form so tests can still
// assert on it.
func signalShortName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGQUIT:
		return "QUIT"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGUSR1:
		return "USR1"
	case syscall.SIGUSR2:
		return "USR2"
	case syscall.SIGSTOP:
		return "STOP"
	case syscall.SIGCONT:
		return "CONT"
	}
	return fmt.Sprintf("%d", int(sig))
}

// signalFromName maps a signal name to a syscall.Signal. Both
// the 'SIGNAME' and 'NAME' spellings are accepted; an unknown
// name produces an error with the offending input quoted so
// the user can correct it.
func signalFromName(name string) (syscall.Signal, error) {
	upper := strings.ToUpper(strings.TrimSpace(name))
	upper = strings.TrimPrefix(upper, "SIG")
	switch upper {
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "INT":
		return syscall.SIGINT, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "HUP":
		return syscall.SIGHUP, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	case "STOP":
		return syscall.SIGSTOP, nil
	case "CONT":
		return syscall.SIGCONT, nil
	}
	return 0, fmt.Errorf("unknown signal %q (try SIGTERM, SIGKILL, SIGINT, SIGUSR1, ...)", name)
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
