package lock_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/lock"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

func TestRunWithTimeoutSucceeds(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), ".lock")

	var ran bool
	err := lock.RunWithTimeout(context.Background(), lockPath, testLogger(), time.Second, func(context.Context, lock.WriterScope) error {
		ran = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, ran)
}

func TestRunWithTimeoutReturnsCallbackError(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), ".lock")
	wantErr := errors.New("callback failed")

	err := lock.RunWithTimeout(context.Background(), lockPath, testLogger(), time.Second, func(context.Context, lock.WriterScope) error {
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
}

func TestRunWithTimeoutReturnsTypedTimeoutError(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), ".lock")

		held := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- lock.Run(context.Background(), lockPath, func(context.Context, lock.WriterScope) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held

		const timeout = 10 * time.Millisecond
		var ran bool
		err := lock.RunWithTimeout(context.Background(), lockPath, testLogger(), timeout, func(context.Context, lock.WriterScope) error {
			ran = true
			return nil
		})
		require.False(t, ran)

		var timeoutErr *lock.TimeoutError
		require.ErrorAs(t, err, &timeoutErr)
		require.Equal(t, lockPath, timeoutErr.Path)
		require.Equal(t, timeout, timeoutErr.Timeout)

		close(release)
		require.NoError(t, <-done)
	})
}

func TestRunWithTimeoutAcquiresAfterRelease(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), ".lock")

		held := make(chan struct{})
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- lock.Run(context.Background(), lockPath, func(context.Context, lock.WriterScope) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held

		waiterDone := make(chan error, 1)
		// ran is written by the waiter goroutine and read here; the only
		// runtime ordering between them is the flock, which the race detector
		// does not model as a happens-before edge, so it must be atomic.
		var ran atomic.Bool
		go func() {
			waiterDone <- lock.RunWithTimeout(context.Background(), lockPath, testLogger(), 2*time.Second, func(context.Context, lock.WriterScope) error {
				ran.Store(true)
				return nil
			})
		}()

		synctest.Wait()
		require.False(t, ran.Load())

		close(release)
		require.NoError(t, <-waiterDone)
		require.True(t, ran.Load())
		require.NoError(t, <-holderDone)
	})
}

func TestRunWithTimeoutZeroDoesNotTranslateParentDeadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), ".lock")

		held := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- lock.Run(context.Background(), lockPath, func(context.Context, lock.WriterScope) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := lock.RunWithTimeout(ctx, lockPath, testLogger(), 0, func(context.Context, lock.WriterScope) error {
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)

		var timeoutErr *lock.TimeoutError
		require.False(t, errors.As(err, &timeoutErr))

		close(release)
		require.NoError(t, <-done)
	})
}
