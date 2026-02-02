package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/manager"
)

// UnloadCmd unloads a managed BPF program by kernel ID.
type UnloadCmd struct {
	OutputFlags
	ProgramID ProgramID `arg:"" name:"program-id" help:"Kernel program ID to unload (supports hex with 0x prefix)."`
}

// Run executes the unload command: mutation under lock, output outside.
func (c *UnloadCmd) Run(cli *CLI, ctx context.Context) error {
	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	// Mutation under lock
	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (manager.UnloadResult, error) {
		return runtime.Manager.Unload(ctx, c.ProgramID.Value)
	})
	if err != nil {
		// On failure, display the outcome if available
		if result.Outcome.Status != "" {
			outcomeStr, fmtErr := FormatOutcome(result.Outcome, &c.OutputFlags)
			if fmtErr == nil {
				_ = cli.PrintErr(outcomeStr)
			}
		}
		return err
	}

	// Output outside lock
	return cli.PrintOutf("Unloaded program %d\n", c.ProgramID.Value)
}
