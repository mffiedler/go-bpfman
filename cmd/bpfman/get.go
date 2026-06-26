package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman/cliformat"
	"github.com/frobware/go-bpfman/cmd/internal/args"
	"github.com/frobware/go-bpfman/cmd/internal/runtime"
)

// GetCmd is a verb-noun alias path mirroring the Rust bpfman CLI
// surface (`bpfman get link <id>`, `bpfman get program <id>`).
// Callers driving the Go binary with Rust-style commands -- notably
// the bpfman-operator integration tests -- reach the same Run
// methods as the native noun-verb form (`bpfman link get <id>`).
type GetCmd struct {
	// Link shows the details of a link by link ID.
	Link GetLinkCmd `cmd:"" help:"Get details of a link by link ID."`

	// Program shows the details of a managed program by program ID.
	Program GetProgramCmd `cmd:"" help:"Get details of a managed program by program ID."`
}

// GetProgramCmd gets details of a managed program by program ID.
type GetProgramCmd struct {
	// OutputFlags carries the -o/--output flag selecting text or
	// JSON rendering.
	cliformat.OutputFlags

	// ProgramID is the kernel ID of the program to show; accepts
	// decimal or 0x-prefixed hex.
	ProgramID args.ProgramID `arg:"" name:"program-id" help:"Program ID (supports hex with 0x prefix)."`
}

// Run executes the get program command.
func (c *GetProgramCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	format, err := c.OutputFlags.Format()
	if err != nil {
		return err
	}

	mgr, cleanup, err := newManager(ctx, cli)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	prog, err := mgr.Get(ctx, c.ProgramID.Value)
	if err != nil {
		return err
	}

	return cliformat.RenderProgram(cli.Out, prog, format)
}

// GetLinkCmd gets details of a link by link ID.
type GetLinkCmd struct {
	// OutputFlags carries the -o/--output flag selecting text or
	// JSON rendering.
	cliformat.OutputFlags

	// LinkID is the ID of the link to show.
	LinkID args.LinkID `arg:"" name:"link-id" help:"Link ID."`
}

// Run executes the get link command.
func (c *GetLinkCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	format, err := c.OutputFlags.Format()
	if err != nil {
		return err
	}

	mgr, cleanup, err := newManager(ctx, cli)
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

	var programName string
	if format.NeedsLinkGetProgramName() {
		programName, err = mgr.ProgramName(ctx, info.Record.ProgramID)
		if err != nil {
			return err
		}
	}

	return cliformat.RenderLinkGet(cli.Out, cliformat.LinkGetView{
		Link:        link,
		ProgramName: programName,
	}, format)
}
