package coherency

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// ObservedState is a point-in-time snapshot of all three state
// sources with pre-built correlated views. Rules consume this;
// they never reach back into raw maps. All I/O happens during
// GatherState; view builders and rules are pure joins over facts.
type ObservedState struct {
	// DB facts.
	dbPrograms    map[kernel.ProgramID]bpfman.ProgramRecord
	dbLinks       []bpfman.LinkRecord
	dbDispatchers []dispatcher.State

	// Kernel facts.
	kernelProgs          map[kernel.ProgramID]bool
	kernelLinks          map[kernel.LinkID]bool
	kernelProgEnumErrors int
	kernelLinkEnumErrors int

	// Filesystem facts: pin existence by path.
	fsPinExists map[string]bool

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

	// Indexes for join operations.
	dbProgPins       map[string]bool
	dbProgIDs        map[kernel.ProgramID]bool
	dbDispatcherKeys map[string]bool

	// Runtime context (immutable after gather).
	layout bpfmanfs.FSLayout

	// Mutation capability for GC operations only.
	// Not used during rule evaluation.
	deleteDispatcher func(dispType string, nsid uint64, ifindex uint32) error

	// Cached views (built lazily on first access, pure joins).
	programs    []ProgramState
	links       []LinkState
	dispatchers []DispatcherState
}

// GatherState builds an ObservedState by scanning all three sources.
// All I/O happens here; the returned state is a pure fact store.
func GatherState(ctx context.Context, store platform.Store, kops platform.KernelOperations, layout bpfmanfs.FSLayout) (*ObservedState, error) {
	s := &ObservedState{
		kernelProgs:           make(map[kernel.ProgramID]bool),
		kernelLinks:           make(map[kernel.LinkID]bool),
		fsPinExists:           make(map[string]bool),
		fsDispatcherLinkCount: make(map[string]int),
		dbDispatcherExtCount:  make(map[kernel.ProgramID]int),
		tcFilterOK:            make(map[string]bool),
		dbProgPins:            make(map[string]bool),
		dbProgIDs:             make(map[kernel.ProgramID]bool),
		dbDispatcherKeys:      make(map[string]bool),
		layout:                layout,
	}

	var err error

	// ----------------------------------------------------------------
	// Phase 1: DB facts
	// ----------------------------------------------------------------

	s.dbPrograms, err = store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list programs: %w", err)
	}

	s.dbLinks, err = store.ListLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}

	s.dbDispatchers, err = store.ListDispatchers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dispatchers: %w", err)
	}

	// Build DB indexes.
	for kernelID, prog := range s.dbPrograms {
		s.dbProgIDs[kernelID] = true
		if prog.Handles.PinPath != "" {
			s.dbProgPins[prog.Handles.PinPath] = true
		}
	}
	for _, d := range s.dbDispatchers {
		s.dbDispatcherKeys[dispatcherKey(d.Type, d.Nsid, d.Ifindex)] = true
	}

	// ----------------------------------------------------------------
	// Phase 2: Kernel facts
	// ----------------------------------------------------------------

	for kp, err := range kops.Programs(ctx) {
		if err != nil {
			s.kernelProgEnumErrors++
			continue
		}
		s.kernelProgs[kp.ID] = true
	}

	for kl, err := range kops.Links(ctx) {
		if err != nil {
			s.kernelLinkEnumErrors++
			continue
		}
		s.kernelLinks[kl.ID] = true
	}

	// ----------------------------------------------------------------
	// Phase 3: Store-derived facts (dispatcher extension counts)
	// ----------------------------------------------------------------

	for _, d := range s.dbDispatchers {
		if count, err := store.CountDispatcherLinks(ctx, d.KernelID); err == nil {
			s.dbDispatcherExtCount[d.KernelID] = count
		}
	}

	// ----------------------------------------------------------------
	// Phase 4: Netlink facts (TC filter checks)
	// ----------------------------------------------------------------

	for _, d := range s.dbDispatchers {
		if d.Type != dispatcher.DispatcherTypeTCIngress && d.Type != dispatcher.DispatcherTypeTCEgress {
			continue
		}
		if d.Priority == 0 {
			continue
		}
		key := dispatcherKey(d.Type, d.Nsid, d.Ifindex)
		parent := tcParentHandle(d.Type)
		_, err := kops.FindTCFilterHandle(ctx, int(d.Ifindex), parent, d.Priority)
		s.tcFilterOK[key] = (err == nil)
	}

	// ----------------------------------------------------------------
	// Phase 5: Filesystem facts - collect paths to stat
	// ----------------------------------------------------------------

	pathsToStat := make(map[string]struct{})

	// Program pin paths from DB.
	for _, prog := range s.dbPrograms {
		if prog.Handles.PinPath != "" {
			pathsToStat[prog.Handles.PinPath] = struct{}{}
		}
	}

	// Link pin paths from DB (non-synthetic only).
	for i := range s.dbLinks {
		link := &s.dbLinks[i]
		if link.PinPath != nil && !link.IsSynthetic() {
			pathsToStat[link.PinPath.String()] = struct{}{}
		}
	}

	// Dispatcher prog pins and XDP link pins.
	fs := layout.BPFFS()
	for _, d := range s.dbDispatchers {
		progPin := fs.DispatcherProgPath(d.Type, d.Nsid, d.Ifindex, d.Revision)
		pathsToStat[progPin] = struct{}{}

		if d.Type == dispatcher.DispatcherTypeXDP {
			linkPin := fs.DispatcherLinkPath(d.Type, d.Nsid, d.Ifindex)
			pathsToStat[linkPin] = struct{}{}
		}
	}

	// ----------------------------------------------------------------
	// Phase 6: Filesystem facts - directory scans for orphans
	// ----------------------------------------------------------------

	s.orphans = make([]FsOrphan, 0)

	// Delegate bpfman-specific bpffs scanning to the scanner.
	scanner := layout.BPFFS().Scanner()

	// Stat all collected paths using the scanner.
	for path := range pathsToStat {
		s.fsPinExists[path] = scanner.PathExists(path)
	}
	fsState, err := scanner.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan bpffs: %w", err)
	}

	// Scan prog pins for orphans.
	for _, pin := range fsState.ProgPins {
		if s.dbProgPins[pin.Path] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path:     pin.Path,
			KernelID: pin.KernelID,
			Kind:     OrphanProgPin,
		})
	}

	// Scan link directories for orphans.
	for _, dir := range fsState.LinkDirs {
		if s.dbProgIDs[dir.ProgramID] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path:     dir.Path,
			KernelID: dir.ProgramID,
			Kind:     OrphanLinkDir,
		})
	}

	// Scan map directories for orphans.
	for _, dir := range fsState.MapDirs {
		if s.dbProgIDs[dir.ProgramID] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path:     dir.Path,
			KernelID: dir.ProgramID,
			Kind:     OrphanMapDir,
		})
	}

	// Scan dispatcher directories for orphans and record link counts
	// for non-orphans.
	for _, d := range fsState.DispatcherDirs {
		key := dispatcherKey(dispatcher.DispatcherType(d.DispType), d.Nsid, d.Ifindex)
		if !s.dbDispatcherKeys[key] {
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
		key := dispatcherKey(dispatcher.DispatcherType(pin.DispType), pin.Nsid, pin.Ifindex)
		if s.dbDispatcherKeys[key] {
			continue
		}
		s.orphans = append(s.orphans, FsOrphan{
			Path: pin.Path,
			Kind: OrphanDispatcherLink,
		})
	}

	// ----------------------------------------------------------------
	// Phase 6a: Scan <base>/programs/ for orphan program dirs
	// ----------------------------------------------------------------

	rt := layout.BytecodeFS()
	programDirs, err := rt.ScanProgramDirs()
	if err != nil {
		return nil, fmt.Errorf("scan program dirs: %w", err)
	}
	for _, pde := range programDirs {
		if pde.Numeric {
			if !s.dbProgIDs[pde.KernelID] {
				s.orphans = append(s.orphans, FsOrphan{
					Path:     pde.Path,
					KernelID: pde.KernelID,
					Kind:     OrphanProgramDir,
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
	// Phase 6b: Scan <base>/.staging/ for orphan staging dirs
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

	// ----------------------------------------------------------------
	// Wire up mutation capability for GC operations only.
	// ----------------------------------------------------------------

	s.deleteDispatcher = func(dispType string, nsid uint64, ifindex uint32) error {
		return store.DeleteDispatcher(ctx, dispType, nsid, ifindex)
	}

	return s, nil
}

// --------------------------------------------------------------------
// View builders: construct correlated tuples from raw facts.
// All joins happen here. Rules never touch raw maps.
// --------------------------------------------------------------------

// Programs returns one ProgramState per DB program, correlated with
// kernel and filesystem state. This is a pure join over gathered facts.
func (s *ObservedState) Programs() []ProgramState {
	if s.programs != nil {
		return s.programs
	}
	for id, prog := range s.dbPrograms {
		ps := ProgramState{
			KernelID: id,
			DB:       &prog,
			Kernel:   s.kernelProgs[id],
			PinPath:  prog.Handles.PinPath,
		}
		if prog.Handles.PinPath != "" {
			if exists, ok := s.fsPinExists[prog.Handles.PinPath]; ok {
				ps.PinExist = &exists
			}
			// Path not in map = stat failed with unknown error.
		}
		s.programs = append(s.programs, ps)
	}
	return s.programs
}

// Links returns one LinkState per DB link, correlated with kernel
// state and filesystem. This is a pure join over gathered facts.
func (s *ObservedState) Links() []LinkState {
	if s.links != nil {
		return s.links
	}
	for i := range s.dbLinks {
		link := &s.dbLinks[i]
		synthetic := link.IsSynthetic()
		inKernel := false
		// For non-synthetic links, ID is the kernel link ID
		if !synthetic {
			inKernel = s.kernelLinks[kernel.LinkID(link.ID)]
		}
		ls := LinkState{
			DB:        link,
			Synthetic: synthetic,
			Kernel:    inKernel,
		}
		if link.PinPath != nil && !synthetic {
			if exists, found := s.fsPinExists[link.PinPath.String()]; found {
				ls.PinExist = &exists
			}
		}
		s.links = append(s.links, ls)
	}
	return s.links
}

// Dispatchers returns one DispatcherState per DB dispatcher,
// correlated with kernel, filesystem, and extension link counts.
// This is a pure join over gathered facts.
func (s *ObservedState) Dispatchers() []DispatcherState {
	if s.dispatchers != nil {
		return s.dispatchers
	}
	fs := s.layout.BPFFS()
	for _, d := range s.dbDispatchers {
		key := dispatcherKey(d.Type, d.Nsid, d.Ifindex)
		revDir := fs.DispatcherRevisionDir(d.Type, d.Nsid, d.Ifindex, d.Revision)
		progPin := fs.DispatcherProgPath(d.Type, d.Nsid, d.Ifindex, d.Revision)

		ds := DispatcherState{
			DB:         &d,
			KernelProg: s.kernelProgs[d.KernelID],
			RevDir:     revDir,
			ProgPin:    progPin,
			LinkCount:  -1,
		}

		// Prog pin existence from gathered facts.
		if exists, ok := s.fsPinExists[progPin]; ok {
			ds.ProgPinExist = &exists
		}

		// XDP link checks from gathered facts.
		if d.Type == dispatcher.DispatcherTypeXDP {
			ds.KernelLink = d.LinkID != 0 && s.kernelLinks[d.LinkID]
			linkPin := fs.DispatcherLinkPath(d.Type, d.Nsid, d.Ifindex)
			if exists, ok := s.fsPinExists[linkPin]; ok {
				ds.LinkPinExist = &exists
			}
		}

		// TC filter check from gathered facts.
		if d.Type == dispatcher.DispatcherTypeTCIngress || d.Type == dispatcher.DispatcherTypeTCEgress {
			if d.Priority > 0 {
				if ok, found := s.tcFilterOK[key]; found {
					ds.TCFilterOK = &ok
				}
			}
		}

		// Extension link count from gathered facts.
		if count, found := s.dbDispatcherExtCount[d.KernelID]; found {
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
func (s *ObservedState) KernelAlive(kernelID kernel.ProgramID) bool {
	return s.kernelProgs[kernelID]
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
		if o.Kind == OrphanProgPin && o.KernelID != 0 && s.kernelProgs[o.KernelID] {
			count++
		}
	}
	return count
}

// DeleteDispatcher delegates to the store to remove a dispatcher.
func (s *ObservedState) DeleteDispatcher(dispType string, nsid uint64, ifindex uint32) error {
	return s.deleteDispatcher(dispType, nsid, ifindex)
}

func dispatcherKey(dt dispatcher.DispatcherType, nsid uint64, ifindex uint32) string {
	return fmt.Sprintf("%s/%d/%d", dt, nsid, ifindex)
}

// tcParentHandle returns the netlink parent handle for a TC
// dispatcher type.
func tcParentHandle(dispType dispatcher.DispatcherType) uint32 {
	switch dispType {
	case dispatcher.DispatcherTypeTCIngress:
		return netlink.HANDLE_MIN_INGRESS
	case dispatcher.DispatcherTypeTCEgress:
		return netlink.HANDLE_MIN_EGRESS
	default:
		return 0
	}
}
