package inspect

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"
	"time"

	"github.com/frobware/go-bpfman"
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
	ListDispatcherSummaries(ctx context.Context) ([]platform.DispatcherSummary, error)
}

// StoreGetter is the subset of platform.Store needed by GetProgram.
type StoreGetter interface {
	Get(ctx context.Context, programID kernel.ProgramID) (bpfman.ProgramRecord, error)
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
	Record bpfman.LinkRecord `json:"record"`
	// Kernel nil means this link is not present in the kernel's link list;
	// pointer + omitempty encodes that absence.
	Kernel   *kernel.Link `json:"kernel,omitempty"`
	Presence Presence     `json:"presence"`
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
// Renamed from ProgramView.
type ProgramView struct {
	ProgramID kernel.ProgramID `json:"program_id"`

	// Managed nil means the program is not recorded in the store (kernel-only or
	// FS-only observation). Pointer + omitempty encodes that absence.
	Managed *bpfman.ProgramRecord `json:"managed,omitempty"`

	// Kernel nil means the program is not currently loaded in the kernel.
	// Pointer + omitempty encodes that absence.
	Kernel *kernel.Program `json:"kernel,omitempty"`

	// FS fields
	FSPinPath   string `json:"fs_pin_path"`  // empty when no bpffs pin was found
	MapsPresent bool   `json:"maps_present"` // true if map pin directory exists

	// Links attached to this program (correlated from Observation.Links)
	Links []LinkRow `json:"links"` // [] when the program has no links

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
			Kernel: v.Kernel, // may be nil
			Links:  links,
		},
	}, true
}

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
		return v.Managed.Handles.PinPath.String()
	}
	return v.FSPinPath
}

// LinkRow is a store-first view of a link with presence annotations.
type LinkRow struct {
	// Managed nil means the link is not recorded in the store (kernel-only
	// observation). Pointer + omitempty encodes that absence.
	Managed *bpfman.LinkRecord `json:"managed,omitempty"`

	// Kernel nil means the link is not present in the kernel's link list.
	// Pointer + omitempty encodes that absence.
	Kernel *kernel.Link `json:"kernel,omitempty"`

	// FSPinPath is the bpf fs pin file backing this link, when one
	// is found. Populated from a walk of bpfman's bpf fs subtree
	// that loads each file as a bpf_link to read its ID, so
	// kernel-only links pinned under dispatcher or TCX subtrees
	// (which the per-program LinkDirs scan does not enter) still
	// surface their pin here. Empty when no pin was located.
	FSPinPath string `json:"fs_pin_path,omitempty"`

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
	return bpfman.LinkKind{}
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

	// Managed nil means the dispatcher is not recorded in the store (an orphan
	// dispatcher observed only on the filesystem). Pointer + omitempty encodes
	// that absence.
	Managed   *platform.DispatcherSummary `json:"managed,omitempty"`
	Revision  uint32                      `json:"revision"`
	ProgramID kernel.ProgramID            `json:"program_id"`
	LinkID    kernel.LinkID               `json:"link_id"`
	Priority  uint32                      `json:"priority"`

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
	Errors []error `json:"-"` // errors don't serialise well to JSON
	// ProgramEnumErrors counts errors during kernel program enumeration.
	ProgramEnumErrors int `json:"program_enum_errors"`
	// LinkEnumErrors counts errors during kernel link enumeration.
	LinkEnumErrors int `json:"link_enum_errors"`
}

// Observation is a point-in-time correlated view of bpfman's state across all sources.
type Observation struct {
	Programs    []ProgramView   `json:"programs"`
	Links       []LinkRow       `json:"links"`
	Dispatchers []DispatcherRow `json:"dispatchers"`
	Meta        SnapshotMeta    `json:"meta"`
}

// ManagedPrograms returns only store-managed programs.
func (o *Observation) ManagedPrograms() []ProgramView {
	var out []ProgramView
	for _, r := range o.Programs {
		if r.Presence.InStore {
			out = append(out, r)
		}
	}
	return out
}

// ManagedLinks returns only store-managed links.
func (o *Observation) ManagedLinks() []LinkRow {
	var out []LinkRow
	for _, r := range o.Links {
		if r.Presence.InStore {
			out = append(out, r)
		}
	}
	return out
}

// ManagedDispatchers returns only store-managed dispatchers.
func (o *Observation) ManagedDispatchers() []DispatcherRow {
	var out []DispatcherRow
	for _, r := range o.Dispatchers {
		if r.ProgPresence.InStore {
			out = append(out, r)
		}
	}
	return out
}

// Snapshot builds an Observation by reading from store, kernel, and filesystem.
// The returned Observation contains all objects from all sources, correlated
// by kernel ID. Use ManagedPrograms() etc. for the default store-first view.
func Snapshot(
	ctx context.Context,
	store StoreLister,
	kern KernelLister,
	scanner *fs.Scanner,
) (*Observation, error) {
	obs := &Observation{
		Meta: SnapshotMeta{
			ObservedAt: time.Now(),
		},
	}

	// Phase 1: Build indexes from kernel and filesystem.
	// We store full kernel.Link objects so that Phase 3 (link
	// correlation) can reuse them without a second enumeration.
	kernelProgs := make(map[kernel.ProgramID]kernel.Program)
	kernelLinks := make(map[kernel.LinkID]kernel.Link)

	for kp, err := range kern.Programs(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
			obs.Meta.ProgramEnumErrors++
			continue
		}
		kernelProgs[kp.ID] = kp
	}

	for kl, err := range kern.Links(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
			obs.Meta.LinkEnumErrors++
			continue
		}
		kernelLinks[kl.ID] = kl
	}

	// FS indexes
	fsProgPins := make(map[kernel.ProgramID]string)  // programID -> path
	fsLinkDirs := make(map[kernel.ProgramID]string)  // programID -> path
	fsMapDirs := make(map[kernel.ProgramID]string)   // programID -> path
	fsDispDirs := make(map[string]*fs.DispatcherDir) // "type/nsid/ifindex" -> dir
	fsDispLinks := make(map[string]string)           // "type/nsid/ifindex" -> path

	// Comprehensive link-pin index built by walking the entire
	// bpf fs subtree and loading each candidate as a bpf_link.
	// Used to set FSPinPath on every LinkRow below, including
	// kernel-only links whose pin lives outside {fs}/links/
	// (extension link slots, TCX, dispatcher stable links).
	fsLinkPins, err := scanLinkPins(ctx, scanner)
	if err != nil {
		obs.Meta.Errors = append(obs.Meta.Errors, err)
	}

	for pin, err := range scanner.ProgPins(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
			continue
		}
		fsProgPins[pin.ProgramID] = pin.Path
	}

	for dir, err := range scanner.LinkDirs(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
			continue
		}
		fsLinkDirs[dir.ProgramID] = dir.Path
	}

	for dir, err := range scanner.MapDirs(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
			continue
		}
		fsMapDirs[dir.ProgramID] = dir.Path
	}

	for dir, err := range scanner.DispatcherDirs(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
			continue
		}
		key := dispatcherKey(dir.DispType, dir.Nsid, dir.Ifindex)
		d := dir // copy
		fsDispDirs[key] = &d
	}

	for pin, err := range scanner.DispatcherLinkPins(ctx) {
		if err != nil {
			obs.Meta.Errors = append(obs.Meta.Errors, err)
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
	for programID, prog := range storeProgs {
		seenProgIDs[programID] = true
		fsPath, inFS := fsProgPins[programID]
		kp, inKernel := kernelProgs[programID]
		_, mapsPresent := fsMapDirs[programID]

		row := ProgramView{
			ProgramID:   programID,
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
		obs.Programs = append(obs.Programs, row)
	}

	// Add kernel-only programs (not in store)
	for programID, kp := range kernelProgs {
		if seenProgIDs[programID] {
			continue
		}
		fsPath, inFS := fsProgPins[programID]
		row := ProgramView{
			ProgramID: programID,
			Kernel:    &kp,
			FSPinPath: fsPath,
			Presence: Presence{
				InStore:  false,
				InKernel: true,
				InFS:     inFS,
			},
		}
		obs.Programs = append(obs.Programs, row)
		seenProgIDs[programID] = true
	}

	// Add FS-only programs (not in store, not in kernel)
	for programID, fsPath := range fsProgPins {
		if seenProgIDs[programID] {
			continue
		}
		row := ProgramView{
			ProgramID: programID,
			FSPinPath: fsPath,
			Presence: Presence{
				InStore:  false,
				InKernel: false,
				InFS:     true,
			},
		}
		obs.Programs = append(obs.Programs, row)
	}

	// Phase 3: Build link rows (store-first).
	// kernelLinks (built in Phase 1) already contains full
	// kernel.Link objects, so no second enumeration is needed.
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
			if kl, ok := kernelLinks[link.ID]; ok {
				kernelLink = &kl
			}
		}

		// Resolve the pin path: prefer the bpf-fs walk index
		// (covers extension links and TCX pins), then fall
		// back to the store's recorded PinPath. The InFS flag
		// tracks whether either source confirmed a live pin.
		var fsPinPath string
		var inFS bool
		if !link.IsSynthetic() {
			if pin, ok := fsLinkPins[link.ID]; ok {
				fsPinPath = pin
				inFS = true
			}
		}
		if fsPinPath == "" && link.PinPath != nil {
			storePath := link.PinPath.String()
			if scanner.PathExists(storePath) {
				fsPinPath = storePath
				inFS = true
			}
		}
		row := LinkRow{
			Managed:   &link,
			Kernel:    kernelLink,
			FSPinPath: fsPinPath,
			Presence: Presence{
				InStore:  true,
				InKernel: kernelLink != nil,
				InFS:     inFS,
			},
		}
		obs.Links = append(obs.Links, row)
	}

	// Add kernel-only links (not in store).
	for kernelLinkID, kl := range kernelLinks {
		if seenKernelLinkIDs[kernelLinkID] {
			continue
		}
		pin, inFS := fsLinkPins[kernelLinkID]
		row := LinkRow{
			Kernel:    &kl,
			FSPinPath: pin,
			Presence: Presence{
				InStore:  false,
				InKernel: true,
				InFS:     inFS,
			},
		}
		obs.Links = append(obs.Links, row)
	}

	// Phase 4: Build dispatcher rows (store-first)
	storeDisps, err := store.ListDispatcherSummaries(ctx)
	if err != nil {
		return nil, err
	}

	seenDispKeys := make(map[string]bool)
	for _, disp := range storeDisps {
		key := dispatcherKey(disp.Key.Type.String(), disp.Key.Nsid, disp.Key.Ifindex)
		seenDispKeys[key] = true

		fsDir := fsDispDirs[key]
		_, linkPinExists := fsDispLinks[key]

		fsLinkCount := -1
		progInFS := false
		if fsDir != nil {
			fsLinkCount = fsDir.LinkCount
			progInFS = true
		}

		var linkID kernel.LinkID
		if disp.Runtime.LinkID != nil {
			linkID = *disp.Runtime.LinkID
		}
		var priority uint32
		if disp.Runtime.FilterPriority != nil {
			priority = uint32(*disp.Runtime.FilterPriority)
		}

		_, progInKernel := kernelProgs[disp.Runtime.ProgramID]
		_, linkInKernel := kernelLinks[linkID]
		d := disp // copy for pointer
		row := DispatcherRow{
			DispType:    disp.Key.Type.String(),
			Nsid:        disp.Key.Nsid,
			Ifindex:     disp.Key.Ifindex,
			Managed:     &d,
			Revision:    disp.Revision,
			ProgramID:   disp.Runtime.ProgramID,
			LinkID:      linkID,
			Priority:    priority,
			FSLinkCount: fsLinkCount,
			ProgPresence: Presence{
				InStore:  true,
				InKernel: progInKernel,
				InFS:     progInFS,
			},
			LinkPresence: Presence{
				InStore:  linkID != 0,
				InKernel: linkID != 0 && linkInKernel,
				InFS:     linkPinExists,
			},
		}
		obs.Dispatchers = append(obs.Dispatchers, row)
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
		obs.Dispatchers = append(obs.Dispatchers, row)
	}

	// Correlate links to programs by ProgramID
	programIndex := make(map[kernel.ProgramID]int, len(obs.Programs))
	for i := range obs.Programs {
		programIndex[obs.Programs[i].ProgramID] = i
	}
	for _, link := range obs.Links {
		if link.Managed == nil {
			continue
		}
		if idx, ok := programIndex[link.Managed.ProgramID]; ok {
			obs.Programs[idx].Links = append(obs.Programs[idx].Links, link)
		}
	}

	// Sort all slices for deterministic output
	slices.SortFunc(obs.Programs, func(a, b ProgramView) int {
		return cmp.Compare(a.ProgramID, b.ProgramID)
	})
	slices.SortFunc(obs.Links, func(a, b LinkRow) int {
		return cmp.Compare(a.ID(), b.ID())
	})
	slices.SortFunc(obs.Dispatchers, func(a, b DispatcherRow) int {
		if c := cmp.Compare(a.DispType, b.DispType); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Nsid, b.Nsid); c != 0 {
			return c
		}
		return cmp.Compare(a.Ifindex, b.Ifindex)
	})

	return obs, nil
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
	programID kernel.ProgramID,
) (ProgramView, error) {
	row := ProgramView{ProgramID: programID}

	// Try store
	prog, err := storeGetter.Get(ctx, programID)
	if err == nil {
		row.Managed = &prog
		row.Presence.InStore = true
	} else if !errors.Is(err, platform.ErrRecordNotFound) {
		// Real error (not just "not found")
		return ProgramView{}, err
	}

	// Try kernel
	kp, err := kern.GetProgramByID(ctx, programID)
	if err == nil {
		row.Kernel = &kp
		row.Presence.InKernel = true
	}
	// Kernel errors (program not found) are not fatal - just means not in kernel

	// Try filesystem
	// If we have store metadata with a pin path, check that specific path
	if row.Managed != nil && row.Managed.Handles.PinPath != "" {
		if scanner.PathExists(row.Managed.Handles.PinPath.String()) {
			row.FSPinPath = row.Managed.Handles.PinPath.String()
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

func dispatcherKey(dispType string, nsid uint64, ifindex uint32) string {
	return dispType + "/" + strconv.FormatUint(nsid, 10) + "/" + strconv.FormatUint(uint64(ifindex), 10)
}
