package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager"
)

// DetachCmd detaches a link.
type DetachCmd struct {
	OutputFlags
	LinkID LinkID `arg:"" name:"link-id" help:"Kernel link ID to detach."`
}

// Run executes the detach command: mutation under lock, output outside.
func (c *DetachCmd) Run(cli *CLI, ctx context.Context) error {
	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	// Mutation under lock
	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (manager.DetachResult, error) {
		return runtime.Manager.Detach(ctx, bpfman.LinkID(c.LinkID.Value))
	})
	if err != nil {
		// On failure, display the outcome if available
		if result.Outcome.Status != "" {
			outcomeStr, fmtErr := FormatOutcome(result.Outcome, &c.OutputFlags)
			if fmtErr == nil {
				format, _ := c.OutputFlags.Format()
				if format == OutputFormatJSON || format == OutputFormatJSONPath {
					_ = cli.PrintOut(outcomeStr)
					return ErrSilent
				}
				_ = cli.PrintErr(outcomeStr)
			}
		}
		return err
	}

	// Output outside lock
	return cli.PrintOutf("Detached link %d\n", c.LinkID.Value)
}
