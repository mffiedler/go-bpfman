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
	// ProgramID is the kernel ID of the program whose metadata this action persists.
	ProgramID kernel.ProgramID

	// Metadata is the program record to write under that ID.
	Metadata bpfman.ProgramRecord
}

func (SaveProgram) isAction() {}

// DeleteProgram removes program metadata from the store.
type DeleteProgram struct {
	// ProgramID is the kernel ID of the program whose metadata this action removes.
	ProgramID kernel.ProgramID
}

func (DeleteProgram) isAction() {}

// Link actions - operations on link metadata

// CreateLink saves a standalone link to the store and allocates a bpfman ID.
type CreateLink struct {
	// Spec is the requested link to persist; the store allocates the bpfman LinkID.
	Spec bpfman.LinkSpec
}

func (CreateLink) isAction() {}

// DeleteLink removes a link from the store by link ID.
type DeleteLink struct {
	// LinkID is the bpfman management handle of the link record this action removes.
	LinkID bpfman.LinkID
}

func (DeleteLink) isAction() {}

// CreatePendingLink saves a standalone link to the store before the
// kernel attach happens. The store allocates the bpfman link ID and
// records the pin path {LinksDir}/{link_id} in the same transaction,
// so no observable state has a bpffs pin the store does not name.
type CreatePendingLink struct {
	// Spec is the requested link to persist before the kernel attach;
	// the store allocates the bpfman LinkID.
	Spec bpfman.LinkSpec

	// LinksDir is the bpffs links directory under which the pin path
	// {LinksDir}/{link_id} is recorded in the same transaction.
	LinksDir string
}

func (CreatePendingLink) isAction() {}

// FinaliseLink records the captured kernel link ID on a pending link
// row created by CreatePendingLink, completing the attach.
type FinaliseLink struct {
	// LinkID is the bpfman management handle of the pending link to complete.
	LinkID bpfman.LinkID

	// KernelLinkID is the captured kernel bpf_link ID to record, or
	// nil when the attach observed none (stored as SQL NULL).
	KernelLinkID *kernel.LinkID
}

func (FinaliseLink) isAction() {}

// Kernel actions - operations on the BPF subsystem

// GetProgramFromStore fetches a program record from the store by
// program ID. Returns bpfman.ProgramRecord via ExecuteResult.
type GetProgramFromStore struct {
	// ProgramID is the kernel ID of the program record this action fetches.
	ProgramID kernel.ProgramID
}

func (GetProgramFromStore) isAction() {}

// CheckProgramNotInStore verifies that no program with the given
// program ID exists in the store. Returns an error if it does.
type CheckProgramNotInStore struct {
	// ProgramID is the kernel ID this action asserts is absent from the store.
	ProgramID kernel.ProgramID
}

func (CheckProgramNotInStore) isAction() {}

// LoadProgram loads a BPF program into the kernel and returns
// the LoadOutput via ExecuteResult.
type LoadProgram struct {
	// Spec is the load request describing the object, the programs to
	// load from it, and any global data to apply.
	Spec bpfman.LoadSpec

	// BPFFS is the bpffs layout into which the loaded program and its
	// maps are pinned.
	BPFFS fs.BPFFS
}

func (LoadProgram) isAction() {}

// UnloadProgram removes a BPF program from the kernel.
type UnloadProgram struct {
	// PinPath is the bpffs program pin whose removal unloads the program from the kernel.
	PinPath bpfman.ProgPinPath
}

func (UnloadProgram) isAction() {}

// RemoveMapsPins removes BPF map pins from the kernel.
type RemoveMapsPins struct {
	// PinPath is the per-program bpffs maps directory whose pinned maps this action removes.
	PinPath string
}

func (RemoveMapsPins) isAction() {}

// Attach actions - kernel attach operations that produce AttachOutput

// AttachTracepoint attaches a pinned program to a kernel tracepoint.
type AttachTracepoint struct {
	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// Group is the tracepoint group (the directory under events/).
	Group string

	// Name is the tracepoint name within the group.
	Name string

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath
}

func (AttachTracepoint) isAction() {}

// AttachKprobe attaches a pinned program to a kernel function.
// If Retprobe is true, attaches as a kretprobe.
type AttachKprobe struct {
	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// FnName is the kernel function the probe attaches to.
	FnName string

	// Offset is the byte offset from the function entry at which to attach; 0 means the entry itself.
	Offset uint64

	// Retprobe selects the return variant (kretprobe), firing on function return, when set.
	Retprobe bool

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath
}

func (AttachKprobe) isAction() {}

// AttachUprobeLocal attaches a pinned program to a user-space function
// in the current namespace. Pid > 0 scopes the probe to that process;
// 0 traces all processes.
type AttachUprobeLocal struct {
	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// Target is the executable or shared library holding the symbol
	// (absolute path or a name resolved on PATH).
	Target string

	// FnName is the symbol to attach by; empty selects offset-based attachment within Target.
	FnName string

	// Offset is the byte offset from the symbol entry at which to attach.
	Offset uint64

	// Pid restricts the probe to that process when > 0; 0 traces all processes.
	Pid int32

	// Retprobe selects the return variant (uretprobe), firing on function return, when set.
	Retprobe bool

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath
}

func (AttachUprobeLocal) isAction() {}

// AttachUprobeContainer attaches a pinned program to a user-space
// function in a container's mount namespace. Requires a WriterScope
// to pass the lock fd to the helper subprocess. Pid > 0 scopes the
// probe to that process (distinct from ContainerPid, which selects
// the mount namespace the target path resolves in); 0 traces all
// processes.
type AttachUprobeContainer struct {
	// Scope is the writer lock scope whose fd is duplicated and passed
	// to the namespace helper subprocess.
	Scope lock.WriterScope

	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// Target is the executable or shared library holding the symbol,
	// resolved within the container mount namespace selected by ContainerPid.
	Target string

	// FnName is the symbol to attach by; empty selects offset-based attachment within Target.
	FnName string

	// Offset is the byte offset from the symbol entry at which to attach.
	Offset uint64

	// Pid restricts the probe to that process when > 0; 0 traces all
	// processes. Distinct from ContainerPid, which selects the mount
	// namespace rather than scoping the probe.
	Pid int32

	// Retprobe selects the return variant (uretprobe), firing on function return, when set.
	Retprobe bool

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath

	// ContainerPid is the PID whose mount namespace the Target path is resolved in.
	ContainerPid int32
}

func (AttachUprobeContainer) isAction() {}

// AttachFentry attaches a pinned program to a kernel function entry point.
type AttachFentry struct {
	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// FnName is the kernel function whose entry the fentry program traces.
	FnName string

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath
}

func (AttachFentry) isAction() {}

// AttachFexit attaches a pinned program to a kernel function exit point.
type AttachFexit struct {
	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// FnName is the kernel function whose exit the fexit program traces.
	FnName string

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath
}

func (AttachFexit) isAction() {}

// Dispatcher actions - operations on dispatcher state

// DeleteDispatcher removes a dispatcher and all its extension link
// records from the store by attach point key.
type DeleteDispatcher struct {
	// Type is the dispatcher type (XDP, TC ingress, TC egress) identifying the attach point.
	Type dispatcher.DispatcherType

	// Nsid is the network namespace ID of the dispatcher to remove.
	Nsid uint64

	// Ifindex is the interface index of the dispatcher to remove.
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
	// PinPath is the bpffs link pin whose kernel link this action
	// detaches (BPF_LINK_DETACH) before removing the pin file.
	PinPath bpfman.LinkPath
}

func (DetachLink) isAction() {}

// DetachTCFilter removes a legacy TC BPF filter via netlink.
// Used to detach TC dispatchers which are attached as clsact filters
// rather than BPF links.
type DetachTCFilter struct {
	// Ifindex is the interface index carrying the TC filter to remove.
	Ifindex int

	// Ifname is the name of that interface.
	Ifname string

	// Parent is the TC parent handle (ingress or egress) the filter is attached to.
	Parent uint32

	// Priority is the filter priority identifying which filter to remove.
	Priority uint16

	// Handle is the kernel-assigned filter handle, pinpointing bpfman's
	// own filter rather than another sharing the priority.
	Handle uint32

	// NetnsPath is the network namespace path in which the interface lives.
	NetnsPath string
}

func (DetachTCFilter) isAction() {}

// PublishBytecode copies a BPF object file to the per-program
// bytecode directory and writes provenance metadata alongside it.
type PublishBytecode struct {
	// ProgramID is the kernel ID of the program whose bytecode directory receives the copy.
	ProgramID kernel.ProgramID

	// SourcePath is the path of the BPF object file to copy into that directory.
	SourcePath string

	// Provenance is the provenance metadata written alongside the copied object.
	Provenance fs.Provenance
}

func (PublishBytecode) isAction() {}

// RemoveProgramDir removes a program bytecode directory by path
// via Bytecode.RemoveProgramDir.
type RemoveProgramDir struct {
	// Path is the program bytecode directory to remove.
	Path string
}

func (RemoveProgramDir) isAction() {}

// GC cleanup actions -- validated filesystem removal operations
// routed through fs.BPFFS and fs.Bytecode typed deletion methods.

// RemoveProgPin removes a program pin via BPFFS.RemoveProgPin.
type RemoveProgPin struct {
	// Path is the bpffs program pin to remove.
	Path bpfman.ProgPinPath
}

func (RemoveProgPin) isAction() {}

// RemoveMapDir removes a map directory via BPFFS.RemoveMapDir.
type RemoveMapDir struct {
	// Path is the per-program maps directory to remove.
	Path bpfman.MapDir
}

func (RemoveMapDir) isAction() {}

// RemoveDispatcherProgPin removes a dispatcher program pin via
// BPFFS.RemoveDispatcherProgPin.
type RemoveDispatcherProgPin struct {
	// Path is the dispatcher program pin to remove.
	Path bpfman.ProgPinPath
}

func (RemoveDispatcherProgPin) isAction() {}

// RemoveDispatcherRevDir removes a dispatcher revision directory via
// BPFFS.RemoveDispatcherRevDir.
type RemoveDispatcherRevDir struct {
	// Path is the dispatcher revision directory to remove.
	Path bpfman.DispatcherRevDir
}

func (RemoveDispatcherRevDir) isAction() {}

// RemoveDispatcherLinkPin removes a dispatcher link pin via
// BPFFS.RemoveDispatcherLinkPin.
type RemoveDispatcherLinkPin struct {
	// Path is the dispatcher extension link pin to remove.
	Path bpfman.LinkPath
}

func (RemoveDispatcherLinkPin) isAction() {}

// RemoveStagingDir removes a staging directory via
// Bytecode.RemoveStagingDir.
type RemoveStagingDir struct {
	// Path is the staging directory to remove.
	Path string
}

func (RemoveStagingDir) isAction() {}

// AttachTCX attaches a pinned program to an interface using the
// kernel-native TCX multi-program mechanism. Returns
// bpfman.AttachOutput via ExecuteResult.
type AttachTCX struct {
	// Ifindex is the interface index to attach to.
	Ifindex int

	// Direction is the traffic direction (ingress or egress) to attach on.
	Direction string

	// ProgPinPath is the bpffs program pin of the program to attach.
	ProgPinPath bpfman.ProgPinPath

	// LinkPinPath is the bpffs path at which to pin the resulting link.
	LinkPinPath bpfman.LinkPath

	// NetnsPath is the network namespace path containing the interface.
	NetnsPath string

	// Order places the program in the kernel's TCX chain relative to existing programs.
	Order bpfman.TCXAttachOrder
}

func (AttachTCX) isAction() {}

// RemoveDispatcher removes a dispatcher from the kernel, the bpffs,
// and the store. The action is the single domain-level intent for
// dispatcher teardown: the executor owns the type-specific recipe
// (XDP link detach vs. TC filter delete) and the ordering between
// kernel detach and filesystem cleanup. A no-op when extension
// links remain.
type RemoveDispatcher struct {
	// Key identifies the dispatcher to tear down (type, nsid, ifindex).
	Key dispatcher.Key
}

func (RemoveDispatcher) isAction() {}

// Shared map pin actions - reference-counted cleanup for PinByName maps

// SaveSharedMapPins records that a program uses the named shared maps.
type SaveSharedMapPins struct {
	// ProgramID is the kernel ID of the program that references the named shared maps.
	ProgramID kernel.ProgramID

	// MapNames are the names of the shared maps the program references.
	MapNames []string
}

func (SaveSharedMapPins) isAction() {}

// CleanupSharedMapPins removes a program's shared map pin entries
// from the store and deletes the filesystem pins for any maps that
// are no longer referenced by other programs.
type CleanupSharedMapPins struct {
	// ProgramID is the kernel ID of the program whose shared map
	// references this action removes; maps left unreferenced are then
	// deleted from bpffs.
	ProgramID kernel.ProgramID
}

func (CleanupSharedMapPins) isAction() {}

// RemoveSharedMapPin removes a shared map pin file from the
// filesystem. Used by GC rules for orphan cleanup.
type RemoveSharedMapPin struct {
	// Path is the shared map pin file to remove.
	Path bpfman.MapPinPath
}

func (RemoveSharedMapPin) isAction() {}

// Deep dispatcher actions - cross-subsystem operations that the
// executor handles internally (kernel + store transactions with
// rollback).

// RebuildXDPDispatcher triggers a full dispatcher rebuild for XDP.
// This handles both first-attach (no dispatcher exists) and
// subsequent-attach (dispatcher exists, rebuild all extensions).
// Returns extensionResult via ExecuteResult.
type RebuildXDPDispatcher struct {
	// ProgramID is the kernel ID of the program being attached, added
	// as a new extension to the rebuilt dispatcher chain.
	ProgramID kernel.ProgramID

	// Ifindex is the interface index whose XDP dispatcher this action rebuilds.
	Ifindex uint32

	// Ifname is the name of that interface.
	Ifname string

	// NetnsPath is the network namespace path containing the interface.
	NetnsPath string

	// ProgPinPath is the bpffs program pin of the program being attached.
	ProgPinPath bpfman.ProgPinPath

	// ProgramName is the name of the program being attached, used along
	// with priority to order the extension within the chain.
	ProgramName string

	// Priority is the attach priority ordering the new extension within the chain.
	Priority int

	// ProceedOn is a bitmask of kernel return codes on which the
	// dispatcher proceeds to the next program.
	ProceedOn uint32

	// Metadata holds user-supplied key/value link labels to persist on
	// the new extension's link record.
	Metadata map[string]string
}

func (RebuildXDPDispatcher) isAction() {}

// RebuildTCDispatcher triggers a full dispatcher rebuild for TC.
// Same semantics as RebuildXDPDispatcher but for TC dispatchers.
// Returns extensionResult via ExecuteResult.
type RebuildTCDispatcher struct {
	// ProgramID is the kernel ID of the program being attached, added
	// as a new extension to the rebuilt dispatcher chain.
	ProgramID kernel.ProgramID

	// Ifindex is the interface index whose TC dispatcher this action rebuilds.
	Ifindex uint32

	// Ifname is the name of that interface.
	Ifname string

	// Direction is the traffic direction (ingress or egress) of the dispatcher.
	Direction bpfman.TCDirection

	// DispType is the dispatcher type (TC ingress or TC egress) identifying the attach point.
	DispType dispatcher.DispatcherType

	// NetnsPath is the network namespace path containing the interface.
	NetnsPath string

	// ProgPinPath is the bpffs program pin of the program being attached.
	ProgPinPath bpfman.ProgPinPath

	// ProgramName is the name of the program being attached, used along
	// with priority to order the extension within the chain.
	ProgramName string

	// Priority is the attach priority ordering the new extension within the chain.
	Priority int

	// ProceedOn is a bitmask of kernel return codes on which the
	// dispatcher proceeds to the next program.
	ProceedOn uint32

	// Metadata holds user-supplied key/value link labels to persist on
	// the new extension's link record.
	Metadata map[string]string
}

func (RebuildTCDispatcher) isAction() {}

// RebuildDispatcherForDetach triggers a full dispatcher rebuild after
// an extension has been detached. ExcludeLinkID identifies the member
// being detached; the rebuild filters it out before deciding whether
// to rebuild with remaining members or remove the empty dispatcher.
type RebuildDispatcherForDetach struct {
	// Key identifies the dispatcher whose chain this action rebuilds after a detach.
	Key dispatcher.Key

	// ExcludeLinkID is the bpfman link ID of the member being detached;
	// the rebuild filters it out before deciding whether to rebuild
	// with the remaining members or remove the now-empty dispatcher.
	ExcludeLinkID bpfman.LinkID
}

func (RebuildDispatcherForDetach) isAction() {}
