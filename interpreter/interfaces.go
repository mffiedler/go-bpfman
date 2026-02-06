// Package interpreter contains interfaces and executors for effects.
// This is the only package that performs actual I/O.
package interpreter

import (
	"context"
	"io"
	"iter"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// LinkWriter writes link metadata to the store.
// SaveLink dispatches to the appropriate detail table based on spec.Details.Kind().
type LinkWriter interface {
	// SaveLink saves a link spec with its details.
	// spec.ID is the primary key (kernel-assigned for real BPF links,
	// or bpfman-assigned synthetic ID for perf_event-based links).
	SaveLink(ctx context.Context, spec bpfman.LinkSpec) error
	DeleteLink(ctx context.Context, linkID bpfman.LinkID) error
}

// LinkReader reads link metadata from the store.
// GetLink performs a two-phase lookup: registry then type-specific details.
type LinkReader interface {
	GetLink(ctx context.Context, linkID bpfman.LinkID) (bpfman.LinkSpec, error)
}

// LinkLister lists links from the store.
type LinkLister interface {
	ListLinks(ctx context.Context) ([]bpfman.LinkSpec, error)
	ListLinksByProgram(ctx context.Context, programKernelID uint32) ([]bpfman.LinkSpec, error)
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
	// GetDispatcher retrieves a dispatcher by type, nsid, and ifindex.
	// Returns store.ErrNotFound if the dispatcher does not exist.
	GetDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (dispatcher.State, error)

	// ListDispatchers returns all dispatchers.
	ListDispatchers(ctx context.Context) ([]dispatcher.State, error)

	// SaveDispatcher creates or updates a dispatcher.
	SaveDispatcher(ctx context.Context, state dispatcher.State) error

	// DeleteDispatcher removes a dispatcher by type, nsid, and ifindex.
	DeleteDispatcher(ctx context.Context, dispType string, nsid uint64, ifindex uint32) error

	// IncrementRevision atomically increments the dispatcher revision.
	// Returns the new revision number. Wraps from MaxUint32 to 1.
	IncrementRevision(ctx context.Context, dispType string, nsid uint64, ifindex uint32) (uint32, error)

	// CountDispatcherLinks returns the number of extension links
	// attached to the dispatcher identified by its kernel program ID.
	CountDispatcherLinks(ctx context.Context, dispatcherKernelID uint32) (int, error)
}

// GCResult contains statistics from garbage collection.
type GCResult struct {
	ProgramsRemoved    int
	DispatchersRemoved int
	LinksRemoved       int
	OrphanPinsRemoved  int
	// LiveOrphans counts programs pinned under bpfman's bpffs root
	// that are still alive in the kernel but have no DB record. GC
	// leaves these untouched because removing the pin would unload
	// a running program. Use 'bpfman doctor' for details.
	LiveOrphans int
}

// GarbageCollector removes stale entries from the store.
type GarbageCollector interface {
	// GC removes all stored entries (programs, dispatchers, links)
	// that don't exist in the provided kernel state. Handles internal
	// ordering constraints (e.g., dependent programs before map owners).
	GC(ctx context.Context, kernelProgramIDs, kernelLinkIDs map[uint32]bool) (GCResult, error)
}

// Store combines program, link, and dispatcher store operations.
type Store interface {
	io.Closer
	ProgramStore
	LinkStore
	DispatcherStore
	Transactional
	GarbageCollector
}

// Transactional provides atomic execution of store operations.
// The callback receives a Store that participates in the transaction.
// If the callback returns nil, the transaction commits.
// If the callback returns an error, the transaction rolls back.
type Transactional interface {
	RunInTransaction(ctx context.Context, fn func(Store) error) error
}

// ProgramReader reads program metadata from the store.
// Get returns store.ErrNotFound if the program does not exist.
type ProgramReader interface {
	Get(ctx context.Context, kernelID uint32) (bpfman.ProgramSpec, error)
}

// ProgramWriter writes program metadata to the store.
type ProgramWriter interface {
	Save(ctx context.Context, kernelID uint32, metadata bpfman.ProgramSpec) error
	Delete(ctx context.Context, kernelID uint32) error
}

// ProgramLister lists all program metadata from the store.
type ProgramLister interface {
	List(ctx context.Context) (map[uint32]bpfman.ProgramSpec, error)
}

// ProgramFinder finds programs by criteria.
// This interface is currently empty but retained for future extensions.
type ProgramFinder interface {
}

// MapOwnershipReader provides access to map ownership information.
type MapOwnershipReader interface {
	// CountDependentPrograms returns the number of programs that share maps
	// with the given program (i.e., programs where map_owner_id = kernelID).
	// This is used to determine if a program's maps can be safely deleted.
	CountDependentPrograms(ctx context.Context, kernelID uint32) (int, error)
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
	GetProgramByID(ctx context.Context, id uint32) (kernel.Program, error)
	GetLinkByID(ctx context.Context, id uint32) (kernel.Link, error)
	GetMapByID(ctx context.Context, id uint32) (kernel.Map, error)
	Maps(ctx context.Context) iter.Seq2[kernel.Map, error]
	Links(ctx context.Context) iter.Seq2[kernel.Link, error]
}

// ProgramLoader loads BPF programs into the kernel.
type ProgramLoader interface {
	// Load loads a BPF program and pins it to the specified bpffs root.
	// The bpffsRoot is the bpffs mount point (e.g., /run/bpfman/fs/).
	// Actual pin paths are computed from the kernel ID:
	//   - Program: <bpffsRoot>/prog_<kernel_id>
	//   - Maps: <bpffsRoot>/maps/<kernel_id>/<map_name>
	Load(ctx context.Context, spec bpfman.LoadSpec, bpffsRoot bpffs.Root) (bpfman.LoadOutput, error)
}

// ProgramUnloader removes BPF programs from the kernel.
type ProgramUnloader interface {
	Unload(ctx context.Context, pinPath string) error
	// UnloadProgram removes a program and its maps using the upstream pin layout.
	UnloadProgram(ctx context.Context, progPinPath, mapsDir string) error
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
// allowing the manager to construct LinkSpec from AttachSpec + AttachOutput.
type ProgramAttacher interface {
	// AttachTracepoint attaches a pinned program to a tracepoint.
	AttachTracepoint(ctx context.Context, progPinPath, group, name, linkPinPath string) (bpfman.AttachOutput, error)
	// AttachXDP attaches a pinned XDP program to a network interface.
	AttachXDP(ctx context.Context, progPinPath string, ifindex int, linkPinPath string) (bpfman.AttachOutput, error)
	// AttachKprobe attaches a pinned program to a kernel function.
	// If retprobe is true, attaches as a kretprobe instead of kprobe.
	AttachKprobe(ctx context.Context, progPinPath, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error)
	// AttachUprobeLocal attaches a pinned program to a user-space function
	// in the current namespace. Does not spawn a helper, so no lock scope needed.
	// target is the path to the binary or library (e.g., /usr/lib/libc.so.6).
	// If retprobe is true, attaches as a uretprobe instead of uprobe.
	AttachUprobeLocal(ctx context.Context, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.AttachOutput, error)
	// AttachUprobeContainer attaches a pinned program to a user-space function
	// in a container's mount namespace. Spawns bpfman-ns helper, so requires
	// lock scope to pass fd.
	// target is the path to the binary or library (resolved in the container's namespace).
	// If retprobe is true, attaches as a uretprobe instead of uprobe.
	// containerPid identifies the target container.
	AttachUprobeContainer(ctx context.Context, scope lock.WriterScope, progPinPath, target, fnName string, offset uint64, retprobe bool, linkPinPath string, containerPid int32) (bpfman.AttachOutput, error)
	// AttachFentry attaches a pinned program to a kernel function entry point.
	// The fnName was specified at load time and stored with the program.
	AttachFentry(ctx context.Context, progPinPath, fnName, linkPinPath string) (bpfman.AttachOutput, error)
	// AttachFexit attaches a pinned program to a kernel function exit point.
	// The fnName was specified at load time and stored with the program.
	AttachFexit(ctx context.Context, progPinPath, fnName, linkPinPath string) (bpfman.AttachOutput, error)
}

// XDPDispatcherResult holds the result of loading an XDP dispatcher.
type XDPDispatcherResult struct {
	DispatcherID  uint32 // Kernel program ID of the dispatcher
	LinkID        uint32 // Kernel link ID
	DispatcherPin string // Pin path for dispatcher program
	LinkPin       string // Pin path for link
}

// TCDispatcherResult holds the result of loading a TC dispatcher.
// Legacy TC uses netlink (clsact qdisc + tc filter) rather than BPF
// links, so there is no link ID or link pin. Instead the kernel
// assigns a handle that identifies the filter for later removal.
type TCDispatcherResult struct {
	DispatcherID  uint32 // Kernel program ID of the dispatcher
	DispatcherPin string // Pin path for dispatcher program
	Handle        uint32 // Kernel-assigned tc filter handle
	Priority      uint16 // tc filter priority (typically 50)
}

// DispatcherAttacher attaches dispatcher programs for multi-program chaining.
type DispatcherAttacher interface {
	// AttachXDPDispatcher loads and attaches an XDP dispatcher to an interface.
	// The dispatcher allows multiple XDP programs to be chained together.
	AttachXDPDispatcher(ctx context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*XDPDispatcherResult, error)

	// AttachXDPExtension loads a program from ELF as Extension type and attaches
	// it to a dispatcher slot. The program is loaded with BPF_PROG_TYPE_EXT
	// targeting the dispatcher's slot function.
	AttachXDPExtension(ctx context.Context, spec dispatcher.XDPExtensionAttachSpec) (bpfman.AttachOutput, error)

	// AttachTCDispatcher loads and attaches a TC dispatcher to an interface
	// using legacy netlink TC (clsact qdisc + BPF tc filter). This matches
	// the upstream Rust bpfman approach and is visible to tc(8) tooling.
	AttachTCDispatcher(ctx context.Context, spec dispatcher.TCDispatcherAttachSpec) (*TCDispatcherResult, error)

	// AttachTCExtension loads a program from ELF as Extension type and attaches
	// it to a TC dispatcher slot. The program is loaded with BPF_PROG_TYPE_EXT
	// targeting the dispatcher's slot function.
	AttachTCExtension(ctx context.Context, spec dispatcher.TCExtensionAttachSpec) (bpfman.AttachOutput, error)

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
	AttachTCX(ctx context.Context, ifindex int, direction, programPinPath, linkPinPath, netns string, order bpfman.TCXAttachOrder) (bpfman.AttachOutput, error)
}

// LinkDetacher detaches links from hooks.
type LinkDetacher interface {
	// DetachLink removes a pinned link by deleting its pin from bpffs.
	// This releases the kernel link if it was the last reference.
	DetachLink(ctx context.Context, linkPinPath string) error
}

// PinRemover removes pins from bpffs.
type PinRemover interface {
	// RemovePin removes a pin or empty directory from bpffs.
	// Returns nil if the path does not exist.
	RemovePin(ctx context.Context, path string) error
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
}

// ImageRef describes an OCI image to pull.
type ImageRef struct {
	// URL is the OCI image reference (e.g., "quay.io/bpfman-bytecode/xdp_pass:latest").
	URL string
	// PullPolicy specifies when to pull the image.
	PullPolicy bpfman.ImagePullPolicy
	// Auth contains optional authentication credentials. Nil for anonymous access.
	Auth *ImageAuth
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
	// Digest is the resolved image digest.
	Digest string
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
