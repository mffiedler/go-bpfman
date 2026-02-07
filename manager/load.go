package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/bpfmanfs"
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
// If the spec specifies an image source (via NewImageLoadSpec), Load will
// first pull the image using the manager's ImagePuller. If no puller is
// configured, an error is returned.
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

	// Phase 0: If this is an image load, pull the image first
	if spec.IsImageLoad() {
		if m.imagePuller == nil {
			primaryErr := fmt.Errorf("image loading requires an image puller, but none configured")
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: "validation",
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}

		ref := interpreter.ImageRef{
			URL:        spec.ImageURL(),
			PullPolicy: spec.ImagePullPolicy(),
		}
		if spec.HasImageAuth() {
			ref.Auth = &interpreter.ImageAuth{
				Username: spec.ImageUsername(),
				Password: spec.ImagePassword(),
			}
		}

		m.logger.InfoContext(ctx, "pulling OCI image",
			"url", ref.URL,
			"pull_policy", ref.PullPolicy)

		pulled, err := m.imagePuller.Pull(ctx, ref)
		if err != nil {
			primaryErr := fmt.Errorf("pull image %s: %w", ref.URL, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPullImage,
				Target: ref.URL,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
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

		// Update spec with the resolved object path and digest
		spec = spec.WithObjectPath(pulled.ObjectPath).
			WithImageProvenance(pulled.URL, pulled.Digest, pulled.PullPolicy)
	}

	// Phase 1: Load into kernel and pin to bpffs
	// The Manager owns the bpffs root path - callers don't need to know it
	loaded, err := m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
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
			KernelID: loaded.Program.ID,
			PinPath:  loaded.PinPath,
		},
	})

	m.logger.InfoContext(ctx, "loaded program",
		"name", spec.ProgramName(),
		"kernel_id", loaded.Program.ID,
		"prog_pin", loaded.PinPath,
		"maps_dir", loaded.MapsDir)

	// Phase 1.5: DB existence check. If a DB row already exists for
	// this kernel_id, it means something is seriously wrong (the
	// kernel reused an ID that we still track, or a concurrent load
	// raced). Hard error and rollback kernel state.
	if _, err := m.store.Get(ctx, loaded.Program.ID); err == nil {
		// DB row exists -- invariant violation.
		primaryErr := fmt.Errorf("program %d already exists in database", loaded.Program.ID)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loaded.Program.ID,
			},
			Error: primaryErr.Error(),
		})
		// Rollback kernel load.
		if rbErr := m.kernel.UnloadProgram(ctx, loaded.PinPath, loaded.MapsDir); rbErr != nil {
			m.logger.ErrorContext(ctx, "rollback kernel unload failed", "kernel_id", loaded.Program.ID, "error", rbErr)
		}
		return fail(primaryErr)
	} else if !errors.Is(err, store.ErrNotFound) {
		// Unexpected store error.
		primaryErr := fmt.Errorf("check existing program %d: %w", loaded.Program.ID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: spec.ProgramName(),
			Error:  primaryErr.Error(),
		})
		if rbErr := m.kernel.UnloadProgram(ctx, loaded.PinPath, loaded.MapsDir); rbErr != nil {
			m.logger.ErrorContext(ctx, "rollback kernel unload failed", "kernel_id", loaded.Program.ID, "error", rbErr)
		}
		return fail(primaryErr)
	}

	// Phase 1.6: Publish bytecode to <base>/programs/{id}/.
	// Register undo step to remove it on failure.
	rt := m.fsctx.BytecodeFS()
	prov := bpfmanfs.Provenance{
		Version:     1,
		KernelID:    loaded.Program.ID,
		ProgramName: spec.ProgramName(),
		Source:      spec.ObjectPath(),
		SourceKind:  sourceKindFromSpec(spec),
		LoadedAt:    now,
	}

	if err := rt.PublishBytecode(loaded.Program.ID, spec.ObjectPath(), prov); err != nil {
		primaryErr := fmt.Errorf("publish bytecode for %d: %w", loaded.Program.ID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindFSPublish,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loaded.Program.ID,
			},
			Error: primaryErr.Error(),
		})
		// Rollback kernel load.
		if rbErr := m.kernel.UnloadProgram(ctx, loaded.PinPath, loaded.MapsDir); rbErr != nil {
			m.logger.ErrorContext(ctx, "rollback kernel unload failed", "kernel_id", loaded.Program.ID, "error", rbErr)
		}
		return fail(primaryErr)
	}

	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindFSPublish,
		Target: spec.ProgramName(),
		Details: outcome.ProgramDetails{
			KernelID: loaded.Program.ID,
		},
	})

	// Phase 2: Persist metadata to DB (single transaction)
	// Use the inferred type from the kernel layer (from ELF section name)
	// rather than the user-specified type.
	//
	// Convert MapOwnerID: 0 means self/no owner (nil), non-zero is a pointer.
	var mapOwnerID *uint32
	if ownerID := spec.MapOwnerID(); ownerID != 0 {
		mapOwnerID = &ownerID
	}

	metadata := bpfman.ProgramRecord{
		KernelID: loaded.Program.ID,
		Load: bpfman.LoadSpec{}.
			WithObjectPath(rt.ProgramBytecodePath(loaded.Program.ID)).
			WithProgramName(spec.ProgramName()).
			WithProgramType(loaded.InferredType).
			WithGlobalData(spec.GlobalData()).
			WithImageProvenance(spec.ImageURL(), spec.ImageDigest(), spec.ImagePullPolicy()).
			WithAttachFunc(spec.AttachFunc()),
		License:       loaded.License,
		GPLCompatible: bpfman.IsGPLCompatible(loaded.License),
		Handles: bpfman.ProgramHandles{
			PinPath:    loaded.PinPath,
			MapPinPath: loaded.MapsDir, // Maps directory for CSI/unload
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
		return txStore.Save(ctx, loaded.Program.ID, metadata)
	})
	if err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "kernel_id", loaded.Program.ID, "error", err)

		// Record store save failure
		storeErr := fmt.Errorf("persist metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveProgram,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loaded.Program.ID,
			},
			Error: storeErr.Error(),
		})

		// Attempt rollback using undoStack for consistent pattern.
		// LIFO order: RemoveProgram runs first, then kernel unload.
		var undo undoStack
		undo.push(func() error {
			return m.kernel.UnloadProgram(ctx, loaded.PinPath, loaded.MapsDir)
		})
		undo.push(func() error {
			return rt.RemoveProgram(loaded.Program.ID)
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
					KernelID:    loaded.Program.ID,
					PinPath:     loaded.PinPath,
					MapsDirPath: loaded.MapsDir,
				},
				Error: rbErrs[0].Err.Error(),
			})
			// Set residual artefacts since rollback failed
			rec.SetResidual([]outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: loaded.Program.ID, Path: loaded.PinPath},
				{Kind: outcome.ArtefactMapsDir, KernelID: loaded.Program.ID, Path: loaded.MapsDir},
				{Kind: outcome.ArtefactProgramDir, KernelID: loaded.Program.ID, Path: rt.ProgramBytecodePath(loaded.Program.ID)},
			}, nil)
		} else {
			m.logger.DebugContext(ctx, "rollback: unloaded program",
				"kernel_id", loaded.Program.ID,
				"pin_path", loaded.PinPath)
			_ = rec.RollbackComplete(outcome.Step{
				Kind:   outcome.StepKindKernelUnload,
				Target: spec.ProgramName(),
				Details: outcome.ProgramDetails{
					KernelID:    loaded.Program.ID,
					PinPath:     loaded.PinPath,
					MapsDirPath: loaded.MapsDir,
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
			KernelID: loaded.Program.ID,
		},
	})

	// Fetch kernel maps for status
	var kernelMaps []kernel.Map
	for _, mapID := range loaded.Program.MapIDs {
		km, err := m.kernel.GetMapByID(ctx, mapID)
		if err == nil {
			kernelMaps = append(kernelMaps, km)
		}
	}

	return bpfman.Program{
		Record: metadata,
		Status: bpfman.ProgramStatus{
			Kernel:      loaded.Program,
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
	progPinPath := m.fsctx.BPFFS().ProgPinPath(kernelID)
	mapsDir := m.fsctx.BPFFS().MapPinDir(kernelID)
	linksDir := m.fsctx.BPFFS().LinkPinDir(kernelID)

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

	// Remove persisted bytecode directory. This is best-effort: if
	// it fails, log and record the residual artefact but do not fail
	// the unload. The DB row is about to be deleted (as part of the
	// actions above), so GC will clean it on the next pass.
	rt := m.fsctx.BytecodeFS()
	if err := rt.RemoveProgram(kernelID); err != nil {
		m.logger.WarnContext(ctx, "failed to remove program dir", "kernel_id", kernelID, "error", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindFSRemoveProgram,
			Target: programName,
			Details: outcome.ProgramDetails{
				KernelID: kernelID,
			},
			Error: err.Error(),
		})
	} else {
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindFSRemoveProgram,
			Target: programName,
			Details: outcome.ProgramDetails{
				KernelID: kernelID,
			},
		})
	}

	// Clean up any dispatchers left empty by the link removal.
	m.cleanupEmptyDispatchers(ctx, dispatcherKeys)

	m.logger.InfoContext(ctx, "unloaded program", "kernel_id", kernelID)
	return nil
}

// computeUnloadSteps generates outcome.Step entries corresponding to computeUnloadActions.
// The steps are in the same order as the actions for easy mapping.
func computeUnloadSteps(kernelID uint32, programName, progPinPath, mapsDir, linksDir string, links []bpfman.LinkRecord) []outcome.Step {
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
func computeUnloadActions(kernelID uint32, progPinPath, mapsDir, linksDir string, links []bpfman.LinkRecord) []action.Action {
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

// sourceKindFromSpec returns the provenance source kind for a LoadSpec.
func sourceKindFromSpec(spec bpfman.LoadSpec) string {
	if spec.HasImageSource() {
		return "image"
	}
	if spec.ObjectPath() != "" {
		return "file"
	}
	return "unknown"
}
