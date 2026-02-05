package cli

import (
	"context"
	"fmt"
)

// UnloadCmd unloads managed BPF programs by kernel ID.
type UnloadCmd struct {
	OutputFlags
	ProgramIDs []ProgramID `arg:"" name:"program-id" help:"Kernel program IDs to unload (supports hex with 0x prefix)." required:""`
}

// Run executes the unload command: mutation under lock, output outside.
func (c *UnloadCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	// Collect results to print after releasing lock
	type result struct {
		id  uint32
		err error
	}
	results := make([]result, 0, len(c.ProgramIDs))

	// Mutation under lock - process all IDs
	lockErr := RunWithLock(ctx, cli, func(ctx context.Context) error {
		for _, pid := range c.ProgramIDs {
			err := mgr.Unload(ctx, pid.Value)
			results = append(results, result{id: pid.Value, err: err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	// Print results outside lock
	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("program %d: %v\n", r.id, r.err)
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d program(s) failed to unload", failCount, len(results))
	}

	return nil
}
