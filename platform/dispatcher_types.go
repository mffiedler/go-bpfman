package platform

import (
	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
)

// DispatcherMember describes an extension program attached to a
// dispatcher slot. Each member occupies a unique position in the
// dispatcher's slot array.
type DispatcherMember struct {
	ProgramID    kernel.ProgramID   `json:"program_id"`
	ProgramName  string             `json:"program_name"`
	ProgPinPath  bpfman.ProgPinPath `json:"prog_pin_path"`
	LinkID       bpfman.LinkID      `json:"link_id"`
	KernelLinkID *kernel.LinkID     `json:"kernel_link_id"`
	LinkPinPath  bpfman.LinkPath    `json:"link_pin_path"`
	Position     int                `json:"position"`
	Priority     int                `json:"priority"`
	ProceedOn    uint32             `json:"proceed_on"`
	Ifname       string             `json:"ifname"`
}

// DispatcherMemberSpec describes an extension program that should be
// persisted as part of a dispatcher snapshot but does not yet have a bpfman
// LinkID allocated by the store.
type DispatcherMemberSpec struct {
	ExistingLinkID *bpfman.LinkID     `json:"existing_link_id,omitempty"`
	ProgramID      kernel.ProgramID   `json:"program_id"`
	ProgramName    string             `json:"program_name"`
	ProgPinPath    bpfman.ProgPinPath `json:"prog_pin_path"`
	KernelLinkID   *kernel.LinkID     `json:"kernel_link_id"`
	LinkPinPath    bpfman.LinkPath    `json:"link_pin_path"`
	Position       int                `json:"position"`
	Priority       int                `json:"priority"`
	ProceedOn      uint32             `json:"proceed_on"`
	Ifname         string             `json:"ifname"`
}

// DispatcherRuntime holds the kernel-assigned identifiers for the
// dispatcher program itself. KernelLinkID is nil for TC dispatchers which
// use netlink filters rather than BPF links. FilterPriority is the
// TC filter priority, nil for XDP.
type DispatcherRuntime struct {
	ProgramID      kernel.ProgramID `json:"program_id"`
	KernelLinkID   *kernel.LinkID   `json:"kernel_link_id,omitempty"`
	FilterPriority *uint16          `json:"filter_priority,omitempty"`
	NetnsPath      string           `json:"netns_path"`
}

// DispatcherSnapshot is a complete point-in-time view of a dispatcher
// and all its extension members. The snapshot is the unit of
// replacement: callers build a complete snapshot and pass it to
// ReplaceDispatcherSnapshot, which atomically replaces all persisted
// state for the dispatcher's attach point.
type DispatcherSnapshot struct {
	Key      dispatcher.Key     `json:"key"`
	Revision uint32             `json:"revision"`
	Runtime  DispatcherRuntime  `json:"runtime"`
	Members  []DispatcherMember `json:"members"`
}

// DispatcherSnapshotSpec is the requested replacement state for a dispatcher.
// Members may refer to existing bpfman link handles or ask the store to
// allocate new ones.
type DispatcherSnapshotSpec struct {
	Key      dispatcher.Key         `json:"key"`
	Revision uint32                 `json:"revision"`
	Runtime  DispatcherRuntime      `json:"runtime"`
	Members  []DispatcherMemberSpec `json:"members"`
}

// DispatcherSummary is a lightweight view of a dispatcher suitable
// for listing. It carries the member count rather than the full
// member list, avoiding the cost of joining detail tables when only
// aggregate information is needed.
type DispatcherSummary struct {
	Key         dispatcher.Key    `json:"key"`
	Revision    uint32            `json:"revision"`
	Runtime     DispatcherRuntime `json:"runtime"`
	MemberCount int               `json:"member_count"`
}
