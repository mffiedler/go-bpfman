package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jobLeakRecorder captures the Jobs that HandleJobLeak fires on.
// The slice doubles as the assertion target: empty means "no
// leaks reported"; populated means "these jobs were flagged".
type jobLeakRecorder struct {
	leaks []*Job
}

func (r *jobLeakRecorder) handle(j *Job) {
	r.leaks = append(r.leaks, j)
}

func TestRunWithDeferScope_UnmanagedJobReported(t *testing.T) {
	t.Parallel()

	rec := &jobLeakRecorder{}
	env := &Env{
		Session:       NewSession(),
		HandleJobLeak: rec.handle,
	}

	job := &Job{PID: 4242, Args: []string{"sleep", "60"}, Origin: "test.bpfman:7"}
	err := WithDeferScope(env, func() error {
		env.RegisterJob(job)
		return nil
	})
	require.NoError(t, err)

	require.Len(t, rec.leaks, 1, "an unmanaged job should be reported once")
	assert.Same(t, job, rec.leaks[0])
	assert.Equal(t, 1, env.Session.JobLeaks(), "session counter should reflect the leak")
}

func TestRunWithDeferScope_ManagedJobNotReported(t *testing.T) {
	t.Parallel()

	rec := &jobLeakRecorder{}
	env := &Env{
		Session:       NewSession(),
		HandleJobLeak: rec.handle,
	}

	job := &Job{PID: 4242, Args: []string{"sleep", "60"}}
	err := WithDeferScope(env, func() error {
		env.RegisterJob(job)
		// Simulate the script having waited or killed the job.
		job.MarkManaged()
		return nil
	})
	require.NoError(t, err)

	assert.Empty(t, rec.leaks, "a managed job must not fire HandleJobLeak")
	assert.Equal(t, 0, env.Session.JobLeaks(), "session counter must stay at zero")
}

func TestRunWithDeferScope_DeferKillRunsBeforeLeakCheck(t *testing.T) {
	t.Parallel()

	// Models 'defer kill $job': the deferred command marks the
	// job Managed, so the post-defers leak walk must see the
	// updated state and skip the job.
	rec := &jobLeakRecorder{}
	job := &Job{PID: 4242, Args: []string{"sleep", "60"}}

	env := &Env{
		Session:       NewSession(),
		HandleJobLeak: rec.handle,
		ExecBind: func(args []Arg) (BindResult, error) {
			job.MarkManaged()
			return BindResult{Rc: Envelope{OK: true}}, nil
		},
	}

	err := WithDeferScope(env, func() error {
		env.RegisterJob(job)
		// Stand-in for 'defer kill $job': any deferred entry
		// suffices because the test ExecBind unconditionally
		// marks the job Managed when the defer fires.
		*env.defers = append(*env.defers, deferEntry{
			Args: []Arg{WordArg{Text: "kill"}},
		})
		return nil
	})
	require.NoError(t, err)

	assert.True(t, job.IsManaged(), "defer must have run and marked the job")
	assert.Empty(t, rec.leaks, "defer-marked job must not be reported as a leak")
	assert.Equal(t, 0, env.Session.JobLeaks())
}

func TestRunWithDeferScope_NestedScopesAreIndependent(t *testing.T) {
	t.Parallel()

	rec := &jobLeakRecorder{}
	inner := &Job{PID: 1, Args: []string{"inner"}}
	outer := &Job{PID: 2, Args: []string{"outer"}}

	env := &Env{
		Session:       NewSession(),
		HandleJobLeak: rec.handle,
	}

	err := WithDeferScope(env, func() error {
		env.RegisterJob(outer)
		// Inner scope leaks its own job; the outer scope's
		// job is invisible to the inner leak walk.
		return WithDeferScope(env, func() error {
			env.RegisterJob(inner)
			return nil
		})
	})
	require.NoError(t, err)

	require.Len(t, rec.leaks, 2, "both scopes leak their own job")
	assert.Same(t, inner, rec.leaks[0], "inner scope reports first (unwinds first)")
	assert.Same(t, outer, rec.leaks[1])
	assert.Equal(t, 2, env.Session.JobLeaks())
}

func TestRunWithDeferScope_NilHandleJobLeakStillCounts(t *testing.T) {
	t.Parallel()

	env := &Env{
		Session: NewSession(),
		// HandleJobLeak deliberately nil: embedders without a
		// renderer must still see a non-zero JobLeaks at end.
	}
	err := WithDeferScope(env, func() error {
		env.RegisterJob(&Job{PID: 1, Args: []string{"x"}})
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, env.Session.JobLeaks())
}

func TestEnv_RegisterJobOutsideScopeIsNoop(t *testing.T) {
	t.Parallel()

	env := &Env{Session: NewSession()}
	// No active scope means env.jobs is nil. Registering must
	// not panic; the job simply has nowhere to be tracked.
	require.NotPanics(t, func() {
		env.RegisterJob(&Job{PID: 1})
	})
}
