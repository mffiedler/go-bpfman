package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
)

// LoadCmd loads a BPF program from an object file or OCI image.
type LoadCmd struct {
	File  LoadFileCmd  `cmd:"" default:"withargs" help:"Load from a local object file."`
	Image LoadImageCmd `cmd:"" help:"Load from an OCI container image."`
}

// LoadFileCmd loads a BPF program from a local object file.
type LoadFileCmd struct {
	cliformat.OutputFlags
	MetadataFlags
	GlobalDataFlags

	Example     ExampleFlag             `name:"example" help:"Show working examples and exit."`
	Path        string                  `short:"p" name:"path" help:"Path to the BPF object file (.o)." required:""`
	Programs    []bpfmancli.ProgramSpec `name:"programs" sep:"," help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (comma-separated or repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the object file are loaded."`
	Application string                  `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`
	MapOwnerID  kernel.ProgramID        `name:"map-owner-id" help:"Program ID of another program to share maps with."`
}

// loadFileResult captures the result of a load file operation.
type loadFileResult struct {
	Programs []bpfman.Program
}

// Run executes the load file command.
func (c *LoadFileCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	return executeLoadFile(ctx, cli, mgr, c)
}

// executeLoadFileResult is the shared implementation for loading a
// BPF program from a local object file, returning the result without
// formatting. Both the CLI command and bpfman-shell call this function.
func executeLoadFileResult(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, c *LoadFileCmd) (loadFileResult, error) {
	_ = cli // reserved for future use; load no longer takes the writer lock.

	// Validate object file exists.
	objPath, err := bpfmancli.ParseObjectPath(c.Path)
	if err != nil {
		return loadFileResult{}, err
	}

	var globalData map[string][]byte
	if len(c.GlobalData) > 0 {
		globalData = bpfmancli.GlobalDataMap(c.GlobalData)
	}

	// load is lockless by construction (docs/PLAN-load-lockless.md):
	// the kernel allocates unique program ids, the bytecode directory
	// is namespaced by that id, and the sqlite commit is one
	// transaction at the end. No flock acquisition is needed.
	req := manager.NewLoadRequest(manager.LoadSource{FilePath: objPath.Path}, loadProgramSpecs(c.Programs), manager.LoadRequestOpts{
		UserMetadata: bpfmancli.MetadataMap(c.Metadata),
		GlobalData:   globalData,
		Application:  c.Application,
		MapOwnerID:   c.MapOwnerID,
	})
	loaded, loadErr := mgr.LoadRequest(ctx, req)
	if loadErr != nil {
		return loadFileResult{}, fmt.Errorf("failed to load programs: %w", loadErr)
	}
	return loadFileResult{Programs: loaded}, nil
}

// executeLoadFile is the shared implementation for loading a BPF
// program from a local object file. The CLI command calls this
// function; bpfman-shell uses executeLoadFileResult directly.
func executeLoadFile(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, c *LoadFileCmd) error {
	result, err := executeLoadFileResult(ctx, cli, mgr, c)
	if err != nil {
		return err
	}

	// Format and emit output outside the lock
	output, err := cliformat.FormatLoadedPrograms(result.Programs, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
