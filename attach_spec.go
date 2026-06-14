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

// ValidateAttachSpec validates the spec kinds whose validation has not
// yet moved into their constructors: uprobe pid bounds and TCX
// priority. XDP, TC, and the simple probe specs are fully refined at
// construction and pass through unchanged. It is invoked by the
// manager as the last gate before acting on a spec.
func ValidateAttachSpec(spec AttachSpec) (AttachSpec, error) {
	switch s := spec.(type) {
	case TracepointAttachSpec:
		if s.programID == 0 {
			return nil, invalidAttachSpec("programID is required")
		}
		if s.group == "" || s.name == "" {
			return nil, invalidAttachSpec("tracepoint is required")
		}
		return s, nil
	case KprobeAttachSpec:
		if s.programID == 0 {
			return nil, invalidAttachSpec("programID is required")
		}
		if s.fnName == "" {
			return nil, invalidAttachSpec("fnName is required")
		}
		return s, nil
	case UprobeAttachSpec:
		if s.programID == 0 {
			return nil, invalidAttachSpec("programID is required")
		}
		if s.target == "" {
			return nil, invalidAttachSpec("target is required")
		}
		if s.pid < 0 {
			return nil, invalidAttachSpec("pid must be non-negative, got %d", s.pid)
		}
		if s.containerPid < 0 {
			return nil, invalidAttachSpec("container pid must be non-negative, got %d", s.containerPid)
		}
		return s, nil
	case FentryAttachSpec:
		if s.programID == 0 {
			return nil, invalidAttachSpec("programID is required")
		}
		return s, nil
	case FexitAttachSpec:
		if s.programID == 0 {
			return nil, invalidAttachSpec("programID is required")
		}
		return s, nil
	case XDPAttachSpec, TCAttachSpec:
		// XDP and TC are fully parsed by their constructors
		// (NewXDPAttachSpec/NewTCAttachSpec): required fields are
		// checked and priority is normalised and validated there.
		// By the time such a spec reaches the library it is already
		// refined, so there is nothing to do here.
		return spec, nil
	case TCXAttachSpec:
		if s.programID == 0 {
			return nil, invalidAttachSpec("programID is required")
		}
		if s.ifname == "" {
			return nil, invalidAttachSpec("ifname is required")
		}
		if s.direction == (TCDirection{}) {
			return nil, invalidAttachSpec("direction is required")
		}
		priority, err := validatePriority(s.priority)
		if err != nil {
			return nil, err
		}
		s.priority = priority
		return s, nil
	default:
		return nil, invalidAttachSpec("unsupported attach spec type %T", spec)
	}
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

func normalisePriority(p int) int {
	if p == 0 {
		return DefaultAttachPriority
	}
	return p
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
func NewUprobeAttachSpec(programID kernel.ProgramID, target string) (UprobeAttachSpec, error) {
	if programID == 0 {
		return UprobeAttachSpec{}, errors.New("programID is required")
	}
	if target == "" {
		return UprobeAttachSpec{}, errors.New("target is required")
	}
	return UprobeAttachSpec{programID: programID, target: target}, nil
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

// WithPid returns a new UprobeAttachSpec with the pid filter set.
// If pid > 0, the probe fires only for that process; 0 traces all
// processes. Distinct from the container pid, which selects the
// mount namespace the target path resolves in, not which process
// triggers the probe.
func (s UprobeAttachSpec) WithPid(pid int32) UprobeAttachSpec {
	s.pid = pid
	return s
}

// WithContainerPid returns a new UprobeAttachSpec with the container PID set.
// If pid > 0, the uprobe will be attached in that container's mount namespace,
// allowing the target path to resolve within the container's filesystem.
func (s UprobeAttachSpec) WithContainerPid(pid int32) UprobeAttachSpec {
	s.containerPid = pid
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
func NewTCXAttachSpec(programID kernel.ProgramID, ifname string, direction TCDirection) (TCXAttachSpec, error) {
	if programID == 0 {
		return TCXAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return TCXAttachSpec{}, errors.New("ifname is required")
	}
	if direction == (TCDirection{}) {
		return TCXAttachSpec{}, errors.New("direction is required")
	}
	return TCXAttachSpec{programID: programID, ifname: ifname, direction: direction, priority: DefaultAttachPriority}, nil
}

// NewTCXAttachSpecFromString parses a TC direction and creates a TCXAttachSpec.
func NewTCXAttachSpecFromString(programID kernel.ProgramID, ifname, direction string) (TCXAttachSpec, error) {
	dir, err := ParseTCDirection(direction)
	if err != nil {
		return TCXAttachSpec{}, err
	}
	return NewTCXAttachSpec(programID, ifname, dir)
}

func (TCXAttachSpec) attachSpec()                   {}
func (s TCXAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TCXAttachSpec) Ifname() string              { return s.ifname }
func (s TCXAttachSpec) Direction() TCDirection      { return s.direction }
func (s TCXAttachSpec) Priority() int               { return s.priority }
func (s TCXAttachSpec) Netns() string               { return s.netns }

// WithPriority returns a new TCXAttachSpec with the priority set.
func (s TCXAttachSpec) WithPriority(p int) TCXAttachSpec {
	s.priority = normalisePriority(p)
	return s
}

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
