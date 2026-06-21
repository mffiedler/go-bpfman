package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/frobware/go-bpfman/fs/runtime"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
)

// NewManager creates a manager for CLI commands.
// Returns the manager and a cleanup function that releases resources.
// The cleanup function should be called when the manager is no longer needed.
func (c *CLI) NewManager(ctx context.Context) (*manager.Manager, func() error, error) {
	return c.NewManagerWithImagePuller(ctx, nil)
}

// NewManagerWithImagePuller creates a manager for CLI commands using
// the supplied image puller. A nil puller is valid for commands that
// never load from OCI images.
func (c *CLI) NewManagerWithImagePuller(ctx context.Context, puller platform.ImagePuller) (*manager.Manager, func() error, error) {
	layout, err := c.Layout()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid runtime directory: %w", err)
	}

	logger := c.Logger()

	store, err := c.newStore(ctx, layout.DBPath(), logger)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}

	// Create kernel adapter
	kernel := ebpf.New(ebpf.WithLogger(logger))

	// Ensure runtime directories and bpffs mount
	ensuredRuntime, err := runtime.New(layout, runtime.RealMounter{}, logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("ensure runtime: %w", err)
	}

	mgr, err := manager.New(ensuredRuntime, puller, store, kernel, ebpf.NewProgramDiscoverer(), logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("create manager: %w", err)
	}

	cleanup := func() error {
		return store.Close()
	}

	return mgr, cleanup, nil
}

func (c *CLI) newStore(ctx context.Context, dbPath string, logger *slog.Logger) (platform.Store, error) {
	return RunWithLockValue(ctx, c, func(ctx context.Context, writeLock lock.WriterScope) (platform.Store, error) {
		return sqlite.New(ctx, dbPath, logger, writeLock)
	})
}
