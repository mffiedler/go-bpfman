package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/cliformat"
)

// GetProgramCmd gets details of a managed program by program ID.
type GetProgramCmd struct {
	cliformat.OutputFlags
	ProgramID ProgramID `arg:"" name:"program-id" help:"Program ID (supports hex with 0x prefix)."`
}

// Run executes the get program command.
func (c *GetProgramCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	prog, err := mgr.Get(ctx, c.ProgramID.Value)
	if err != nil {
		return err
	}

	output, err := cliformat.FormatProgram(prog, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// GetLinkCmd gets details of a link by link ID.
type GetLinkCmd struct {
	cliformat.OutputFlags
	LinkID LinkID `arg:"" name:"link-id" help:"Link ID."`
}

// Run executes the get link command.
func (c *GetLinkCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	info, err := mgr.GetLinkInfo(ctx, c.LinkID.Value)
	if err != nil {
		return err
	}

	// Build the composite Link type from LinkInfo
	link := bpfman.Link{
		Record: info.Record,
		Status: bpfman.LinkStatus{
			Kernel:     info.Kernel,
			KernelSeen: info.Presence.InKernel,
			PinPresent: info.Presence.InFS,
		},
	}

	output, err := cliformat.FormatLinkResult(link, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
