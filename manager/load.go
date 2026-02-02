package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/outcome"
)

// LoadOpts contains optional metadata for a Load operation.
type LoadOpts struct {
	UserMetadata map[string]string
	Owner        string
}

// LoadResult contains the result of a Load operation.
type LoadResult struct {
	Program bpfman.ManagedProgram
	Outcome outcome.ManagerOperationOutcome
}

// UnloadResult contains the result of an Unload operation.
type UnloadResult struct {
	Outcome outcome.ManagerOperationOutcome
}

// AttachResult contains the result of an Attach operation.
type AttachResult struct {
	Link    bpfman.Link
	Outcome outcome.ManagerOperationOutcome
}

// DetachResult contains the result of a Detach operation.
type DetachResult struct {
	Outcome outcome.ManagerOperationOutcome
}

// Load loads a BPF program and stores its metadata atomically.
//
// See package documentation for details on the atomic load model.
//
// Pin paths are computed from the kernel ID following the upstream convention:
//   - Program: <bpffs>/prog_<kernel_id>
//   - Maps: <bpffs>/maps/<kernel_id>/<map_name>
//
// On failure, previously completed steps are rolled back:
//   - If kernel load fails: nothing to clean up
//   - If DB persist fails: unpin program and maps from kernel
//
// The Outcome field provides structured information about what completed,
// failed, and what cleanup was attempted.
func (m *Manager) Load(ctx context.Context, spec bpfman.LoadSpec, opts LoadOpts) (result LoadResult, retErr error) {
	rec := outcome.NewRecorder(&result.Outcome)
	now := time.Now()

	// Phase 1: Load into kernel and pin to bpffs
	// The Manager owns the bpffs root path - callers don't need to know it
	loaded, err := m.kernel.Load(ctx, spec, bpffs.Root(m.dirs.FS()))
	if err != nil {
		retErr = fmt.Errorf("load program %s: %w", spec.ProgramName(), err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindKernelLoad,
			Target: spec.ProgramName(),
			Error:  retErr.Error(),
		})
		result.Outcome.Error = retErr.Error()
		return
	}

	// Record successful kernel load
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindKernelLoad,
		Target: spec.ProgramName(),
		Details: outcome.ProgramDetails{
			KernelID: loaded.Kernel.ID,
			PinPath:  loaded.Managed.PinPath,
		},
	})

	m.logger.InfoContext(ctx, "loaded program",
		"name", spec.ProgramName(),
		"kernel_id", loaded.Kernel.ID,
		"prog_pin", loaded.Managed.PinPath,
		"maps_dir", loaded.Managed.PinDir)

	// Phase 2: Persist metadata to DB (single transaction)
	// Use the inferred type from the kernel layer (from ELF section name)
	// rather than the user-specified type.
	//
	// Convert MapOwnerID: 0 means self/no owner (nil), non-zero is a pointer.
	var mapOwnerID *uint32
	if ownerID := spec.MapOwnerID(); ownerID != 0 {
		mapOwnerID = &ownerID
	}

	metadata := bpfman.ProgramSpec{
		KernelID: loaded.Kernel.ID,
		Load: bpfman.ProgramLoadSpec{
			ProgramType:   loaded.Managed.Type,
			ObjectPath:    spec.ObjectPath(),
			ImageSource:   spec.ImageSource(),
			AttachFunc:    spec.AttachFunc(),
			GlobalData:    spec.GlobalData(),
			GPLCompatible: bpfman.ExtractGPLCompatible(loaded.Kernel),
		},
		Handles: bpfman.ProgramHandles{
			PinPath:    loaded.Managed.PinPath,
			MapPinPath: loaded.Managed.PinDir, // Maps directory for CSI/unload
			MapOwnerID: mapOwnerID,
		},
		Meta: bpfman.ProgramMeta{
			Name:     spec.ProgramName(),
			Owner:    opts.Owner,
			Metadata: opts.UserMetadata,
		},
		CreatedAt: now,
	}

	// Save atomically persists program metadata. RunInTransaction ensures
	// the upsert, tag updates, and metadata index updates all commit or
	// roll back together.
	err = m.store.RunInTransaction(ctx, func(txStore interpreter.Store) error {
		return txStore.Save(ctx, loaded.Kernel.ID, metadata)
	})
	if err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "kernel_id", loaded.Kernel.ID, "error", err)

		// Record store save failure
		storeErr := fmt.Errorf("persist metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveProgram,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loaded.Kernel.ID,
			},
			Error: storeErr.Error(),
		})

		// Attempt rollback
		rec.BeginRollback()
		if rbErr := m.kernel.UnloadProgram(ctx, loaded.Managed.PinPath, loaded.Managed.PinDir); rbErr != nil {
			_ = rec.RollbackFail(outcome.Step{
				Kind:   outcome.StepKindKernelUnload,
				Target: spec.ProgramName(),
				Details: outcome.ProgramDetails{
					KernelID:    loaded.Kernel.ID,
					PinPath:     loaded.Managed.PinPath,
					MapsDirPath: loaded.Managed.PinDir,
				},
				Error: rbErr.Error(),
			})
			retErr = errors.Join(storeErr, fmt.Errorf("rollback failed: %w", rbErr))
		} else {
			_ = rec.RollbackComplete(outcome.Step{
				Kind:   outcome.StepKindKernelUnload,
				Target: spec.ProgramName(),
				Details: outcome.ProgramDetails{
					KernelID:    loaded.Kernel.ID,
					PinPath:     loaded.Managed.PinPath,
					MapsDirPath: loaded.Managed.PinDir,
				},
			})
			retErr = storeErr
		}
		result.Outcome.Error = retErr.Error()
		return
	}

	// Record successful store save
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreSaveProgram,
		Target: spec.ProgramName(),
		Details: outcome.ProgramDetails{
			KernelID: loaded.Kernel.ID,
		},
	})

	result.Program = loaded
	return
}

// Unload removes a BPF program, its links, and metadata.
//
// Pattern: FETCH -> COMPUTE -> EXECUTE
//
// The Outcome field provides structured information about what completed
// and failed during the unload operation.
func (m *Manager) Unload(ctx context.Context, kernelID uint32) (result UnloadResult, retErr error) {
	rec := outcome.NewRecorder(&result.Outcome)

	// FETCH: Get metadata and links (for link cleanup)
	progSpec, err := m.store.Get(ctx, kernelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Check if program exists in kernel but isn't managed by bpfman
			if _, kerr := m.kernel.GetProgramByID(ctx, kernelID); kerr == nil {
				retErr = bpfman.ErrProgramNotManaged{ID: kernelID}
			} else {
				retErr = bpfman.ErrProgramNotFound{ID: kernelID}
			}
		} else {
			retErr = fmt.Errorf("get program %d: %w", kernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fmt.Sprintf("%d", kernelID),
			Error:  retErr.Error(),
		})
		result.Outcome.Error = retErr.Error()
		return
	}

	programName := progSpec.Meta.Name

	// FETCH: Check for dependent programs (map sharing)
	// Programs that share maps with this program must be unloaded first.
	depCount, err := m.store.CountDependentPrograms(ctx, kernelID)
	if err != nil {
		retErr = fmt.Errorf("check dependent programs for %d: %w", kernelID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: programName,
			Error:  retErr.Error(),
		})
		result.Outcome.Error = retErr.Error()
		return
	}
	if depCount > 0 {
		retErr = fmt.Errorf("cannot unload program %d: %d dependent program(s) share its maps; unload dependents first", kernelID, depCount)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: programName,
			Error:  retErr.Error(),
		})
		result.Outcome.Error = retErr.Error()
		return
	}

	links, err := m.store.ListLinksByProgram(ctx, kernelID)
	if err != nil {
		retErr = fmt.Errorf("list links for program %d: %w", kernelID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: programName,
			Error:  retErr.Error(),
		})
		result.Outcome.Error = retErr.Error()
		return
	}

	// FETCH: Collect dispatcher keys for any TC/XDP links before
	// the unload actions delete them from the store. We need these
	// to check whether the dispatchers are now empty afterwards.
	dispatcherKeys := m.collectDispatcherKeys(ctx, links)

	// COMPUTE: Build paths from convention (kernel ID + bpffs root)
	progPinPath := m.dirs.ProgPinPath(kernelID)
	mapsDir := filepath.Join(m.dirs.FS(), "maps", fmt.Sprintf("%d", kernelID))
	linksDir := m.dirs.LinkPinDir(kernelID)

	// COMPUTE: Build unload actions and step mapping
	actions := computeUnloadActions(kernelID, progPinPath, mapsDir, linksDir, links)
	steps := computeUnloadSteps(kernelID, programName, progPinPath, mapsDir, linksDir, links)

	m.logger.InfoContext(ctx, "unloading program", "kernel_id", kernelID, "links", len(links))

	// EXECUTE: Run all actions using ExecuteAllWithResult for outcome tracking
	execWithResult, ok := m.executor.(interpreter.ActionExecutorWithResult)
	if !ok {
		// Fallback for executors that don't support result tracking
		if err := m.executor.ExecuteAll(ctx, actions); err != nil {
			retErr = fmt.Errorf("execute unload actions: %w", err)
			result.Outcome.Error = retErr.Error()
			return
		}
		// Record all steps as completed
		for _, step := range steps {
			_ = rec.Complete(step)
		}
	} else {
		execResult := execWithResult.ExecuteAllWithResult(ctx, actions)

		// Record completed steps
		for i := 0; i < execResult.CompletedCount; i++ {
			if i < len(steps) {
				_ = rec.Complete(steps[i])
			}
		}

		// If there was a failure, record it
		if execResult.Error != nil {
			if execResult.FailedIndex < len(steps) {
				failedStep := steps[execResult.FailedIndex]
				failedStep.Error = execResult.Error.Error()
				_ = rec.Fail(failedStep)
			}
			retErr = fmt.Errorf("execute unload actions: %w", execResult.Error)
			result.Outcome.Error = retErr.Error()
			return
		}
	}

	// Clean up any dispatchers left empty by the link removal.
	m.cleanupEmptyDispatchers(ctx, dispatcherKeys)

	m.logger.InfoContext(ctx, "unloaded program", "kernel_id", kernelID)
	return
}

// computeUnloadSteps generates outcome.Step entries corresponding to computeUnloadActions.
// The steps are in the same order as the actions for easy mapping.
func computeUnloadSteps(kernelID uint32, programName, progPinPath, mapsDir, linksDir string, links []bpfman.LinkSpec) []outcome.Step {
	var steps []outcome.Step

	// Detach links
	for _, link := range links {
		if link.PinPath != nil {
			steps = append(steps, outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: fmt.Sprintf("%d", link.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.ID),
					PinPath: link.PinPath.String(),
				},
			})
		}
	}

	// Remove links directory
	steps = append(steps, outcome.Step{
		Kind:   outcome.StepKindKernelRemovePin,
		Target: linksDir,
	})

	// Unload program pin
	steps = append(steps, outcome.Step{
		Kind:   outcome.StepKindKernelUnload,
		Target: programName,
		Details: outcome.ProgramDetails{
			KernelID: kernelID,
			PinPath:  progPinPath,
		},
	})

	// Unload maps directory
	steps = append(steps, outcome.Step{
		Kind:   outcome.StepKindKernelUnload,
		Target: programName,
		Details: outcome.ProgramDetails{
			KernelID:    kernelID,
			MapsDirPath: mapsDir,
		},
	})

	// Delete program metadata
	steps = append(steps, outcome.Step{
		Kind:   outcome.StepKindStoreDeleteProgram,
		Target: programName,
		Details: outcome.ProgramDetails{
			KernelID: kernelID,
		},
	})

	return steps
}

// computeUnloadActions is a pure function that computes the actions needed
// to unload a program and its associated links.
//
// Action order:
// 1. DetachLink for each link
// 2. UnloadProgram (program pin)
// 3. UnloadProgram (maps directory)
// 4. DeleteProgram
func computeUnloadActions(kernelID uint32, progPinPath, mapsDir, linksDir string, links []bpfman.LinkSpec) []action.Action {
	var actions []action.Action

	// Detach links first, then remove the links directory.
	for _, link := range links {
		if link.PinPath != nil {
			actions = append(actions, action.DetachLink{PinPath: link.PinPath.String()})
		}
	}
	actions = append(actions, action.RemovePin{Path: linksDir})

	// Unload program pin and maps directory, then delete metadata
	actions = append(actions,
		action.UnloadProgram{PinPath: progPinPath},
		action.UnloadProgram{PinPath: mapsDir},
		action.DeleteProgram{KernelID: kernelID},
	)

	return actions
}
