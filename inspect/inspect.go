package inspect

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"slices"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

// ErrNotFound is returned when a program is not found in any source.
var ErrNotFound = errors.New("not found")

// StoreLister is the subset of platform.Store needed by Snapshot.
type StoreLister interface {
	List(ctx context.Context) (map[kernel.ProgramID]bpfman.ProgramRecord, error)
	ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error)
	ListDispatchers(ctx context.Context) ([]dispatcher.State, error)
}

// StoreGetter is the subset of platform.Store needed by GetProgram.
type StoreGetter interface {
	Get(ctx context.Context, kernelID kernel.ProgramID) (bpfman.ProgramRecord, error)
}

// KernelLister is the subset of platform.KernelSource needed by Snapshot.
type KernelLister interface {
	Programs(ctx context.Context) iter.Seq2[kernel.Program, error]
	Links(ctx context.Context) iter.Seq2[kernel.Link, error]
}

// KernelGetter is the subset of platform.KernelSource needed by GetProgram.
type KernelGetter interface {
	GetProgramByID(ctx context.Context, id kernel.ProgramID) (kernel.Program, error)
}

// LinkGetter is the subset of platform.Store needed by GetLink.
type LinkGetter interface {
	GetLink(ctx context.Context, linkID kernel.LinkID) (bpfman.LinkRecord, error)
}

// KernelLinkGetter is the subset of platform.KernelSource needed by GetLink.
type KernelLinkGetter interface {
	GetLinkByID(ctx context.Context, id kernel.LinkID) (kernel.Link, error)
}

// LinkInfo is the result of GetLink, containing record and presence.
type LinkInfo struct {
	Record   bpfman.LinkRecord `json:"record"`
	Kernel   *kernel.Link      `json:"kernel,omitempty"` // may be nil if not in kernel
	Presence Presence          `json:"presence"`
}

// DispatcherGetter is the subset of platform.Store needed by GetDispatcher.
type DispatcherGetter interface {
	GetDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (dispatcher.State, error)
}

// DispatcherInfo is the result of GetDispatcher, containing state and presence.
type DispatcherInfo struct {
	State        dispatcher.State `json:"state"`
	ProgPresence Presence         `json:"prog_presence"` // dispatcher program presence
	LinkPresence Presence         `json:"link_presence"` // XDP link presence (for XDP dispatchers)
}

// Presence indicates where an object exists across the three sources.
type Presence struct {
	InStore  bool `json:"in_store"`
	InKernel bool `json:"in_kernel"`
	InFS     bool `json:"in_fs"`
}

// Managed returns true if the object is tracked in the store.
func (p Presence) Managed() bool { return p.InStore }

// OrphanFS returns true if the object exists only on the filesystem.
func (p Presence) OrphanFS() bool { return p.InFS && !p.InStore && !p.InKernel }

// KernelOnly returns true if the object exists only in the kernel.
func (p Presence) KernelOnly() bool { return p.InKernel && !p.InStore }

// ProgramView is a correlation view of a program across store, kernel, and FS.
// Renamed from ProgramRow.
type ProgramView struct {
	KernelID kernel.ProgramID `json:"kernel_id"`

	// Store fields (valid when Presence.InStore is true)
	Managed *bpfman.ProgramRecord `json:"managed,omitempty"`

	// Kernel fields (valid when Presence.InKernel is true)
	Kernel *kernel.Program `json:"kernel,omitempty"`

	// FS fields
	FSPinPath   string `json:"fs_pin_path,omitempty"` // from bpffs scan (may differ from store)
	MapsPresent bool   `json:"maps_present"`          // true if map pin directory exists

	// Links attached to this program (correlated from World.Links)
	Links []LinkRow `json:"links,omitempty"`

	Presence Presence `json:"presence"`
}

// AsProgram constructs a bpfman.Program composite from a store-managed program.
// Returns (Program, true) when Managed != nil (even if kernel/fs are missing).
// Status reflects what's actually present vs missing.
func (v ProgramView) AsProgram() (bpfman.Program, bool) {
	if v.Managed == nil {
		return bpfman.Program{}, false // not store-managed, can't construct
	}

	// Convert links
	var links []bpfman.Link
	for _, lr := range v.Links {
		if link, ok := lr.AsLink(); ok {
			links = append(links, link)
		}
	}

	return bpfman.Program{
		Record: *v.Managed,
		Status: bpfman.ProgramStatus{
			Kernel:      v.Kernel,        // may be nil
			PinPresent:  v.Presence.InFS, // Record.PinPath exists
			MapsPresent: v.MapsPresent,   // map pin directory exists
			Links:       links,
		},
	}, true
}

// ProgramRow is an alias for ProgramView for backwards compatibility.
// Deprecated: Use ProgramView instead.
type ProgramRow = ProgramView

// Name returns the program name (from store if available, else kernel).
func (v ProgramView) Name() string {
	if v.Managed != nil {
		return v.Managed.Meta.Name
	}
	if v.Kernel != nil {
		return v.Kernel.Name
	}
	return ""
}

// Type returns the program type (from store if available, else kernel).
func (v ProgramView) Type() string {
	if v.Managed != nil {
		return v.Managed.Load.ProgramType().String()
	}
	if v.Kernel != nil {
		return v.Kernel.ProgramType.String()
	}
	return ""
}

// PinPath returns the pin path (from store if available, else FS).
func (v ProgramView) PinPath() string {
	if v.Managed != nil && v.Managed.Handles.PinPath != "" {
		return v.Managed.Handles.PinPath
	}
	return v.FSPinPath
}

// LinkRow is a store-first view of a link with presence annotations.
type LinkRow struct {
	// Store fields (valid when Presence.InStore is true)
	Managed *bpfman.LinkRecord `json:"managed,omitempty"`

	// Kernel fields (valid when Presence.InKernel is true)
	Kernel *kernel.Link `json:"kernel,omitempty"`

	Presence Presence `json:"presence"`
}

// ID returns the link's durable bpfman ID.
func (r LinkRow) ID() kernel.LinkID {
	if r.Managed != nil {
		return r.Managed.ID
	}
	return 0
}

// KernelLinkID returns the kernel link ID if available.
// For non-synthetic links, the Spec.ID is the kernel link ID.
// For synthetic links (perf_event-based), returns nil.
func (r LinkRow) KernelLinkID() *kernel.LinkID {
	if r.Managed != nil && !r.Managed.IsSynthetic() {
		id := r.Managed.ID
		return &id
	}
	if r.Kernel != nil {
		return &r.Kernel.ID
	}
	return nil
}

// Kind returns the link kind (from store if available).
func (r LinkRow) Kind() bpfman.LinkKind {
	if r.Managed != nil {
		return r.Managed.Kind
	}
	return ""
}

// PinPath returns the pin path (from store if available).
func (r LinkRow) PinPath() string {
	if r.Managed != nil && r.Managed.PinPath != nil {
		return r.Managed.PinPath.String()
	}
	return ""
}

// IsSynthetic returns true if this is a synthetic link (no kernel link ID).
func (r LinkRow) IsSynthetic() bool {
	if r.Managed != nil {
		return r.Managed.IsSynthetic()
	}
	return false
}

// HasPin returns true if this link has a pin path.
func (r LinkRow) HasPin() bool {
	if r.Managed != nil {
		return r.Managed.HasPin()
	}
	return false
}

// AsLink constructs a bpfman.Link composite from a store-managed link.
// Returns (Link, true) when Managed != nil.
func (r LinkRow) AsLink() (bpfman.Link, bool) {
	if r.Managed == nil {
		return bpfman.Link{}, false
	}
	return bpfman.Link{
		Record: *r.Managed,
		Status: bpfman.LinkStatus{
			Kernel:     r.Kernel,
			KernelSeen: r.Presence.InKernel,
			PinPresent: r.Presence.InFS,
		},
	}, true
}

// DispatcherRow is a store-first view of a dispatcher with presence annotations.
type DispatcherRow struct {
	// Key fields
	DispType string `json:"disp_type"`
	Nsid     uint64 `json:"nsid"`
	Ifindex  uint32 `json:"ifindex"`

	// Store fields (valid when ProgPresence.InStore is true)
	Managed  *dispatcher.State `json:"managed,omitempty"`
	Revision uint32            `json:"revision"`
	KernelID kernel.ProgramID  `json:"kernel_id"`
	LinkID   kernel.LinkID     `json:"link_id"`
	Priority uint32            `json:"priority"`

	// Presence tracks where the dispatcher's components exist
	ProgPresence Presence `json:"prog_presence"` // dispatcher program
	LinkPresence Presence `json:"link_presence"` // XDP link (for XDP dispatchers)

	// FS-derived
	FSLinkCount int `json:"fs_link_count"` // count of link_* files in revision dir (-1 if unknown)
}

// SnapshotMeta contains metadata about the snapshot.
type SnapshotMeta struct {
	// ObservedAt is when the snapshot was taken.
	ObservedAt time.Time `json:"observed_at"`
	// Errors encountered during snapshot (non-fatal)
	Errors []error `json:"-"` // errors don't serialize well to JSON
	// ProgramEnumErrors counts errors during kernel program enumeration.
	ProgramEnumErrors int `json:"program_enum_errors"`
	// LinkEnumErrors counts errors during kernel link enumeration.
	LinkEnumErrors int `json:"link_enum_errors"`
}

// World is a point-in-time snapshot of bpfman's state across all sources.
type World struct {
	Programs    []ProgramRow    `json:"programs"`
	Links       []LinkRow       `json:"links"`
	Dispatchers []DispatcherRow `json:"dispatchers"`
	Meta        SnapshotMeta    `json:"meta"`
}

// ManagedPrograms returns only store-managed programs.
func (w *World) ManagedPrograms() []ProgramRow {
	var out []ProgramRow
	for _, r := range w.Programs {
		if r.Presence.InStore {
			out = append(out, r)
		}
	}
	return out
}

// ManagedLinks returns only store-managed links.
func (w *World) ManagedLinks() []LinkRow {
	var out []LinkRow
	for _, r := range w.Links {
		if r.Presence.InStore {
			out = append(out, r)
		}
	}
	return out
}

// ManagedDispatchers returns only store-managed dispatchers.
func (w *World) ManagedDispatchers() []DispatcherRow {
	var out []DispatcherRow
	for _, r := range w.Dispatchers {
		if r.ProgPresence.InStore {
			out = append(out, r)
		}
	}
	return out
}

// Snapshot builds a World by reading from store, kernel, and filesystem.
// The returned World contains all objects from all sources, correlated
// by kernel ID. Use ManagedPrograms() etc. for the default store-first view.
func Snapshot(
	ctx context.Context,
	store StoreLister,
	kern KernelLister,
	scanner *fs.Scanner,
) (*World, error) {
	w := &World{
		Meta: SnapshotMeta{
			ObservedAt: time.Now(),
		},
	}

	// Phase 1: Build indexes from kernel and filesystem
	kernelProgs := make(map[kernel.ProgramID]kernel.Program)
	kernelLinks := make(map[kernel.LinkID]bool)

	for kp, err := range kern.Programs(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			w.Meta.ProgramEnumErrors++
			continue
		}
		kernelProgs[kp.ID] = kp
	}

	for kl, err := range kern.Links(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			w.Meta.LinkEnumErrors++
			continue
		}
		kernelLinks[kl.ID] = true
	}

	// FS indexes
	fsProgPins := make(map[kernel.ProgramID]string)  // kernelID -> path
	fsLinkDirs := make(map[kernel.ProgramID]string)  // programID -> path
	fsMapDirs := make(map[kernel.ProgramID]string)   // programID -> path
	fsDispDirs := make(map[string]*fs.DispatcherDir) // "type/nsid/ifindex" -> dir
	fsDispLinks := make(map[string]string)           // "type/nsid/ifindex" -> path

	for pin, err := range scanner.ProgPins(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			continue
		}
		fsProgPins[pin.KernelID] = pin.Path
	}

	for dir, err := range scanner.LinkDirs(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			continue
		}
		fsLinkDirs[dir.ProgramID] = dir.Path
	}

	for dir, err := range scanner.MapDirs(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			continue
		}
		fsMapDirs[dir.ProgramID] = dir.Path
	}

	for dir, err := range scanner.DispatcherDirs(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			continue
		}
		key := dispatcherKey(dir.DispType, dir.Nsid, dir.Ifindex)
		d := dir // copy
		fsDispDirs[key] = &d
	}

	for pin, err := range scanner.DispatcherLinkPins(ctx) {
		if err != nil {
			w.Meta.Errors = append(w.Meta.Errors, err)
			continue
		}
		key := dispatcherKey(pin.DispType, pin.Nsid, pin.Ifindex)
		fsDispLinks[key] = pin.Path
	}

	// Phase 2: Build program rows (store-first)
	storeProgs, err := store.List(ctx)
	if err != nil {
		return nil, err
	}

	seenProgIDs := make(map[kernel.ProgramID]bool)
	for kernelID, prog := range storeProgs {
		seenProgIDs[kernelID] = true
		fsPath, inFS := fsProgPins[kernelID]
		kp, inKernel := kernelProgs[kernelID]
		_, mapsPresent := fsMapDirs[kernelID]

		row := ProgramView{
			KernelID:    kernelID,
			Managed:     &prog,
			FSPinPath:   fsPath,
			MapsPresent: mapsPresent,
			Presence: Presence{
				InStore:  true,
				InKernel: inKernel,
				InFS:     inFS,
			},
		}
		if inKernel {
			row.Kernel = &kp
		}
		w.Programs = append(w.Programs, row)
	}

	// Add kernel-only programs (not in store)
	for kernelID, kp := range kernelProgs {
		if seenProgIDs[kernelID] {
			continue
		}
		fsPath, inFS := fsProgPins[kernelID]
		row := ProgramView{
			KernelID:  kernelID,
			Kernel:    &kp,
			FSPinPath: fsPath,
			Presence: Presence{
				InStore:  false,
				InKernel: true,
				InFS:     inFS,
			},
		}
		w.Programs = append(w.Programs, row)
		seenProgIDs[kernelID] = true
	}

	// Add FS-only programs (not in store, not in kernel)
	for kernelID, fsPath := range fsProgPins {
		if seenProgIDs[kernelID] {
			continue
		}
		row := ProgramView{
			KernelID:  kernelID,
			FSPinPath: fsPath,
			Presence: Presence{
				InStore:  false,
				InKernel: false,
				InFS:     true,
			},
		}
		w.Programs = append(w.Programs, row)
	}

	// Phase 3: Build link rows (store-first)
	// Also build kernel link map for correlation
	kernelLinkMap := make(map[kernel.LinkID]kernel.Link)
	for kl, err := range kern.Links(ctx) {
		if err != nil {
			continue
		}
		kernelLinkMap[kl.ID] = kl
	}

	storeLinks, err := store.ListLinks(ctx)
	if err != nil {
		return nil, err
	}

	seenKernelLinkIDs := make(map[kernel.LinkID]bool)
	for _, link := range storeLinks {
		// Track kernel link IDs we've seen from store.
		// For non-synthetic links, ID is the kernel link ID.
		if !link.IsSynthetic() {
			seenKernelLinkIDs[link.ID] = true
		}

		// Check kernel presence
		var kernelLink *kernel.Link
		if !link.IsSynthetic() {
			if kl, ok := kernelLinkMap[link.ID]; ok {
				kernelLink = &kl
			}
		}

		var inFS bool
		if link.PinPath != nil {
			inFS = scanner.PathExists(link.PinPath.String())
		}
		row := LinkRow{
			Managed: &link,
			Kernel:  kernelLink,
			Presence: Presence{
				InStore:  true,
				InKernel: kernelLink != nil,
				InFS:     inFS,
			},
		}
		w.Links = append(w.Links, row)
	}

	// Add kernel-only links (not in store)
	for kernelLinkID, kl := range kernelLinkMap {
		if seenKernelLinkIDs[kernelLinkID] {
			continue
		}
		row := LinkRow{
			Kernel: &kl,
			Presence: Presence{
				InStore:  false,
				InKernel: true,
				InFS:     false,
			},
		}
		w.Links = append(w.Links, row)
	}

	// Phase 4: Build dispatcher rows (store-first)
	storeDisps, err := store.ListDispatchers(ctx)
	if err != nil {
		return nil, err
	}

	seenDispKeys := make(map[string]bool)
	for _, disp := range storeDisps {
		key := dispatcherKey(string(disp.Type), disp.Nsid, disp.Ifindex)
		seenDispKeys[key] = true

		fsDir := fsDispDirs[key]
		_, linkPinExists := fsDispLinks[key]

		fsLinkCount := -1
		progInFS := false
		if fsDir != nil {
			fsLinkCount = fsDir.LinkCount
			progInFS = true
		}

		_, progInKernel := kernelProgs[disp.KernelID]
		row := DispatcherRow{
			DispType:    string(disp.Type),
			Nsid:        disp.Nsid,
			Ifindex:     disp.Ifindex,
			Managed:     &disp,
			Revision:    disp.Revision,
			KernelID:    disp.KernelID,
			LinkID:      disp.LinkID,
			Priority:    uint32(disp.Priority),
			FSLinkCount: fsLinkCount,
			ProgPresence: Presence{
				InStore:  true,
				InKernel: progInKernel,
				InFS:     progInFS,
			},
			LinkPresence: Presence{
				InStore:  disp.LinkID != 0,
				InKernel: disp.LinkID != 0 && kernelLinks[disp.LinkID],
				InFS:     linkPinExists,
			},
		}
		w.Dispatchers = append(w.Dispatchers, row)
	}

	// Add FS-only dispatchers (orphan dirs)
	for key, fsDir := range fsDispDirs {
		if seenDispKeys[key] {
			continue
		}
		_, linkPinExists := fsDispLinks[key]
		row := DispatcherRow{
			DispType:    fsDir.DispType,
			Nsid:        fsDir.Nsid,
			Ifindex:     fsDir.Ifindex,
			Revision:    fsDir.Revision,
			FSLinkCount: fsDir.LinkCount,
			ProgPresence: Presence{
				InStore:  false,
				InKernel: false,
				InFS:     true,
			},
			LinkPresence: Presence{
				InStore:  false,
				InKernel: false,
				InFS:     linkPinExists,
			},
		}
		w.Dispatchers = append(w.Dispatchers, row)
	}

	// Correlate links to programs by ProgramID
	programIndex := make(map[kernel.ProgramID]int, len(w.Programs))
	for i := range w.Programs {
		programIndex[w.Programs[i].KernelID] = i
	}
	for _, link := range w.Links {
		if link.Managed == nil {
			continue
		}
		if idx, ok := programIndex[link.Managed.ProgramID]; ok {
			w.Programs[idx].Links = append(w.Programs[idx].Links, link)
		}
	}

	// Sort all slices for deterministic output
	slices.SortFunc(w.Programs, func(a, b ProgramView) int {
		return cmp.Compare(a.KernelID, b.KernelID)
	})
	slices.SortFunc(w.Links, func(a, b LinkRow) int {
		return cmp.Compare(a.ID(), b.ID())
	})
	slices.SortFunc(w.Dispatchers, func(a, b DispatcherRow) int {
		if c := cmp.Compare(a.DispType, b.DispType); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Nsid, b.Nsid); c != 0 {
			return c
		}
		return cmp.Compare(a.Ifindex, b.Ifindex)
	})

	return w, nil
}

// GetProgram retrieves a single program by kernel ID, correlating state
// from store, kernel, and filesystem. This is more efficient than Snapshot
// for single-program lookups as it performs targeted queries rather than
// enumerating everything.
//
// Returns ErrNotFound if the program does not exist in any source.
func GetProgram(
	ctx context.Context,
	storeGetter StoreGetter,
	kern KernelGetter,
	scanner *fs.Scanner,
	kernelID kernel.ProgramID,
) (ProgramView, error) {
	row := ProgramView{KernelID: kernelID}

	// Try store
	prog, err := storeGetter.Get(ctx, kernelID)
	if err == nil {
		row.Managed = &prog
		row.Presence.InStore = true
	} else if !errors.Is(err, platform.ErrRecordNotFound) {
		// Real error (not just "not found")
		return ProgramView{}, err
	}

	// Try kernel
	kp, err := kern.GetProgramByID(ctx, kernelID)
	if err == nil {
		row.Kernel = &kp
		row.Presence.InKernel = true
	}
	// Kernel errors (program not found) are not fatal - just means not in kernel

	// Try filesystem
	// If we have store metadata with a pin path, check that specific path
	if row.Managed != nil && row.Managed.Handles.PinPath != "" {
		if scanner.PathExists(row.Managed.Handles.PinPath) {
			row.FSPinPath = row.Managed.Handles.PinPath
			row.Presence.InFS = true
		}
	}

	// If not found in any source, return error
	if !row.Presence.InStore && !row.Presence.InKernel && !row.Presence.InFS {
		return ProgramView{}, ErrNotFound
	}

	return row, nil
}

// GetLink retrieves a single link by its durable bpfman ID, correlating state
// from store, kernel, and filesystem. This is more efficient than Snapshot
// for single-link lookups as it performs targeted queries rather than
// enumerating everything.
//
// Returns ErrNotFound if the link does not exist in any source.
func GetLink(
	ctx context.Context,
	linkGetter LinkGetter,
	kern KernelLinkGetter,
	scanner *fs.Scanner,
	linkID kernel.LinkID,
) (LinkInfo, error) {
	info := LinkInfo{}

	// Try store - this returns the full record with details
	record, err := linkGetter.GetLink(ctx, linkID)
	if err == nil {
		info.Record = record
		info.Presence.InStore = true
	} else if !errors.Is(err, platform.ErrRecordNotFound) {
		// Real error (not just "not found")
		return LinkInfo{}, err
	}

	// Try kernel (skip for synthetic links which don't have kernel link IDs).
	// For non-synthetic links, the Spec.ID is the kernel link ID.
	if info.Presence.InStore && !record.IsSynthetic() {
		kl, err := kern.GetLinkByID(ctx, record.ID)
		if err == nil {
			info.Kernel = &kl
			info.Presence.InKernel = true
		}
		// Kernel errors (link not found) are not fatal - just means not in kernel
	}

	// Try filesystem - check if pin path exists
	if info.Presence.InStore && record.PinPath != nil {
		if scanner.PathExists(record.PinPath.String()) {
			info.Presence.InFS = true
		}
	}

	// If not found in store, return error (links are store-first)
	if !info.Presence.InStore {
		return LinkInfo{}, ErrNotFound
	}

	return info, nil
}

// GetDispatcher retrieves a single dispatcher by its key (type, nsid, ifindex),
// correlating state from store, kernel, and filesystem. This is more efficient
// than Snapshot for single-dispatcher lookups.
//
// Returns ErrNotFound if the dispatcher does not exist in any source.
func GetDispatcher(
	ctx context.Context,
	dispGetter DispatcherGetter,
	kern KernelGetter,
	kernLinkGetter KernelLinkGetter,
	scanner *fs.Scanner,
	dispType string,
	nsid uint64,
	ifindex uint32,
) (DispatcherInfo, error) {
	info := DispatcherInfo{}

	// Try store
	state, err := dispGetter.GetDispatcher(ctx, dispType, nsid, ifindex)
	if err == nil {
		info.State = state
		info.ProgPresence.InStore = true
		if state.LinkID != 0 {
			info.LinkPresence.InStore = true
		}
	} else if !errors.Is(err, platform.ErrRecordNotFound) {
		// Real error (not just "not found")
		return DispatcherInfo{}, err
	}

	// Try kernel for dispatcher program
	if info.ProgPresence.InStore && info.State.KernelID != 0 {
		_, err := kern.GetProgramByID(ctx, info.State.KernelID)
		if err == nil {
			info.ProgPresence.InKernel = true
		}
	}

	// Try kernel for dispatcher link (XDP only)
	if info.LinkPresence.InStore && info.State.LinkID != 0 {
		_, err := kernLinkGetter.GetLinkByID(ctx, info.State.LinkID)
		if err == nil {
			info.LinkPresence.InKernel = true
		}
	}

	// Try filesystem for dispatcher directory
	// Dispatcher dirs follow pattern: {dispType}/dispatcher_{nsid}_{ifindex}_{revision}
	if info.ProgPresence.InStore {
		// Check if the dispatcher directory exists
		key := dispatcherKey(dispType, nsid, ifindex)
		for dir, err := range scanner.DispatcherDirs(ctx) {
			if err != nil {
				continue
			}
			dirKey := dispatcherKey(dir.DispType, dir.Nsid, dir.Ifindex)
			if dirKey == key {
				info.ProgPresence.InFS = true
				break
			}
		}
	}

	// Try filesystem for dispatcher link pin (XDP only)
	if info.LinkPresence.InStore {
		for pin, err := range scanner.DispatcherLinkPins(ctx) {
			if err != nil {
				continue
			}
			if pin.DispType == dispType && pin.Nsid == nsid && pin.Ifindex == ifindex {
				info.LinkPresence.InFS = true
				break
			}
		}
	}

	// If not found in store, return error (dispatchers are always store-first)
	if !info.ProgPresence.InStore {
		return DispatcherInfo{}, ErrNotFound
	}

	return info, nil
}

func dispatcherKey(dispType string, nsid uint64, ifindex uint32) string {
	return dispType + "/" + uitoa64(nsid) + "/" + uitoa32(ifindex)
}

func uitoa32(n uint32) string {
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func uitoa64(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
