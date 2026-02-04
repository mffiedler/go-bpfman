package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
)

// GetCmd gets details of a program or link.
type GetCmd struct {
	Program GetProgramCmd `cmd:"" help:"Get a loaded eBPF program using the Program Id."`
	Link    GetLinkCmd    `cmd:"" help:"Get a loaded eBPF program's attachment using the Link Id."`
}

// GetProgramCmd gets details of a managed program by kernel ID.
type GetProgramCmd struct {
	OutputFlags
	ProgramID ProgramID `arg:"" name:"program-id" help:"Kernel program ID (supports hex with 0x prefix)."`
}

// Run executes the get program command.
func (c *GetProgramCmd) Run(cli *CLI, ctx context.Context) error {
	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	prog, err := runtime.Manager.Get(ctx, c.ProgramID.Value)
	if err != nil {
		return err
	}

	output, err := FormatProgram(prog, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// GetLinkCmd gets details of a link by kernel link ID.
type GetLinkCmd struct {
	OutputFlags
	LinkID LinkID `arg:"" name:"link-id" help:"Kernel link ID."`
}

// Run executes the get link command.
func (c *GetLinkCmd) Run(cli *CLI, ctx context.Context) error {
	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	record, err := runtime.Manager.GetLink(ctx, bpfman.LinkID(c.LinkID.Value))
	if err != nil {
		return err
	}

	// Look up program to get the BPF function name
	var bpfFunction string
	if record.ProgramID != 0 {
		prog, err := runtime.Manager.Get(ctx, record.ProgramID)
		if err == nil && prog.Status.Kernel != nil {
			bpfFunction = prog.Status.Kernel.Name
		}
	}

	output, err := FormatLinkInfo(bpfFunction, record, record.Details, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
