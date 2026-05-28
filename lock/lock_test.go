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
