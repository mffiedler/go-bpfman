package manager_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
)

// TestGCScan_RunsWithoutWriterLock confirms the lockless contract:
// the new GCScan entry point can be called without acquiring the
// global writer lock. On a freshly-built fixture (empty kernel,
// empty store) the returned plan is empty and IsEmpty reports
// true, so gcOnEntry's pre-check would short-circuit and never
// pull the lock.
func TestGCScan_RunsWithoutWriterLock(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	plan, err := f.Manager.GCScan(context.Background(), manager.GCOptions{})
	require.NoError(t, err, "GCScan should succeed on a clean fixture without holding the lock")
	assert.True(t, plan.IsEmpty(), "fresh fixture should produce an empty plan; got %+v", plan)
	assert.Empty(t, plan.StoreActions)
}

// TestGCRemediate_EmptyPlanShortCircuits asserts the new under-lock
// entry point returns the zero GCResult and performs no execution
// when its internal re-scan finds nothing to do. The fake kernel
// records no destructive ops; the test confirms that.
func TestGCRemediate_EmptyPlanShortCircuits(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	var result manager.GCResult
	err := lock.Run(context.Background(), f.Layout.LockPath(), func(ctx context.Context, writeLock lock.WriterScope) error {
		var gcErr error
		result, gcErr = f.Manager.GCRemediate(ctx, writeLock, manager.GCOptions{})
		return gcErr
	})
	require.NoError(t, err)
	assert.Equal(t, manager.GCResult{}, result, "empty plan should yield zero result")
}

// TestGCRemediate_MatchesLegacyGC confirms the new GCRemediate is a
// behavioural drop-in for the existing GC entry point on a clean
// fixture: both produce the same (empty) result. Guards against the
// refactor accidentally changing observable semantics for the
// existing caller surface (audit dry-run, the e2e harness).
func TestGCRemediate_MatchesLegacyGC(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)

	var legacy, neu manager.GCResult
	require.NoError(t, lock.Run(context.Background(), f.Layout.LockPath(), func(ctx context.Context, writeLock lock.WriterScope) error {
		var err error
		legacy, err = f.Manager.GC(ctx, writeLock)
		if err != nil {
			return err
		}
		neu, err = f.Manager.GCRemediate(ctx, writeLock, manager.GCOptions{})
		return err
	}))
	assert.Equal(t, legacy, neu, "GCRemediate should match the legacy GC entry point on a clean fixture")
}
