package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/manager"
)

// NewManager creates a manager for CLI commands.
// The returned manager must be closed when no longer needed.
func (c *CLI) NewManager(ctx context.Context) (*manager.Manager, error) {
	logger, err := c.Logger()
	if err != nil {
		return nil, fmt.Errorf("create logger: %w", err)
	}

	root, err := c.Root()
	if err != nil {
		return nil, fmt.Errorf("invalid runtime directory: %w", err)
	}

	mgr, err := manager.SetupRuntimeEnv(ctx, root, logger)
	if err != nil {
		return nil, fmt.Errorf("setup runtime: %w", err)
	}

	return mgr, nil
}
