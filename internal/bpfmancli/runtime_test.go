package bpfmancli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewStoreWaitsForRuntimeWriterLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cli := &CLI{LockTimeout: 10 * time.Millisecond, RuntimeDir: dir, logger: logger}

	lockPath := filepath.Join(dir, ".lock")
	dbPath := filepath.Join(dir, "store.db")
	readyPath := filepath.Join(dir, "holder.ready")
	holder := exec.Command("flock", lockPath, "sh", "-c", `touch "$0"; sleep 1`, readyPath)
	holder.Stdout = os.Stdout
	holder.Stderr = os.Stderr
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		store, err := cli.newStore(context.Background(), dbPath, logger)
		if store != nil {
			_ = store.Close()
		}
		return err != nil && strings.Contains(err.Error(), "timed out waiting for lock")
	}, time.Second, time.Millisecond)
	_, err := os.Stat(dbPath)
	require.True(t, os.IsNotExist(err), "database was created while runtime lock was held")
}
