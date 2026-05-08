package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// DetachCmd detaches links.
type DetachCmd struct {
	cliformat.OutputFlags
	LinkIDs []LinkID `arg:"" name:"link-id" help:"Link IDs to detach." required:""`
}

// Run executes the detach command: mutation under lock, output outside.
func (c *DetachCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	ids := make([]kernel.LinkID, len(c.LinkIDs))
	for i, lid := range c.LinkIDs {
		ids[i] = lid.Value
	}
	return runBatchMutation(ctx, cli, ids, "link", "detach",
		func(ctx context.Context, writeLock lock.WriterScope, id kernel.LinkID) error {
			return mgr.Detach(ctx, writeLock, id)
		})
}
