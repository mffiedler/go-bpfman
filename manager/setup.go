package manager

import (
	"context"
	"log/slog"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/ebpf"
	"github.com/frobware/go-bpfman/interpreter/store/sqlite"
)

// RuntimeEnv holds the initialised runtime environment for BPF management.
// Use SetupRuntimeEnv to create this, and call Close when done.
type RuntimeEnv struct {
	Store   interpreter.Store
	Kernel  interpreter.KernelOperations
	Manager *Manager
	Dirs    config.RuntimeDirs
	Logger  *slog.Logger
}

// Close releases resources held by the runtime environment.
func (r *RuntimeEnv) Close() error {
	if r.Store != nil {
		return r.Store.Close()
	}
	return nil
}

// SetupRuntimeEnv initialises the runtime environment for BPF management.
// It ensures runtime directories exist, mounts bpffs, opens the database,
// and creates a manager.
//
// This is the single entry point for setting up the runtime environment.
// Both the CLI and server use this to avoid duplicating setup logic.
func SetupRuntimeEnv(ctx context.Context, dirs config.RuntimeDirs, logger *slog.Logger) (*RuntimeEnv, error) {
	if logger == nil {
		logger = slog.Default()
	}
	setupLogger := logger.With("component", "setup")

	root := fs.FromRuntimeDirs(dirs)

	setupLogger.Debug("ensuring runtime directories",
		"base", dirs.Base(),
		"fs", dirs.FS(),
		"db", dirs.DB(),
		"sock", dirs.Sock())

	if err := root.EnsureDirectories(); err != nil {
		setupLogger.Error("failed to ensure directories", "error", err)
		return nil, err
	}
	setupLogger.Debug("runtime directories ready")

	setupLogger.Debug("opening database", "path", dirs.DBPath())
	store, err := sqlite.New(ctx, dirs.DBPath(), logger)
	if err != nil {
		setupLogger.Error("failed to open database", "error", err)
		return nil, err
	}
	setupLogger.Debug("database opened")

	kernel := ebpf.New(ebpf.WithLogger(logger))
	mgr := New(dirs, root, store, kernel, ebpf.NewProgramDiscoverer(), logger)

	setupLogger.Debug("runtime environment ready")
	return &RuntimeEnv{
		Store:   store,
		Kernel:  kernel,
		Manager: mgr,
		Dirs:    dirs,
		Logger:  logger,
	}, nil
}

// Setup creates a Manager with default implementations and returns
// a cleanup function to close the store.
//
// It ensures runtime directories exist and bpffs is mounted before
// opening the database.
//
// Deprecated: Use SetupRuntimeEnv for access to all components.
func Setup(ctx context.Context, dirs config.RuntimeDirs, logger *slog.Logger) (*Manager, func(), error) {
	env, err := SetupRuntimeEnv(ctx, dirs, logger)
	if err != nil {
		return nil, nil, err
	}
	return env.Manager, func() { env.Close() }, nil
}
