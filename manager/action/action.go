package action

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
)

// Action represents an effect to be executed.
// Actions are data - they describe what to do, not how.
type Action interface {
	isAction()
}

// Store actions - operations on the metadata store

// SaveProgram saves program metadata to the store.
type SaveProgram struct {
	ProgramID kernel.ProgramID
	Metadata  bpfman.ProgramRecord
}

func (SaveProgram) isAction() {}

// DeleteProgram removes program metadata from the store.
type DeleteProgram struct {
	ProgramID kernel.ProgramID
}

func (DeleteProgram) isAction() {}

// Link actions - operations on link metadata

// SaveLink saves a link to the store.
// Record.ID is the primary key: kernel-assigned for real BPF links,
// or bpfman-assigned synthetic ID (0x80000000+) for perf_event-based links.
type SaveLink struct {
	Record bpfman.LinkRecord
}

func (SaveLink) isAction() {}

// DeleteLink removes a link from the store by link ID.
type DeleteLink struct {
	LinkID kernel.LinkID
}

func (DeleteLink) isAction() {}

// Kernel actions - operations on the BPF subsystem

// GetProgramFromStore fetches a program record from the store by
// program ID. Returns bpfman.ProgramRecord via ExecuteResult.
type GetProgramFromStore struct {
	ProgramID kernel.ProgramID
}

func (GetProgramFromStore) isAction() {}

// CheckProgramNotInStore verifies that no program with the given
// program ID exists in the store. Returns an error if it does.
type CheckProgramNotInStore struct {
	ProgramID kernel.ProgramID
}

func (CheckProgramNotInStore) isAction() {}

// LoadProgram loads a BPF program into the kernel and returns
// the LoadOutput via ExecuteResult.
type LoadProgram struct {
	Spec  bpfman.LoadSpec
	BPFFS fs.BPFFS
}

func (LoadProgram) isAction() {}

// UnloadProgram removes a BPF program from the kernel.
type UnloadProgram struct {
	PinPath bpfman.ProgPinPath
}

func (UnloadProgram) isAction() {}

// RemoveMapsPins removes BPF map pins from the kernel.
type RemoveMapsPins struct {
	PinPath string
}

func (RemoveMapsPins) isAction() {}

// Attach actions - kernel attach operations that produce AttachOutput

// AttachTracepoint attaches a pinned program to a kernel tracepoint.
type AttachTracepoint struct {
	ProgPinPath bpfman.ProgPinPath
	Group       string
	Name        string
	LinkPinPath bpfman.LinkPath
}

func (AttachTracepoint) isAction() {}

// AttachKprobe attaches a pinned program to a kernel function.
// If Retprobe is true, attaches as a kretprobe.
type AttachKprobe struct {
	ProgPinPath bpfman.ProgPinPath
	FnName      string
	Offset      uint64
	Retprobe    bool
	LinkPinPath bpfman.LinkPath
}

func (AttachKprobe) isAction() {}

// AttachUprobeLocal attaches a pinned program to a user-space function
// in the current namespace.
type AttachUprobeLocal struct {
	ProgPinPath bpfman.ProgPinPath
	Target      string
	FnName      string
	Offset      uint64
	Retprobe    bool
	LinkPinPath bpfman.LinkPath
}

func (AttachUprobeLocal) isAction() {}

// AttachUprobeContainer attaches a pinned program to a user-space
// function in a container's mount namespace. Requires a WriterScope
// to pass the lock fd to the helper subprocess.
type AttachUprobeContainer struct {
	Scope        lock.WriterScope
	ProgPinPath  bpfman.ProgPinPath
	Target       string
	FnName       string
	Offset       uint64
	Retprobe     bool
	LinkPinPath  bpfman.LinkPath
	ContainerPid int32
}

func (AttachUprobeContainer) isAction() {}

// AttachFentry attaches a pinned program to a kernel function entry point.
type AttachFentry struct {
	ProgPinPath bpfman.ProgPinPath
	FnName      string
	LinkPinPath bpfman.LinkPath
}

func (AttachFentry) isAction() {}

// AttachFexit attaches a pinned program to a kernel function exit point.
type AttachFexit struct {
	ProgPinPath bpfman.ProgPinPath
	FnName      string
	LinkPinPath bpfman.LinkPath
}

func (AttachFexit) isAction() {}

// Dispatcher actions - operations on dispatcher state

// DeleteDispatcher removes a dispatcher and all its extension link
// records from the store by attach point key.
type DeleteDispatcher struct {
	Type    dispatcher.DispatcherType
	Nsid    uint64
	Ifindex uint32
}

func (DeleteDispatcher) isAction() {}

// Kernel link actions - operations on kernel links

// DetachLink tears down a kernel-attached BPF link synchronously
// (BPF_LINK_DETACH) and removes its bpffs pin. The PinPath field is
// typed bpfman.LinkPath so the action cannot be invoked on an
// arbitrary path; only layout helpers that produce link pin paths
// can satisfy the type. This makes it a build error to feed a
// non-link path here, and conversely to feed a link path to the
// program-pin removal path on KernelOperations (RemovePin takes a
// bpfman.ProgPinPath, which is plain os.Remove and would leave a
// kernel link live until RCU teardown completes).
type DetachLink struct {
	PinPath bpfman.LinkPath
}

func (DetachLink) isAction() {}

// DetachTCFilter removes a legacy TC BPF filter via netlink.
// Used to detach TC dispatchers which are attached as clsact filters
// rather than BPF links.
type DetachTCFilter struct {
	Ifindex  int
	Ifname   string
	Parent   uint32 // ingress or egress parent handle
	Priority uint16
	Handle   uint32
}

func (DetachTCFilter) isAction() {}

// PublishBytecode copies a BPF object file to the per-program
// bytecode directory and writes provenance metadata alongside it.
type PublishBytecode struct {
	ProgramID  kernel.ProgramID
	SourcePath string
	Provenance fs.Provenance
}

func (PublishBytecode) isAction() {}

// RemoveProgramDir removes a program bytecode directory by path
// via Bytecode.RemoveProgramDir.
type RemoveProgramDir struct {
	Path string
}

func (RemoveProgramDir) isAction() {}

// GC cleanup actions -- validated filesystem removal operations
// routed through fs.BPFFS and fs.Bytecode typed deletion methods.

// RemoveProgPin removes a program pin via BPFFS.RemoveProgPin.
type RemoveProgPin struct {
	Path bpfman.ProgPinPath
}

func (RemoveProgPin) isAction() {}

// RemoveLinkDir removes a link directory via BPFFS.RemoveLinkDir.
type RemoveLinkDir struct {
	Path bpfman.LinkDir
}

func (RemoveLinkDir) isAction() {}

// RemoveMapDir removes a map directory via BPFFS.RemoveMapDir.
type RemoveMapDir struct {
	Path bpfman.MapDir
}

func (RemoveMapDir) isAction() {}

// RemoveDispatcherProgPin removes a dispatcher program pin via
// BPFFS.RemoveDispatcherProgPin.
type RemoveDispatcherProgPin struct {
	Path bpfman.ProgPinPath
}

func (RemoveDispatcherProgPin) isAction() {}

// RemoveDispatcherRevDir removes a dispatcher revision directory via
// BPFFS.RemoveDispatcherRevDir.
type RemoveDispatcherRevDir struct {
	Path bpfman.DispatcherRevDir
}

func (RemoveDispatcherRevDir) isAction() {}

// RemoveDispatcherLinkPin removes a dispatcher link pin via
// BPFFS.RemoveDispatcherLinkPin.
type RemoveDispatcherLinkPin struct {
	Path bpfman.LinkPath
}

func (RemoveDispatcherLinkPin) isAction() {}

// RemoveStagingDir removes a staging directory via
// Bytecode.RemoveStagingDir.
type RemoveStagingDir struct {
	Path string
}

func (RemoveStagingDir) isAction() {}

// AttachTCX attaches a pinned program to an interface using the
// kernel-native TCX multi-program mechanism. Returns
// bpfman.AttachOutput via ExecuteResult.
type AttachTCX struct {
	Ifindex     int
	Direction   string
	ProgPinPath bpfman.ProgPinPath
	LinkPinPath bpfman.LinkPath
	NetnsPath   string
	Order       bpfman.TCXAttachOrder
}

func (AttachTCX) isAction() {}

// RemoveDispatcher removes a dispatcher from the kernel, the bpffs,
// and the store. The action is the single domain-level intent for
// dispatcher teardown: the executor owns the type-specific recipe
// (XDP link detach vs. TC filter delete) and the ordering between
// kernel detach and filesystem cleanup. A no-op when extension
// links remain.
type RemoveDispatcher struct {
	Key dispatcher.Key
}

func (RemoveDispatcher) isAction() {}

// Shared map pin actions - reference-counted cleanup for PinByName maps

// SaveSharedMapPins records that a program uses the named shared maps.
type SaveSharedMapPins struct {
	ProgramID kernel.ProgramID
	MapNames  []string
}

func (SaveSharedMapPins) isAction() {}

// CleanupSharedMapPins removes a program's shared map pin entries
// from the store and deletes the filesystem pins for any maps that
// are no longer referenced by other programs.
type CleanupSharedMapPins struct {
	ProgramID kernel.ProgramID
}

func (CleanupSharedMapPins) isAction() {}

// RemoveSharedMapPin removes a shared map pin file from the
// filesystem. Used by GC rules for orphan cleanup.
type RemoveSharedMapPin struct {
	Path bpfman.MapPinPath
}

func (RemoveSharedMapPin) isAction() {}

// Deep dispatcher actions - cross-subsystem operations that the
// executor handles internally (kernel + store transactions with
// rollback). These replace direct manager method calls for dispatcher
// attach, moving all cross-subsystem complexity behind the opcode
// boundary.

// RebuildXDPDispatcher triggers a full dispatcher rebuild for XDP.
// This handles both first-attach (no dispatcher exists) and
// subsequent-attach (dispatcher exists, rebuild all extensions).
// Returns extensionResult via ExecuteResult.
type RebuildXDPDispatcher struct {
	ProgramID   kernel.ProgramID
	Ifindex     uint32
	Ifname      string
	NetnsPath   string
	ProgPinPath bpfman.ProgPinPath
	ProgramName string
	Priority    int
	ProceedOn   uint32
}

func (RebuildXDPDispatcher) isAction() {}

// RebuildTCDispatcher triggers a full dispatcher rebuild for TC.
// Same semantics as RebuildXDPDispatcher but for TC dispatchers.
// Returns extensionResult via ExecuteResult.
type RebuildTCDispatcher struct {
	ProgramID   kernel.ProgramID
	Ifindex     uint32
	Ifname      string
	Direction   bpfman.TCDirection
	DispType    dispatcher.DispatcherType
	NetnsPath   string
	ProgPinPath bpfman.ProgPinPath
	ProgramName string
	Priority    int
	ProceedOn   uint32
}

func (RebuildTCDispatcher) isAction() {}

// RebuildDispatcherForDetach triggers a full dispatcher rebuild after
// an extension has been detached. ExcludeLinkID identifies the member
// being detached; the rebuild filters it out before deciding whether
// to rebuild with remaining members or remove the empty dispatcher.
type RebuildDispatcherForDetach struct {
	Key           dispatcher.Key
	ExcludeLinkID kernel.LinkID
}

func (RebuildDispatcherForDetach) isAction() {}
