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
	ProgramID   kernel.ProgramID
	ProgramName string
	ProgPinPath string
	LinkID      kernel.LinkID
	LinkPinPath bpfman.LinkPath
	Position    int
	Priority    int
	ProceedOn   uint32
	Ifname      string
}

// DispatcherRuntime holds the kernel-assigned identifiers for the
// dispatcher program itself. LinkID is nil for TC dispatchers which
// use netlink filters rather than BPF links. FilterPriority is the
// TC filter priority, nil for XDP.
type DispatcherRuntime struct {
	ProgramID      kernel.ProgramID
	LinkID         *kernel.LinkID
	FilterPriority *uint16
}

// DispatcherSnapshot is a complete point-in-time view of a dispatcher
// and all its extension members. The snapshot is the unit of
// replacement: callers build a complete snapshot and pass it to
// ReplaceDispatcherSnapshot, which atomically replaces all persisted
// state for the dispatcher's attach point.
type DispatcherSnapshot struct {
	Key      dispatcher.Key
	Revision uint32
	Runtime  DispatcherRuntime
	Members  []DispatcherMember
}

// DispatcherSummary is a lightweight view of a dispatcher suitable
// for listing. It carries the member count rather than the full
// member list, avoiding the cost of joining detail tables when only
// aggregate information is needed.
type DispatcherSummary struct {
	Key         dispatcher.Key
	Revision    uint32
	Runtime     DispatcherRuntime
	MemberCount int
}
