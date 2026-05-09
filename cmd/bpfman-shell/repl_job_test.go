package main

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/shell"
)

// waitForJob blocks until the job exits or the timeout fires.
// Tests use a short timeout so a hung job fails the test fast
// rather than draining the suite's overall budget.
func waitForJob(t *testing.T, j *shell.Job) {
	t.Helper()
	select {
	case <-j.Done:
	case <-time.After(5 * time.Second):
		t.Fatalf("job pid %d did not exit within timeout", j.PID)
	}
}

func TestReplStart_SpawnsAndCapturesStdout(t *testing.T) {
	t.Parallel()

	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "echo hello"},
	})
	require.NoError(t, err)
	assert.Equal(t, shell.OriginJob, val.Kind())

	job, ok := val.Origin().(*shell.Job)
	require.True(t, ok, "Origin should be *shell.Job, got %T", val.Origin())
	assert.Greater(t, job.PID, 0, "PID should be set")
	assert.Equal(t, []string{"sh", "-c", "echo hello"}, job.Args)

	waitForJob(t, job)

	job.Mu.Lock()
	defer job.Mu.Unlock()
	assert.Equal(t, "hello\n", job.Stdout)
	assert.Empty(t, job.Stderr)
	assert.Equal(t, 0, job.ExitCode)
}

func TestReplStart_NonZeroExitCodeCaptured(t *testing.T) {
	t.Parallel()

	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "echo boom 1>&2; exit 7"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)

	waitForJob(t, job)

	job.Mu.Lock()
	defer job.Mu.Unlock()
	assert.Empty(t, job.Stdout)
	assert.Equal(t, "boom\n", job.Stderr)
	assert.Equal(t, 7, job.ExitCode)
}

func TestReplStart_NoArgsIsError(t *testing.T) {
	t.Parallel()

	_, err := replStart(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start requires at least one argument")
}

func TestReplStart_StructuredArgRejected(t *testing.T) {
	t.Parallel()

	// A program-typed Value cannot flatten into argv text, the
	// same constraint exec applies via runExternal.
	prog := shell.ValueFromMap(map[string]any{"id": "42"}).WithKind(shell.OriginProgram)
	_, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "echo"},
		shell.StructuredValueArg{Name: "prog", Value: prog},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "argument 2 is a program value")
}

func TestReplStart_LaunchFailureIsStructuralError(t *testing.T) {
	t.Parallel()

	// A non-existent binary fails at Start(), not after the
	// process runs. The error path produces no Job: this is
	// 'structural failure' and propagates back to halt the bind
	// rather than landing in a not-ok envelope.
	_, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "__definitely_not_a_real_command_2026__"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start __definitely_not_a_real_command_2026__")
}

func TestReplWait_BlocksAndCapturesEnvelope(t *testing.T) {
	t.Parallel()

	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "echo hello; sleep 0.05; echo bye"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)

	env, err := replWait(context.Background(), []shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.True(t, env.OK, "successful exit -> ok envelope")
	assert.Equal(t, 0, env.Code)
	assert.Equal(t, "hello\nbye\n", env.Stdout)
	assert.Empty(t, env.Stderr)
	assert.True(t, job.IsManaged(), "wait must mark the job managed")
}

func TestReplWait_AfterAlreadyCompleted(t *testing.T) {
	t.Parallel()

	// 'start ls /run' may exit before this goroutine reaches
	// replWait. The cached envelope must still be returned;
	// the future-shaped semantics are the whole point.
	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "echo done"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)

	// Drain the reaper before replWait sees it. After this
	// point the job is in the 'completed' state from your
	// state machine: result cached, Done closed, Managed
	// still false.
	waitForJob(t, job)
	require.False(t, job.IsManaged())

	env, err := replWait(context.Background(), []shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.True(t, env.OK)
	assert.Equal(t, "done\n", env.Stdout)
	assert.True(t, job.IsManaged(), "wait on a completed job still marks it managed")
}

func TestReplWait_NonZeroExitProducesNotOk(t *testing.T) {
	t.Parallel()

	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "exit 7"},
	})
	require.NoError(t, err)

	env, err := replWait(context.Background(), []shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.False(t, env.OK, "non-zero exit -> not ok")
	assert.Equal(t, 7, env.Code)
}

func TestReplWait_ContextCancelReturnsNotOk(t *testing.T) {
	t.Parallel()

	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "sleep 60"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)

	// Cancel the wait's context before the long sleep
	// finishes. wait should return promptly with a not-ok
	// envelope citing the cancellation reason. The
	// underlying process keeps running until syscall.Kill or
	// the start ctx ends; tests clean up by killing
	// directly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env, err := replWait(ctx, []shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.False(t, env.OK)
	assert.Equal(t, -1, env.Code)
	assert.Contains(t, env.Stderr, "context canceled")

	// Tear down the still-running process so the test does
	// not leak a sleep into the suite.
	_ = syscall.Kill(-job.PID, syscall.SIGKILL)
	waitForJob(t, job)
}

func TestReplWait_RejectsNonJobArg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		arg  shell.Arg
	}{
		{"plain word", shell.WordArg{Text: "hello"}},
		{"non-job structured", shell.StructuredValueArg{
			Name:  "prog",
			Value: shell.ValueFromMap(nil).WithKind(shell.OriginProgram),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := replWait(context.Background(), []shell.Arg{tc.arg})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "$job")
		})
	}
}

func TestReplWait_NoArgsIsError(t *testing.T) {
	t.Parallel()

	_, err := replWait(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one argument")
}

func TestReplKill_TerminatesAndMarksManaged(t *testing.T) {
	t.Parallel()

	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "sleep 60"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)

	env, err := replKill([]shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.True(t, env.OK, "kill that delivered a signal is ok")
	assert.True(t, job.IsManaged(), "kill marks the job managed")

	waitForJob(t, job)
	job.Mu.Lock()
	defer job.Mu.Unlock()
	assert.True(t, job.Killed, "Killed flag persists for wait to read")
}

func TestReplKill_KilledThenWaitReportsOk(t *testing.T) {
	t.Parallel()

	// Per design: a killed job is a clean cleanup outcome.
	// 'kill $job; wait $job' should yield ok: true even
	// though the underlying process exited via signal.
	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "sleep 60"},
	})
	require.NoError(t, err)

	_, err = replKill([]shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)

	env, err := replWait(context.Background(), []shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.True(t, env.OK, "wait on a killed job reports ok")
}

func TestReplKill_AlreadyExitedIsOk(t *testing.T) {
	t.Parallel()

	// 'kill' is best-effort: if the process exited on its
	// own before kill landed, ESRCH is treated as success.
	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "exit 0"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)
	waitForJob(t, job)

	env, err := replKill([]shell.Arg{
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.True(t, env.OK, "kill against already-exited job is ok (ESRCH swallowed)")
}

func TestReplKill_SignalFlag(t *testing.T) {
	t.Parallel()

	// '--signal=USR1' overrides the SIGTERM default. The
	// shell traps USR1 and exits with code 42 so the test
	// can confirm the signal was delivered without relying
	// on signal-status reporting.
	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "trap 'exit 42' USR1; sleep 60"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)

	// Give the trap a moment to install before signalling;
	// otherwise the USR1 may arrive while sh is still
	// initialising and the trap has no effect.
	time.Sleep(50 * time.Millisecond)

	env, err := replKill([]shell.Arg{
		shell.WordArg{Text: "--signal=USR1"},
		shell.StructuredValueArg{Name: "job", Value: val},
	})
	require.NoError(t, err)
	assert.True(t, env.OK)

	waitForJob(t, job)
	job.Mu.Lock()
	defer job.Mu.Unlock()
	assert.Equal(t, 42, job.ExitCode, "trap fired -> the chosen signal was delivered")
}

func TestReplKill_UnknownSignalIsError(t *testing.T) {
	t.Parallel()

	_, err := replKill([]shell.Arg{
		shell.WordArg{Text: "--signal=NOSUCHSIG"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown signal")
}

func TestReplKill_RejectsNonJobArg(t *testing.T) {
	t.Parallel()

	_, err := replKill([]shell.Arg{
		shell.WordArg{Text: "not-a-job"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$job")
}

func TestReplKill_NoArgsIsError(t *testing.T) {
	t.Parallel()

	_, err := replKill(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kill requires a $job argument")
}

func TestReplStart_ProcessGroupIsSet(t *testing.T) {
	t.Parallel()

	// The child runs in its own process group so 'kill' can
	// later signal the whole group. Verify by reading
	// /proc/<pid>/stat which exposes pgid as the fifth field.
	val, err := replStart(context.Background(), []shell.Arg{
		shell.WordArg{Text: "sh"},
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "sleep 0.1"},
	})
	require.NoError(t, err)
	job := val.Origin().(*shell.Job)
	defer waitForJob(t, job)

	// While the process is alive, its pgid must equal its own
	// PID (Setpgid: true makes the child its own group leader).
	pgid, err := syscall.Getpgid(job.PID)
	require.NoError(t, err)
	assert.Equal(t, job.PID, pgid, "child should be its own process-group leader")
}
