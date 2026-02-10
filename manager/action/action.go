package action

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/dispatcher"
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
	KernelID uint32
	Metadata bpfman.ProgramRecord
}

func (SaveProgram) isAction() {}

// DeleteProgram removes program metadata from the store.
type DeleteProgram struct {
	KernelID uint32
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
	LinkID bpfman.LinkID
}

func (DeleteLink) isAction() {}

// Kernel actions - operations on the BPF subsystem

// GetProgramFromStore fetches a program record from the store by
// kernel ID. Returns bpfman.ProgramRecord via ExecuteResult.
type GetProgramFromStore struct {
	KernelID uint32
}

func (GetProgramFromStore) isAction() {}

// CheckProgramNotInStore verifies that no program with the given
// kernel ID exists in the store. Returns an error if it does.
type CheckProgramNotInStore struct {
	KernelID uint32
}

func (CheckProgramNotInStore) isAction() {}

// LoadProgram loads a BPF program into the kernel and returns
// the LoadOutput via ExecuteResult.
type LoadProgram struct {
	Spec  bpfman.LoadSpec
	BPFFS bpfmanfs.BPFFS
}

func (LoadProgram) isAction() {}

// UnloadProgram removes a BPF program from the kernel.
type UnloadProgram struct {
	PinPath string
}

func (UnloadProgram) isAction() {}

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

// Batch groups multiple actions to be executed together.
type Batch struct {
	Actions []Action
}

func (Batch) isAction() {}

// Sequence executes actions in order, stopping on first error.
type Sequence struct {
	Actions []Action
}

func (Sequence) isAction() {}

// Dispatcher actions - operations on dispatcher state

// SaveDispatcher creates or updates a dispatcher in the store.
type SaveDispatcher struct {
	State dispatcher.State
}

func (SaveDispatcher) isAction() {}

// DeleteDispatcher removes a dispatcher from the store.
type DeleteDispatcher struct {
	Type    string
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
	KernelID   uint32
	SourcePath string
	Provenance bpfmanfs.Provenance
}

func (PublishBytecode) isAction() {}

// RemoveProgramDir removes the persisted bytecode directory for a program.
type RemoveProgramDir struct {
	KernelID uint32
}

func (RemoveProgramDir) isAction() {}
