package manager

import (
	"context"
	"log/slog"

	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/interpreter/ebpf"
	"github.com/frobware/go-bpfman/interpreter/store/sqlite"
)

// SetupRuntimeEnv initialises the runtime environment for BPF management.
// It ensures runtime directories exist, mounts bpffs, opens the database,
// and creates a manager.
//
// This is the single entry point for setting up the runtime environment.
// Both the CLI and server use this to avoid duplicating setup logic.
// Call Manager.Close() when done to release resources.
func SetupRuntimeEnv(ctx context.Context, root fs.Root, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	setupLogger := logger.With("component", "setup")

	setupLogger.Debug("ensuring runtime directories",
		"base", root.Base(),
		"fs", root.BPFFSMountPoint(),
		"db", root.DBPath(),
		"sock", root.Base()+"-sock")

	if err := root.EnsureDirectories(); err != nil {
		setupLogger.Error("failed to ensure directories", "error", err)
		return nil, err
	}
	setupLogger.Debug("runtime directories ready")

	setupLogger.Debug("opening database", "path", root.DBPath())
	store, err := sqlite.New(ctx, root.DBPath(), logger)
	if err != nil {
		setupLogger.Error("failed to open database", "error", err)
		return nil, err
	}
	setupLogger.Debug("database opened")

	kernel := ebpf.New(ebpf.WithLogger(logger))
	mgr := New(root, store, kernel, ebpf.NewProgramDiscoverer(), logger)

	setupLogger.Debug("runtime environment ready")
	return mgr, nil
}
