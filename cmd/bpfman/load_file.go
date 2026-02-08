package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/outcome"
)

// LoadCmd loads a BPF program from an object file or OCI image.
type LoadCmd struct {
	File  LoadFileCmd  `cmd:"" default:"withargs" help:"Load from a local object file."`
	Image LoadImageCmd `cmd:"" help:"Load from an OCI container image."`
}

// LoadFileCmd loads a BPF program from a local object file.
type LoadFileCmd struct {
	OutputFlags
	MetadataFlags
	GlobalDataFlags

	Path        string        `short:"p" name:"path" help:"Path to the BPF object file (.o)." required:""`
	Programs    []ProgramSpec `name:"programs" help:"TYPE:NAME or TYPE:NAME:ATTACH_FUNC program to load (can be repeated). For fentry/fexit, ATTACH_FUNC is required. If not specified, all programs in the object file are loaded."`
	Application string        `short:"a" name:"application" help:"Application name to group programs (stored as bpfman.io/application metadata)."`
	MapOwnerID  uint32        `name:"map-owner-id" help:"Program ID of another program to share maps with."`
}

// loadFileResult captures both successful programs and any failure outcome.
type loadFileResult struct {
	Programs      []bpfman.Program
	FailedOutcome outcome.OperationOutcome
}

// Run executes the load file command.
func (c *LoadFileCmd) Run(cli *CLI, ctx context.Context) error {
	// Validate object file exists (before acquiring lock)
	objPath, err := ParseObjectPath(c.Path)
	if err != nil {
		return err
	}

	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (loadFileResult, error) {
		// Convert global data
		var globalData map[string][]byte
		if len(c.GlobalData) > 0 {
			globalData = GlobalDataMap(c.GlobalData)
		}

		// Build metadata map, adding application if specified
		metadata := MetadataMap(c.Metadata)
		if c.Application != "" {
			if metadata == nil {
				metadata = make(map[string]string)
			}
			metadata["bpfman.io/application"] = c.Application
		}

		// Convert CLI ProgramSpec to manager.ProgramSpec
		var programs []manager.ProgramSpec
		for _, prog := range c.Programs {
			programs = append(programs, manager.ProgramSpec{
				Name:       prog.Name,
				Type:       prog.Type,
				AttachFunc: prog.AttachFunc,
				MapOwnerID: c.MapOwnerID,
			})
		}

		loaded, loadErr := mgr.Load(ctx, manager.LoadSource{
			FilePath: objPath.Path,
		}, programs, manager.LoadOpts{
			UserMetadata: metadata,
			GlobalData:   globalData,
		})

		var res loadFileResult
		if loadErr != nil {
			var me *manager.ManagerError
			if errors.As(loadErr, &me) {
				res.FailedOutcome = me.Outcome
			}
			return res, fmt.Errorf("failed to load programs: %w", loadErr)
		}
		res.Programs = loaded
		return res, nil
	})
	if err != nil {
		if result.FailedOutcome.Status != "" {
			return displayOutcomeError(cli, err, result.FailedOutcome, &c.OutputFlags)
		}
		return err
	}

	// Format and emit output outside the lock
	output, err := FormatLoadedPrograms(result.Programs, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
