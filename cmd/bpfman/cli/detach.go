package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
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
	err = RunWithLock(ctx, cli, func(ctx context.Context) error {
		return runtime.Manager.Detach(ctx, bpfman.LinkID(c.LinkID.Value))
	})
	if err != nil {
		if o := extractOutcome(err); o.Status != "" {
			return displayOutcomeError(cli, err, o, &c.OutputFlags)
		}
		return err
	}

	// Output outside lock
	return cli.PrintOutf("Detached link %d\n", c.LinkID.Value)
}
