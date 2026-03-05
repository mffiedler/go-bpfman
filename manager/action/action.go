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
	PinPath string
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
	ProgPinPath string
	Group       string
	Name        string
	LinkPinPath string
}

func (AttachTracepoint) isAction() {}

// AttachKprobe attaches a pinned program to a kernel function.
// If Retprobe is true, attaches as a kretprobe.
type AttachKprobe struct {
	ProgPinPath string
	FnName      string
	Offset      uint64
	Retprobe    bool
	LinkPinPath string
}

func (AttachKprobe) isAction() {}

// AttachUprobeLocal attaches a pinned program to a user-space function
// in the current namespace.
type AttachUprobeLocal struct {
	ProgPinPath string
	Target      string
	FnName      string
	Offset      uint64
	Retprobe    bool
	LinkPinPath string
}

func (AttachUprobeLocal) isAction() {}

// AttachUprobeContainer attaches a pinned program to a user-space
// function in a container's mount namespace. Requires a WriterScope
// to pass the lock fd to the helper subprocess.
type AttachUprobeContainer struct {
	Scope        lock.WriterScope
	ProgPinPath  string
	Target       string
	FnName       string
	Offset       uint64
	Retprobe     bool
	LinkPinPath  string
	ContainerPid int32
}

func (AttachUprobeContainer) isAction() {}

// AttachFentry attaches a pinned program to a kernel function entry point.
type AttachFentry struct {
	ProgPinPath string
	FnName      string
	LinkPinPath string
}

func (AttachFentry) isAction() {}

// AttachFexit attaches a pinned program to a kernel function exit point.
type AttachFexit struct {
	ProgPinPath string
	FnName      string
	LinkPinPath string
}

func (AttachFexit) isAction() {}

// Dispatcher actions - operations on dispatcher state

// SaveDispatcher creates or updates a dispatcher in the store.
type SaveDispatcher struct {
	State dispatcher.State
}

func (SaveDispatcher) isAction() {}

// DeleteDispatcher removes a dispatcher from the store.
type DeleteDispatcher struct {
	Type    dispatcher.DispatcherType
	Nsid    uint64
	Ifindex uint32
}

func (DeleteDispatcher) isAction() {}

// Kernel link actions - operations on kernel links

// DetachLink removes a link pin from bpffs, releasing the kernel link.
type DetachLink struct {
	PinPath string
}

func (DetachLink) isAction() {}

// Filesystem actions - operations on bpffs pins

// RemovePin removes a pin from bpffs. Ignores "not exist" errors.
type RemovePin struct {
	Path string
}

func (RemovePin) isAction() {}

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

// RemoveProgramDir removes the persisted bytecode directory for a program.
type RemoveProgramDir struct {
	ProgramID kernel.ProgramID
}

func (RemoveProgramDir) isAction() {}

// GC cleanup actions -- validated filesystem removal operations
// routed through fs.BPFFS and fs.Bytecode typed deletion methods.

// RemoveProgPin removes a program pin via BPFFS.RemoveProgPin.
type RemoveProgPin struct {
	Path string
}

func (RemoveProgPin) isAction() {}

// RemoveLinkDir removes a link directory via BPFFS.RemoveLinkDir.
type RemoveLinkDir struct {
	Path string
}

func (RemoveLinkDir) isAction() {}

// RemoveMapDir removes a map directory via BPFFS.RemoveMapDir.
type RemoveMapDir struct {
	Path string
}

func (RemoveMapDir) isAction() {}

// RemoveDispatcherProgPin removes a dispatcher program pin via
// BPFFS.RemoveDispatcherProgPin.
type RemoveDispatcherProgPin struct {
	Path string
}

func (RemoveDispatcherProgPin) isAction() {}

// RemoveDispatcherRevDir removes a dispatcher revision directory via
// BPFFS.RemoveDispatcherRevDir.
type RemoveDispatcherRevDir struct {
	Path string
}

func (RemoveDispatcherRevDir) isAction() {}

// RemoveDispatcherLinkPin removes a dispatcher link pin via
// BPFFS.RemoveDispatcherLinkPin.
type RemoveDispatcherLinkPin struct {
	Path string
}

func (RemoveDispatcherLinkPin) isAction() {}

// RemoveProgramDirByPath removes a program bytecode directory by path
// via Bytecode.RemoveProgramDir.
type RemoveProgramDirByPath struct {
	Path string
}

func (RemoveProgramDirByPath) isAction() {}

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
	ProgPinPath string
	LinkPinPath string
	NetnsPath   string
	Order       bpfman.TCXAttachOrder
}

func (AttachTCX) isAction() {}

// CleanupEmptyDispatcher checks whether the given dispatcher has any
// remaining extension links and, if empty, removes it from both the
// kernel and the store. A no-op when extensions remain.
type CleanupEmptyDispatcher struct {
	State dispatcher.State
}

func (CleanupEmptyDispatcher) isAction() {}

// Deep dispatcher actions - cross-subsystem operations that the
// executor handles internally (kernel + store transactions with
// rollback). These replace direct manager method calls for dispatcher
// attach, moving all cross-subsystem complexity behind the opcode
// boundary.

// EnsureXDPDispatcher looks up an existing XDP dispatcher for the
// given interface, or creates one if none exists. Returns
// dispatcher.State via ExecuteResult.
type EnsureXDPDispatcher struct {
	Ifindex   uint32
	NetnsPath string
}

func (EnsureXDPDispatcher) isAction() {}

// EnsureTCDispatcher looks up an existing TC dispatcher for the given
// interface and direction, or creates one if none exists. Returns
// dispatcher.State via ExecuteResult.
type EnsureTCDispatcher struct {
	Ifindex   uint32
	Ifname    string
	Direction bpfman.TCDirection
	DispType  dispatcher.DispatcherType
	NetnsPath string
}

func (EnsureTCDispatcher) isAction() {}

// AttachXDPExtension attaches a user program as an extension to an
// XDP dispatcher slot. Includes stale-dispatcher recovery: if the
// first attempt fails with os.ErrNotExist, the dispatcher is
// recreated and the attach retried once. Returns extensionResult via
// ExecuteResult (package-internal type; callers use action.Produce).
type AttachXDPExtension struct {
	DispState   dispatcher.State
	NetnsPath   string
	ObjectPath  string
	ProgramName string
	MapPinDir   string
}

func (AttachXDPExtension) isAction() {}

// AttachTCExtension attaches a user program as an extension to a TC
// dispatcher slot. Same stale-dispatcher recovery as XDP. Returns
// extensionResult via ExecuteResult.
type AttachTCExtension struct {
	DispState   dispatcher.State
	Ifname      string
	Direction   bpfman.TCDirection
	DispType    dispatcher.DispatcherType
	NetnsPath   string
	ObjectPath  string
	ProgramName string
	MapPinDir   string
}

func (AttachTCExtension) isAction() {}
