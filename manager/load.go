package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
	"github.com/frobware/go-bpfman/outcome"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/store"
)

// loadResult tracks a successfully loaded program before DB persist.
type loadResult struct {
	spec   bpfman.LoadSpec
	output bpfman.LoadOutput
}

// loadOpts contains optional metadata for a single-program load operation.
type loadOpts struct {
	UserMetadata map[string]string
	Owner        string
}

// loadedKey is the binding key for the kernel load output produced by
// the Produce node in loadPlan.
var loadedKey = operation.NewKey[bpfman.LoadOutput]("loaded")

// load loads a single BPF program into the kernel and publishes its
// bytecode. It does not persist anything to the store; the caller is
// responsible for building the ProgramRecord and saving it.
//
// Pattern: build plan -> Run interpreter -> extract result from bindings.
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec) (bpfman.LoadOutput, error) {
	now := time.Now()
	rt := m.fsctx.BytecodeFS()

	// Mutable pointer set by the Produce node on success; the
	// residual probe captures it to enumerate leftover artefacts
	// when rollback fails.
	var loadedPtr *bpfman.LoadOutput

	begin := func(ctx context.Context) *operation.RunState {
		rs := m.beginOp(ctx)
		rs.Probe = func(ctx context.Context) ([]outcome.Artefact, error) {
			if loadedPtr == nil {
				return nil, nil
			}
			l := *loadedPtr
			return []outcome.Artefact{
				{Kind: outcome.ArtefactProgramPin, KernelID: l.Program.ID, Path: l.PinPath},
				{Kind: outcome.ArtefactMapsDir, KernelID: l.Program.ID, Path: l.MapsDir},
				{Kind: outcome.ArtefactProgramDir, KernelID: l.Program.ID, Path: rt.ProgramBytecodePath(l.Program.ID)},
			}, nil
		}
		return rs
	}

	plan := m.loadPlan(spec, now, &loadedPtr)
	b, err := operation.Run(ctx, begin, m.executor, plan)
	if err != nil {
		return bpfman.LoadOutput{}, wrapOpErr(err)
	}

	return operation.Get(b, loadedKey), nil
}

// loadPlan builds the operation plan for loading a single program.
//
// Nodes:
//  1. Validate "preflight" -- reject image-based specs missing object path.
//  2. Produce loadedKey -- kernel load, produces LoadOutput binding.
//  3. Do "db-check" -- verify kernel ID not already in store.
//  4. Do "fs-publish" -- publish bytecode and provenance.
//
// The plan does not persist to the store. The caller is responsible
// for building the ProgramRecord and saving it after all programs in
// a batch have loaded successfully.
func (m *Manager) loadPlan(spec bpfman.LoadSpec, now time.Time, loadedPtr **bpfman.LoadOutput) operation.Plan {
	rt := m.fsctx.BytecodeFS()
	programName := spec.ProgramName()

	// 1. Preflight validation.
	preflight := operation.Validate("preflight", outcome.StepKindPreflight, "validation",
		func(_ context.Context, _ *operation.Bindings) error {
			if spec.HasImageSource() && spec.ObjectPath() == "" {
				return fmt.Errorf("load requires objectPath to be set; image pulling is handled by Load")
			}
			return nil
		},
	)

	// 2. Kernel load: produces the LoadOutput binding.
	kernelLoad := operation.Produce(loadedKey, outcome.StepKindKernelLoad, programName,
		func(ctx context.Context, _ *operation.Bindings) (bpfman.LoadOutput, error) {
			loaded, err := m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
			if err != nil {
				return bpfman.LoadOutput{}, fmt.Errorf("load program %s: %w", programName, err)
			}
			*loadedPtr = &loaded
			return loaded, nil
		},
		operation.DetailsFn(func(b *operation.Bindings) any {
			l := operation.Get(b, loadedKey)
			return outcome.ProgramDetails{
				KernelID: l.Program.ID,
				PinPath:  l.PinPath,
			}
		}),
		operation.UndoFrom(func(b *operation.Bindings) []operation.UndoEntry {
			l := operation.Get(b, loadedKey)
			return []operation.UndoEntry{
				{
					Action: action.UnloadProgram{PinPath: l.PinPath},
					Step: outcome.Step{
						Kind:   outcome.StepKindKernelUnload,
						Target: programName,
						Details: outcome.ProgramDetails{
							KernelID: l.Program.ID,
							PinPath:  l.PinPath,
						},
					},
					Severity: operation.SeverityError,
				},
				{
					Action: action.UnloadProgram{PinPath: l.MapsDir},
					Step: outcome.Step{
						Kind:   outcome.StepKindKernelUnload,
						Target: programName,
						Details: outcome.ProgramDetails{
							KernelID:    l.Program.ID,
							MapsDirPath: l.MapsDir,
						},
					},
					Severity: operation.SeverityError,
				},
			}
		}),
	)

	// 3. DB existence check.
	dbCheck := operation.Do("db-check", outcome.StepKindPreflight, programName,
		func(ctx context.Context, b *operation.Bindings) error {
			l := operation.Get(b, loadedKey)
			if _, err := m.store.Get(ctx, l.Program.ID); err == nil {
				return fmt.Errorf("program %d already exists in database", l.Program.ID)
			} else if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("check existing program %d: %w", l.Program.ID, err)
			}
			return nil
		},
	)

	// 4. Publish bytecode and provenance.
	fsPublish := operation.Do("fs-publish", outcome.StepKindFSPublish, programName,
		func(ctx context.Context, b *operation.Bindings) error {
			l := operation.Get(b, loadedKey)
			prov := bpfmanfs.Provenance{
				Version:     1,
				KernelID:    l.Program.ID,
				ProgramName: programName,
				Source:      spec.ObjectPath(),
				SourceKind:  sourceKindFromSpec(spec),
				LoadedAt:    now,
			}
			return rt.PublishBytecode(l.Program.ID, spec.ObjectPath(), prov)
		},
		operation.DetailsFn(func(b *operation.Bindings) any {
			l := operation.Get(b, loadedKey)
			return outcome.ProgramDetails{KernelID: l.Program.ID}
		}),
		operation.UndoFrom(func(b *operation.Bindings) []operation.UndoEntry {
			l := operation.Get(b, loadedKey)
			return []operation.UndoEntry{{
				Action: action.RemoveProgramDir{KernelID: l.Program.ID},
				Step: outcome.Step{
					Kind:   outcome.StepKindFSRemoveProgram,
					Target: programName,
					Details: outcome.ProgramDetails{
						KernelID: l.Program.ID,
					},
				},
				Severity: operation.SeverityWarning,
			}}
		}),
	)

	return operation.Build(preflight, kernelLoad, dbCheck, fsPublish)
}

// buildProgramRecord constructs the ProgramRecord from load inputs.
// Pure function, no I/O.
func buildProgramRecord(
	spec bpfman.LoadSpec,
	loaded bpfman.LoadOutput,
	opts loadOpts,
	rt bpfmanfs.BytecodeFS,
	now time.Time,
) bpfman.ProgramRecord {
	var mapOwnerID *uint32
	if ownerID := spec.MapOwnerID(); ownerID != 0 {
		mapOwnerID = &ownerID
	}
	return bpfman.ProgramRecord{
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
}

// LoadSource describes where to load BPF programs from.
// Exactly one of FilePath or Image must be set.
type LoadSource struct {
	FilePath string             // local ELF object file
	Image    *platform.ImageRef // OCI image to pull
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
//
// The operation proceeds in three phases:
//  1. Resolve source (image pull or file validation, program discovery).
//  2. Load each program into the kernel and publish bytecode (no DB).
//  3. Persist all program records in a single DB transaction.
//
// If phase 2 fails partway through, all previously loaded programs
// are cleaned up. If phase 3 fails, all loaded programs are cleaned
// up. Each cleanup call is independent; a failure in one does not
// prevent subsequent attempts.
func (m *Manager) Load(ctx context.Context, source LoadSource, programs []ProgramSpec, opts LoadOpts) (result []bpfman.Program, retErr error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o, func(err error) {
		m.logger.Error("outcome recorder: invariant violation", "error", err)
	})

	fail := func(err error) ([]bpfman.Program, error) {
		var cause error
		if me, ok := err.(*ManagerError); ok {
			cause = me.Cause
		} else {
			cause = err
		}
		rec.Finalise()
		return nil, &ManagerError{Outcome: o, Cause: cause}
	}

	// Phase 1: Resolve source to an object path.
	var objectPath string
	var pulled *platform.PulledImage

	if source.FilePath != "" && source.Image != nil {
		retErr = fmt.Errorf("exactly one of FilePath or Image must be set")
		rec.FailStep(outcome.StepKindPreflight, "validation", retErr)
		o.PrimaryError = retErr.Error()
		return fail(retErr)
	}

	if source.FilePath != "" {
		if _, err := os.Stat(source.FilePath); err != nil {
			retErr = fmt.Errorf("object file %s: %w", source.FilePath, err)
			rec.FailStep(outcome.StepKindPreflight, "validation", retErr)
			o.PrimaryError = retErr.Error()
			return fail(retErr)
		}
		objectPath = source.FilePath
	} else if source.Image != nil {
		if m.imagePuller == nil {
			retErr = fmt.Errorf("image puller is required")
			rec.FailStep(outcome.StepKindPreflight, "validation", retErr)
			o.PrimaryError = retErr.Error()
			return fail(retErr)
		}

		m.logger.InfoContext(ctx, "pulling OCI image",
			"url", source.Image.URL,
			"pull_policy", source.Image.PullPolicy)

		p, err := m.imagePuller.Pull(ctx, *source.Image)
		if err != nil {
			retErr = fmt.Errorf("pull image %s: %w", source.Image.URL, err)
			rec.FailStep(outcome.StepKindPullImage, source.Image.URL, retErr)
			o.PrimaryError = retErr.Error()
			return fail(retErr)
		}

		rec.CompleteStep(outcome.StepKindPullImage, source.Image.URL, outcome.ImageDetails{
			URL:        source.Image.URL,
			Digest:     p.Digest,
			ObjectPath: p.ObjectPath,
		})

		m.logger.InfoContext(ctx, "pulled OCI image",
			"url", source.Image.URL,
			"object_path", p.ObjectPath)

		objectPath = p.ObjectPath
		pulled = &p
	} else {
		retErr = fmt.Errorf("exactly one of FilePath or Image must be set")
		rec.FailStep(outcome.StepKindPreflight, "validation", retErr)
		o.PrimaryError = retErr.Error()
		return fail(retErr)
	}

	// Phase 1b: Discover or validate programs.
	if len(programs) == 0 {
		discovered, err := m.programDiscoverer.DiscoverPrograms(objectPath)
		if err != nil {
			retErr = fmt.Errorf("discover programs: %w", err)
			rec.FailStep(outcome.StepKindDiscoverPrograms, objectPath, retErr)
			o.PrimaryError = retErr.Error()
			return fail(retErr)
		}

		rec.CompleteStep(outcome.StepKindDiscoverPrograms, objectPath, outcome.ImageDetails{
			ObjectPath: objectPath,
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
			rec.FailStep(outcome.StepKindPreflight, "validate_programs", retErr)
			o.PrimaryError = retErr.Error()
			return fail(retErr)
		}
	}

	// Phase 2: Load each program into the kernel (no DB persist).
	var loaded []loadResult
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
			for j := i + 1; j < len(programs); j++ {
				if _, recErr := rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].Name,
				}); recErr != nil {
					m.logger.Error("outcome recorder: invariant violation", "error", recErr)
				}
			}
			retErr = fmt.Errorf("invalid load spec for %q: %w", prog.Name, specErr)
			rec.FailStep(outcome.StepKindKernelLoad, prog.Name, retErr)
			o.PrimaryError = retErr.Error()
			m.cleanupLoaded(ctx, loaded, &rec)
			return fail(retErr)
		}

		// Apply global data (per-program overrides take precedence).
		globalData := opts.GlobalData
		if prog.GlobalData != nil {
			globalData = prog.GlobalData
		}
		if globalData != nil {
			spec = spec.WithGlobalData(globalData)
		}

		// Map sharing logic.
		if prog.MapOwnerID != 0 {
			spec = spec.WithMapOwnerID(prog.MapOwnerID)
		} else if opts.ShareMaps && i > 0 && mapOwnerKernelID != 0 {
			spec = spec.WithMapOwnerID(mapOwnerKernelID)
		}

		// Record image provenance if loaded from an image.
		if pulled != nil {
			spec = spec.WithImageProvenance(pulled.URL, pulled.Digest, pulled.PullPolicy)
		}

		output, loadErr := m.load(ctx, spec)
		if loadErr != nil {
			for j := i + 1; j < len(programs); j++ {
				if _, recErr := rec.Skip(outcome.Step{
					Kind:   outcome.StepKindKernelLoad,
					Target: programs[j].Name,
				}); recErr != nil {
					m.logger.Error("outcome recorder: invariant violation", "error", recErr)
				}
			}
			retErr = fmt.Errorf("load program %q: %w", spec.ProgramName(), loadErr)
			rec.FailStep(outcome.StepKindKernelLoad, spec.ProgramName(), retErr)
			o.PrimaryError = retErr.Error()
			m.cleanupLoaded(ctx, loaded, &rec)
			return fail(retErr)
		}

		rec.CompleteStep(outcome.StepKindKernelLoad, spec.ProgramName(), outcome.ProgramDetails{
			KernelID: output.Program.ID,
			PinPath:  output.PinPath,
		})
		rec.CompleteStep(outcome.StepKindFSPublish, spec.ProgramName(), outcome.ProgramDetails{
			KernelID: output.Program.ID,
		})

		m.logger.InfoContext(ctx, "loaded program",
			"name", spec.ProgramName(),
			"kernel_id", output.Program.ID,
			"pin_path", output.PinPath)

		if i == 0 {
			mapOwnerKernelID = output.Program.ID
		}

		loaded = append(loaded, loadResult{spec: spec, output: output})
	}

	// Phase 3: Persist all program records in a single DB transaction.
	rt := m.fsctx.BytecodeFS()
	now := time.Now()
	singleOpts := loadOpts{
		UserMetadata: opts.UserMetadata,
		Owner:        opts.Owner,
	}

	if err := m.store.RunInTransaction(ctx, func(tx platform.Store) error {
		for _, lr := range loaded {
			record := buildProgramRecord(lr.spec, lr.output, singleOpts, rt, now)
			if err := tx.Save(ctx, lr.output.Program.ID, record); err != nil {
				return fmt.Errorf("save program %d: %w", lr.output.Program.ID, err)
			}
		}
		return nil
	}); err != nil {
		retErr = fmt.Errorf("persist program records: %w", err)
		rec.FailStep(outcome.StepKindStoreSaveProgram, "batch", retErr)
		o.PrimaryError = retErr.Error()
		m.cleanupLoaded(ctx, loaded, &rec)
		return fail(retErr)
	}

	// Phase 4: Build return value.
	var result2 []bpfman.Program
	for _, lr := range loaded {
		record := buildProgramRecord(lr.spec, lr.output, singleOpts, rt, now)

		var kernelMaps []kernel.Map
		for _, mapID := range lr.output.Program.MapIDs {
			km, err := m.kernel.GetMapByID(ctx, mapID)
			if err == nil {
				kernelMaps = append(kernelMaps, km)
			}
		}

		result2 = append(result2, bpfman.Program{
			Record: record,
			Status: bpfman.ProgramStatus{
				Kernel:      lr.output.Program,
				PinPresent:  true,
				MapsPresent: len(kernelMaps) > 0,
				Maps:        kernelMaps,
			},
		})
	}

	rec.Finalise()
	return result2, nil
}

// cleanupLoaded calls unload for each program in reverse order,
// recording rollback on the batch recorder. Each unload is a separate
// plan execution; a failure in one does not prevent subsequent
// attempts.
//
// On partial rollback failure, the recorder is populated with
// RollbackErrors and Residual artefacts so that SystemState and
// ManualCleanupRequired reflect the actual post-rollback state.
func (m *Manager) cleanupLoaded(ctx context.Context, loaded []loadResult, rec *outcome.ManagerOperationRecorder) {
	if len(loaded) == 0 {
		return
	}

	rec.BeginRollback()
	rt := m.fsctx.BytecodeFS()
	var rollbackErrors []outcome.RollbackError
	var residual []outcome.Artefact

	for i := len(loaded) - 1; i >= 0; i-- {
		lr := loaded[i]
		name := lr.spec.ProgramName()
		id := lr.output.Program.ID
		err := m.unload(ctx, id, name, nil, false)
		step := outcome.Step{
			Kind:   outcome.StepKindKernelUnload,
			Target: name,
			Details: outcome.ProgramDetails{
				KernelID: id,
				PinPath:  lr.output.PinPath,
			},
		}
		if err != nil {
			m.logger.WarnContext(ctx, "rollback: failed to unload program",
				"kernel_id", id, "name", name, "error", err)
			step.Error = err.Error()
			rec.RollbackFail(step)
			rollbackErrors = append(rollbackErrors, outcome.RollbackError{
				Step: len(rec.Outcome().Timeline) - 1,
				Err:  err.Error(),
			})
			residual = append(residual,
				outcome.Artefact{Kind: outcome.ArtefactProgramPin, KernelID: id, Path: lr.output.PinPath},
				outcome.Artefact{Kind: outcome.ArtefactMapsDir, KernelID: id, Path: lr.output.MapsDir},
				outcome.Artefact{Kind: outcome.ArtefactProgramDir, KernelID: id, Path: rt.ProgramBytecodePath(id)},
			)
		} else {
			m.logger.DebugContext(ctx, "rollback: unloaded program",
				"kernel_id", id, "name", name)
			rec.RollbackComplete(step)
		}
	}

	if len(rollbackErrors) > 0 {
		rec.SetRollbackErrors(rollbackErrors)
	}
	if len(residual) > 0 {
		rec.SetResidual(residual, nil)
	}
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
