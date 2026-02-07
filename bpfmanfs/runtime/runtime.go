package runtime

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/frobware/go-bpfman/bpfmanfs"
)

// New creates runtime directories and ensures bpffs is mounted.
// Returns a FilesystemContext capability token that proves the filesystem
// is ready. Call once at startup before constructing a manager.
//
// The returned FilesystemContext should be passed to manager.New().
func New(layout bpfmanfs.FSLayout, mounter Mounter, logger *slog.Logger) (bpfmanfs.FilesystemContext, error) {
	if logger == nil {
		logger = slog.Default()
	}
	setupLogger := logger.With("component", "setup")

	setupLogger.Debug("ensuring runtime directories",
		"base", layout.Base(),
		"fs", layout.BPFFSMountPoint(),
		"db", layout.DBPath())

	for _, dir := range layout.RuntimeDirs() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			setupLogger.Error("failed to create directory",
				"dir", dir, "error", err)
			return bpfmanfs.FilesystemContext{}, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	if err := mounter.EnsureMounted(layout.BPFFSMountPoint()); err != nil {
		setupLogger.Error("failed to mount bpffs", "error", err)
		return bpfmanfs.FilesystemContext{}, err
	}

	setupLogger.Debug("runtime directories ready")
	return bpfmanfs.NewFilesystemContext(layout), nil
}
