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
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/store"
)

// loadedKey is the binding key for the per-program load plan.
var loadedKey = operation.NewKey[bpfman.LoadOutput]("loaded")

// loadOpts contains optional metadata for a single-program load operation.
type loadOpts struct {
	UserMetadata map[string]string
	Owner        string
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
// On failure, all previously loaded programs are cleaned up by
// calling Unload for each.
func (m *Manager) Load(ctx context.Context, source LoadSource, programs []ProgramSpec, opts LoadOpts) ([]bpfman.Program, error) {
	objectPath, pulled, err := m.resolveBatchSource(ctx, source)
	if err != nil {
		return nil, err
	}

	programs, err = m.resolveBatchPrograms(ctx, objectPath, programs, opts)
	if err != nil {
		return nil, err
	}

	specs, err := buildLoadSpecs(objectPath, programs, opts, pulled)
	if err != nil {
		return nil, fmt.Errorf("build load specs: %w", err)
	}

	rt := m.fsctx.BytecodeFS()
	perProgOpts := loadOpts{
		UserMetadata: opts.UserMetadata,
		Owner:        opts.Owner,
	}

	var loaded []bpfman.Program
	cleanupLoaded := func() {
		for j := len(loaded) - 1; j >= 0; j-- {
			if uerr := m.Unload(ctx, loaded[j].Record.KernelID); uerr != nil {
				m.logger.Error("failed to unload during batch rollback",
					"kernel_id", loaded[j].Record.KernelID, "error", uerr)
			}
		}
	}

	for i, spec := range specs {
		if opts.ShareMaps && i > 0 && spec.MapOwnerID() == 0 {
			spec = spec.WithMapOwnerID(loaded[0].Record.KernelID)
		}

		now := time.Now()
		b, err := operation.Run(ctx, m.logger, m.executor, m.loadPlan(spec, perProgOpts, now))
		if err != nil {
			cleanupLoaded()
			return nil, err
		}

		lo := operation.Get(b, loadedKey)
		record := buildProgramRecord(spec, lo, perProgOpts, rt, now)

		var kernelMaps []kernel.Map
		for _, mapID := range lo.Program.MapIDs {
			km, err := m.kernel.GetMapByID(ctx, mapID)
			if err == nil {
				kernelMaps = append(kernelMaps, km)
			}
		}

		loaded = append(loaded, bpfman.Program{
			Record: record,
			Status: bpfman.ProgramStatus{
				Kernel:      lo.Program,
				PinPresent:  true,
				MapsPresent: len(kernelMaps) > 0,
				Maps:        kernelMaps,
			},
		})
	}
	return loaded, nil
}

// loadPlan builds the per-program plan: kernel-load, db-check,
// fs-publish, store-save.
func (m *Manager) loadPlan(spec bpfman.LoadSpec, opts loadOpts, now time.Time) operation.Plan {
	programName := spec.ProgramName()
	rt := m.fsctx.BytecodeFS()

	return operation.Build(
		operation.Produce(loadedKey, programName,
			func(ctx context.Context, b *operation.Bindings) (bpfman.LoadOutput, error) {
				loaded, err := action.Produce[bpfman.LoadOutput](ctx, m.executor, action.LoadProgram{
					Spec:  spec,
					BPFFS: m.fsctx.BPFFS(),
				})
				if err != nil {
					return bpfman.LoadOutput{}, fmt.Errorf("load program %s: %w", programName, err)
				}
				m.logger.InfoContext(ctx, "loaded program",
					"name", programName,
					"kernel_id", loaded.Program.ID,
					"pin_path", loaded.PinPath)
				return loaded, nil
			},
			operation.UndoFrom(func(b *operation.Bindings) []action.Action {
				l := operation.Get(b, loadedKey)
				return []action.Action{
					action.UnloadProgram{PinPath: l.PinPath},
					action.UnloadProgram{PinPath: l.MapsDir},
				}
			}),
		),

		operation.Do("db-check", programName,
			func(ctx context.Context, b *operation.Bindings) error {
				l := operation.Get(b, loadedKey)
				if _, err := m.store.Get(ctx, l.Program.ID); err == nil {
					return fmt.Errorf("program %d already exists in database", l.Program.ID)
				} else if !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check existing program %d: %w", l.Program.ID, err)
				}
				return nil
			},
		),

		operation.Do("fs-publish", programName,
			func(ctx context.Context, b *operation.Bindings) error {
				l := operation.Get(b, loadedKey)
				return m.executor.Execute(ctx, action.PublishBytecode{
					KernelID:   l.Program.ID,
					SourcePath: spec.ObjectPath(),
					Provenance: bpfmanfs.Provenance{
						Version:     1,
						KernelID:    l.Program.ID,
						ProgramName: programName,
						Source:      spec.ObjectPath(),
						SourceKind:  sourceKindFromSpec(spec),
						LoadedAt:    now,
					},
				})
			},
			operation.UndoFrom(func(b *operation.Bindings) []action.Action {
				l := operation.Get(b, loadedKey)
				return []action.Action{
					action.RemoveProgramDir{KernelID: l.Program.ID},
				}
			}),
		),

		operation.Do("store-save", programName,
			func(ctx context.Context, b *operation.Bindings) error {
				l := operation.Get(b, loadedKey)
				record := buildProgramRecord(spec, l, opts, rt, now)
				return m.executor.Execute(ctx, action.SaveProgram{
					KernelID: l.Program.ID,
					Metadata: record,
				})
			},
		),
	)
}

// resolveBatchSource resolves the LoadSource to an object path and
// optional PulledImage.
func (m *Manager) resolveBatchSource(
	ctx context.Context,
	source LoadSource,
) (string, *platform.PulledImage, error) {
	if source.FilePath != "" && source.Image != nil {
		return "", nil, fmt.Errorf("exactly one of FilePath or Image must be set")
	}

	if source.FilePath != "" {
		if _, err := os.Stat(source.FilePath); err != nil {
			return "", nil, fmt.Errorf("object file %s: %w", source.FilePath, err)
		}
		return source.FilePath, nil, nil
	}

	if source.Image != nil {
		if m.imagePuller == nil {
			return "", nil, fmt.Errorf("image puller is required")
		}

		m.logger.InfoContext(ctx, "pulling OCI image",
			"url", source.Image.URL,
			"pull_policy", source.Image.PullPolicy)

		p, err := m.imagePuller.Pull(ctx, *source.Image)
		if err != nil {
			return "", nil, fmt.Errorf("pull image %s: %w", source.Image.URL, err)
		}

		m.logger.InfoContext(ctx, "pulled OCI image",
			"url", source.Image.URL,
			"object_path", p.ObjectPath)

		return p.ObjectPath, &p, nil
	}

	return "", nil, fmt.Errorf("exactly one of FilePath or Image must be set")
}

// resolveBatchPrograms discovers or validates the program list.
func (m *Manager) resolveBatchPrograms(
	ctx context.Context,
	objectPath string,
	programs []ProgramSpec,
	opts LoadOpts,
) ([]ProgramSpec, error) {
	if len(programs) == 0 {
		discovered, err := m.programDiscoverer.DiscoverPrograms(objectPath)
		if err != nil {
			return nil, fmt.Errorf("discover programs: %w", err)
		}

		result := make([]ProgramSpec, 0, len(discovered))
		for _, d := range discovered {
			globalData := opts.GlobalData
			result = append(result, ProgramSpec{
				Name:       d.Name,
				Type:       d.Type,
				AttachFunc: d.AttachFunc,
				GlobalData: globalData,
			})
		}
		m.logger.InfoContext(ctx, "auto-discovered programs",
			"count", len(result))
		return result, nil
	}

	programNames := make([]string, len(programs))
	for i, p := range programs {
		programNames[i] = p.Name
	}
	if err := m.programDiscoverer.ValidatePrograms(objectPath, programNames); err != nil {
		return nil, err
	}
	return programs, nil
}

// buildLoadSpecs constructs validated LoadSpecs from the resolved
// programs. Global data and image provenance are applied; map sharing
// is deferred to Load execution time.
func buildLoadSpecs(
	objectPath string,
	programs []ProgramSpec,
	opts LoadOpts,
	pulled *platform.PulledImage,
) ([]bpfman.LoadSpec, error) {
	specs := make([]bpfman.LoadSpec, 0, len(programs))
	for _, prog := range programs {
		var spec bpfman.LoadSpec
		var err error
		if prog.Type.RequiresAttachFunc() {
			spec, err = bpfman.NewAttachLoadSpec(objectPath, prog.Name, prog.Type, prog.AttachFunc)
		} else {
			spec, err = bpfman.NewLoadSpec(objectPath, prog.Name, prog.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid load spec for %q: %w", prog.Name, err)
		}

		globalData := opts.GlobalData
		if prog.GlobalData != nil {
			globalData = prog.GlobalData
		}
		if globalData != nil {
			spec = spec.WithGlobalData(globalData)
		}

		if prog.MapOwnerID != 0 {
			spec = spec.WithMapOwnerID(prog.MapOwnerID)
		}

		if pulled != nil {
			spec = spec.WithImageProvenance(pulled.URL, pulled.Digest, pulled.PullPolicy)
		}

		specs = append(specs, spec)
	}
	return specs, nil
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
