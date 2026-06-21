package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman/cliformat"
	"github.com/frobware/go-bpfman/cmd/internal/args"
	"github.com/frobware/go-bpfman/cmd/internal/runtime"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// UnloadCmd unloads managed BPF programs by program ID.
type UnloadCmd struct {
	cliformat.OutputFlags
	ProgramIDs    []args.ProgramID `arg:"" name:"program-id" help:"Program IDs to unload (supports hex with 0x prefix)." required:""`
	IgnoreMissing bool             `name:"ignore-missing" help:"Treat 'program not found' as success rather than an error; useful for idempotent cleanup (e.g. defer)."`
}

// Run executes the unload command: mutation under lock, output outside.
func (c *UnloadCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	ids := make([]kernel.ProgramID, len(c.ProgramIDs))
	for i, pid := range c.ProgramIDs {
		ids[i] = pid.Value
	}
	return runtime.RunBatchMutation(ctx, cli, ids, "program", "unload",
		func(ctx context.Context, writeLock lock.WriterScope, id kernel.ProgramID) error {
			err := mgr.Unload(ctx, writeLock, id)
			if err != nil && c.IgnoreMissing {
				var notFound bpfman.ErrProgramNotFound
				if errors.As(err, &notFound) {
					return nil
				}
			}
			return err
		})
}
