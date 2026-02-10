package bpfman

import (
	"fmt"
	"maps"
	"time"

	"github.com/frobware/go-bpfman/kernel"
	"k8s.io/apimachinery/pkg/labels"
)

// ProgramType represents the type of BPF program.
type ProgramType uint32

const (
	ProgramTypeUnspecified ProgramType = iota
	ProgramTypeXDP
	ProgramTypeTC
	ProgramTypeTCX
	ProgramTypeTracepoint
	ProgramTypeKprobe
	ProgramTypeKretprobe
	ProgramTypeUprobe
	ProgramTypeUretprobe
	ProgramTypeFentry
	ProgramTypeFexit
)

// allProgramTypes is the canonical list of valid program types.
// ParseProgramType and ProgramTypeNames derive from this.
var allProgramTypes = []ProgramType{
	ProgramTypeXDP,
	ProgramTypeTC,
	ProgramTypeTCX,
	ProgramTypeTracepoint,
	ProgramTypeKprobe,
	ProgramTypeKretprobe,
	ProgramTypeUprobe,
	ProgramTypeUretprobe,
	ProgramTypeFentry,
	ProgramTypeFexit,
}

// programTypeByName maps lowercase type names to ProgramType values.
// Built from allProgramTypes to ensure consistency.
var programTypeByName = func() map[string]ProgramType {
	m := make(map[string]ProgramType, len(allProgramTypes))
	for _, pt := range allProgramTypes {
		m[pt.String()] = pt
	}
	return m
}()

// AllProgramTypes returns all valid program types.
func AllProgramTypes() []ProgramType {
	return allProgramTypes
}

// ProgramTypeNames returns all valid program type names as strings.
func ProgramTypeNames() []string {
	names := make([]string, len(allProgramTypes))
	for i, t := range allProgramTypes {
		names[i] = t.String()
	}
	return names
}

// String returns the string representation of the program type.
func (t ProgramType) String() string {
	switch t {
	case ProgramTypeXDP:
		return "xdp"
	case ProgramTypeTC:
		return "tc"
	case ProgramTypeTCX:
		return "tcx"
	case ProgramTypeTracepoint:
		return "tracepoint"
	case ProgramTypeKprobe:
		return "kprobe"
	case ProgramTypeKretprobe:
		return "kretprobe"
	case ProgramTypeUprobe:
		return "uprobe"
	case ProgramTypeUretprobe:
		return "uretprobe"
	case ProgramTypeFentry:
		return "fentry"
	case ProgramTypeFexit:
		return "fexit"
	default:
		return "unspecified"
	}
}

// MarshalText implements encoding.TextMarshaler so ProgramType
// serialises as its string name in JSON.
func (t ProgramType) MarshalText() ([]byte, error) {
	return []byte(t.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler so ProgramType
// can be parsed from its string name in JSON.
func (t *ProgramType) UnmarshalText(text []byte) error {
	parsed, ok := ParseProgramType(string(text))
	if !ok {
		return fmt.Errorf("invalid program type: %q", string(text))
	}
	*t = parsed
	return nil
}

// ParseProgramType parses a string into a ProgramType.
// Returns the program type and true if valid, or ProgramTypeUnspecified and false if not.
func ParseProgramType(s string) (ProgramType, bool) {
	pt, ok := programTypeByName[s]
	return pt, ok
}

// ProgramHandles contains stable filesystem handles for management.
// These are outputs of load, used for lifecycle operations.
type ProgramHandles struct {
	PinPath    string            `json:"pin_path"`
	MapPinPath string            `json:"map_pin_path,omitempty"`
	MapOwnerID *kernel.ProgramID `json:"map_owner_id,omitempty"`
}

// ProgramMeta contains operator-facing management metadata.
// Searchable/editable without affecting the loaded program.
type ProgramMeta struct {
	Name        string            `json:"name"`            // human-readable label
	Owner       string            `json:"owner,omitempty"` // who manages this
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"` // arbitrary key/value for selection
}

// ProgramRecord is the stored record of a loaded program (DB-backed).
// KernelID is the DB primary key and user-facing identity.
//
// Note: ProgramRecord is distinct from LoadSpec. LoadSpec describes how to load
// a program (validated input), while ProgramRecord describes a loaded program's
// stored state (output). They share some fields but serve different purposes.
type ProgramRecord struct {
	// Identity - KernelID is the DB primary key and user-facing ID
	KernelID kernel.ProgramID `json:"kernel_id"`
	Load     LoadSpec         `json:"load"`
	// License and GPLCompatible are discovered at load time from the ELF.
	// They live on ProgramRecord (not LoadSpec) because they're properties
	// of the loaded program, not part of the load request.
	License       string         `json:"license,omitempty"`
	GPLCompatible bool           `json:"gpl_compatible"`
	Handles       ProgramHandles `json:"handles"`
	Meta          ProgramMeta    `json:"meta"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// ProgramStatus is observed state (kernel + filesystem).
// This is "what actually exists right now".
type ProgramStatus struct {
	Kernel      *kernel.Program      `json:"kernel,omitempty"` // nil means not in kernel
	Stats       *kernel.ProgramStats `json:"stats,omitempty"`  // runtime stats (requires kernel.bpf_stats_enabled=1)
	PinPresent  bool                 `json:"pin_present"`      // filesystem check
	MapsPresent bool                 `json:"maps_present"`     // filesystem check
	Links       []Link               `json:"links,omitempty"`  // links with spec + status
	Maps        []kernel.Map         `json:"maps,omitempty"`   // kernel maps
}

// Program is the canonical domain object combining record and status.
// Record comes from the store (what bpfman manages).
// Status comes from observation (kernel enumeration + filesystem checks).
type Program struct {
	Record ProgramRecord `json:"record"`
	Status ProgramStatus `json:"status"`
}

// WithDescription returns a new ProgramRecord with the description set.
func (p ProgramRecord) WithDescription(desc string) ProgramRecord {
	cp := p
	cp.Meta.Description = desc
	cp.Meta.Metadata = cloneMap(p.Meta.Metadata)
	// Clone global data by reconstructing the LoadSpec with cloned data
	cp.Load = cp.Load.WithGlobalData(cloneMap(p.Load.GlobalData()))
	return cp
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	result := make(map[K]V, len(m))
	maps.Copy(result, m)
	return result
}

// LoadOutput is the raw result of kernel.Load().
// This is transient I/O boundary data, not stored in the DB.
type LoadOutput struct {
	PinPath      string          // where program was pinned
	MapsDir      string          // where maps were pinned
	Program      *kernel.Program // kernel info (ID, MapIDs, etc)
	License      string          // from ELF, for GPL check
	InferredType ProgramType     // inferred from ELF if user didn't specify
}

// IsGPLCompatible checks if a license string is GPL compatible.
// This matches the kernel's license_is_gpl_compatible() function.
func IsGPLCompatible(license string) bool {
	switch license {
	case "GPL", "GPL v2", "GPL and additional rights",
		"Dual BSD/GPL", "Dual MIT/GPL", "Dual MPL/GPL":
		return true
	default:
		return false
	}
}

// TestLoadSpec creates a LoadSpec with the given program type.
// This is a convenience constructor for tests.
func TestLoadSpec(programType ProgramType) LoadSpec {
	return LoadSpec{}.WithProgramType(programType)
}

// TestLoadSpecWithPath creates a LoadSpec with the given program type and object path.
// This is a convenience constructor for tests.
func TestLoadSpecWithPath(programType ProgramType, objectPath string) LoadSpec {
	return LoadSpec{}.
		WithProgramType(programType).
		WithObjectPath(objectPath)
}

// HostInfo contains system information about the observed host.
type HostInfo struct {
	Sysname  string `json:"sysname"`
	Nodename string `json:"nodename"`
	Release  string `json:"release"`
	Version  string `json:"version"`
	Machine  string `json:"machine"`
}

// ProgramListResult contains programs with observation metadata.
type ProgramListResult struct {
	ObservedAt time.Time `json:"observed_at"`
	Host       HostInfo  `json:"host"`
	Programs   []Program `json:"programs"`
}

// ListOption configures program list filtering.
type ListOption func(*listOptions)

// listOptions holds the accumulated filter state.
type listOptions struct {
	attached *bool // nil = don't filter, true = attached only, false = unattached only
	types    map[ProgramType]struct{}
	selector labels.Selector
}

// Matches returns true if the program matches all filter criteria.
func (o *listOptions) Matches(prog *Program) bool {
	return o.matchesAttachment(prog) &&
		o.matchesType(prog) &&
		o.matchesLabels(prog)
}

func (o *listOptions) matchesAttachment(prog *Program) bool {
	if o.attached == nil {
		return true
	}
	hasLinks := hasActiveLinks(prog)
	if *o.attached {
		return hasLinks
	}
	return !hasLinks
}

func (o *listOptions) matchesType(prog *Program) bool {
	if len(o.types) == 0 {
		return true
	}
	_, ok := o.types[prog.Record.Load.ProgramType()]
	return ok
}

func (o *listOptions) matchesLabels(prog *Program) bool {
	if o.selector == nil {
		return true
	}
	return o.selector.Matches(labels.Set(prog.Record.Meta.Metadata))
}

// hasActiveLinks returns true if the program has at least one link
// with kernel presence (actually attached, not just a DB record).
func hasActiveLinks(prog *Program) bool {
	for _, link := range prog.Status.Links {
		if link.Status.Kernel != nil {
			return true
		}
	}
	return false
}

// ApplyListOptions applies the given options and returns the configured listOptions.
func ApplyListOptions(opts ...ListOption) *listOptions {
	o := &listOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// WithAttached filters to programs with active kernel links.
func WithAttached() ListOption {
	return func(o *listOptions) {
		t := true
		o.attached = &t
	}
}

// WithUnattached filters to programs without active kernel links.
func WithUnattached() ListOption {
	return func(o *listOptions) {
		f := false
		o.attached = &f
	}
}

// WithTypes filters to programs of the specified types.
func WithTypes(types ...ProgramType) ListOption {
	return func(o *listOptions) {
		if o.types == nil {
			o.types = make(map[ProgramType]struct{})
		}
		for _, t := range types {
			o.types[t] = struct{}{}
		}
	}
}

// MatchingLabels filters to programs with matching label key-value pairs.
func MatchingLabels(lbls map[string]string) ListOption {
	return func(o *listOptions) {
		o.selector = labels.SelectorFromSet(labels.Set(lbls))
	}
}

// MatchingSelector filters to programs matching the label selector.
func MatchingSelector(sel labels.Selector) ListOption {
	return func(o *listOptions) {
		o.selector = sel
	}
}
