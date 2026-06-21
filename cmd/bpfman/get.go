package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
)

// GetCmd is a verb-noun alias path mirroring the Rust bpfman CLI
// surface (`bpfman get link <id>`, `bpfman get program <id>`).
// Callers driving the Go binary with Rust-style commands -- notably
// the bpfman-operator integration tests -- reach the same Run
// methods as the native noun-verb form (`bpfman link get <id>`).
type GetCmd struct {
	Link    GetLinkCmd    `cmd:"" help:"Get details of a link by link ID."`
	Program GetProgramCmd `cmd:"" help:"Get details of a managed program by program ID."`
}

// GetProgramCmd gets details of a managed program by program ID.
type GetProgramCmd struct {
	cliformat.OutputFlags
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID (supports hex with 0x prefix)."`
}

// Run executes the get program command.
func (c *GetProgramCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	prog, err := mgr.Get(ctx, c.ProgramID.Value)
	if err != nil {
		return err
	}

	return cliformat.RenderProgram(cli.Out, prog, &c.OutputFlags)
}

// GetLinkCmd gets details of a link by link ID.
type GetLinkCmd struct {
	cliformat.OutputFlags
	LinkID bpfmancli.LinkID `arg:"" name:"link-id" help:"Link ID."`
}

// Run executes the get link command.
func (c *GetLinkCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
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

	needName, err := c.OutputFlags.NeedsLinkGetProgramName()
	if err != nil {
		return err
	}
	var programName string
	if needName {
		programName, err = mgr.ProgramName(ctx, info.Record.ProgramID)
		if err != nil {
			return err
		}
	}

	return cliformat.RenderLinkGet(cli.Out, cliformat.LinkGetView{
		Link:        link,
		ProgramName: programName,
	}, &c.OutputFlags)
}
