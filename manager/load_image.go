package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/outcome"
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
	Outcome  outcome.ManagerOperationOutcome
}

// LoadImage loads BPF programs from an OCI container image.
// It pulls the image, extracts the bytecode, and loads each specified program.
//
// On success, Outcome.Status == StatusSuccess and all programs are loaded.
// On failure, Outcome contains completed, failed, and skipped steps plus
// cleanup information if rollback was attempted.
func (m *Manager) LoadImage(ctx context.Context, puller interpreter.ImagePuller, ref interpreter.ImageRef, programs []ImageProgramSpec, opts LoadImageOpts) (result LoadImageResult, retErr error) {
	rec := outcome.NewRecorder(&result.Outcome)
	defer func() { rec.Finalise() }()

	if puller == nil {
		retErr = fmt.Errorf("image puller is required")
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: "validation",
			Error:  retErr.Error(),
		})
		result.Outcome.PrimaryError = retErr.Error()
		return
	}

	// Pull the image
	m.logger.InfoContext(ctx, "pulling OCI image",
		"url", ref.URL,
		"pull_policy", ref.PullPolicy)

	pulled, err := puller.Pull(ctx, ref)
	if err != nil {
		retErr = fmt.Errorf("pull image %s: %w", ref.URL, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPullImage,
			Target: ref.URL,
			Error:  retErr.Error(),
		})
		result.Outcome.PrimaryError = retErr.Error()
		return
	}

	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindPullImage,
		Target: ref.URL,
		Details: outcome.ImageDetails{
			URL:        ref.URL,
			Digest:     pulled.Digest,
			ObjectPath: pulled.ObjectPath,
		},
	})

	m.logger.InfoContext(ctx, "pulled OCI image",
		"url", ref.URL,
		"object_path", pulled.ObjectPath)

	// If no programs specified, auto-discover from the pulled object file
	if len(programs) == 0 {
		discovered, err := m.programDiscoverer.DiscoverPrograms(pulled.ObjectPath)
		if err != nil {
			retErr = fmt.Errorf("discover programs in image: %w", err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindDiscoverPrograms,
				Target: pulled.ObjectPath,
				Error:  retErr.Error(),
			})
			result.Outcome.PrimaryError = retErr.Error()
			return
		}

		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindDiscoverPrograms,
			Target: pulled.ObjectPath,
			Details: outcome.ImageDetails{
				ObjectPath: pulled.ObjectPath,
			},
		})

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
			retErr = err
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: "validate_programs",
				Error:  retErr.Error(),
			})
			result.Outcome.PrimaryError = retErr.Error()
			return
		}
	}

	// Load each program, with rollback on failure.
	// We use a defer with named return values so cleanup modifies the actual return.
	success := false
	defer func() {
		if success {
			return
		}
		if len(result.Programs) == 0 {
			return // Nothing to clean up
		}
		rec.BeginRollback()
		for _, loaded := range result.Programs {
			if _, err := m.Unload(ctx, loaded.Kernel.ID); err != nil {
				m.logger.WarnContext(ctx, "rollback: failed to unload program",
					"kernel_id", loaded.Kernel.ID,
					"name", loaded.Kernel.Name,
					"error", err)
				_ = rec.RollbackFail(outcome.Step{
					Kind:   outcome.StepKindKernelUnload,
					Target: loaded.Kernel.Name,
					Details: outcome.ProgramDetails{
						KernelID: loaded.Kernel.ID,
						PinPath:  loaded.Managed.PinPath,
					},
					Error: err.Error(),
				})
			} else {
				m.logger.DebugContext(ctx, "rollback: unloaded program",
					"kernel_id", loaded.Kernel.ID,
					"name", loaded.Kernel.Name)
				_ = rec.RollbackComplete(outcome.Step{
					Kind:   outcome.StepKindKernelUnload,
					Target: loaded.Kernel.Name,
					Details: outcome.ProgramDetails{
						KernelID: loaded.Kernel.ID,
						PinPath:  loaded.Managed.PinPath,
					},
				})
			}
		}
	}()

	for i, prog := range programs {
		// Build load spec for this program
		var spec bpfman.LoadSpec
		var specErr error
		if prog.ProgramType.RequiresAttachFunc() {
			spec, specErr = bpfman.NewAttachLoadSpec(pulled.ObjectPath, prog.ProgramName, prog.ProgramType, prog.AttachFunc)
		} else {
			spec, specErr = bpfman.NewLoadSpec(pulled.ObjectPath, prog.ProgramName, prog.ProgramType)
		}
		if specErr != nil {
			retErr = fmt.Errorf("invalid load spec for %q: %w", prog.ProgramName, specErr)
			// Mark remaining programs as skipped BEFORE recording failure
			// (recorder doesn't allow Skip after Fail)
			for j := i + 1; j < len(programs); j++ {
				_ = rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].ProgramName,
				})
			}
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindKernelLoad,
				Target: prog.ProgramName,
				Error:  retErr.Error(),
			})
			result.Outcome.PrimaryError = retErr.Error()
			return
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
		loadResult, loadErr := m.Load(ctx, spec, loadOpts)
		if loadErr != nil {
			retErr = fmt.Errorf("load program %q from image: %w", prog.ProgramName, loadErr)
			// Mark remaining programs as skipped BEFORE recording failure
			// (recorder doesn't allow Skip after Fail)
			for j := i + 1; j < len(programs); j++ {
				_ = rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].ProgramName,
				})
			}
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindKernelLoad,
				Target: prog.ProgramName,
				Error:  retErr.Error(),
			})
			result.Outcome.PrimaryError = retErr.Error()
			return
		}

		loaded := loadResult.Program

		// Load succeeded - record both kernel and store steps
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindKernelLoad,
			Target: prog.ProgramName,
			Details: outcome.ProgramDetails{
				KernelID: loaded.Kernel.ID,
				PinPath:  loaded.Managed.PinPath,
			},
		})
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindStoreSaveProgram,
			Target: prog.ProgramName,
			Details: outcome.ProgramDetails{
				KernelID: loaded.Kernel.ID,
			},
		})

		m.logger.InfoContext(ctx, "loaded program from image",
			"name", prog.ProgramName,
			"kernel_id", loaded.Kernel.ID,
			"pin_path", loaded.Managed.PinPath)

		result.Programs = append(result.Programs, loaded)
	}

	success = true
	return
}
