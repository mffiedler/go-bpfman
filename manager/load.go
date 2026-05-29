package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
	"github.com/frobware/go-bpfman/platform"
)

// loadedKey is the binding key for the per-program load plan.
var loadedKey = operation.NewKey[bpfman.LoadOutput]("loaded")

// ApplicationMetadataKey is the metadata key used to group loaded
// programs by application name.
const ApplicationMetadataKey = "bpfman.io/application"

// ErrImagePullerNotConfigured is returned when an image LoadSource is
// requested but the manager was created without an OCI image puller.
var ErrImagePullerNotConfigured = errors.New("OCI image loading not configured")

// PullBytecode pre-pulls an OCI image to the local cache without
// loading any programs.
func (m *Manager) PullBytecode(ctx context.Context, ref platform.ImageRef) (platform.PulledImage, error) {
	if m.imagePuller == nil {
		return platform.PulledImage{}, ErrImagePullerNotConfigured
	}
	return m.imagePuller.Pull(ctx, ref)
}

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
	rt fs.Bytecode,
	now time.Time,
) bpfman.ProgramRecord {
	var mapOwnerID *kernel.ProgramID
	if ownerID := spec.MapOwnerID(); ownerID != 0 {
		mapOwnerID = &ownerID
	}
	return bpfman.ProgramRecord{
		ProgramID: loaded.Program.ID,
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
			MapsDir:    loaded.MapsDir,
			MapOwnerID: mapOwnerID,
		},
		Meta: bpfman.ProgramMeta{
			Name:     spec.ProgramName(),
			Owner:    opts.Owner,
			Metadata: opts.UserMetadata,
		},
		// A freshly-loaded program has not been updated;
		// UpdatedAt stays nil at the type level so the JSON
		// surfaces it as null, distinct from CreatedAt. Operations
		// that legitimately mutate the record assign UpdatedAt
		// before persisting.
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
	MapOwnerID kernel.ProgramID  // explicit external map owner (0 = none)
}

// LoadOpts configures a Load operation.
type LoadOpts struct {
	UserMetadata map[string]string
	GlobalData   map[string][]byte // batch-level, overridden per-program
	Owner        string
	ShareMaps    bool // first program owns maps, subsequent auto-share
}

// LoadRequest carries an already parsed load request across a
// front-end boundary.
type LoadRequest struct {
	Source   LoadSource
	Programs []ProgramSpec
	Opts     LoadOpts
}

// LoadRequestOpts configures construction of a LoadRequest from
// front-end inputs.
type LoadRequestOpts struct {
	UserMetadata map[string]string
	GlobalData   map[string][]byte
	Application  string
	MapOwnerID   kernel.ProgramID
	Owner        string
	ShareMaps    bool
}

// NewLoadRequest applies manager-owned load defaults and returns the
// request value consumed by LoadRequest.
func NewLoadRequest(source LoadSource, programs []ProgramSpec, opts LoadRequestOpts) LoadRequest {
	return LoadRequest{
		Source:   source,
		Programs: applyLoadRequestMapOwner(programs, opts.MapOwnerID),
		Opts: LoadOpts{
			UserMetadata: loadRequestMetadata(opts.UserMetadata, opts.Application),
			GlobalData:   opts.GlobalData,
			Owner:        opts.Owner,
			ShareMaps:    opts.ShareMaps,
		},
	}
}

func loadRequestMetadata(metadata map[string]string, application string) map[string]string {
	if len(metadata) == 0 && application == "" {
		return nil
	}

	out := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	if application != "" {
		out[ApplicationMetadataKey] = application
	}
	return out
}

func applyLoadRequestMapOwner(programs []ProgramSpec, mapOwnerID kernel.ProgramID) []ProgramSpec {
	out := append([]ProgramSpec(nil), programs...)
	if mapOwnerID == 0 {
		return out
	}
	for i := range out {
		if out[i].MapOwnerID == 0 {
			out[i].MapOwnerID = mapOwnerID
		}
	}
	return out
}

// LoadRequest loads the programs described by req.
func (m *Manager) LoadRequest(ctx context.Context, req LoadRequest) ([]bpfman.Program, error) {
	return m.Load(ctx, req.Source, req.Programs, req.Opts)
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
	programs = normalizeLoadProgramSpecs(programs, opts)

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

	// Reap store records for programs whose kernel object is gone
	// before loading the new generation. Such rows are left behind by
	// a prior generation that died without an Unload (daemon restart,
	// external unload, or a ClusterBpfApplication deleted and
	// recreated) and they poison later TCX attaches with ENOENT (see
	// reapDeadProgramRecords). Best-effort: a reap failure must not
	// block the load.
	if err := m.reapDeadProgramRecords(ctx); err != nil {
		m.logger.WarnContext(ctx, "reaping dead program records before load failed (continuing)", "error", err)
	}

	// Decide whether the load needs the cross-process writer
	// lock. Loads of objects without LIBBPF_PIN_BY_NAME maps touch
	// no shared bpffs state and remain lockless. Loads of objects
	// with pinByName maps share a name-derived bpffs pin path
	// across every loader, so we serialise the per-program loop +
	// Phase B against other mutations (especially unloads of the
	// same shared map) under the writer flock. The image-pull
	// step above already ran lockless; the lock only wraps the
	// post-source work.
	needsLock := false
	for _, spec := range specs {
		has, err := m.kernel.HasPinByName(spec)
		if err != nil {
			return nil, fmt.Errorf("pre-check pinByName: %w", err)
		}
		if has {
			needsLock = true
			break
		}
	}

	body := func() ([]bpfman.Program, error) {
		return m.loadBody(ctx, specs, opts)
	}

	if !needsLock {
		return body()
	}

	var loaded []bpfman.Program
	runErr := lock.Run(ctx, m.rt.Layout().LockPath(), func(_ context.Context, _ lock.WriterScope) error {
		var lerr error
		loaded, lerr = body()
		return lerr
	})
	return loaded, runErr
}

func normalizeLoadProgramSpecs(programs []ProgramSpec, opts LoadOpts) []ProgramSpec {
	if len(programs) == 0 || len(opts.UserMetadata) == 0 {
		return programs
	}

	out := make([]ProgramSpec, len(programs))
	copy(out, programs)
	for i, spec := range out {
		out[i].Type = resolveActualProgramType(spec.Type, spec.Name, opts.UserMetadata)
	}
	return out
}

func resolveActualProgramType(programType bpfman.ProgramType, programName string, metadata map[string]string) bpfman.ProgramType {
	if metadata == nil {
		return programType
	}

	key := actualTypeMetadataKey(programName)
	if actualTypeStr, ok := metadata[key]; ok {
		if actualType, err := bpfman.ParseProgramType(actualTypeStr); err == nil {
			return actualType
		}
	}

	return programType
}

func actualTypeMetadataKey(programName string) string {
	return "bpfman.io/actual-type:" + programName
}

// reapDeadProgramRecords removes store records for managed programs
// whose kernel object no longer exists. Such rows are left behind when
// a prior generation's programs die without an Unload -- a daemon
// restart, an external unload, or a ClusterBpfApplication deleted and
// recreated -- and they poison later attaches: the TCX attach order
// anchors a new program against an existing one by kernel program ID,
// and an anchor pointing at a dead program makes the kernel reject the
// attach with ENOENT (see attachTCX). Nothing else prunes them --
// PlanFromObservation deliberately leaves store-managed rows "for the
// next bpfman invocation to reconcile", and that reconcile lived
// nowhere until here.
//
// The load path stays thin: observe an immutable snapshot, produce a
// pure plan from that snapshot, then interpret the plan against the
// store/bpffs. Observation failure aborts the reap before any
// destructive action, because a failed kernel enumeration cannot
// distinguish "dead" from "could not inspect".
func (m *Manager) reapDeadProgramRecords(ctx context.Context) error {
	return lock.Run(ctx, m.rt.Layout().LockPath(), func(_ context.Context, _ lock.WriterScope) error {
		snap, err := m.observeDeadProgramReapSnapshot(ctx)
		if err != nil {
			return err
		}
		return m.executeReapPlan(ctx, planReap(snap))
	})
}

type reapSnapshot struct {
	storePrograms     map[kernel.ProgramID]bpfman.ProgramRecord
	liveKernelProgram map[kernel.ProgramID]bool
}

func (m *Manager) observeDeadProgramReapSnapshot(ctx context.Context) (reapSnapshot, error) {
	progs, err := m.store.List(ctx)
	if err != nil {
		return reapSnapshot{}, fmt.Errorf("list store programs: %w", err)
	}

	live := make(map[kernel.ProgramID]bool)
	for kp, err := range m.kernel.Programs(ctx) {
		if err != nil {
			return reapSnapshot{}, fmt.Errorf("enumerate kernel programs: %w", err)
		}
		live[kp.ID] = true
	}

	return reapSnapshot{
		storePrograms:     progs,
		liveKernelProgram: live,
	}, nil
}

type reapActionKind int

const (
	reapDeadProgramRecord reapActionKind = iota
)

type reapAction struct {
	Kind      reapActionKind
	ProgramID kernel.ProgramID
}

func (m *Manager) executeReapPlan(ctx context.Context, plan []reapAction) error {
	for _, action := range plan {
		switch action.Kind {
		case reapDeadProgramRecord:
			m.executeReapDeadProgramRecord(ctx, action.ProgramID)
		}
	}
	return nil
}

func (m *Manager) executeReapDeadProgramRecord(ctx context.Context, id kernel.ProgramID) {
	// Mirror unload's post-detach cleanup -- the kernel objects are
	// already gone, so there is no detach to do -- releasing the
	// shared-map pin references and the bpffs/bytecode residue before
	// the row (and the shared_map_pins reference data it carries) is
	// dropped. Each step is best-effort.
	if err := m.removeProgramLinksDir(m.rt.BPFFS().LinkPinDir(id)); err != nil {
		m.logger.WarnContext(ctx, "reap: remove links dir", "program_id", id, "error", err)
	}
	if err := m.removeProgramMapsPins(ctx, m.rt.BPFFS().MapPinDir(id)); err != nil {
		m.logger.WarnContext(ctx, "reap: remove map pins", "program_id", id, "error", err)
	}
	if err := m.cleanupSharedMapPins(ctx, id); err != nil {
		m.logger.WarnContext(ctx, "reap: cleanup shared map pins", "program_id", id, "error", err)
	}
	if err := m.store.Delete(ctx, id); err != nil {
		m.logger.WarnContext(ctx, "reap: delete dead program record failed",
			"program_id", id, "error", err)
		return
	}
	if err := m.removeProgramBytecodeDir(id); err != nil {
		m.logger.WarnContext(ctx, "reap: remove bytecode dir", "program_id", id, "error", err)
	}
	m.logger.InfoContext(ctx, "reaped dead program record absent from kernel",
		"program_id", id)
}

// planReap decides which dead program records to delete and in what
// order. It is pure: the observed store/kernel snapshot goes in, an
// ordered slice of actions comes out, with no IO. The map-sharing
// dependency is read from each record's MapOwnerID rather than queried,
// so the dependents-first ordering can be decided -- and tested -- on
// plain data.
//
// A program may be deleted only once nothing still records it as map
// owner: managed_programs.map_owner_id is ON DELETE RESTRICT, and a
// live dependent's shared maps must not be pulled out from under it.
// deps counts every program (live or dead) that names each program as
// its owner; a dead program is emitted only when its count reaches
// zero, and emitting it decrements its own owner's count, which can
// unblock that owner on a later pass. A dead owner whose dependent is
// still live therefore stays put -- correctly. Iteration is over
// sorted IDs so the plan is deterministic.
func planReap(snap reapSnapshot) []reapAction {
	progs := snap.storePrograms
	dead := make(map[kernel.ProgramID]bool)
	for id := range progs {
		if !snap.liveKernelProgram[id] {
			dead[id] = true
		}
	}

	deps := make(map[kernel.ProgramID]int, len(progs))
	for _, rec := range progs {
		if owner := rec.Handles.MapOwnerID; owner != nil {
			deps[*owner]++
		}
	}

	deadIDs := make([]kernel.ProgramID, 0, len(dead))
	for id := range dead {
		deadIDs = append(deadIDs, id)
	}
	slices.Sort(deadIDs)

	removed := make(map[kernel.ProgramID]bool, len(dead))
	plan := make([]reapAction, 0, len(dead))
	for {
		progress := false
		for _, id := range deadIDs {
			if removed[id] || deps[id] > 0 {
				continue
			}
			plan = append(plan, reapAction{
				Kind:      reapDeadProgramRecord,
				ProgramID: id,
			})
			removed[id] = true
			if owner := progs[id].Handles.MapOwnerID; owner != nil {
				deps[*owner]--
			}
			progress = true
		}
		if !progress {
			break
		}
	}
	return plan
}

// loadBody runs the per-program load loop and the batched Phase B
// store commit. Caller decides whether to wrap this in the
// cross-process writer lock; the body itself is lock-agnostic.
func (m *Manager) loadBody(ctx context.Context, specs []bpfman.LoadSpec, opts LoadOpts) ([]bpfman.Program, error) {
	rt := m.rt.Bytecode()
	bpffs := m.rt.BPFFS()
	perProgOpts := loadOpts{
		UserMetadata: opts.UserMetadata,
		Owner:        opts.Owner,
	}

	// Phase A: per-program kernel + filesystem work. Lockless;
	// see docs/PLAN-load-lockless.md. The kernel allocates each
	// program a unique id, the bytecode directory is namespaced
	// by that id, and no shared state is mutated -- so two
	// concurrent loads cannot collide.
	type loadedItem struct {
		out    bpfman.LoadOutput
		spec   bpfman.LoadSpec
		record bpfman.ProgramRecord
		now    time.Time
	}
	var loaded []bpfman.Program
	var items []loadedItem
	// Cleanup invariant: in v2 the rollback can fire either after the
	// per-program kernel/fs work succeeded but before the phase-B
	// commit transaction, or during phase B when the commit fails.
	// Either way no sqlite row was persisted for any of these programs
	// (the transaction either rolled back or never started), so the
	// unload runs with persisted=false to skip the store delete and
	// avoid a misleading "record not found" error.
	cleanupLoaded := func() {
		for j := len(loaded) - 1; j >= 0; j-- {
			r := loaded[j].Record
			if uerr := m.unload(ctx, r.ProgramID, r.Meta.Name, nil, false); uerr != nil {
				m.logger.Error("failed to unload during batch rollback",
					"program_id", r.ProgramID, "error", uerr)
			}
		}
	}

	for i, spec := range specs {
		if opts.ShareMaps && i > 0 && spec.MapOwnerID() == 0 {
			spec = spec.WithMapOwnerID(loaded[0].Record.ProgramID)
		}

		// Pin the timestamp to UTC and second precision so the
		// in-memory record matches what the sqlite store
		// persists (the Save path formats UTC at time.RFC3339).
		// Without this, Load returns local-tz ns-precise time
		// while Get reads back UTC second-precise, surfacing as
		// a spurious Load/Get asymmetry on every script.
		now := time.Now().UTC().Truncate(time.Second)
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

		// Derive the path strings the wire shape exposes. Pure
		// construction from the program ID and runtime layout
		// helpers, no syscalls: the values are the canonical
		// locations bpfman would write to or read from. Callers
		// that want "does this currently exist" stat themselves.
		programID := lo.Program.ID
		mapOwner := programID
		if record.Handles.MapOwnerID != nil {
			mapOwner = *record.Handles.MapOwnerID
		}

		loaded = append(loaded, bpfman.Program{
			Record: record,
			Status: bpfman.ProgramStatus{
				Kernel:   lo.Program,
				ProgPin:  bpffs.ProgPinPath(programID),
				MapDir:   bpffs.MapPinDir(mapOwner),
				LinkDir:  bpffs.LinkPinDir(programID),
				Bytecode: rt.ProgramBytecodePath(programID),
				Maps:     bpfman.ToMapStatus(kernelMaps),
			},
		})
		items = append(items, loadedItem{out: lo, spec: spec, record: record, now: now})
	}

	// Phase B: single sqlite transaction commits the whole batch.
	// No flock: load is lockless by construction
	// (docs/PLAN-load-lockless.md). The kernel allocates each
	// program a unique id, the bytecode dir is namespaced by that
	// id, and the primary-key constraint on `programs` makes the
	// commit non-conflicting across concurrent loads. Sqlite's
	// own writer mutex serialises the commits themselves.
	//
	// We call platform.Store methods directly rather than going
	// through the executor because the executor's SaveProgram and
	// SaveSharedMapPins handlers each open their own internal
	// transaction, which would nest here.
	if err := m.store.RunInTransaction(ctx, "load", func(tx platform.Store) error {
		for _, it := range items {
			if _, err := tx.Get(ctx, it.out.Program.ID); err == nil {
				return fmt.Errorf("program %d already exists in database", it.out.Program.ID)
			} else if !errors.Is(err, platform.ErrRecordNotFound) {
				return fmt.Errorf("check existing program %d: %w", it.out.Program.ID, err)
			}
			if err := tx.Save(ctx, it.out.Program.ID, it.record); err != nil {
				return fmt.Errorf("save program %d: %w", it.out.Program.ID, err)
			}
			if len(it.out.SharedMapNames) > 0 {
				if err := tx.SaveSharedMapPins(ctx, it.out.Program.ID, it.out.SharedMapNames); err != nil {
					return fmt.Errorf("save shared map pins for program %d: %w", it.out.Program.ID, err)
				}
			}
		}
		return nil
	}); err != nil {
		cleanupLoaded()
		return nil, err
	}

	return loaded, nil
}

// loadPlan builds the per-program plan: kernel-load and
// fs-publish. The remaining sqlite work (db-consistency-check,
// store-save, save-shared-maps) is batched into a single
// transaction at the end of the load, see Manager.Load's
// phase B.
func (m *Manager) loadPlan(spec bpfman.LoadSpec, opts loadOpts, now time.Time) operation.Plan {
	_ = opts // reserved: phase B builds program records from the spec + load output directly.
	programName := spec.ProgramName()
	rt := m.rt.Bytecode()

	return operation.Build(
		operation.Produce(loadedKey, programName,
			func(ctx context.Context, exec action.ExecutorWithResult, b *operation.Bindings) (bpfman.LoadOutput, error) {
				loaded, err := action.Produce[bpfman.LoadOutput](ctx, exec, action.LoadProgram{
					Spec:  spec,
					BPFFS: m.rt.BPFFS(),
				})
				if err != nil {
					return bpfman.LoadOutput{}, fmt.Errorf("load program %s: %w", programName, err)
				}
				m.logger.InfoContext(ctx, "loaded program",
					"name", programName,
					"program_id", loaded.Program.ID,
					"pin_path", loaded.PinPath)
				return loaded, nil
			},
			operation.UndoFrom(func(b *operation.Bindings) []action.Action {
				l := operation.Get(b, loadedKey)
				return []action.Action{
					action.UnloadProgram{PinPath: l.PinPath},
					action.RemoveMapsPins{PinPath: l.MapsDir.String()},
				}
			}),
		),

		operation.Do("fs-publish", programName,
			func(ctx context.Context, exec action.ExecutorWithResult, b *operation.Bindings) error {
				l := operation.Get(b, loadedKey)
				return exec.Execute(ctx, action.PublishBytecode{
					ProgramID:  l.Program.ID,
					SourcePath: spec.ObjectPath(),
					Provenance: fs.Provenance{
						Version:     1,
						ProgramID:   l.Program.ID,
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
					action.RemoveProgramDir{Path: rt.ProgramDir(l.Program.ID)},
				}
			}),
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
			return "", nil, ErrImagePullerNotConfigured
		}
		if !source.Image.PullPolicy.Valid() {
			return "", nil, fmt.Errorf("image pull policy is required")
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
