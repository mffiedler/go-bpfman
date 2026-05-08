package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// UnloadCmd unloads managed BPF programs by program ID.
type UnloadCmd struct {
	cliformat.OutputFlags
	ProgramIDs []ProgramID `arg:"" name:"program-id" help:"Program IDs to unload (supports hex with 0x prefix)." required:""`
}

// Run executes the unload command: mutation under lock, output outside.
func (c *UnloadCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	ids := make([]kernel.ProgramID, len(c.ProgramIDs))
	for i, pid := range c.ProgramIDs {
		ids[i] = pid.Value
	}
	return runBatchMutation(ctx, cli, ids, "program", "unload",
		func(ctx context.Context, writeLock lock.WriterScope, id kernel.ProgramID) error {
			return mgr.Unload(ctx, writeLock, id)
		})
}
