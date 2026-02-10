package dispatcher

import "github.com/frobware/go-bpfman/kernel"

// Key uniquely identifies a dispatcher by its type, network namespace,
// and interface index.
type Key struct {
	Type    DispatcherType `json:"type"`
	Nsid    uint64         `json:"nsid"`
	Ifindex uint32         `json:"ifindex"`
}

// State represents the persistent state of a dispatcher.
// A dispatcher manages multi-program chaining for XDP or TC attachments.
type State struct {
	// Type is the dispatcher type (xdp, tc-ingress, tc-egress).
	Type DispatcherType `json:"type"`

	// Nsid is the network namespace inode number.
	// This uniquely identifies the network namespace.
	Nsid uint64 `json:"nsid"`

	// Ifindex is the network interface index.
	Ifindex uint32 `json:"ifindex"`

	// Revision is the current dispatcher revision.
	// Incremented on each atomic update, wraps at MaxUint32.
	Revision uint32 `json:"revision"`

	// KernelID is the kernel program ID of the dispatcher.
	KernelID kernel.ProgramID `json:"kernel_id"`

	// LinkID is the kernel link ID (XDP link for XDP dispatchers).
	// Zero for TC dispatchers which use legacy netlink instead of BPF links.
	LinkID kernel.LinkID `json:"link_id"`

	// Priority is the tc filter priority.
	// Only set for TC dispatchers (legacy netlink). Zero for XDP.
	Priority uint16 `json:"priority,omitempty"`
}
