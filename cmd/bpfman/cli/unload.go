package cli

import (
	"context"
	"errors"
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
	err = RunWithLock(ctx, cli, func(ctx context.Context) error {
		return runtime.Manager.Unload(ctx, c.ProgramID.Value)
	})
	if err != nil {
		// On failure, display the outcome if available
		var me *manager.ManagerError
		if errors.As(err, &me) {
			outcomeStr, fmtErr := FormatOutcome(me.Outcome, &c.OutputFlags)
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
	return cli.PrintOutf("Unloaded program %d\n", c.ProgramID.Value)
}
