package bpfman

import (
	"fmt"
	"time"

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
func IsSyntheticLinkID(id kernel.LinkID) bool {
	return id >= SyntheticLinkIDBase
}

// TCXAttachOrder specifies where to insert a TCX program in the chain.
// Programs are ordered by priority, with lower priority values running first.
// This type maps to cilium/ebpf's link.Anchor for kernel attachment.
type TCXAttachOrder struct {
	// First attaches at the head of the chain (runs before all others).
	First bool `json:"first"`
	// Last attaches at the tail of the chain (runs after all others).
	Last bool `json:"last"`
	// BeforeProgID attaches before the program with this kernel ID. Zero means not set.
	BeforeProgID kernel.ProgramID `json:"before_prog_id"`
	// AfterProgID attaches after the program with this kernel ID. Zero means not set.
	AfterProgID kernel.ProgramID `json:"after_prog_id"`
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
func TCXAttachBefore(progID kernel.ProgramID) TCXAttachOrder {
	return TCXAttachOrder{BeforeProgID: progID}
}

// TCXAttachAfter returns an order that attaches after the given program.
func TCXAttachAfter(progID kernel.ProgramID) TCXAttachOrder {
	return TCXAttachOrder{AfterProgID: progID}
}

// LinkPath represents a pinned link path within a bpffs.
// This is a newtype to prevent accidentally passing arbitrary strings
// where a validated link pin path is expected.
type LinkPath string

// String returns the path as a string.
func (p LinkPath) String() string { return string(p) }

// NewLinkPath creates a *LinkPath from a string, returning nil if empty.
// This is a convenience function for converting optional string pin paths.
func NewLinkPath(s string) *LinkPath {
	if s == "" {
		return nil
	}
	p := LinkPath(s)
	return &p
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
	Offset   uint64 `json:"offset"`
	Retprobe bool   `json:"retprobe"`
}

func (KprobeDetails) linkDetails() {}
func (d KprobeDetails) Kind() LinkKind {
	if d.Retprobe {
		return LinkKindKretprobe
	}
	return LinkKindKprobe
}

// UprobeDetails contains fields specific to uprobe/uretprobe attachments.
// FnName and Offset form two attach modes: a non-empty FnName attaches by
// symbol; an empty FnName with a non-zero Offset attaches at that offset
// within Target. PID 0 means attach system-wide; a non-zero PID restricts
// the probe to that process. ContainerPid 0 means not container-scoped.
type UprobeDetails struct {
	Target       string `json:"target"`
	FnName       string `json:"fn_name"`
	Offset       uint64 `json:"offset"`
	PID          int32  `json:"pid"`
	Retprobe     bool   `json:"retprobe"`
	ContainerPid int32  `json:"container_pid"`
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
// Netns empty means the root network namespace.
type XDPDetails struct {
	Interface    string           `json:"interface"`
	Ifindex      uint32           `json:"ifindex"`
	Priority     int32            `json:"priority"`
	Position     int32            `json:"position"`
	ProceedOn    []int32          `json:"proceed_on"`
	Netns        string           `json:"netns"`
	Nsid         uint64           `json:"nsid"`
	DispatcherID kernel.ProgramID `json:"dispatcher_id"`
	Revision     uint32           `json:"revision"`
}

func (XDPDetails) linkDetails()   {}
func (XDPDetails) Kind() LinkKind { return LinkKindXDP }

// TCDirection represents the direction of TC traffic (ingress or egress).
// The unexported field prevents construction of invalid values; use the
// package-level variables or ParseTCDirection.
type TCDirection struct{ v string }

var (
	TCDirectionIngress = TCDirection{"ingress"}
	TCDirectionEgress  = TCDirection{"egress"}
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
		return TCDirection{}, fmt.Errorf("invalid TC direction %q: must be 'ingress' or 'egress'", s)
	}
}

func (d TCDirection) String() string               { return d.v }
func (d TCDirection) MarshalText() ([]byte, error) { return []byte(d.v), nil }

func (d *TCDirection) UnmarshalText(b []byte) error {
	parsed, err := ParseTCDirection(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// TCDetails contains fields specific to TC attachments.
// Netns empty means the root network namespace.
type TCDetails struct {
	Interface    string           `json:"interface"`
	Ifindex      uint32           `json:"ifindex"`
	Direction    TCDirection      `json:"direction"`
	Priority     int32            `json:"priority"`
	Position     int32            `json:"position"`
	ProceedOn    []int32          `json:"proceed_on"`
	Netns        string           `json:"netns"`
	Nsid         uint64           `json:"nsid"`
	DispatcherID kernel.ProgramID `json:"dispatcher_id"`
	Revision     uint32           `json:"revision"`
}

func (TCDetails) linkDetails()   {}
func (TCDetails) Kind() LinkKind { return LinkKindTC }

// TCXDetails contains fields specific to TCX attachments.
// Netns empty means the root network namespace.
type TCXDetails struct {
	Interface string      `json:"interface"`
	Ifindex   uint32      `json:"ifindex"`
	Direction TCDirection `json:"direction"`
	Priority  int32       `json:"priority"`
	Position  int32       `json:"position"`
	Netns     string      `json:"netns"`
	Nsid      uint64      `json:"nsid"`
}

func (TCXDetails) linkDetails()   {}
func (TCXDetails) Kind() LinkKind { return LinkKindTCX }

// TCXLinkInfo combines link summary with TCX-specific details.
// Used for computing attach order based on priority.
type TCXLinkInfo struct {
	KernelLinkID    kernel.LinkID    `json:"kernel_link_id"`
	KernelProgramID kernel.ProgramID `json:"kernel_program_id"`
	Priority        int32            `json:"priority"`
}

// LinkKind is bpfman's discriminator for link types.
// Distinct from kernel.Link.LinkType which is kernel-reported.
// The unexported field prevents construction of invalid values; use the
// package-level variables or ParseLinkKind.
type LinkKind struct{ v string }

var (
	LinkKindTracepoint = LinkKind{"tracepoint"}
	LinkKindKprobe     = LinkKind{"kprobe"}
	LinkKindKretprobe  = LinkKind{"kretprobe"}
	LinkKindUprobe     = LinkKind{"uprobe"}
	LinkKindUretprobe  = LinkKind{"uretprobe"}
	LinkKindFentry     = LinkKind{"fentry"}
	LinkKindFexit      = LinkKind{"fexit"}
	LinkKindXDP        = LinkKind{"xdp"}
	LinkKindTC         = LinkKind{"tc"}
	LinkKindTCX        = LinkKind{"tcx"}
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
		names[i] = k.v
	}
	return names
}

func (k LinkKind) String() string               { return k.v }
func (k LinkKind) MarshalText() ([]byte, error) { return []byte(k.v), nil }

func (k *LinkKind) UnmarshalText(b []byte) error {
	parsed, err := ParseLinkKind(string(b))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// ParseLinkKind parses a string into a LinkKind.
// Returns the LinkKind and a nil error if valid, or the zero value and
// an error if unrecognised.
func ParseLinkKind(s string) (LinkKind, error) {
	switch s {
	case "tracepoint":
		return LinkKindTracepoint, nil
	case "kprobe":
		return LinkKindKprobe, nil
	case "kretprobe":
		return LinkKindKretprobe, nil
	case "uprobe":
		return LinkKindUprobe, nil
	case "uretprobe":
		return LinkKindUretprobe, nil
	case "fentry":
		return LinkKindFentry, nil
	case "fexit":
		return LinkKindFexit, nil
	case "xdp":
		return LinkKindXDP, nil
	case "tc":
		return LinkKindTC, nil
	case "tcx":
		return LinkKindTCX, nil
	default:
		return LinkKind{}, fmt.Errorf("unknown link kind %q", s)
	}
}

// LinkRecord is the stored record of an attached link (DB-backed).
// ID is the user-facing identity: kernel-assigned for real BPF links,
// or bpfman-assigned (0x80000000+) for synthetic/perf_event links.
type LinkRecord struct {
	ID        kernel.LinkID    `json:"id"`
	ProgramID kernel.ProgramID `json:"program_id"` // program this attaches to
	Kind      LinkKind         `json:"kind"`
	// PinPath nil distinguishes an ephemeral link from one with a pin. Kept as a
	// pointer so absence is representable at the type level; omitempty reflects that.
	PinPath *LinkPath `json:"pin_path,omitempty"`
	// Details nil means "no per-kind detail available"; omitempty mirrors the
	// nil-interface semantics the rest of the code already relies on.
	Details   LinkDetails `json:"details,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	// Note: When Details is non-nil, Kind must equal Details.Kind(); constructors enforce this
}

// LinkListResult wraps link list output for consistent JSON structure.
// The wrapper provides a stable path for jsonpath queries (e.g., {.links[*].id}).
type LinkListResult struct {
	Links []LinkRecord `json:"links"`
}

// LinkListOption configures link list filtering.
type LinkListOption func(*linkListOptions)

// linkListOptions holds the accumulated filter state.
type linkListOptions struct {
	kinds     map[LinkKind]struct{}
	programID *kernel.ProgramID
}

// Matches returns true if the link matches all filter criteria.
func (o *linkListOptions) Matches(link *LinkRecord) bool {
	return o.matchesKind(link) && o.matchesProgramID(link)
}

func (o *linkListOptions) matchesKind(link *LinkRecord) bool {
	if len(o.kinds) == 0 {
		return true
	}
	_, ok := o.kinds[link.Kind]
	return ok
}

func (o *linkListOptions) matchesProgramID(link *LinkRecord) bool {
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
func WithProgramID(id kernel.ProgramID) LinkListOption {
	return func(o *linkListOptions) {
		o.programID = &id
	}
}

// IsSynthetic returns true if this is a synthetic link (perf_event-based, no kernel link).
func (r LinkRecord) IsSynthetic() bool { return IsSyntheticLinkID(r.ID) }

// HasPin returns true if this link has a pin path.
func (r LinkRecord) HasPin() bool { return r.PinPath != nil }

// LinkStatus is observed state (kernel + fs).
// This is "what actually exists right now".
type LinkStatus struct {
	// Kernel nil means the link is not currently in the kernel's link list or is
	// a synthetic perf_event link with no kernel link ID. Pointer + omitempty
	// encodes that absence; a present pointer carries the kernel-reported view.
	Kernel     *kernel.Link `json:"kernel,omitempty"`
	KernelSeen bool         `json:"kernel_seen"` // true if kernel enumeration succeeded (distinguishes "not found" from "unknown")
	PinPresent bool         `json:"pin_present"` // true if pin path exists on filesystem
}

// HasKernelLinkID is a capability interface for domain objects that
// carry a kernel-assigned link ID. The typed argument parsers use
// this to extract a link ID from an origin-backed structured value
// without depending on a concrete type.
type HasKernelLinkID interface {
	KernelLinkID() kernel.LinkID
}

// Compile-time interface assertions.
var (
	_ HasKernelLinkID = Link{}
	_ HasKernelLinkID = LinkRecord{}
)

// Link is the canonical domain object combining record and status.
// Record comes from the store (what bpfman manages).
// Status comes from observation (kernel enumeration + filesystem checks).
type Link struct {
	Record LinkRecord `json:"record"`
	Status LinkStatus `json:"status"`
}

// KernelLinkID returns the link's kernel-assigned ID.
func (l Link) KernelLinkID() kernel.LinkID { return l.Record.ID }

// KernelLinkID returns the record's kernel-assigned link ID.
func (r LinkRecord) KernelLinkID() kernel.LinkID { return r.ID }

// AttachOutput is the raw result of a kernel attach operation.
// This is transient I/O boundary data - the manager uses it along with
// the AttachSpec to construct the stored LinkSpec.
//
// AttachOutput parallels LoadOutput for programs: it captures what the
// kernel returned without mixing in user-provided metadata.
type AttachOutput struct {
	LinkID     kernel.LinkID // kernel-assigned link ID, or synthetic ID for perf_event
	KernelLink *kernel.Link  // kernel info, nil for synthetic links
	PinPath    string        // where link was pinned, empty if ephemeral
	Synthetic  bool          // true if this is a synthetic link (no kernel link)
}

// NewPinnedLinkRecord creates a fully-detailed record for a pinned link.
// Kind is derived from details to enforce the invariant.
func NewPinnedLinkRecord(id kernel.LinkID, programID kernel.ProgramID, details LinkDetails, pin LinkPath, createdAt time.Time) LinkRecord {
	return LinkRecord{
		ID:        id,
		ProgramID: programID,
		Kind:      details.Kind(),
		PinPath:   &pin,
		Details:   details,
		CreatedAt: createdAt,
	}
}

// NewEphemeralLinkRecord creates a fully-detailed record for an ephemeral (unpinned) link.
// Kind is derived from details to enforce the invariant.
func NewEphemeralLinkRecord(id kernel.LinkID, programID kernel.ProgramID, details LinkDetails, createdAt time.Time) LinkRecord {
	return LinkRecord{
		ID:        id,
		ProgramID: programID,
		Kind:      details.Kind(),
		Details:   details,
		CreatedAt: createdAt,
	}
}
