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
	err := WithJobScope(env, func() error {
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
	err := WithJobScope(env, func() error {
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

	// Compose 'WithJobScope { WithDeferScope { body } }' the
	// same way the drivers do. Inner defer scope unwinds first
	// (so 'defer kill' marks the job), outer job scope unwinds
	// after (so the leak walk sees the updated state).
	err := WithJobScope(env, func() error {
		return WithDeferScope(env, func() error {
			env.RegisterJob(job)
			// Stand-in for 'defer kill $job': any deferred
			// entry suffices because the test ExecBind
			// unconditionally marks the job Managed when
			// the defer fires.
			*env.defers = append(*env.defers, deferEntry{
				Args: []Arg{WordArg{Text: "kill"}},
			})
			return nil
		})
	})
	require.NoError(t, err)

	assert.True(t, job.IsManaged(), "defer must have run and marked the job")
	assert.Empty(t, rec.leaks, "defer-marked job must not be reported as a leak")
	assert.Equal(t, 0, env.Session.JobLeaks())
}

func TestWithJobScope_NestedJobScopesAreIndependent(t *testing.T) {
	t.Parallel()

	// Two explicit job scopes: each fires its own leak walk
	// when it unwinds. Nesting is rare in practice (drivers
	// open one outer job scope per session unit) but the
	// mechanism must compose for any embedder that wants
	// finer-grained tracking.
	rec := &jobLeakRecorder{}
	inner := &Job{PID: 1, Args: []string{"inner"}}
	outer := &Job{PID: 2, Args: []string{"outer"}}

	env := &Env{
		Session:       NewSession(),
		HandleJobLeak: rec.handle,
	}

	err := WithJobScope(env, func() error {
		env.RegisterJob(outer)
		return WithJobScope(env, func() error {
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

func TestWithJobScope_DefBodyDoesNotOpenNewJobScope(t *testing.T) {
	t.Parallel()

	// A def body opens its own defer scope but inherits the
	// caller's job scope: a job started inside a def joins the
	// caller's registry, and returning the handle for the
	// caller to wait does not leak. Models the
	// 'WithJobScope { ... WithDeferScope { def body } ... }'
	// driver shape.
	rec := &jobLeakRecorder{}
	job := &Job{PID: 1, Args: []string{"sleep"}}

	env := &Env{
		Session:       NewSession(),
		HandleJobLeak: rec.handle,
	}

	err := WithJobScope(env, func() error {
		// def body: only a defer scope, no nested job scope.
		err := WithDeferScope(env, func() error {
			env.RegisterJob(job)
			return nil
		})
		// No leak fired yet: outer job scope still active.
		assert.Empty(t, rec.leaks)
		// Caller marks the job (stands in for a wait
		// outside the def).
		job.MarkManaged()
		return err
	})
	require.NoError(t, err)
	assert.Empty(t, rec.leaks, "managed job in caller's scope must not leak")
}

func TestRunWithDeferScope_NilHandleJobLeakStillCounts(t *testing.T) {
	t.Parallel()

	env := &Env{
		Session: NewSession(),
		// HandleJobLeak deliberately nil: embedders without a
		// renderer must still see a non-zero JobLeaks at end.
	}
	err := WithJobScope(env, func() error {
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
