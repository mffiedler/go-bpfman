package bpfman

import (
	"fmt"
	"math"
	"time"

	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/kernel"
)

// SyntheticLinkIDBase is the base value for synthetic link IDs.
// Synthetic IDs are generated in the range 0x80000000-0xFFFFFFFF for
// perf_event-based links (e.g., container uprobes) that lack kernel link IDs.
// Real kernel link IDs are small sequential numbers, so this range avoids
// collision. GC should skip links with synthetic IDs since they can't be
// enumerated via the kernel's link iterator.
const SyntheticLinkIDBase = 0x80000000

// IsSyntheticLinkID returns true if the link ID is synthetic (not from kernel).
// Synthetic IDs are used for perf_event-based links that cannot be pinned
// and don't have real kernel link IDs.
func IsSyntheticLinkID(id uint32) bool {
	return id >= SyntheticLinkIDBase
}

// TCXAttachOrder specifies where to insert a TCX program in the chain.
// Programs are ordered by priority, with lower priority values running first.
// This type maps to cilium/ebpf's link.Anchor for kernel attachment.
type TCXAttachOrder struct {
	// First attaches at the head of the chain (runs before all others).
	First bool `json:"first,omitempty"`
	// Last attaches at the tail of the chain (runs after all others).
	Last bool `json:"last,omitempty"`
	// BeforeProgID attaches before the program with this kernel ID.
	// Zero means not set.
	BeforeProgID uint32 `json:"before_prog_id,omitempty"`
	// AfterProgID attaches after the program with this kernel ID.
	// Zero means not set.
	AfterProgID uint32 `json:"after_prog_id,omitempty"`
}

// TCXAttachFirst returns an order that attaches at the head of the chain.
func TCXAttachFirst() TCXAttachOrder {
	return TCXAttachOrder{First: true}
}

// TCXAttachLast returns an order that attaches at the tail of the chain.
func TCXAttachLast() TCXAttachOrder {
	return TCXAttachOrder{Last: true}
}

// TCXAttachBefore returns an order that attaches before the given program.
func TCXAttachBefore(progID uint32) TCXAttachOrder {
	return TCXAttachOrder{BeforeProgID: progID}
}

// TCXAttachAfter returns an order that attaches after the given program.
func TCXAttachAfter(progID uint32) TCXAttachOrder {
	return TCXAttachOrder{AfterProgID: progID}
}

// LinkDetails is a sealed interface for type-specific link details.
// Use type assertion or type switch to access the concrete type.
// The interface is sealed via the unexported linkDetails() method -
// only types in this package can implement it.
type LinkDetails interface {
	linkDetails()   // unexported - only our types can implement
	Kind() LinkKind // returns the kind for this detail type
}

// TracepointDetails contains fields specific to tracepoint attachments.
type TracepointDetails struct {
	Group string `json:"group"`
	Name  string `json:"name"`
}

func (TracepointDetails) linkDetails()   {}
func (TracepointDetails) Kind() LinkKind { return LinkKindTracepoint }

// KprobeDetails contains fields specific to kprobe/kretprobe attachments.
type KprobeDetails struct {
	FnName   string `json:"fn_name"`
	Offset   uint64 `json:"offset,omitempty"`
	Retprobe bool   `json:"retprobe,omitempty"`
}

func (KprobeDetails) linkDetails() {}
func (d KprobeDetails) Kind() LinkKind {
	if d.Retprobe {
		return LinkKindKretprobe
	}
	return LinkKindKprobe
}

// UprobeDetails contains fields specific to uprobe/uretprobe attachments.
type UprobeDetails struct {
	Target       string `json:"target"`
	FnName       string `json:"fn_name,omitempty"`
	Offset       uint64 `json:"offset,omitempty"`
	PID          int32  `json:"pid,omitempty"`
	Retprobe     bool   `json:"retprobe,omitempty"`
	ContainerPid int32  `json:"container_pid,omitempty"`
}

func (UprobeDetails) linkDetails() {}
func (d UprobeDetails) Kind() LinkKind {
	if d.Retprobe {
		return LinkKindUretprobe
	}
	return LinkKindUprobe
}

// FentryDetails contains fields specific to fentry attachments.
type FentryDetails struct {
	FnName string `json:"fn_name"`
}

func (FentryDetails) linkDetails()   {}
func (FentryDetails) Kind() LinkKind { return LinkKindFentry }

// FexitDetails contains fields specific to fexit attachments.
type FexitDetails struct {
	FnName string `json:"fn_name"`
}

func (FexitDetails) linkDetails()   {}
func (FexitDetails) Kind() LinkKind { return LinkKindFexit }

// XDPDetails contains fields specific to XDP attachments.
type XDPDetails struct {
	Interface    string  `json:"interface"`
	Ifindex      uint32  `json:"ifindex"`
	Priority     int32   `json:"priority"`
	Position     int32   `json:"position"`
	ProceedOn    []int32 `json:"proceed_on"`
	Netns        string  `json:"netns,omitempty"`
	Nsid         uint64  `json:"nsid"`
	DispatcherID uint32  `json:"dispatcher_id"`
	Revision     uint32  `json:"revision"`
}

func (XDPDetails) linkDetails()   {}
func (XDPDetails) Kind() LinkKind { return LinkKindXDP }

// TCDirection represents the direction of TC traffic (ingress or egress).
// This is a closed set - use ParseTCDirection to create from strings.
type TCDirection string

const (
	TCDirectionIngress TCDirection = "ingress"
	TCDirectionEgress  TCDirection = "egress"
)

// ParseTCDirection parses a string into a TCDirection.
// Returns an error if the string is not "ingress" or "egress".
func ParseTCDirection(s string) (TCDirection, error) {
	switch s {
	case "ingress":
		return TCDirectionIngress, nil
	case "egress":
		return TCDirectionEgress, nil
	default:
		return "", fmt.Errorf("invalid TC direction %q: must be 'ingress' or 'egress'", s)
	}
}

func (d TCDirection) String() string { return string(d) }

// TCDetails contains fields specific to TC attachments.
type TCDetails struct {
	Interface    string      `json:"interface"`
	Ifindex      uint32      `json:"ifindex"`
	Direction    TCDirection `json:"direction"`
	Priority     int32       `json:"priority"`
	Position     int32       `json:"position"`
	ProceedOn    []int32     `json:"proceed_on"`
	Netns        string      `json:"netns,omitempty"`
	Nsid         uint64      `json:"nsid"`
	DispatcherID uint32      `json:"dispatcher_id"`
	Revision     uint32      `json:"revision"`
}

func (TCDetails) linkDetails()   {}
func (TCDetails) Kind() LinkKind { return LinkKindTC }

// TCXDetails contains fields specific to TCX attachments.
type TCXDetails struct {
	Interface string      `json:"interface"`
	Ifindex   uint32      `json:"ifindex"`
	Direction TCDirection `json:"direction"`
	Priority  int32       `json:"priority"`
	Netns     string      `json:"netns,omitempty"`
	Nsid      uint64      `json:"nsid,omitempty"`
}

func (TCXDetails) linkDetails()   {}
func (TCXDetails) Kind() LinkKind { return LinkKindTCX }

// TCXLinkInfo combines link summary with TCX-specific details.
// Used for computing attach order based on priority.
type TCXLinkInfo struct {
	KernelLinkID    uint32 `json:"kernel_link_id"`
	KernelProgramID uint32 `json:"kernel_program_id"`
	Priority        int32  `json:"priority"`
}

// LinkKind is bpfman's discriminator for link types.
// Distinct from kernel.Link.LinkType which is kernel-reported.
type LinkKind string

const (
	LinkKindTracepoint LinkKind = "tracepoint"
	LinkKindKprobe     LinkKind = "kprobe"
	LinkKindKretprobe  LinkKind = "kretprobe"
	LinkKindUprobe     LinkKind = "uprobe"
	LinkKindUretprobe  LinkKind = "uretprobe"
	LinkKindFentry     LinkKind = "fentry"
	LinkKindFexit      LinkKind = "fexit"
	LinkKindXDP        LinkKind = "xdp"
	LinkKindTC         LinkKind = "tc"
	LinkKindTCX        LinkKind = "tcx"
)

// allLinkKinds is the canonical list of valid link kinds.
var allLinkKinds = []LinkKind{
	LinkKindTracepoint,
	LinkKindKprobe,
	LinkKindKretprobe,
	LinkKindUprobe,
	LinkKindUretprobe,
	LinkKindFentry,
	LinkKindFexit,
	LinkKindXDP,
	LinkKindTC,
	LinkKindTCX,
}

// AllLinkKinds returns all valid link kinds.
func AllLinkKinds() []LinkKind {
	return allLinkKinds
}

// LinkKindNames returns all valid link kind names as strings.
func LinkKindNames() []string {
	names := make([]string, len(allLinkKinds))
	for i, k := range allLinkKinds {
		names[i] = string(k)
	}
	return names
}

// ParseLinkKind parses a string into a LinkKind.
// Returns the LinkKind and true if valid, or empty string and false if invalid.
func ParseLinkKind(s string) (LinkKind, bool) {
	switch s {
	case "tracepoint":
		return LinkKindTracepoint, true
	case "kprobe":
		return LinkKindKprobe, true
	case "kretprobe":
		return LinkKindKretprobe, true
	case "uprobe":
		return LinkKindUprobe, true
	case "uretprobe":
		return LinkKindUretprobe, true
	case "fentry":
		return LinkKindFentry, true
	case "fexit":
		return LinkKindFexit, true
	case "xdp":
		return LinkKindXDP, true
	case "tc":
		return LinkKindTC, true
	case "tcx":
		return LinkKindTCX, true
	default:
		return "", false
	}
}

// LinkID is bpfman's identifier for a link.
// Opaque to callers; currently backed by kernel/synthetic link ID.
// uint64 to accommodate a future independent autoincrement id.
//
// Implementation note: The current schema uses kernel_link_id as the primary
// key. During this refactor, LinkID is populated from kernel_link_id for
// compatibility. Callers must treat LinkID as opaque; it must not be used
// as a kernel correlation key outside inspect/store internals.
type LinkID uint64

// IsSynthetic returns true if this ID was minted by bpfman (not a kernel-assigned link ID).
// Future-safe: returns false for ids > MaxUint32 (future autoincrement range).
func (id LinkID) IsSynthetic() bool {
	if id > math.MaxUint32 {
		return false // future autoincrement ids are not synthetic
	}
	return IsSyntheticLinkID(uint32(id))
}

// LinkSpec is what bpfman intends to manage (DB-backed).
// This is the "desired state" - what the user asked for.
// ID is the user-facing identity: kernel-assigned for real BPF links,
// or bpfman-assigned (0x80000000+) for synthetic/perf_event links.
type LinkSpec struct {
	ID        LinkID          `json:"id"`
	ProgramID uint32          `json:"program_id"` // program this attaches to
	Kind      LinkKind        `json:"kind"`
	PinPath   *bpffs.LinkPath `json:"pin_path,omitempty"` // nil == ephemeral
	Details   LinkDetails     `json:"details,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	// Note: When Details is non-nil, Kind must equal Details.Kind(); constructors enforce this
}

// LinkListResult wraps link list output for consistent JSON structure.
// The wrapper provides a stable path for jsonpath queries (e.g., {.links[*].id}).
type LinkListResult struct {
	Links []LinkSpec `json:"links"`
}

// LinkListOption configures link list filtering.
type LinkListOption func(*linkListOptions)

// linkListOptions holds the accumulated filter state.
type linkListOptions struct {
	kinds     map[LinkKind]struct{}
	programID *uint32
}

// Matches returns true if the link matches all filter criteria.
func (o *linkListOptions) Matches(link *LinkSpec) bool {
	return o.matchesKind(link) && o.matchesProgramID(link)
}

func (o *linkListOptions) matchesKind(link *LinkSpec) bool {
	if len(o.kinds) == 0 {
		return true
	}
	_, ok := o.kinds[link.Kind]
	return ok
}

func (o *linkListOptions) matchesProgramID(link *LinkSpec) bool {
	if o.programID == nil {
		return true
	}
	return link.ProgramID == *o.programID
}

// ApplyLinkListOptions applies the given options and returns the configured filter.
func ApplyLinkListOptions(opts ...LinkListOption) *linkListOptions {
	o := &linkListOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// WithKinds filters to links of the specified kinds.
func WithKinds(kinds ...LinkKind) LinkListOption {
	return func(o *linkListOptions) {
		if o.kinds == nil {
			o.kinds = make(map[LinkKind]struct{})
		}
		for _, k := range kinds {
			o.kinds[k] = struct{}{}
		}
	}
}

// WithProgramID filters to links attached to the given program.
func WithProgramID(id uint32) LinkListOption {
	return func(o *linkListOptions) {
		o.programID = &id
	}
}

// IsSynthetic returns true if this is a synthetic link (perf_event-based, no kernel link).
func (s LinkSpec) IsSynthetic() bool { return s.ID.IsSynthetic() }

// HasPin returns true if this link has a pin path.
func (s LinkSpec) HasPin() bool { return s.PinPath != nil }

// LinkStatus is observed state (kernel + fs).
// This is "what actually exists right now".
type LinkStatus struct {
	Kernel     *kernel.Link `json:"kernel,omitempty"` // nil if not in kernel or synthetic
	KernelSeen bool         `json:"kernel_seen"`      // true if kernel enumeration succeeded (distinguishes "not found" from "unknown")
	PinPresent bool         `json:"pin_present"`      // true if pin path exists on filesystem
}

// Link is the canonical domain object combining spec and status.
// Spec comes from the store (what bpfman manages).
// Status comes from observation (kernel enumeration + filesystem checks).
type Link struct {
	Spec   LinkSpec   `json:"spec"`
	Status LinkStatus `json:"status"`
}

// AttachOutput is the raw result of a kernel attach operation.
// This is transient I/O boundary data - the manager uses it along with
// the AttachSpec to construct the stored LinkSpec.
//
// AttachOutput parallels LoadOutput for programs: it captures what the
// kernel returned without mixing in user-provided metadata.
type AttachOutput struct {
	LinkID     uint32       // kernel-assigned link ID, or synthetic ID for perf_event
	KernelLink *kernel.Link // kernel info, nil for synthetic links
	PinPath    string       // where link was pinned, empty if ephemeral
	Synthetic  bool         // true if this is a synthetic link (no kernel link)
}

// NewPinnedLinkSpec creates a fully-detailed spec for a pinned link.
// Kind is derived from details to enforce the invariant.
func NewPinnedLinkSpec(id LinkID, programID uint32, details LinkDetails, pin bpffs.LinkPath, createdAt time.Time) LinkSpec {
	return LinkSpec{
		ID:        id,
		ProgramID: programID,
		Kind:      details.Kind(),
		PinPath:   &pin,
		Details:   details,
		CreatedAt: createdAt,
	}
}

// NewEphemeralLinkSpec creates a fully-detailed spec for an ephemeral (unpinned) link.
// Kind is derived from details to enforce the invariant.
func NewEphemeralLinkSpec(id LinkID, programID uint32, details LinkDetails, createdAt time.Time) LinkSpec {
	return LinkSpec{
		ID:        id,
		ProgramID: programID,
		Kind:      details.Kind(),
		Details:   details,
		CreatedAt: createdAt,
	}
}
