package lock_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/lock"
)

// TestRunCreatesLockDirOnFirstTouch covers the CI regression where
// the lockfile's parent directory does not yet exist (no daemon has
// initialised the runtime root). acquireWriter must MkdirAll the
// parent so first-touch CLI invocations and scripted scenarios
// succeed, matching the existing O_CREATE behaviour for the lock
// file itself.
func TestRunCreatesLockDirOnFirstTouch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	lockPath := filepath.Join(root, "deep", "missing", "parent", ".lock")

	err := lock.Run(context.Background(), lockPath, func(ctx context.Context, _ lock.WriterScope) error {
		return nil
	})
	require.NoError(t, err)
}

// TestRunPanicsOnSamePathReentry proves the deadlock tripwire: a nested
// Run for a path the context already holds is a programmer error (it
// would otherwise EWOULDBLOCK against the held flock until the deadline),
// so Run panics immediately rather than reusing or blocking. The fix is
// to thread the held WriterScope to the callee.
func TestRunPanicsOnSamePathReentry(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), ".lock")

	err := lock.Run(context.Background(), lockPath, func(ctx context.Context, _ lock.WriterScope) error {
		require.Panics(t, func() {
			_ = lock.Run(ctx, lockPath, func(context.Context, lock.WriterScope) error {
				return nil
			})
		})
		return nil
	})
	require.NoError(t, err)
}

// TestRunAllowsNestedDifferentLockPath proves the tripwire is path-scoped:
// holding one lock and acquiring a genuinely different path nests cleanly,
// since the two flocks cannot self-contend.
func TestRunAllowsNestedDifferentLockPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outer := filepath.Join(dir, "outer.lock")
	inner := filepath.Join(dir, "inner.lock")

	var innerRan bool
	err := lock.Run(context.Background(), outer, func(ctx context.Context, _ lock.WriterScope) error {
		return lock.Run(ctx, inner, func(context.Context, lock.WriterScope) error {
			innerRan = true
			return nil
		})
	})
	require.NoError(t, err)
	require.True(t, innerRan)
}

// TestRunEscapedContextDoesNotTripAfterRelease proves the tripwire is
// liveness-aware: a context that escapes its callback and outlives Run
// must not report the lock as held once it has been released, so reusing
// that context for a later same-path Run acquires cleanly rather than
// panicking on a stale breadcrumb.
func TestRunEscapedContextDoesNotTripAfterRelease(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), ".lock")

	var escaped context.Context
	err := lock.Run(context.Background(), lockPath, func(ctx context.Context, _ lock.WriterScope) error {
		escaped = ctx
		return nil
	})
	require.NoError(t, err)

	var ran bool
	require.NotPanics(t, func() {
		err = lock.Run(escaped, lockPath, func(context.Context, lock.WriterScope) error {
			ran = true
			return nil
		})
	})
	require.NoError(t, err)
	require.True(t, ran)
}
