package bpfman

import (
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman/kernel"
)

const (
	// DefaultAttachPriority is the run priority used when a dispatcher
	// attach request omits priority or explicitly asks for priority 0.
	DefaultAttachPriority = 50
)

var ErrInvalidAttachSpec = errors.New("invalid attach spec")

// AttachSpec is a sealed interface satisfied by all concrete attach
// spec types.  The unexported marker method prevents external packages
// from implementing it, so the set of valid types is closed.
type AttachSpec interface {
	attachSpec() // sealed marker
	ProgramID() kernel.ProgramID
	// Metadata returns user-supplied key/value link labels, nil when none.
	Metadata() map[string]string
}

func invalidAttachSpec(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidAttachSpec, fmt.Sprintf(format, args...))
}

func validatePriority(p int) (int, error) {
	if p < 0 {
		return 0, invalidAttachSpec("priority must be non-negative, got %d", p)
	}
	if p == 0 {
		return DefaultAttachPriority, nil
	}
	return p, nil
}

// attachMetadata carries user-supplied link labels shared by every attach
// spec. Embedding it gives each concrete spec the Metadata accessor that
// satisfies AttachSpec; the per-type WithMetadata builders set it and
// return the concrete type for fluent chaining.
type attachMetadata struct {
	metadata map[string]string
}

// Metadata returns the user-supplied link labels, nil when none were set.
func (m attachMetadata) Metadata() map[string]string { return m.metadata }

// TracepointAttachSpec specifies how to attach a tracepoint.
type TracepointAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
	group     string
	name      string
}

// NewTracepointAttachSpec creates a TracepointAttachSpec from an
// already parsed tracepoint.
func NewTracepointAttachSpec(programID kernel.ProgramID, tracepoint Tracepoint) (TracepointAttachSpec, error) {
	if programID == 0 {
		return TracepointAttachSpec{}, errors.New("programID is required")
	}
	if tracepoint == (Tracepoint{}) {
		return TracepointAttachSpec{}, errors.New("tracepoint is required")
	}
	return TracepointAttachSpec{programID: programID, group: tracepoint.Group(), name: tracepoint.Name()}, nil
}

// NewTracepointAttachSpecFromString parses a tracepoint identifier and
// creates a TracepointAttachSpec.
func NewTracepointAttachSpecFromString(programID kernel.ProgramID, tracepoint string) (TracepointAttachSpec, error) {
	tp, err := ParseTracepoint(tracepoint)
	if err != nil {
		return TracepointAttachSpec{}, err
	}
	return NewTracepointAttachSpec(programID, tp)
}

func (TracepointAttachSpec) attachSpec()                   {}
func (s TracepointAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TracepointAttachSpec) Group() string               { return s.group }
func (s TracepointAttachSpec) Name() string                { return s.name }

// KprobeAttachSpec specifies how to attach a kprobe/kretprobe.
// Note: retprobe is NOT part of the spec - it's derived from the program type.
type KprobeAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
	fnName    string
	offset    uint64
}

// NewKprobeAttachSpec creates a KprobeAttachSpec with validated fields.
func NewKprobeAttachSpec(programID kernel.ProgramID, fnName string) (KprobeAttachSpec, error) {
	if programID == 0 {
		return KprobeAttachSpec{}, errors.New("programID is required")
	}
	if fnName == "" {
		return KprobeAttachSpec{}, errors.New("fnName is required")
	}
	return KprobeAttachSpec{programID: programID, fnName: fnName}, nil
}

func (KprobeAttachSpec) attachSpec()                   {}
func (s KprobeAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s KprobeAttachSpec) FnName() string              { return s.fnName }
func (s KprobeAttachSpec) Offset() uint64              { return s.offset }

// WithOffset returns a new KprobeAttachSpec with the offset set.
func (s KprobeAttachSpec) WithOffset(offset uint64) KprobeAttachSpec {
	s.offset = offset
	return s
}

// UprobeAttachSpec specifies how to attach a uprobe/uretprobe.
// Note: retprobe is NOT part of the spec - it's derived from the program type.
type UprobeAttachSpec struct {
	attachMetadata
	programID    kernel.ProgramID
	target       string
	fnName       string // optional - can use offset only
	offset       uint64
	pid          int32 // if > 0, scope the probe to this process
	containerPid int32 // if > 0, attach in this container's mount namespace
}

// NewUprobeAttachSpec creates a UprobeAttachSpec with validated fields.
// pid and containerPid are parsed here: each must be non-negative, and
// 0 means unset. fnName and offset remain optional builder fields as
// they need no validation.
func NewUprobeAttachSpec(programID kernel.ProgramID, target string, pid, containerPid int32) (UprobeAttachSpec, error) {
	if programID == 0 {
		return UprobeAttachSpec{}, errors.New("programID is required")
	}
	if target == "" {
		return UprobeAttachSpec{}, errors.New("target is required")
	}
	if pid < 0 {
		return UprobeAttachSpec{}, invalidAttachSpec("pid must be non-negative, got %d", pid)
	}
	if containerPid < 0 {
		return UprobeAttachSpec{}, invalidAttachSpec("container pid must be non-negative, got %d", containerPid)
	}
	return UprobeAttachSpec{programID: programID, target: target, pid: pid, containerPid: containerPid}, nil
}

func (UprobeAttachSpec) attachSpec()                   {}
func (s UprobeAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s UprobeAttachSpec) Target() string              { return s.target }
func (s UprobeAttachSpec) FnName() string              { return s.fnName }
func (s UprobeAttachSpec) Offset() uint64              { return s.offset }
func (s UprobeAttachSpec) Pid() int32                  { return s.pid }
func (s UprobeAttachSpec) ContainerPid() int32         { return s.containerPid }

// WithFnName returns a new UprobeAttachSpec with the function name set.
func (s UprobeAttachSpec) WithFnName(fnName string) UprobeAttachSpec {
	s.fnName = fnName
	return s
}

// WithOffset returns a new UprobeAttachSpec with the offset set.
func (s UprobeAttachSpec) WithOffset(offset uint64) UprobeAttachSpec {
	s.offset = offset
	return s
}

// FentryAttachSpec specifies how to attach fentry.
// Note: fnName comes from the program's stored metadata, not user input.
type FentryAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
}

// NewFentryAttachSpec creates a FentryAttachSpec with validated fields.
func NewFentryAttachSpec(programID kernel.ProgramID) (FentryAttachSpec, error) {
	if programID == 0 {
		return FentryAttachSpec{}, errors.New("programID is required")
	}
	return FentryAttachSpec{programID: programID}, nil
}

func (FentryAttachSpec) attachSpec()                   {}
func (s FentryAttachSpec) ProgramID() kernel.ProgramID { return s.programID }

// FexitAttachSpec specifies how to attach fexit.
// Note: fnName comes from the program's stored metadata, not user input.
type FexitAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
}

// NewFexitAttachSpec creates a FexitAttachSpec with validated fields.
func NewFexitAttachSpec(programID kernel.ProgramID) (FexitAttachSpec, error) {
	if programID == 0 {
		return FexitAttachSpec{}, errors.New("programID is required")
	}
	return FexitAttachSpec{programID: programID}, nil
}

func (FexitAttachSpec) attachSpec()                   {}
func (s FexitAttachSpec) ProgramID() kernel.ProgramID { return s.programID }

// XDPAttachSpec specifies how to attach XDP.
type XDPAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
	ifname    string
	priority  int
	proceedOn []int32
	netns     string // optional network namespace path
}

// NewXDPAttachSpec creates an XDPAttachSpec with validated fields.
// The interface is named, not pre-resolved: the manager resolves the
// name to an ifindex inside the target namespace at attach time.
// Priority is parsed here -- the single boundary: 0 normalises to
// DefaultAttachPriority and a negative value is rejected, so the
// stored value is the effective value and the library never re-checks.
func NewXDPAttachSpec(programID kernel.ProgramID, ifname string, priority int) (XDPAttachSpec, error) {
	if programID == 0 {
		return XDPAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return XDPAttachSpec{}, errors.New("ifname is required")
	}
	prio, err := validatePriority(priority)
	if err != nil {
		return XDPAttachSpec{}, err
	}
	return XDPAttachSpec{programID: programID, ifname: ifname, priority: prio}, nil
}

func (XDPAttachSpec) attachSpec()                   {}
func (s XDPAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s XDPAttachSpec) Ifname() string              { return s.ifname }
func (s XDPAttachSpec) Priority() int               { return s.priority }
func (s XDPAttachSpec) ProceedOn() []int32          { return s.proceedOn }
func (s XDPAttachSpec) Netns() string               { return s.netns }

// WithProceedOn returns a new XDPAttachSpec with the proceed-on actions set.
func (s XDPAttachSpec) WithProceedOn(po []int32) XDPAttachSpec {
	s.proceedOn = po
	return s
}

// WithProceedOnActions returns a new XDPAttachSpec with the proceed-on
// actions set from parsed domain values.
func (s XDPAttachSpec) WithProceedOnActions(actions []XDPAction) XDPAttachSpec {
	return s.WithProceedOn(XDPActionCodes(actions))
}

// WithNetns returns a new XDPAttachSpec with the network namespace path set.
// If non-empty, attachment is performed in that network namespace.
func (s XDPAttachSpec) WithNetns(netns string) XDPAttachSpec {
	s.netns = netns
	return s
}

// TCAttachSpec specifies how to attach TC.
type TCAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
	ifname    string
	direction TCDirection
	priority  int
	proceedOn []int32
	netns     string // optional network namespace path
}

// NewTCAttachSpec creates a TCAttachSpec with validated fields.
// Priority is parsed here -- the single boundary: 0 normalises to
// DefaultAttachPriority and a negative value is rejected, so the
// stored value is the effective value and the library never re-checks.
func NewTCAttachSpec(programID kernel.ProgramID, ifname string, direction TCDirection, priority int) (TCAttachSpec, error) {
	if programID == 0 {
		return TCAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return TCAttachSpec{}, errors.New("ifname is required")
	}
	if direction == (TCDirection{}) {
		return TCAttachSpec{}, errors.New("direction is required")
	}
	prio, err := validatePriority(priority)
	if err != nil {
		return TCAttachSpec{}, err
	}
	return TCAttachSpec{programID: programID, ifname: ifname, direction: direction, priority: prio}, nil
}

// NewTCAttachSpecFromString parses a TC direction and creates a TCAttachSpec.
func NewTCAttachSpecFromString(programID kernel.ProgramID, ifname, direction string, priority int) (TCAttachSpec, error) {
	dir, err := ParseTCDirection(direction)
	if err != nil {
		return TCAttachSpec{}, err
	}
	return NewTCAttachSpec(programID, ifname, dir, priority)
}

func (TCAttachSpec) attachSpec()                   {}
func (s TCAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TCAttachSpec) Ifname() string              { return s.ifname }
func (s TCAttachSpec) Direction() TCDirection      { return s.direction }
func (s TCAttachSpec) Priority() int               { return s.priority }
func (s TCAttachSpec) ProceedOn() []int32          { return s.proceedOn }
func (s TCAttachSpec) Netns() string               { return s.netns }

// WithProceedOn returns a new TCAttachSpec with the proceed-on actions set.
func (s TCAttachSpec) WithProceedOn(po []int32) TCAttachSpec {
	s.proceedOn = po
	return s
}

// WithProceedOnActions returns a new TCAttachSpec with the proceed-on
// actions set from parsed domain values.
func (s TCAttachSpec) WithProceedOnActions(actions []TCAction) TCAttachSpec {
	return s.WithProceedOn(TCActionCodes(actions))
}

// WithNetns returns a new TCAttachSpec with the network namespace path set.
// If non-empty, attachment is performed in that network namespace.
func (s TCAttachSpec) WithNetns(netns string) TCAttachSpec {
	s.netns = netns
	return s
}

// TCXAttachSpec specifies how to attach TCX.
type TCXAttachSpec struct {
	attachMetadata
	programID kernel.ProgramID
	ifname    string
	direction TCDirection
	priority  int
	netns     string // optional network namespace path
}

// NewTCXAttachSpec creates a TCXAttachSpec with validated fields.
// Priority is a userspace ordering key, stored verbatim: lower values
// run earlier and a negative value is rejected. Zero is a real
// priority that runs first and is deliberately NOT remapped to
// DefaultAttachPriority -- that 50 default is an XDP/TC dispatcher
// concept TCX has no equivalent of. An omitted Go CLI flag and an
// omitted proto3 int32 field both arrive as 0, which matches Rust's
// gRPC/runtime behaviour, so 0 is the correct unspecified value; do
// not "fix" it to 50 or 1000 (the proto's "default 1000" comment is
// stale even upstream).
func NewTCXAttachSpec(programID kernel.ProgramID, ifname string, direction TCDirection, priority int) (TCXAttachSpec, error) {
	if programID == 0 {
		return TCXAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return TCXAttachSpec{}, errors.New("ifname is required")
	}
	if direction == (TCDirection{}) {
		return TCXAttachSpec{}, errors.New("direction is required")
	}
	if priority < 0 {
		return TCXAttachSpec{}, invalidAttachSpec("priority must be non-negative, got %d", priority)
	}
	return TCXAttachSpec{programID: programID, ifname: ifname, direction: direction, priority: priority}, nil
}

// NewTCXAttachSpecFromString parses a TC direction and creates a TCXAttachSpec.
func NewTCXAttachSpecFromString(programID kernel.ProgramID, ifname, direction string, priority int) (TCXAttachSpec, error) {
	dir, err := ParseTCDirection(direction)
	if err != nil {
		return TCXAttachSpec{}, err
	}
	return NewTCXAttachSpec(programID, ifname, dir, priority)
}

func (TCXAttachSpec) attachSpec()                   {}
func (s TCXAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TCXAttachSpec) Ifname() string              { return s.ifname }
func (s TCXAttachSpec) Direction() TCDirection      { return s.direction }
func (s TCXAttachSpec) Priority() int               { return s.priority }
func (s TCXAttachSpec) Netns() string               { return s.netns }

// WithNetns returns a new TCXAttachSpec with the network namespace path set.
// If non-empty, attachment is performed in that network namespace.
func (s TCXAttachSpec) WithNetns(netns string) TCXAttachSpec {
	s.netns = netns
	return s
}

// WithMetadata builders attach user key/value labels to a spec. They are
// grouped here because the body is identical for every attach kind (set
// the embedded attachMetadata field, return the concrete type); each
// returns its own type so it composes with the other WithX builders.

func (s TracepointAttachSpec) WithMetadata(md map[string]string) TracepointAttachSpec {
	s.metadata = md
	return s
}

func (s KprobeAttachSpec) WithMetadata(md map[string]string) KprobeAttachSpec {
	s.metadata = md
	return s
}

func (s UprobeAttachSpec) WithMetadata(md map[string]string) UprobeAttachSpec {
	s.metadata = md
	return s
}

func (s FentryAttachSpec) WithMetadata(md map[string]string) FentryAttachSpec {
	s.metadata = md
	return s
}

func (s FexitAttachSpec) WithMetadata(md map[string]string) FexitAttachSpec {
	s.metadata = md
	return s
}

func (s XDPAttachSpec) WithMetadata(md map[string]string) XDPAttachSpec {
	s.metadata = md
	return s
}

func (s TCAttachSpec) WithMetadata(md map[string]string) TCAttachSpec {
	s.metadata = md
	return s
}

func (s TCXAttachSpec) WithMetadata(md map[string]string) TCXAttachSpec {
	s.metadata = md
	return s
}
