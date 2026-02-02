package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter/ebpf"
	"github.com/frobware/go-bpfman/manager"
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

// Run executes the load file command.
func (c *LoadFileCmd) Run(cli *CLI, ctx context.Context) error {
	// Validate object file exists (before acquiring lock)
	objPath, err := ParseObjectPath(c.Path)
	if err != nil {
		return err
	}

	// If no programs specified, auto-discover from object file
	programs := c.Programs
	if len(programs) == 0 {
		discovered, err := ebpf.DiscoverPrograms(objPath.Path)
		if err != nil {
			return fmt.Errorf("discover programs: %w", err)
		}
		programs = make([]ProgramSpec, 0, len(discovered))
		for _, d := range discovered {
			programs = append(programs, ProgramSpec{
				Name:       d.Name,
				Type:       d.Type,
				AttachFunc: d.AttachFunc,
			})
		}
	} else {
		// Validate all requested programs exist before loading any
		programNames := make([]string, len(programs))
		for i, p := range programs {
			programNames[i] = p.Name
		}
		if err := ebpf.ValidatePrograms(objPath.Path, programNames); err != nil {
			return err
		}
	}

	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	results, err := RunWithLockValue(ctx, cli, func(ctx context.Context) ([]bpfman.ManagedProgram, error) {
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

		results := make([]bpfman.ManagedProgram, 0, len(programs))

		// Use defer with success flag to ensure cleanup on any error path
		success := false
		defer func() {
			if success {
				return
			}
			for _, loaded := range results {
				if err := runtime.Manager.Unload(ctx, loaded.Kernel.ID); err != nil {
					runtime.Logger.Warn("rollback: failed to unload program",
						"kernel_id", loaded.Kernel.ID,
						"name", loaded.Kernel.Name,
						"error", err)
				} else {
					runtime.Logger.Debug("rollback: unloaded program",
						"kernel_id", loaded.Kernel.ID,
						"name", loaded.Kernel.Name)
				}
			}
		}()

		for _, prog := range programs {
			// Build load spec using the appropriate constructor
			var spec bpfman.LoadSpec
			var err error
			if prog.Type.RequiresAttachFunc() {
				spec, err = bpfman.NewAttachLoadSpec(objPath.Path, prog.Name, prog.Type, prog.AttachFunc)
			} else {
				spec, err = bpfman.NewLoadSpec(objPath.Path, prog.Name, prog.Type)
			}
			if err != nil {
				return nil, fmt.Errorf("invalid load spec for %q: %w", prog.Name, err)
			}

			// Apply optional fields
			if globalData != nil {
				spec = spec.WithGlobalData(globalData)
			}
			if c.MapOwnerID != 0 {
				spec = spec.WithMapOwnerID(c.MapOwnerID)
			}

			opts := manager.LoadOpts{
				UserMetadata: metadata,
			}

			// Load through manager
			loaded, err := runtime.Manager.Load(ctx, spec, opts)
			if err != nil {
				return nil, fmt.Errorf("failed to load program %q: %w", prog.Name, err)
			}
			results = append(results, loaded)
		}

		success = true
		return results, nil
	})
	if err != nil {
		return err
	}

	// Format and emit output outside the lock
	output, err := FormatLoadedPrograms(results, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}
