package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
)

// DetachCmd detaches links.
type DetachCmd struct {
	OutputFlags
	LinkIDs []LinkID `arg:"" name:"link-id" help:"Kernel link IDs to detach." required:""`
}

// Run executes the detach command: mutation under lock, output outside.
func (c *DetachCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer mgr.Close()

	// Collect results to print after releasing lock
	type result struct {
		id  uint32
		err error
	}
	results := make([]result, 0, len(c.LinkIDs))

	// Mutation under lock - process all IDs
	lockErr := RunWithLock(ctx, cli, func(ctx context.Context) error {
		for _, lid := range c.LinkIDs {
			err := mgr.Detach(ctx, bpfman.LinkID(lid.Value))
			results = append(results, result{id: lid.Value, err: err})
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
			_ = cli.PrintErrf("link %d: %v\n", r.id, r.err)
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d link(s) failed to detach", failCount, len(results))
	}

	return nil
}
