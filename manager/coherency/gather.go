package coherency

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// ObservedState is a point-in-time snapshot of all three state
// sources with pre-built correlated views. Rules consume this;
// they never reach back into raw maps. All I/O happens during
// GatherState; view builders and rules are pure joins over facts.
type ObservedState struct {
	// Correlated world from inspect.Snapshot.
	world *inspect.World

	// Kernel-alive index derived from World.
	kernelAlive map[kernel.ProgramID]bool

	// Filesystem facts: directory scans.
	// These are built during gather from bpffs directory listings.
	orphans []FsOrphan

	// Filesystem facts: dispatcher revision directory link counts.
	// Key is dispatcherKey(type, nsid, ifindex).
	fsDispatcherLinkCount map[string]int

	// Store-derived facts: dispatcher extension link counts.
	// Key is dispatcher kernel program ID.
	dbDispatcherExtCount map[kernel.ProgramID]int

	// Netlink-derived facts: TC filter existence.
	// Key is dispatcherKey(type, nsid, ifindex).
	tcFilterOK map[string]bool

	// Runtime context (immutable after gather).
	layout fs.Layout

	// Cached views (built lazily on first access, pure joins).
	programs    []ProgramState
	links       []LinkState
	dispatchers []DispatcherState
}

// GatherState builds an ObservedState by scanning all three sources.
// All I/O happens here; the returned state is a pure fact store.
func GatherState(ctx context.Context, store platform.Store, kops platform.KernelOperations, layout fs.Layout) (*ObservedState, error) {
	s := &ObservedState{
		kernelAlive:           make(map[kernel.ProgramID]bool),
		fsDispatcherLinkCount: make(map[string]int),
		dbDispatcherExtCount:  make(map[kernel.ProgramID]int),
		tcFilterOK:            make(map[string]bool),
		layout:                layout,
	}

	// ----------------------------------------------------------------
	// Phase 1: Correlated world via inspect.Snapshot
	// ----------------------------------------------------------------

	scanner := layout.BPFFS().Scanner()
	world, err := inspect.Snapshot(ctx, store, kops, scanner)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	s.world = world

	// ----------------------------------------------------------------
	// Phase 2: Build indexes from World
	// ----------------------------------------------------------------

	// Kernel-alive index: any program present in the kernel.
	for _, p := range world.Programs {
		if p.Presence.InKernel {
			s.kernelAlive[p.ProgramID] = true
		}
	}

	// DB index sets for orphan detection.
	dbProgPins := make(map[string]bool)
	dbProgIDs := make(map[kernel.ProgramID]bool)
	dbDispatcherKeys := make(map[string]bool)

	for _, p := range world.ManagedPrograms() {
		dbProgIDs[p.ProgramID] = true
		if p.Managed != nil && p.Managed.Handles.PinPath != "" {
			dbProgPins[p.Managed.Handles.PinPath] = true
		}
	}
	for _, d := range world.ManagedDispatchers() {
		dt, err := dispatcher.ParseDispatcherType(d.DispType)
		if err != nil {
			return nil, fmt.Errorf("parse dispatcher type %q: %w", d.DispType, err)
		}
		dbDispatcherKeys[dispatcherKey(dt, d.Nsid, d.Ifindex)] = true
	}

	// ----------------------------------------------------------------
	// Phase 3: Store-derived facts (dispatcher extension counts)
	// ----------------------------------------------------------------

	for _, d := range world.ManagedDispatchers() {
		if d.Managed == nil {
			continue
		}
		s.dbDispatcherExtCount[d.Managed.Runtime.ProgramID] = d.Managed.MemberCount
	}

	// ----------------------------------------------------------------
	// Phase 4: Netlink facts (TC filter checks)
	// ----------------------------------------------------------------

	for _, d := range world.ManagedDispatchers() {
		if d.Managed == nil {
			continue
		}
		dt := d.Managed.Key.Type
		if dt != dispatcher.DispatcherTypeTCIngress && dt != dispatcher.DispatcherTypeTCEgress {
			continue
		}
		if d.Managed.Runtime.FilterPriority == nil || *d.Managed.Runtime.FilterPriority == 0 {
			continue
		}
		key := dispatcherKey(dt, d.Managed.Key.Nsid, d.Managed.Key.Ifindex)
		parent := dispatcher.TCParentHandle(dt)
		_, err := kops.FindTCFilterHandle(ctx, int(d.Managed.Key.Ifindex), parent, *d.Managed.Runtime.FilterPriority)
		s.tcFilterOK[key] = (err == nil)
	}

	// ----------------------------------------------------------------
	// Phase 5: Filesystem orphan detection via scanner.Scan
	// ----------------------------------------------------------------

	s.orphans = make([]FsOrphan, 0)

	fsState, err := scanner.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan bpffs: %w", err)
	}

	// Scan prog pins for orphans.
	for _, pin := range fsState.ProgPins {
		if dbProgPins[pin.Path] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path:      pin.Path,
			ProgramID: pin.ProgramID,
			Kind:      OrphanProgPin,
		})
	}

	// Scan link directories for orphans.
	for _, dir := range fsState.LinkDirs {
		if dbProgIDs[dir.ProgramID] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path:      dir.Path,
			ProgramID: dir.ProgramID,
			Kind:      OrphanLinkDir,
		})
	}

	// Scan map directories for orphans.
	for _, dir := range fsState.MapDirs {
		if dbProgIDs[dir.ProgramID] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path:      dir.Path,
			ProgramID: dir.ProgramID,
			Kind:      OrphanMapDir,
		})
	}

	// Scan dispatcher directories for orphans and record link counts
	// for non-orphans.
	for _, d := range fsState.DispatcherDirs {
		dt, err := dispatcher.ParseDispatcherType(d.DispType)
		if err != nil {
			return nil, fmt.Errorf("parse dispatcher type %q from fs: %w", d.DispType, err)
		}
		key := dispatcherKey(dt, d.Nsid, d.Ifindex)
		if !dbDispatcherKeys[key] {
			s.orphans = append(s.orphans, FsOrphan{
				Path: d.Path,
				Kind: OrphanDispatcherDir,
			})
			continue
		}
		s.fsDispatcherLinkCount[key] = d.LinkCount
	}

	// Scan dispatcher link pins for orphans.
	for _, pin := range fsState.DispatcherLinkPins {
		dt, err := dispatcher.ParseDispatcherType(pin.DispType)
		if err != nil {
			return nil, fmt.Errorf("parse dispatcher type %q from fs link pin: %w", pin.DispType, err)
		}
		key := dispatcherKey(dt, pin.Nsid, pin.Ifindex)
		if dbDispatcherKeys[key] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path: pin.Path,
			Kind: OrphanDispatcherLink,
		})
	}

	// ----------------------------------------------------------------
	// Phase 5a: Scan <base>/programs/ for orphan program dirs
	// ----------------------------------------------------------------

	rt := layout.Bytecode()
	programDirs, err := rt.ScanProgramDirs()
	if err != nil {
		return nil, fmt.Errorf("scan program dirs: %w", err)
	}
	for _, pde := range programDirs {
		if pde.Numeric {
			if !dbProgIDs[pde.ProgramID] {
				s.orphans = append(s.orphans, FsOrphan{
					Path:      pde.Path,
					ProgramID: pde.ProgramID,
					Kind:      OrphanProgramDir,
				})
			}
		} else {
			s.orphans = append(s.orphans, FsOrphan{
				Path: pde.Path,
				Kind: OrphanProgramDirUnk,
			})
		}
	}

	// ----------------------------------------------------------------
	// Phase 5b: Scan <base>/.staging/ for orphan staging dirs
	// ----------------------------------------------------------------

	stagingDirs, err := rt.ScanStagingDirs()
	if err != nil {
		return nil, fmt.Errorf("scan staging dirs: %w", err)
	}
	for _, path := range stagingDirs {
		s.orphans = append(s.orphans, FsOrphan{
			Path: path,
			Kind: OrphanStagingDir,
		})
	}

	return s, nil
}

// --------------------------------------------------------------------
// View builders: construct correlated tuples from World facts.
// All joins happen here. Rules never touch raw maps.
// --------------------------------------------------------------------

// Programs returns one ProgramState per managed program, correlated
// with kernel and filesystem state. Derived from the World snapshot.
func (s *ObservedState) Programs() []ProgramState {
	if s.programs != nil {
		return s.programs
	}
	for _, p := range s.world.ManagedPrograms() {
		ps := ProgramState{
			ProgramID: p.ProgramID,
			DB:        p.Managed,
			Kernel:    p.Presence.InKernel,
			PinPath:   p.PinPath(),
		}
		// For managed programs, inspect always checks the pin
		// path, so Presence.InFS is definitive.
		if p.Managed != nil && p.Managed.Handles.PinPath != "" {
			exists := p.Presence.InFS
			ps.PinExist = &exists
		}
		s.programs = append(s.programs, ps)
	}
	return s.programs
}

// Links returns one LinkState per managed link, correlated with
// kernel state and filesystem. Derived from the World snapshot.
func (s *ObservedState) Links() []LinkState {
	if s.links != nil {
		return s.links
	}
	for _, lr := range s.world.ManagedLinks() {
		synthetic := lr.IsSynthetic()
		ls := LinkState{
			DB:        lr.Managed,
			Synthetic: synthetic,
			Kernel:    lr.Presence.InKernel,
		}
		if lr.HasPin() && !synthetic {
			exists := lr.Presence.InFS
			ls.PinExist = &exists
		}
		s.links = append(s.links, ls)
	}
	return s.links
}

// Dispatchers returns one DispatcherState per managed dispatcher,
// correlated with kernel, filesystem, and extension link counts.
// Derived from the World snapshot plus additional gathered facts.
func (s *ObservedState) Dispatchers() []DispatcherState {
	if s.dispatchers != nil {
		return s.dispatchers
	}
	bpffs := s.layout.BPFFS()
	for _, dr := range s.world.ManagedDispatchers() {
		if dr.Managed == nil {
			continue
		}
		summary := dr.Managed
		dt := summary.Key.Type
		nsid := summary.Key.Nsid
		ifindex := summary.Key.Ifindex

		// Convert summary to dispatcher.State for rules
		// compatibility. Rules access d.DB.Type, d.DB.Nsid,
		// etc. directly.
		state := &dispatcher.State{
			Type:      dt,
			Nsid:      nsid,
			Ifindex:   ifindex,
			Revision:  summary.Revision,
			ProgramID: summary.Runtime.ProgramID,
		}
		if summary.Runtime.LinkID != nil {
			state.LinkID = *summary.Runtime.LinkID
		}
		if summary.Runtime.FilterPriority != nil {
			state.Priority = *summary.Runtime.FilterPriority
		}

		key := dispatcherKey(dt, nsid, ifindex)
		revDir := bpffs.DispatcherRevisionDir(dt, nsid, ifindex, summary.Revision)
		progPin := bpffs.DispatcherProgPath(dt, nsid, ifindex, summary.Revision)

		ds := DispatcherState{
			DB:         state,
			KernelProg: dr.ProgPresence.InKernel,
			RevDir:     revDir,
			ProgPin:    progPin,
			LinkCount:  -1,
		}

		// Prog pin existence from World presence.
		exists := dr.ProgPresence.InFS
		ds.ProgPinExist = &exists

		// XDP link checks from World presence.
		if dt == dispatcher.DispatcherTypeXDP {
			ds.KernelLink = dr.LinkPresence.InKernel
			linkExists := dr.LinkPresence.InFS
			ds.LinkPinExist = &linkExists
			ds.LinkPin = bpffs.DispatcherLinkPath(dt, nsid, ifindex)
		}

		// TC filter check from gathered facts.
		if dt == dispatcher.DispatcherTypeTCIngress || dt == dispatcher.DispatcherTypeTCEgress {
			if state.Priority > 0 {
				if ok, found := s.tcFilterOK[key]; found {
					ds.TCFilterOK = &ok
				}
			}
		}

		// Extension link count from gathered facts.
		if count, found := s.dbDispatcherExtCount[state.ProgramID]; found {
			ds.LinkCount = count
		}

		s.dispatchers = append(s.dispatchers, ds)
	}
	return s.dispatchers
}

// OrphanFsEntries returns filesystem entries under the bpffs tree
// that have no corresponding DB record. The list is pre-built during
// GatherState; this method is a pure accessor.
func (s *ObservedState) OrphanFsEntries() []FsOrphan {
	return s.orphans
}

// DispatcherFsLinkCount returns the count of link_* files in the
// dispatcher's revision directory. The count is pre-computed during
// GatherState; this method is a pure lookup. Returns -1 if unknown.
func (s *ObservedState) DispatcherFsLinkCount(ds DispatcherState) int {
	if ds.DB == nil {
		return -1
	}
	key := dispatcherKey(ds.DB.Type, ds.DB.Nsid, ds.DB.Ifindex)
	if count, ok := s.fsDispatcherLinkCount[key]; ok {
		return count
	}
	return -1
}

// KernelAlive reports whether a kernel program ID is alive.
func (s *ObservedState) KernelAlive(programID kernel.ProgramID) bool {
	return s.kernelAlive[programID]
}

// LiveOrphans returns the count of orphan program pins where the
// kernel program is still alive. These are programs that bpfman
// originally loaded (pinned under bpfman's bpffs root) but no longer
// tracks in its database, typically after a database wipe while pins
// survived. Standard GC leaves these untouched because removing the
// pin would unload a running program; use --prune to remove them.
func (s *ObservedState) LiveOrphans() int {
	count := 0
	for _, o := range s.orphans {
		if o.Kind == OrphanProgPin && o.ProgramID != 0 && s.kernelAlive[o.ProgramID] {
			count++
		}
	}
	return count
}

func dispatcherKey(dt dispatcher.DispatcherType, nsid uint64, ifindex uint32) string {
	return fmt.Sprintf("%s/%d/%d", dt, nsid, ifindex)
}
