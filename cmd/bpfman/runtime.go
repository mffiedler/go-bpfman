package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/interpreter/ebpf"
	"github.com/frobware/go-bpfman/interpreter/store/sqlite"
	"github.com/frobware/go-bpfman/manager"
)

// NewManager creates a manager for CLI commands.
// Returns the manager and a cleanup function that releases resources.
// The cleanup function should be called when the manager is no longer needed.
func (c *CLI) NewManager(ctx context.Context) (*manager.Manager, func() error, error) {
	root, err := c.Root()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid runtime directory: %w", err)
	}

	logger := c.Logger()

	// Create store
	store, err := sqlite.New(ctx, root.DBPath(), logger)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}

	// Create kernel adapter
	kernel := ebpf.New(ebpf.WithLogger(logger))

	// Create manager with real mounter
	mgr, err := manager.New(root, store, kernel, ebpf.NewProgramDiscoverer(), manager.RealMounter{}, logger)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("create manager: %w", err)
	}

	cleanup := func() error {
		return store.Close()
	}

	return mgr, cleanup, nil
}
