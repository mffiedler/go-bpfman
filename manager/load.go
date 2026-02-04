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
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/outcome"
)

// LoadOpts contains optional metadata for a Load operation.
type LoadOpts struct {
	UserMetadata map[string]string
	Owner        string
}

// ManagerError is returned when a manager operation fails.
// It implements error and contains the full operation outcome,
// including timeline, rollback errors, and residual artefacts.
//
// Callers can use errors.As() to extract structured details:
//
//	prog, err := mgr.Load(ctx, spec, opts)
//	if err != nil {
//	    var me *ManagerError
//	    if errors.As(err, &me) {
//	        // Access me.Outcome.RollbackErrors, me.Outcome.Timeline, etc.
//	    }
//	}
type ManagerError struct {
	Outcome outcome.OperationOutcome
	Cause   error // Underlying error, accessible via errors.As/errors.Is
}

// Error returns the primary error message.
func (e *ManagerError) Error() string {
	return e.Outcome.PrimaryError
}

// Unwrap returns the underlying error for use with errors.Is/errors.As.
func (e *ManagerError) Unwrap() error {
	return e.Cause
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
// On failure, returns a *ManagerError containing the full operation outcome
// with timeline, rollback errors, and residual artefacts.
func (m *Manager) Load(ctx context.Context, spec bpfman.LoadSpec, opts LoadOpts) (bpfman.Program, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)
	now := time.Now()

	fail := func(primaryErr error) (bpfman.Program, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Program{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	// Phase 1: Load into kernel and pin to bpffs
	// The Manager owns the bpffs root path - callers don't need to know it
	loaded, err := m.kernel.Load(ctx, spec, bpffs.Root(m.dirs.FS()))
	if err != nil {
		primaryErr := fmt.Errorf("load program %s: %w", spec.ProgramName(), err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindKernelLoad,
			Target: spec.ProgramName(),
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
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

		// Attempt rollback using undoStack for consistent pattern
		var undo undoStack
		undo.push(func() error {
			return m.kernel.UnloadProgram(ctx, loaded.Managed.PinPath, loaded.Managed.PinDir)
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
			}
			rec.SetRollbackErrors(toOutcomeErrors(rbErrs))
			_ = rec.RollbackFail(outcome.Step{
				Kind:   outcome.StepKindKernelUnload,
				Target: spec.ProgramName(),
				Details: outcome.ProgramDetails{
					KernelID:    loaded.Kernel.ID,
					PinPath:     loaded.Managed.PinPath,
					MapsDirPath: loaded.Managed.PinDir,
				},
				Error: rbErrs[0].Err.Error(),
			})
			// Set residual artefacts since rollback failed
			rec.SetResidual([]outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: loaded.Kernel.ID, Path: loaded.Managed.PinPath},
				{Kind: outcome.ArtefactMapsDir, KernelID: loaded.Kernel.ID, Path: loaded.Managed.PinDir},
			}, nil)
		} else {
			m.logger.DebugContext(ctx, "rollback: unloaded program",
				"kernel_id", loaded.Kernel.ID,
				"pin_path", loaded.Managed.PinPath)
			_ = rec.RollbackComplete(outcome.Step{
				Kind:   outcome.StepKindKernelUnload,
				Target: spec.ProgramName(),
				Details: outcome.ProgramDetails{
					KernelID:    loaded.Kernel.ID,
					PinPath:     loaded.Managed.PinPath,
					MapsDirPath: loaded.Managed.PinDir,
				},
			})
		}
		return fail(storeErr)
	}

	// Record successful store save
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreSaveProgram,
		Target: spec.ProgramName(),
		Details: outcome.ProgramDetails{
			KernelID: loaded.Kernel.ID,
		},
	})

	// Fetch kernel maps for status
	var kernelMaps []kernel.Map
	for _, mapID := range loaded.Kernel.MapIDs {
		km, err := m.kernel.GetMapByID(ctx, mapID)
		if err == nil {
			kernelMaps = append(kernelMaps, km)
		}
	}

	return bpfman.Program{
		Spec: metadata,
		Status: bpfman.ProgramStatus{
			Kernel:      loaded.Kernel,
			PinPresent:  true,
			MapsPresent: len(kernelMaps) > 0,
			Links:       nil, // No links yet, just loaded
			Maps:        kernelMaps,
		},
	}, nil
}

// Unload removes a BPF program, its links, and metadata.
//
// Pattern: FETCH -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) Unload(ctx context.Context, kernelID uint32) error {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) error {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return &ManagerError{Outcome: o, Cause: primaryErr}
	}

	// FETCH: Get metadata and links (for link cleanup)
	progSpec, err := m.store.Get(ctx, kernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			// Check if program exists in kernel but isn't managed by bpfman
			if _, kerr := m.kernel.GetProgramByID(ctx, kernelID); kerr == nil {
				primaryErr = bpfman.ErrProgramNotManaged{ID: kernelID}
			} else {
				primaryErr = bpfman.ErrProgramNotFound{ID: kernelID}
			}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", kernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fmt.Sprintf("%d", kernelID),
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	programName := progSpec.Meta.Name

	// FETCH: Check for dependent programs (map sharing)
	// Programs that share maps with this program must be unloaded first.
	depCount, err := m.store.CountDependentPrograms(ctx, kernelID)
	if err != nil {
		primaryErr := fmt.Errorf("check dependent programs for %d: %w", kernelID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: programName,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}
	if depCount > 0 {
		primaryErr := fmt.Errorf("cannot unload program %d: %d dependent program(s) share its maps; unload dependents first", kernelID, depCount)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: programName,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	links, err := m.store.ListLinksByProgram(ctx, kernelID)
	if err != nil {
		primaryErr := fmt.Errorf("list links for program %d: %w", kernelID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: programName,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
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
			primaryErr := fmt.Errorf("execute unload actions: %w", err)
			return fail(primaryErr)
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
			primaryErr := fmt.Errorf("execute unload actions: %w", execResult.Error)
			return fail(primaryErr)
		}
	}

	// Clean up any dispatchers left empty by the link removal.
	m.cleanupEmptyDispatchers(ctx, dispatcherKeys)

	m.logger.InfoContext(ctx, "unloaded program", "kernel_id", kernelID)
	return nil
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
