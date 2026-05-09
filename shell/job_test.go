package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValueFromJob_OriginAndKind(t *testing.T) {
	t.Parallel()

	j := &Job{PID: 4321, Args: []string{"ip", "link", "show"}}
	v := ValueFromJob(j)

	assert.Equal(t, OriginJob, v.Kind())
	got, ok := v.Origin().(*Job)
	require.True(t, ok, "Origin() should be *Job, got %T", v.Origin())
	assert.Same(t, j, got, "Origin() must be the same pointer so wait/kill mutate the live job")
}

func TestValueFromJob_PIDFieldAccess(t *testing.T) {
	t.Parallel()

	v := ValueFromJob(&Job{PID: 4321})
	got, err := v.Lookup("$job", "pid")
	require.NoError(t, err)
	s, err := got.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "4321", s)
}

func TestValueFromJob_OnlyPIDIsExposed(t *testing.T) {
	t.Parallel()

	// 'pid' is the only field in the mirror; the script reaches
	// stdout/stderr/exit-code through 'wait', not through
	// $job.<field>. Confirm that absent fields error rather
	// than silently returning an empty string.
	v := ValueFromJob(&Job{PID: 99})
	for _, field := range []string{"stdout", "stderr", "code", "exit_code", "killed"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			_, err := v.LookupValue("$job", field)
			require.Error(t, err, "field %q should not resolve on a job", field)
		})
	}
}

func TestJob_MarkManagedRoundTrip(t *testing.T) {
	t.Parallel()

	j := &Job{PID: 1}
	assert.False(t, j.IsManaged(), "fresh job is unmanaged")
	j.MarkManaged()
	assert.True(t, j.IsManaged(), "MarkManaged sets Managed")
}

func TestOriginKind_JobString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "job", OriginJob.String())
}
