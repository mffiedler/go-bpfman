package lock_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

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

func TestRunReusesActiveScopeForSameLockPath(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), ".lock")

	outerErr := errors.New("outer scope")
	err := lock.Run(context.Background(), lockPath, func(ctx context.Context, _ lock.WriterScope) error {
		blockedCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()

		return lock.Run(blockedCtx, lockPath, func(context.Context, lock.WriterScope) error {
			return outerErr
		})
	})
	require.ErrorIs(t, err, outerErr)
}
