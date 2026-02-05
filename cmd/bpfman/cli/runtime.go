package cli

import (
	"context"
	"fmt"

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

	mgr, cleanup, err := manager.SetupRuntimeEnv(ctx, root, c.Logger())
	if err != nil {
		return nil, nil, fmt.Errorf("setup runtime: %w", err)
	}

	return mgr, cleanup, nil
}
