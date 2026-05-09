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
