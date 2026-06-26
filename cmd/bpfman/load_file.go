package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman/cliformat"
	"github.com/frobware/go-bpfman/cmd/internal/args"
	"github.com/frobware/go-bpfman/cmd/internal/runtime"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
)

// LoadCmd loads a BPF program from an object file or OCI image.
type LoadCmd struct {
	// File loads programs from a local object file; it is the default
	// subcommand when "load" is given no further verb.
	File LoadFileCmd `cmd:"" default:"withargs" help:"Load from a local object file."`

	// Image loads programs from an OCI container image holding the
	// bytecode.
	Image LoadImageCmd `cmd:"" help:"Load from an OCI container image."`
}

// LoadFileCmd loads a BPF program from a local object file.
type LoadFileCmd struct {
	cliformat.OutputFlags
	MetadataFlags
	GlobalDataFlags

	// Path is the filesystem path to the BPF object file (.o) to load.
	Path string `arg:"" name:"path" help:"Path to the BPF object file (.o)."`

	// Programs selects which programs in the object to load, each given as
	// TYPE:NAME or TYPE:NAME:ATTACH_FUNC (comma-separated or repeated). For
	// fentry/fexit the ATTACH_FUNC component is required. When empty, every
	// program in the object file is loaded.
	Programs []args.ProgramSpec `name:"programs" sep:"," help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (comma-separated or repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the object file are loaded."`

	// Application groups the loaded programs under an application name,
	// stored as the bpfman.io/application metadata key.
	Application string `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`

	// MapOwnerID is the kernel program ID of an already-loaded program
	// whose maps these programs should share instead of creating their own.
	MapOwnerID kernel.ProgramID `name:"map-owner-id" help:"Program ID of another program to share maps with."`
}

// loadFileResult captures the result of a load file operation.
type loadFileResult struct {
	Programs []bpfman.Program
}

// Run loads the selected programs from the local object file at Path
// (applying metadata, global data, application grouping and any
// map-owner share) and renders the loaded programs in the chosen output
// format.
func (c *LoadFileCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	format, err := c.OutputFlags.Format()
	if err != nil {
		return err
	}

	mgr, cleanup, err := newManager(ctx, cli)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	return executeLoadFile(ctx, cli, mgr, c, format)
}

// executeLoadFileResult is the shared implementation for loading a
// BPF program from a local object file, returning the result without
// formatting. Both the CLI command and bpfman-shell call this function.
func executeLoadFileResult(ctx context.Context, cli *runtime.CLI, mgr *manager.Manager, c *LoadFileCmd) (loadFileResult, error) {
	_ = cli // load does not take the writer lock.

	// Validate object file exists.
	objPath, err := args.ParseObjectPath(c.Path)
	if err != nil {
		return loadFileResult{}, err
	}

	var globalData map[string][]byte
	if len(c.GlobalData) > 0 {
		globalData = args.GlobalDataMap(c.GlobalData)
	}

	// Manager.Load decides whether the request needs the writer flock:
	// ordinary loads stay lockless, while explicit map-owner joins and
	// PinByName loads serialise internally.
	req := manager.NewLoadRequest(manager.LoadSource{FilePath: objPath.Path}, loadProgramSpecs(c.Programs), manager.LoadRequestOpts{
		UserMetadata: args.MetadataMap(c.Metadata),
		GlobalData:   globalData,
		Application:  c.Application,
		MapOwnerID:   c.MapOwnerID,
	})
	loaded, loadErr := mgr.LoadFromRequest(ctx, req)
	if loadErr != nil {
		return loadFileResult{}, fmt.Errorf("failed to load programs: %w", loadErr)
	}

	return loadFileResult{Programs: loaded}, nil
}

// executeLoadFile is the shared implementation for loading a BPF
// program from a local object file. The CLI command calls this
// function; bpfman-shell uses executeLoadFileResult directly.
func executeLoadFile(ctx context.Context, cli *runtime.CLI, mgr *manager.Manager, c *LoadFileCmd, format cliformat.OutputFormat) error {
	result, err := executeLoadFileResult(ctx, cli, mgr, c)
	if err != nil {
		return err
	}

	// Format and emit output outside the lock
	return cliformat.RenderLoadedPrograms(cli.Out, cliformat.LoadedProgramsView{Programs: result.Programs}, format)
}
