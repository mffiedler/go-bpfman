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

	var loaded []bpfman.Program
	for i, spec := range specs {
		if opts.ShareMaps && i > 0 && spec.MapOwnerID() == 0 {
			spec = spec.WithMapOwnerID(loaded[0].Record.KernelID)
		}
		prog, err := m.load(ctx, spec, loadOpts{
			UserMetadata: opts.UserMetadata,
			Owner:        opts.Owner,
		})
		if err != nil {
			m.cleanupLoaded(ctx, loaded)
			return nil, err
		}
		loaded = append(loaded, prog)
	}
	return loaded, nil
}

// loadPlan builds the per-program plan: kernel-load, db-check,
// fs-publish, store-save.
func (m *Manager) loadPlan(spec bpfman.LoadSpec, opts loadOpts, now time.Time) operation.Plan {
	programName := spec.ProgramName()
	rt := m.fsctx.BytecodeFS()

	return operation.Build(
		operation.Produce(loadedKey, outcome.StepKindKernelLoad, programName,
			func(ctx context.Context, b *operation.Bindings) (bpfman.LoadOutput, error) {
				loaded, err := m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
				if err != nil {
					return bpfman.LoadOutput{}, fmt.Errorf("load program %s: %w", programName, err)
				}
				m.logger.InfoContext(ctx, "loaded program",
					"name", programName,
					"kernel_id", loaded.Program.ID,
					"pin_path", loaded.PinPath)
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
		),

		operation.Do("db-check", outcome.StepKindPreflight, programName,
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

		operation.Do("fs-publish", outcome.StepKindFSPublish, programName,
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
		),

		operation.Do("store-save", outcome.StepKindStoreSaveProgram, programName,
			func(ctx context.Context, b *operation.Bindings) error {
				l := operation.Get(b, loadedKey)
				record := buildProgramRecord(spec, l, opts, rt, now)
				return m.store.RunInTransaction(ctx, func(tx platform.Store) error {
					return tx.Save(ctx, l.Program.ID, record)
				})
			},
		),
	)
}

// load executes a single-program load plan and returns the result.
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec, opts loadOpts) (bpfman.Program, error) {
	now := time.Now()
	rt := m.fsctx.BytecodeFS()

	b, err := operation.Run(ctx, m.beginOp, m.executor, m.loadPlan(spec, opts, now))
	if err != nil {
		return bpfman.Program{}, wrapOpErr(err)
	}

	loaded := operation.Get(b, loadedKey)
	record := buildProgramRecord(spec, loaded, opts, rt, now)

	var kernelMaps []kernel.Map
	for _, mapID := range loaded.Program.MapIDs {
		km, err := m.kernel.GetMapByID(ctx, mapID)
		if err == nil {
			kernelMaps = append(kernelMaps, km)
		}
	}

	return bpfman.Program{
		Record: record,
		Status: bpfman.ProgramStatus{
			Kernel:      loaded.Program,
			PinPresent:  true,
			MapsPresent: len(kernelMaps) > 0,
			Maps:        kernelMaps,
		},
	}, nil
}

// cleanupLoaded unloads previously loaded programs in reverse order.
// Each program has a completed store-save, so Unload can find and
// fully clean it. Errors are logged but not returned.
func (m *Manager) cleanupLoaded(ctx context.Context, programs []bpfman.Program) {
	for i := len(programs) - 1; i >= 0; i-- {
		if err := m.Unload(ctx, programs[i].Record.KernelID); err != nil {
			m.logger.Error("failed to unload during batch rollback",
				"kernel_id", programs[i].Record.KernelID, "error", err)
		}
	}
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
