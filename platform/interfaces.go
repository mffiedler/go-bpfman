package platform

import (
	"context"
	"errors"
	"io"
	"iter"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// ErrRecordNotFound is returned when a store lookup by ID finds no
// matching row.
var ErrRecordNotFound = errors.New("record not found")

// ErrInterfaceNotFound marks a failure to resolve a network interface
// name (in its netns) to an ifindex -- an unknown interface or an
// unreachable netns path. Both stem from caller-supplied input, so a
// front-end can map it to an invalid-argument status rather than an
// internal error. InterfaceResolver implementations wrap it.
var ErrInterfaceNotFound = errors.New("interface not found")

// LinkWriter writes standalone link metadata to the store.
// Dispatcher-backed XDP/TC member links are persisted and removed
// through DispatcherStore snapshot operations, not through LinkWriter.
type LinkWriter interface {
	// SaveLink persists a standalone link record and its
	// type-specific details. Dispatcher-backed XDP/TC member links
	// are persisted through DispatcherStore.ReplaceDispatcherSnapshot,
	// not through SaveLink.
	SaveLink(ctx context.Context, record bpfman.LinkRecord) error

	// DeleteLink removes a standalone link record. Returns an
	// error if the link is dispatcher-backed (XDP/TC); those must
	// be removed via DispatcherStore lifecycle operations.
	DeleteLink(ctx context.Context, linkID kernel.LinkID) error
}

// LinkReader reads link metadata from the store.
// GetLink performs a two-phase lookup: registry then type-specific details.
type LinkReader interface {
	GetLink(ctx context.Context, linkID kernel.LinkID) (bpfman.LinkRecord, error)
}

// LinkLister lists links from the store.
type LinkLister interface {
	ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error)
	ListLinksByProgram(ctx context.Context, programID kernel.ProgramID) ([]bpfman.LinkRecord, error)
	// ListTCXLinksByInterface returns all TCX links for a given interface/direction/namespace.
	// Used for computing attach order based on priority.
	ListTCXLinksByInterface(ctx context.Context, nsid uint64, ifindex uint32, direction string) ([]bpfman.TCXLinkInfo, error)
}

// LinkStore combines all link store operations.
type LinkStore interface {
	LinkWriter
	LinkReader
	LinkLister
}

// DispatcherStore manages dispatcher state.
type DispatcherStore interface {
	// GetDispatcherSnapshot retrieves a complete snapshot of a
	// dispatcher and all its extension members, identified by key.
	// Returns ErrRecordNotFound if the dispatcher does not exist.
	GetDispatcherSnapshot(ctx context.Context, key dispatcher.Key) (DispatcherSnapshot, error)

	// ListDispatcherSummaries returns lightweight summaries of all
	// dispatchers, including member counts. This replaces the N+1
	// pattern of ListDispatchers + CountDispatcherLinks per dispatcher.
	ListDispatcherSummaries(ctx context.Context) ([]DispatcherSummary, error)

	// ReplaceDispatcherSnapshot atomically replaces all persisted
	// state for a dispatcher's attach point. The snapshot must
	// contain all members (existing and new). Old extension link
	// records for the attach point are removed and replaced with
	// the snapshot's members in a single transaction.
	ReplaceDispatcherSnapshot(ctx context.Context, snap DispatcherSnapshot) error

	// DeleteDispatcherSnapshot removes a dispatcher and all its
	// extension link records by attach point key.
	DeleteDispatcherSnapshot(ctx context.Context, key dispatcher.Key) error
}

// SharedMapPinStore tracks which programs reference shared PinByName
// map pins, enabling reference-counted cleanup on unload.
type SharedMapPinStore interface {
	// SaveSharedMapPins records that the given program uses the
	// named shared maps. Duplicate entries are silently ignored.
	SaveSharedMapPins(ctx context.Context, programID kernel.ProgramID, mapNames []string) error

	// DeleteSharedMapPins removes a program's shared map pin
	// entries and returns the map names that are no longer
	// referenced by any program (orphaned). The caller is
	// responsible for removing the corresponding filesystem pins.
	DeleteSharedMapPins(ctx context.Context, programID kernel.ProgramID) (orphanedMaps []string, err error)

	// ListReferencedSharedMaps returns all shared map names that
	// are still referenced by at least one program. Used by the
	// GC gather phase to detect filesystem orphans.
	ListReferencedSharedMaps(ctx context.Context) ([]string, error)
}

// Store combines program, link, dispatcher, and shared map pin store operations.
type Store interface {
	io.Closer
	ProgramStore
	LinkStore
	DispatcherStore
	SharedMapPinStore
	Transactional
}

// Transactional provides atomic execution of store operations.
// The callback receives a Store that participates in the transaction.
// If the callback returns nil, the transaction commits.
// If the callback returns an error, the transaction rolls back.
//
// name identifies the transaction class for instrumentation: the
// store-side timing logs (wait_ms, held_ms) carry it as the tx
// field so log queries can group throughput and tail latency by
// transaction kind ("load", "cleanup_shared_map_pins", etc.)
// rather than seeing every transaction as a single anonymous
// workload. Use snake_case names that describe what the
// transaction does, not the calling context's internal phase
// organisation.
type Transactional interface {
	RunInTransaction(ctx context.Context, name string, fn func(Store) error) error
}

// ProgramReader reads program metadata from the store.
// Get returns ErrRecordNotFound if the program does not exist.
type ProgramReader interface {
	Get(ctx context.Context, programID kernel.ProgramID) (bpfman.ProgramRecord, error)
}

// ProgramWriter writes program metadata to the store.
type ProgramWriter interface {
	Save(ctx context.Context, programID kernel.ProgramID, metadata bpfman.ProgramRecord) error
	Delete(ctx context.Context, programID kernel.ProgramID) error
}

// ProgramLister lists all program metadata from the store.
type ProgramLister interface {
	List(ctx context.Context) (map[kernel.ProgramID]bpfman.ProgramRecord, error)
}

// ProgramFinder finds programs by criteria.
// This interface is currently empty but retained for future extensions.
type ProgramFinder interface {
}

// MapOwnershipReader provides access to map ownership information.
type MapOwnershipReader interface {
	// CountDependentPrograms returns the number of programs that share maps
	// with the given program (i.e., programs where map_owner_id = programID).
	// This is used to determine if a program's maps can be safely deleted.
	CountDependentPrograms(ctx context.Context, programID kernel.ProgramID) (int, error)
}

// ProgramStore combines all store operations.
type ProgramStore interface {
	ProgramReader
	ProgramWriter
	ProgramLister
	ProgramFinder
	MapOwnershipReader
}

// KernelSource provides access to kernel BPF objects.
type KernelSource interface {
	Programs(ctx context.Context) iter.Seq2[kernel.Program, error]
	GetProgramByID(ctx context.Context, id kernel.ProgramID) (kernel.Program, error)
	GetProgramStatsByID(ctx context.Context, id kernel.ProgramID) (*kernel.ProgramStats, error)
	GetLinkByID(ctx context.Context, id kernel.LinkID) (kernel.Link, error)
	GetMapByID(ctx context.Context, id kernel.MapID) (kernel.Map, error)
	Maps(ctx context.Context) iter.Seq2[kernel.Map, error]
	Links(ctx context.Context) iter.Seq2[kernel.Link, error]
}

// ProgramLoader loads BPF programs into the kernel.
type ProgramLoader interface {
	// Load loads a BPF program and pins it using the bpffs paths.
	// Pin paths are computed from the kernel ID using bpffs methods:
	//   - Program: bpffs.ProgPinPath(program_id)
	//   - Maps: bpffs.MapPinDir(program_id) / <map_name>
	Load(ctx context.Context, spec bpfman.LoadSpec, bpffs fs.BPFFS) (bpfman.LoadOutput, error)

	// HasPinByName reports whether the bytecode referenced by spec
	// declares any LIBBPF_PIN_BY_NAME maps. The manager calls this
	// before the per-program load loop to decide whether to
	// acquire the cross-process writer lock for the load: shared
	// pin paths are the one resource that two concurrent loaders
	// can race on, so loads that touch them serialise while loads
	// without them stay lockless.
	HasPinByName(spec bpfman.LoadSpec) (bool, error)
}

// ProgramUnloader removes BPF programs from the kernel.
type ProgramUnloader interface {
	Unload(ctx context.Context, pinPath string) error
	// UnloadProgram removes a program and its maps using the upstream pin layout.
	UnloadProgram(ctx context.Context, progPinPath bpfman.ProgPinPath, mapsDir string) error
}

// PinInspector provides raw inspection of bpffs pins.
type PinInspector interface {
	// ListPinDir scans a bpffs directory and returns its contents.
	ListPinDir(ctx context.Context, pinDir string, includeMaps bool) (*kernel.PinDirContents, error)
	// GetPinned loads and returns info about a pinned program.
	GetPinned(ctx context.Context, pinPath string) (*kernel.PinnedProgram, error)
}

// ProgramAttacher attaches programs to hooks.
// All methods return AttachOutput (raw kernel result) rather than Link,
// allowing the manager to construct LinkRecord from AttachSpec + AttachOutput.
type ProgramAttacher interface {
	// AttachTracepoint attaches a pinned program to a tracepoint.
	AttachTracepoint(ctx context.Context, progPinPath bpfman.ProgPinPath, group, name string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error)
	// AttachXDP attaches a pinned XDP program to a network interface.
	AttachXDP(ctx context.Context, progPinPath bpfman.ProgPinPath, ifindex int, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error)
	// AttachKprobe attaches a pinned program to a kernel function.
	// If retprobe is true, attaches as a kretprobe instead of kprobe.
	AttachKprobe(ctx context.Context, progPinPath bpfman.ProgPinPath, fnName string, offset uint64, retprobe bool, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error)
	// AttachUprobeLocal attaches a pinned program to a user-space function
	// in the current namespace. Does not spawn a helper, so no lock scope needed.
	// target is the path to the binary or library (e.g., /usr/lib/libc.so.6).
	// If retprobe is true, attaches as a uretprobe instead of uprobe.
	AttachUprobeLocal(ctx context.Context, progPinPath bpfman.ProgPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error)
	// AttachUprobeContainer attaches a pinned program to a user-space function
	// in a container's mount namespace. Spawns bpfman-ns helper, so requires
	// lock scope to pass fd.
	// target is the path to the binary or library (resolved in the container's namespace).
	// If retprobe is true, attaches as a uretprobe instead of uprobe.
	// containerPid identifies the target container.
	AttachUprobeContainer(ctx context.Context, scope lock.WriterScope, progPinPath bpfman.ProgPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath bpfman.LinkPath, containerPid int32) (bpfman.AttachOutput, error)
	// AttachFentry attaches a pinned program to a kernel function entry point.
	// The fnName was specified at load time and stored with the program.
	AttachFentry(ctx context.Context, progPinPath bpfman.ProgPinPath, fnName string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error)
	// AttachFexit attaches a pinned program to a kernel function exit point.
	// The fnName was specified at load time and stored with the program.
	AttachFexit(ctx context.Context, progPinPath bpfman.ProgPinPath, fnName string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error)
}

// XDPDispatcherResult holds the result of loading an XDP dispatcher.
type XDPDispatcherResult struct {
	DispatcherID  kernel.ProgramID   // Kernel program ID of the dispatcher
	LinkID        kernel.LinkID      // Kernel link ID
	DispatcherPin bpfman.ProgPinPath // Pin path for dispatcher program
	LinkPin       bpfman.LinkPath    // Pin path for link
}

// TCDispatcherResult holds the result of loading a TC dispatcher.
// Legacy TC uses netlink (clsact qdisc + tc filter) rather than BPF
// links, so there is no link ID or link pin. Instead the kernel
// assigns a handle that identifies the filter for later removal.
type TCDispatcherResult struct {
	DispatcherID  kernel.ProgramID   // Kernel program ID of the dispatcher
	DispatcherPin bpfman.ProgPinPath // Pin path for dispatcher program
	Handle        uint32             // Kernel-assigned tc filter handle
	Priority      uint16             // tc filter priority (typically 50)
}

// ExtensionLinkInfo is the kernel-reported state of a pinned freplace
// extension link, read via BPF_LINK_GET_INFO_BY_FD. Diagnostic; used
// to verify each freplace's trampoline is observably installed before
// the dispatcher swap.
type ExtensionLinkInfo struct {
	LinkID       kernel.LinkID    // Kernel link ID
	TargetProgID kernel.ProgramID // Kernel program ID of the dispatcher being replaced into
	TargetBtfID  uint32           // BTF type ID of the stub function being replaced
	AttachType   uint32           // Kernel attach type
}

// DispatcherAttacher attaches dispatcher programs for multi-program chaining.
type DispatcherAttacher interface {
	// AttachXDPDispatcher loads and attaches an XDP dispatcher to an interface.
	// The dispatcher allows multiple XDP programs to be chained together.
	// Uses .rodata-based config baked in at load time.
	AttachXDPDispatcher(ctx context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*XDPDispatcherResult, error)

	// AttachXDPExtension attaches a pinned Extension program to a
	// dispatcher slot via freplace link.
	AttachXDPExtension(ctx context.Context, spec dispatcher.XDPExtensionAttachSpec) (bpfman.AttachOutput, error)

	// AttachTCDispatcher loads and attaches a TC dispatcher to an interface
	// using legacy netlink TC (clsact qdisc + BPF tc filter). This matches
	// the upstream Rust bpfman approach and is visible to tc(8) tooling.
	// Uses .rodata-based config baked in at load time.
	AttachTCDispatcher(ctx context.Context, spec dispatcher.TCDispatcherAttachSpec) (*TCDispatcherResult, error)

	// AttachTCExtension attaches a pinned Extension program to a TC
	// dispatcher slot via freplace link.
	AttachTCExtension(ctx context.Context, spec dispatcher.TCExtensionAttachSpec) (bpfman.AttachOutput, error)

	// ExtensionLinkInfo reads BPF_LINK_GET_INFO_BY_FD on a pinned
	// freplace extension link and returns the kernel-reported
	// trampoline target. Diagnostic; used to verify each freplace
	// is observably installed before swapping the dispatcher.
	ExtensionLinkInfo(ctx context.Context, linkPinPath bpfman.LinkPath) (ExtensionLinkInfo, error)

	// UpdateXDPDispatcherLink atomically updates an existing XDP
	// dispatcher BPF link to point to a new dispatcher program.
	// Used during rebuild to swap from old to new dispatcher.
	UpdateXDPDispatcherLink(ctx context.Context, linkPinPath bpfman.LinkPath, newProgPinPath bpfman.ProgPinPath) error

	// LoadAndPinXDPDispatcher loads an XDP dispatcher program with
	// the given .rodata config and pins it at progPinPath. Does not
	// create an XDP link. Returns the kernel program ID.
	LoadAndPinXDPDispatcher(ctx context.Context, cfg dispatcher.XDPConfig, progPinPath bpfman.ProgPinPath) (kernel.ProgramID, error)

	// LoadAndPinTCDispatcher loads a TC dispatcher program with
	// the given .rodata config and pins it at progPinPath. Does not
	// create a TC filter. Returns the kernel program ID.
	LoadAndPinTCDispatcher(ctx context.Context, cfg dispatcher.TCConfig, progPinPath bpfman.ProgPinPath) (kernel.ProgramID, error)

	// CreateXDPLink creates an XDP link from a pinned dispatcher
	// program to a network interface, optionally in a specific
	// network namespace. Returns the link info.
	CreateXDPLink(ctx context.Context, progPinPath bpfman.ProgPinPath, ifindex int, linkPinPath bpfman.LinkPath, netnsPath string) (*XDPDispatcherResult, error)

	// CreateTCFilter creates a TC filter from a pinned dispatcher
	// program on a network interface, optionally in a specific
	// network namespace. Creates the clsact qdisc if needed.
	CreateTCFilter(ctx context.Context, progPinPath bpfman.ProgPinPath, ifindex int, ifname string, direction bpfman.TCDirection, netnsPath string) (*TCDispatcherResult, error)

	// AttachTCX attaches a loaded program directly to an interface using TCX link.
	// Unlike TC which uses dispatchers, TCX uses native kernel multi-program support.
	// The program must already be pinned at programPinPath.
	//
	// Parameters:
	//   - ifindex: Network interface index
	//   - direction: "ingress" or "egress"
	//   - programPinPath: Path where the program is pinned
	//   - linkPinPath: Path to pin the TCX link
	//   - netns: Optional network namespace path. If non-empty, attachment is performed in that namespace.
	//   - order: Specifies where to insert the program in the TCX chain based on priority.
	AttachTCX(ctx context.Context, ifindex int, direction string, programPinPath bpfman.ProgPinPath, linkPinPath bpfman.LinkPath, netns string, order bpfman.TCXAttachOrder) (bpfman.AttachOutput, error)
}

// LinkDetacher detaches links from hooks.
type LinkDetacher interface {
	// DetachLink removes a pinned link by deleting its pin from bpffs.
	// This releases the kernel link if it was the last reference.
	DetachLink(ctx context.Context, linkPinPath bpfman.LinkPath) error
}

// PinRemover removes program pins from bpffs.
type PinRemover interface {
	// RemovePin removes a program pin from bpffs. The bpfman.ProgPinPath
	// type ensures only program pin paths --- not link pins, map pins,
	// or arbitrary strings --- can be passed in. For a kernel-attached
	// BPF link, DetachLink is required because dropping the userland
	// reference does not synchronously detach the link from its
	// attach point. Returns nil if the path does not exist.
	RemovePin(ctx context.Context, p bpfman.ProgPinPath) error
}

// TCFilterDetacher removes legacy TC BPF filters via netlink.
type TCFilterDetacher interface {
	// DetachTCFilter removes a tc filter identified by ifindex, parent,
	// priority, and handle. This is the counterpart to the netlink-based
	// attachment performed by AttachTCDispatcher.
	DetachTCFilter(ctx context.Context, ifindex int, ifname string, parent uint32, priority uint16, handle uint32) error

	// FindTCFilterHandle looks up the kernel-assigned handle for a TC
	// BPF filter by listing filters on the given parent and matching
	// the specified priority.
	FindTCFilterHandle(ctx context.Context, ifindex int, parent uint32, priority uint16) (uint32, error)
}

// MapRepinner re-pins maps to new locations.
type MapRepinner interface {
	// RepinMap loads a pinned map and re-pins it to a new path.
	// Used by CSI to expose maps to per-pod bpffs.
	RepinMap(ctx context.Context, srcPath, dstPath string) error
}

// TracepointLister enumerates kernel tracepoints visible via tracefs.
type TracepointLister interface {
	// ListTracepoints returns all tracepoints as "group/name" strings
	// read from /sys/kernel/tracing/events/. Hidden tracefs metadata
	// files (enable, filter, header_page, etc.) are skipped. Returns
	// an empty slice if tracefs is unavailable; callers should treat
	// that as "cannot validate" rather than "no tracepoints exist".
	ListTracepoints(ctx context.Context) ([]string, error)
}

// KernelOperations combines all kernel operations.
type KernelOperations interface {
	KernelSource
	ProgramLoader
	ProgramUnloader
	PinInspector
	ProgramAttacher
	DispatcherAttacher
	LinkDetacher
	PinRemover
	MapRepinner
	TCFilterDetacher
	TracepointLister
	InterfaceResolver
}

// InterfaceResolver resolves a network interface name to its kernel
// ifindex within a network namespace. netnsPath is the path to the
// target namespace (for example /proc/<pid>/ns/net); an empty path
// resolves in the daemon's own namespace. Resolution must happen
// inside the target namespace because a name like a pod's "eth0"
// exists only there, not in the host. This is the single resolution
// boundary: the manager owns it, and the gRPC server and CLI pass
// interface names through untouched.
type InterfaceResolver interface {
	InterfaceByName(ctx context.Context, name, netnsPath string) (ifindex int, err error)
}

// ImageRef describes an OCI image to pull.
type ImageRef struct {
	URL        string
	PullPolicy bpfman.ImagePullPolicy
	Auth       *ImageAuth // nil for anonymous access
}

// ImageAuth contains credentials for authenticating to an OCI registry.
type ImageAuth struct {
	Username string
	Password string
}

// PulledImage is the result of successfully pulling an OCI image.
type PulledImage struct {
	// ObjectPath is the path to the extracted ELF bytecode file.
	ObjectPath string
	// Programs maps program names to their types from the io.ebpf.programs label.
	Programs map[string]string
	// Maps maps map names to their types from the io.ebpf.maps label.
	Maps map[string]string
	// URL is the OCI image reference that was pulled.
	URL string
	// Digest is the resolved image digest.
	Digest string
	// PullPolicy is the policy that was used when pulling.
	PullPolicy bpfman.ImagePullPolicy
}

// ImagePuller fetches BPF bytecode from OCI images.
type ImagePuller interface {
	// Pull downloads an image and returns the extracted bytecode.
	// The returned ObjectPath is valid until the puller is closed or
	// the cache is cleaned.
	Pull(ctx context.Context, ref ImageRef) (PulledImage, error)
}

// SignatureVerifier verifies OCI image signatures.
type SignatureVerifier interface {
	// Verify checks that the image has a valid signature.
	// Returns nil if verification succeeds or is not required.
	// Returns an error if the image signature is invalid or missing
	// (when unsigned images are not allowed).
	Verify(ctx context.Context, imageRef string) error
}

// DiscoveredProgram represents a program found in a BPF object file.
type DiscoveredProgram struct {
	Name        string
	SectionName string
	Type        bpfman.ProgramType
	AttachFunc  string // For fentry/fexit, extracted from section name (e.g., "fentry/vfs_read" -> "vfs_read")
}

// ProgramDiscoverer discovers programs in BPF object files.
type ProgramDiscoverer interface {
	// DiscoverPrograms scans a BPF object file and returns all loadable
	// programs. Programs with fentry/fexit types are skipped because they
	// require an explicit attach function.
	DiscoverPrograms(objectPath string) ([]DiscoveredProgram, error)

	// ValidatePrograms checks that all specified program names exist in
	// the object file. Returns an error listing missing programs.
	ValidatePrograms(objectPath string, programNames []string) error
}
