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

// LoadImage loads BPF programs from an OCI container image.
// It pulls the image, extracts the bytecode, and loads each specified program.
//
// On success, returns the loaded programs.
// On failure, returns a *ManagerError containing the full operation outcome
// with timeline, rollback errors, and residual artefacts.
func (m *Manager) LoadImage(ctx context.Context, puller interpreter.ImagePuller, ref interpreter.ImageRef, programs []ImageProgramSpec, opts LoadImageOpts) (result []bpfman.Program, retErr error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	var loaded []bpfman.Program

	// Defer handles rollback, finalization, and setting the final error.
	// Using named return values allows the defer to modify retErr after
	// rollback completes, ensuring the ManagerError captures the complete outcome.
	defer func() {
		if retErr != nil && len(loaded) > 0 {
			// Rollback any loaded programs before finalizing
			rec.BeginRollback()
			for _, prog := range loaded {
				kernelID := prog.Spec.KernelID
				progName := prog.Spec.Meta.Name
				pinPath := prog.Spec.Handles.PinPath
				if err := m.Unload(ctx, kernelID); err != nil {
					m.logger.WarnContext(ctx, "rollback: failed to unload program",
						"kernel_id", kernelID,
						"name", progName,
						"error", err)
					_ = rec.RollbackFail(outcome.Step{
						Kind:   outcome.StepKindKernelUnload,
						Target: progName,
						Details: outcome.ProgramDetails{
							KernelID: kernelID,
							PinPath:  pinPath,
						},
						Error: err.Error(),
					})
				} else {
					m.logger.DebugContext(ctx, "rollback: unloaded program",
						"kernel_id", kernelID,
						"name", progName)
					_ = rec.RollbackComplete(outcome.Step{
						Kind:   outcome.StepKindKernelUnload,
						Target: progName,
						Details: outcome.ProgramDetails{
							KernelID: kernelID,
							PinPath:  pinPath,
						},
					})
				}
			}
		}
		rec.Finalise()
		// If there was an error, update retErr with the complete outcome
		if retErr != nil {
			// Extract the original cause from the existing ManagerError if present
			var cause error
			if me, ok := retErr.(*ManagerError); ok {
				cause = me.Cause
			} else {
				cause = retErr
			}
			retErr = &ManagerError{Outcome: o, Cause: cause}
		}
	}()

	if puller == nil {
		retErr = fmt.Errorf("image puller is required")
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: "validation",
			Error:  retErr.Error(),
		})
		o.PrimaryError = retErr.Error()
		return nil, retErr
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
		o.PrimaryError = retErr.Error()
		return nil, retErr
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
			o.PrimaryError = retErr.Error()
			return nil, retErr
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
			o.PrimaryError = retErr.Error()
			return nil, retErr
		}
	}

	// Load each program
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
			o.PrimaryError = retErr.Error()
			return nil, retErr
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
		loadedProg, loadErr := m.Load(ctx, spec, loadOpts)
		if loadErr != nil {
			retErr = fmt.Errorf("load program %q from image: %w", spec.ProgramName(), loadErr)
			// Mark remaining programs as skipped BEFORE recording failure
			for j := i + 1; j < len(programs); j++ {
				_ = rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].ProgramName,
				})
			}
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindKernelLoad,
				Target: spec.ProgramName(),
				Error:  retErr.Error(),
			})
			o.PrimaryError = retErr.Error()
			return nil, retErr
		}

		// Load succeeded - record both kernel and store steps
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindKernelLoad,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loadedProg.Spec.KernelID,
				PinPath:  loadedProg.Spec.Handles.PinPath,
			},
		})
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindStoreSaveProgram,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loadedProg.Spec.KernelID,
			},
		})

		m.logger.InfoContext(ctx, "loaded program from image",
			"name", spec.ProgramName(),
			"kernel_id", loadedProg.Spec.KernelID,
			"pin_path", loadedProg.Spec.Handles.PinPath)

		loaded = append(loaded, loadedProg)
	}

	return loaded, nil
}
