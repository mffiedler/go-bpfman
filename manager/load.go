package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/outcome"
)

// loadOpts contains optional metadata for a single-program load operation.
type loadOpts struct {
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

// load loads a single BPF program and stores its metadata atomically.
//
// This is an internal method called by Load for each program in a batch.
// Callers should use the public Load method instead.
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec, opts loadOpts) (bpfman.Program, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)
	now := time.Now()

	fail := func(primaryErr error) (bpfman.Program, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Program{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	// Reject image-based specs without an object path
	if spec.HasImageSource() && spec.ObjectPath() == "" {
		primaryErr := fmt.Errorf("load requires objectPath to be set; image pulling is handled by Load")
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: "validation",
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
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

	// Accumulate rollback steps as each phase succeeds.  Every
	// failure path after this point calls rollbackLoad rather than
	// doing ad-hoc cleanup.
	var undo undoStack
	rt := m.fsctx.BytecodeFS()

	undo.push(func() error {
		return m.kernel.UnloadProgram(ctx, loaded.PinPath, loaded.MapsDir)
	})

	rollbackLoad := func(primaryErr error) (bpfman.Program, error) {
		m.logger.ErrorContext(ctx, "load failed, rolling back",
			"kernel_id", loaded.Program.ID, "error", primaryErr)

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
		return fail(primaryErr)
	}

	// Phase 1.5: DB existence check.  If a row already exists for
	// this kernel_id the kernel reused an ID we still track, or a
	// concurrent load raced.
	if _, err := m.store.Get(ctx, loaded.Program.ID); err == nil {
		primaryErr := fmt.Errorf("program %d already exists in database", loaded.Program.ID)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loaded.Program.ID,
			},
			Error: primaryErr.Error(),
		})
		return rollbackLoad(primaryErr)
	} else if !errors.Is(err, store.ErrNotFound) {
		primaryErr := fmt.Errorf("check existing program %d: %w", loaded.Program.ID, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: spec.ProgramName(),
			Error:  primaryErr.Error(),
		})
		return rollbackLoad(primaryErr)
	}

	// Phase 1.6: Publish bytecode to <base>/programs/{id}/.
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
		return rollbackLoad(primaryErr)
	}

	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindFSPublish,
		Target: spec.ProgramName(),
		Details: outcome.ProgramDetails{
			KernelID: loaded.Program.ID,
		},
	})

	// Bytecode published; add its removal to the undo stack.
	undo.push(func() error {
		return rt.RemoveProgram(loaded.Program.ID)
	})

	// Phase 2: Persist metadata to DB (single transaction)
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
			MapPinPath: loaded.MapsDir,
			MapOwnerID: mapOwnerID,
		},
		Meta: bpfman.ProgramMeta{
			Name:     spec.ProgramName(),
			Owner:    opts.Owner,
			Metadata: opts.UserMetadata,
		},
		CreatedAt: now,
	}

	err = m.store.RunInTransaction(ctx, func(txStore interpreter.Store) error {
		return txStore.Save(ctx, loaded.Program.ID, metadata)
	})
	if err != nil {
		storeErr := fmt.Errorf("persist metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveProgram,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loaded.Program.ID,
			},
			Error: storeErr.Error(),
		})
		return rollbackLoad(storeErr)
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

	// Fetch stats (nil if collection not enabled)
	stats, _ := m.kernel.GetProgramStatsByID(ctx, loaded.Program.ID)

	return bpfman.Program{
		Record: metadata,
		Status: bpfman.ProgramStatus{
			Kernel:      loaded.Program,
			Stats:       stats,
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

	// COMPUTE: Build paired unload plan (action + outcome step).
	plan := computeUnloadPlan(kernelID, programName, progPinPath, mapsDir, linksDir, links)

	m.logger.InfoContext(ctx, "unloading program", "kernel_id", kernelID, "links", len(links))

	// EXECUTE: Extract actions for the executor and record outcomes.
	actions := make([]action.Action, len(plan))
	for i, p := range plan {
		actions[i] = p.action
	}
	execResult := m.executor.ExecuteAllWithResult(ctx, actions)

	for i := 0; i < execResult.CompletedCount; i++ {
		_ = rec.Complete(plan[i].step)
	}
	if execResult.Error != nil {
		if execResult.FailedIndex < len(plan) {
			failedStep := plan[execResult.FailedIndex].step
			failedStep.Error = execResult.Error.Error()
			_ = rec.Fail(failedStep)
		}
		primaryErr := fmt.Errorf("execute unload actions: %w", execResult.Error)
		return fail(primaryErr)
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

// LoadSource describes where to load BPF programs from.
// Exactly one of FilePath or Image must be set.
type LoadSource struct {
	FilePath string                // local ELF object file
	Image    *interpreter.ImageRef // OCI image to pull
}

// ProgramSpec describes a program to load from an ELF object file.
// It is source-agnostic.
type ProgramSpec struct {
	Name       string
	Type       bpfman.ProgramType
	AttachFunc string            // required for fentry/fexit
	GlobalData map[string][]byte // per-program overrides (optional)
	MapOwnerID uint32            // explicit external map owner (0 = none)
}

// LoadOpts configures a Load operation.
type LoadOpts struct {
	UserMetadata map[string]string
	GlobalData   map[string][]byte // batch-level, overridden per-program
	Owner        string
	ShareMaps    bool // first program owns maps, subsequent auto-share
}

// Load loads one or more BPF programs from a file or OCI image.
//
// If programs is nil, all programs in the ELF are auto-discovered.
// If programs is non-nil, only those programs are loaded after
// validation.
//
// When ShareMaps is true, the first program owns maps and subsequent
// programs automatically share via MapOwnerID, unless a program
// specifies an explicit MapOwnerID.
//
// On failure, all previously loaded programs are rolled back.
func (m *Manager) Load(ctx context.Context, source LoadSource, programs []ProgramSpec, opts LoadOpts) (result []bpfman.Program, retErr error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	var loaded []bpfman.Program

	// Defer handles rollback, finalization, and setting the final error.
	defer func() {
		if retErr != nil && len(loaded) > 0 {
			rec.BeginRollback()
			for _, prog := range loaded {
				kernelID := prog.Record.KernelID
				progName := prog.Record.Meta.Name
				pinPath := prog.Record.Handles.PinPath
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
		if retErr != nil {
			var cause error
			if me, ok := retErr.(*ManagerError); ok {
				cause = me.Cause
			} else {
				cause = retErr
			}
			retErr = &ManagerError{Outcome: o, Cause: cause}
		}
	}()

	// Step 1: Resolve source to an object path
	var objectPath string
	var pulled *interpreter.PulledImage

	if source.FilePath != "" && source.Image != nil {
		retErr = fmt.Errorf("exactly one of FilePath or Image must be set")
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: "validation",
			Error:  retErr.Error(),
		})
		o.PrimaryError = retErr.Error()
		return nil, retErr
	}

	if source.FilePath != "" {
		// Validate file existence
		if _, err := os.Stat(source.FilePath); err != nil {
			retErr = fmt.Errorf("object file %s: %w", source.FilePath, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: "validation",
				Error:  retErr.Error(),
			})
			o.PrimaryError = retErr.Error()
			return nil, retErr
		}
		objectPath = source.FilePath
	} else if source.Image != nil {
		if m.imagePuller == nil {
			retErr = fmt.Errorf("image puller is required")
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: "validation",
				Error:  retErr.Error(),
			})
			o.PrimaryError = retErr.Error()
			return nil, retErr
		}

		m.logger.InfoContext(ctx, "pulling OCI image",
			"url", source.Image.URL,
			"pull_policy", source.Image.PullPolicy)

		p, err := m.imagePuller.Pull(ctx, *source.Image)
		if err != nil {
			retErr = fmt.Errorf("pull image %s: %w", source.Image.URL, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPullImage,
				Target: source.Image.URL,
				Error:  retErr.Error(),
			})
			o.PrimaryError = retErr.Error()
			return nil, retErr
		}

		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindPullImage,
			Target: source.Image.URL,
			Details: outcome.ImageDetails{
				URL:        source.Image.URL,
				Digest:     p.Digest,
				ObjectPath: p.ObjectPath,
			},
		})

		m.logger.InfoContext(ctx, "pulled OCI image",
			"url", source.Image.URL,
			"object_path", p.ObjectPath)

		objectPath = p.ObjectPath
		pulled = &p
	} else {
		retErr = fmt.Errorf("exactly one of FilePath or Image must be set")
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: "validation",
			Error:  retErr.Error(),
		})
		o.PrimaryError = retErr.Error()
		return nil, retErr
	}

	// Step 2: Discover or validate programs
	if len(programs) == 0 {
		discovered, err := m.programDiscoverer.DiscoverPrograms(objectPath)
		if err != nil {
			retErr = fmt.Errorf("discover programs: %w", err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindDiscoverPrograms,
				Target: objectPath,
				Error:  retErr.Error(),
			})
			o.PrimaryError = retErr.Error()
			return nil, retErr
		}

		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindDiscoverPrograms,
			Target: objectPath,
			Details: outcome.ImageDetails{
				ObjectPath: objectPath,
			},
		})

		programs = make([]ProgramSpec, 0, len(discovered))
		for _, d := range discovered {
			globalData := opts.GlobalData
			programs = append(programs, ProgramSpec{
				Name:       d.Name,
				Type:       d.Type,
				AttachFunc: d.AttachFunc,
				GlobalData: globalData,
			})
		}
		m.logger.InfoContext(ctx, "auto-discovered programs",
			"count", len(programs))
	} else {
		programNames := make([]string, len(programs))
		for i, p := range programs {
			programNames[i] = p.Name
		}
		if err := m.programDiscoverer.ValidatePrograms(objectPath, programNames); err != nil {
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

	// Step 3: Load each program
	var mapOwnerKernelID uint32

	for i, prog := range programs {
		var spec bpfman.LoadSpec
		var specErr error
		if prog.Type.RequiresAttachFunc() {
			spec, specErr = bpfman.NewAttachLoadSpec(objectPath, prog.Name, prog.Type, prog.AttachFunc)
		} else {
			spec, specErr = bpfman.NewLoadSpec(objectPath, prog.Name, prog.Type)
		}
		if specErr != nil {
			retErr = fmt.Errorf("invalid load spec for %q: %w", prog.Name, specErr)
			for j := i + 1; j < len(programs); j++ {
				_ = rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].Name,
				})
			}
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindKernelLoad,
				Target: prog.Name,
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

		// Map sharing logic
		if prog.MapOwnerID != 0 {
			spec = spec.WithMapOwnerID(prog.MapOwnerID)
		} else if opts.ShareMaps && i > 0 && mapOwnerKernelID != 0 {
			spec = spec.WithMapOwnerID(mapOwnerKernelID)
		}

		// Record image provenance if loaded from an image
		if pulled != nil {
			spec = spec.WithImageProvenance(pulled.URL, pulled.Digest, pulled.PullPolicy)
		}

		singleOpts := loadOpts{
			UserMetadata: opts.UserMetadata,
			Owner:        opts.Owner,
		}

		loadedProg, loadErr := m.load(ctx, spec, singleOpts)
		if loadErr != nil {
			retErr = fmt.Errorf("load program %q: %w", spec.ProgramName(), loadErr)
			for j := i + 1; j < len(programs); j++ {
				_ = rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].Name,
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

		// Record completion steps
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindKernelLoad,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loadedProg.Record.KernelID,
				PinPath:  loadedProg.Record.Handles.PinPath,
			},
		})
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindFSPublish,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loadedProg.Record.KernelID,
			},
		})
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindStoreSaveProgram,
			Target: spec.ProgramName(),
			Details: outcome.ProgramDetails{
				KernelID: loadedProg.Record.KernelID,
			},
		})

		m.logger.InfoContext(ctx, "loaded program",
			"name", spec.ProgramName(),
			"kernel_id", loadedProg.Record.KernelID,
			"pin_path", loadedProg.Record.Handles.PinPath)

		// First program becomes map owner
		if i == 0 {
			mapOwnerKernelID = loadedProg.Record.KernelID
		}

		loaded = append(loaded, loadedProg)
	}

	return loaded, nil
}

// unloadEntry pairs an action with the outcome step that describes it.
// This eliminates the parallel-array coupling between the former
// computeUnloadActions and computeUnloadSteps functions.
type unloadEntry struct {
	action action.Action
	step   outcome.Step
}

// computeUnloadPlan returns the paired action/step sequence for
// unloading a program and its associated links.
//
// Order: detach each link, remove links directory, unload program
// pin, unload maps directory, delete program metadata.
func computeUnloadPlan(kernelID uint32, programName, progPinPath, mapsDir, linksDir string, links []bpfman.LinkRecord) []unloadEntry {
	var plan []unloadEntry

	for _, link := range links {
		if link.PinPath != nil {
			plan = append(plan, unloadEntry{
				action: action.DetachLink{PinPath: link.PinPath.String()},
				step: outcome.Step{
					Kind:   outcome.StepKindKernelDetachLink,
					Target: fmt.Sprintf("%d", link.ID),
					Details: outcome.LinkDetails{
						LinkID:  uint32(link.ID),
						PinPath: link.PinPath.String(),
					},
				},
			})
		}
	}

	plan = append(plan, unloadEntry{
		action: action.RemovePin{Path: linksDir},
		step: outcome.Step{
			Kind:   outcome.StepKindKernelRemovePin,
			Target: linksDir,
		},
	})

	plan = append(plan, unloadEntry{
		action: action.UnloadProgram{PinPath: progPinPath},
		step: outcome.Step{
			Kind:   outcome.StepKindKernelUnload,
			Target: programName,
			Details: outcome.ProgramDetails{
				KernelID: kernelID,
				PinPath:  progPinPath,
			},
		},
	})

	plan = append(plan, unloadEntry{
		action: action.UnloadProgram{PinPath: mapsDir},
		step: outcome.Step{
			Kind:   outcome.StepKindKernelUnload,
			Target: programName,
			Details: outcome.ProgramDetails{
				KernelID:    kernelID,
				MapsDirPath: mapsDir,
			},
		},
	})

	plan = append(plan, unloadEntry{
		action: action.DeleteProgram{KernelID: kernelID},
		step: outcome.Step{
			Kind:   outcome.StepKindStoreDeleteProgram,
			Target: programName,
			Details: outcome.ProgramDetails{
				KernelID: kernelID,
			},
		},
	})

	return plan
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
