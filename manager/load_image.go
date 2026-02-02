package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter"
)

// ImageProgramSpec describes a program to load from an OCI image.
// Unlike LoadSpec, this doesn't require objectPath/pinPath since those are
// determined after pulling the image.
type ImageProgramSpec struct {
	ProgramName string
	ProgramType bpfman.ProgramType
	AttachFunc  string            // Required for fentry/fexit
	GlobalData  map[string][]byte // Per-program overrides (optional)
	MapOwnerID  uint32            // Share maps with another program (optional)
}

// LoadImageOpts configures image loading.
type LoadImageOpts struct {
	UserMetadata map[string]string
	GlobalData   map[string][]byte
}

// LoadImageResult contains the loaded programs from an OCI image.
type LoadImageResult struct {
	Programs []bpfman.ManagedProgram
}

// LoadImage loads BPF programs from an OCI container image.
// It pulls the image, extracts the bytecode, and loads each specified program.
func (m *Manager) LoadImage(ctx context.Context, puller interpreter.ImagePuller, ref interpreter.ImageRef, programs []ImageProgramSpec, opts LoadImageOpts) (LoadImageResult, error) {
	if puller == nil {
		return LoadImageResult{}, fmt.Errorf("image puller is required")
	}

	// Pull the image
	m.logger.InfoContext(ctx, "pulling OCI image",
		"url", ref.URL,
		"pull_policy", ref.PullPolicy)

	pulled, err := puller.Pull(ctx, ref)
	if err != nil {
		return LoadImageResult{}, fmt.Errorf("pull image %s: %w", ref.URL, err)
	}

	m.logger.InfoContext(ctx, "pulled OCI image",
		"url", ref.URL,
		"object_path", pulled.ObjectPath)

	// If no programs specified, auto-discover from the pulled object file
	if len(programs) == 0 {
		discovered, err := m.programDiscoverer.DiscoverPrograms(pulled.ObjectPath)
		if err != nil {
			return LoadImageResult{}, fmt.Errorf("discover programs in image: %w", err)
		}
		programs = make([]ImageProgramSpec, 0, len(discovered))
		for _, d := range discovered {
			programs = append(programs, ImageProgramSpec{
				ProgramName: d.Name,
				ProgramType: d.Type,
				AttachFunc:  d.AttachFunc,
				GlobalData:  opts.GlobalData,
			})
		}
		m.logger.InfoContext(ctx, "auto-discovered programs",
			"count", len(programs))
	} else {
		// Validate all requested programs exist before loading any
		programNames := make([]string, len(programs))
		for i, p := range programs {
			programNames[i] = p.ProgramName
		}
		if err := m.programDiscoverer.ValidatePrograms(pulled.ObjectPath, programNames); err != nil {
			return LoadImageResult{}, err
		}
	}

	// Load each program, with rollback on failure
	results := make([]bpfman.ManagedProgram, 0, len(programs))

	// Use defer with success flag to ensure cleanup on any error path
	success := false
	defer func() {
		if success {
			return
		}
		for _, loaded := range results {
			if err := m.Unload(ctx, loaded.Kernel.ID); err != nil {
				m.logger.WarnContext(ctx, "rollback: failed to unload program",
					"kernel_id", loaded.Kernel.ID,
					"name", loaded.Kernel.Name,
					"error", err)
			} else {
				m.logger.DebugContext(ctx, "rollback: unloaded program",
					"kernel_id", loaded.Kernel.ID,
					"name", loaded.Kernel.Name)
			}
		}
	}()

	for _, prog := range programs {
		// Build load spec for this program
		var spec bpfman.LoadSpec
		var specErr error
		if prog.ProgramType.RequiresAttachFunc() {
			spec, specErr = bpfman.NewAttachLoadSpec(pulled.ObjectPath, prog.ProgramName, prog.ProgramType, prog.AttachFunc)
		} else {
			spec, specErr = bpfman.NewLoadSpec(pulled.ObjectPath, prog.ProgramName, prog.ProgramType)
		}
		if specErr != nil {
			return LoadImageResult{}, fmt.Errorf("invalid load spec for %q: %w", prog.ProgramName, specErr)
		}

		// Apply global data (per-program overrides take precedence)
		globalData := opts.GlobalData
		if prog.GlobalData != nil {
			globalData = prog.GlobalData
		}
		if globalData != nil {
			spec = spec.WithGlobalData(globalData)
		}

		// Set map owner ID if specified
		if prog.MapOwnerID != 0 {
			spec = spec.WithMapOwnerID(prog.MapOwnerID)
		}

		// Record image source in the spec
		imageSource := &bpfman.ImageSource{
			URL:        ref.URL,
			Digest:     pulled.Digest,
			PullPolicy: ref.PullPolicy,
		}
		spec = spec.WithImageSource(imageSource)

		loadOpts := LoadOpts{
			UserMetadata: opts.UserMetadata,
		}

		// Load through manager
		loaded, loadErr := m.Load(ctx, spec, loadOpts)
		if loadErr != nil {
			return LoadImageResult{}, fmt.Errorf("load program %q from image: %w", prog.ProgramName, loadErr)
		}

		m.logger.InfoContext(ctx, "loaded program from image",
			"name", prog.ProgramName,
			"kernel_id", loaded.Kernel.ID,
			"pin_path", loaded.Managed.PinPath)

		results = append(results, loaded)
	}

	success = true
	return LoadImageResult{Programs: results}, nil
}
