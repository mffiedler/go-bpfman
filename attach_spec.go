package bpfman

import (
	"errors"

	"github.com/frobware/go-bpfman/kernel"
)

// AttachSpec is a sealed interface satisfied by all concrete attach
// spec types.  The unexported marker method prevents external packages
// from implementing it, so the set of valid types is closed.
type AttachSpec interface {
	attachSpec() // sealed marker
	ProgramID() kernel.ProgramID
}

// TracepointAttachSpec specifies how to attach a tracepoint.
type TracepointAttachSpec struct {
	programID kernel.ProgramID
	group     string
	name      string
}

// NewTracepointAttachSpec creates a TracepointAttachSpec with validated fields.
func NewTracepointAttachSpec(programID kernel.ProgramID, group, name string) (TracepointAttachSpec, error) {
	if programID == 0 {
		return TracepointAttachSpec{}, errors.New("programID is required")
	}
	if group == "" {
		return TracepointAttachSpec{}, errors.New("group is required")
	}
	if name == "" {
		return TracepointAttachSpec{}, errors.New("name is required")
	}
	return TracepointAttachSpec{programID: programID, group: group, name: name}, nil
}

func (TracepointAttachSpec) attachSpec()                   {}
func (s TracepointAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TracepointAttachSpec) Group() string               { return s.group }
func (s TracepointAttachSpec) Name() string                { return s.name }

// KprobeAttachSpec specifies how to attach a kprobe/kretprobe.
// Note: retprobe is NOT part of the spec - it's derived from the program type.
type KprobeAttachSpec struct {
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
	programID    kernel.ProgramID
	target       string
	fnName       string // optional - can use offset only
	offset       uint64
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
	programID kernel.ProgramID
	ifname    string
	ifindex   int
	netns     string // optional network namespace path
}

// NewXDPAttachSpec creates an XDPAttachSpec with validated fields.
func NewXDPAttachSpec(programID kernel.ProgramID, ifname string, ifindex int) (XDPAttachSpec, error) {
	if programID == 0 {
		return XDPAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return XDPAttachSpec{}, errors.New("ifname is required")
	}
	if ifindex <= 0 {
		return XDPAttachSpec{}, errors.New("ifindex must be positive")
	}
	return XDPAttachSpec{programID: programID, ifname: ifname, ifindex: ifindex}, nil
}

func (XDPAttachSpec) attachSpec()                   {}
func (s XDPAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s XDPAttachSpec) Ifname() string              { return s.ifname }
func (s XDPAttachSpec) Ifindex() int                { return s.ifindex }
func (s XDPAttachSpec) Netns() string               { return s.netns }

// WithNetns returns a new XDPAttachSpec with the network namespace path set.
// If non-empty, attachment is performed in that network namespace.
func (s XDPAttachSpec) WithNetns(netns string) XDPAttachSpec {
	s.netns = netns
	return s
}

// TCAttachSpec specifies how to attach TC.
type TCAttachSpec struct {
	programID kernel.ProgramID
	ifname    string
	ifindex   int
	direction TCDirection
	priority  int
	proceedOn []int32
	netns     string // optional network namespace path
}

// NewTCAttachSpec creates a TCAttachSpec with validated fields.
// direction must be a valid TCDirection (use ParseTCDirection to parse from strings).
func NewTCAttachSpec(programID kernel.ProgramID, ifname string, ifindex int, direction TCDirection) (TCAttachSpec, error) {
	if programID == 0 {
		return TCAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return TCAttachSpec{}, errors.New("ifname is required")
	}
	if ifindex <= 0 {
		return TCAttachSpec{}, errors.New("ifindex must be positive")
	}
	if direction == (TCDirection{}) {
		return TCAttachSpec{}, errors.New("direction is required")
	}
	return TCAttachSpec{programID: programID, ifname: ifname, ifindex: ifindex, direction: direction}, nil
}

func (TCAttachSpec) attachSpec()                   {}
func (s TCAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TCAttachSpec) Ifname() string              { return s.ifname }
func (s TCAttachSpec) Ifindex() int                { return s.ifindex }
func (s TCAttachSpec) Direction() TCDirection      { return s.direction }
func (s TCAttachSpec) Priority() int               { return s.priority }
func (s TCAttachSpec) ProceedOn() []int32          { return s.proceedOn }
func (s TCAttachSpec) Netns() string               { return s.netns }

// WithPriority returns a new TCAttachSpec with the priority set.
func (s TCAttachSpec) WithPriority(p int) TCAttachSpec {
	s.priority = p
	return s
}

// WithProceedOn returns a new TCAttachSpec with the proceed-on actions set.
func (s TCAttachSpec) WithProceedOn(po []int32) TCAttachSpec {
	s.proceedOn = po
	return s
}

// WithNetns returns a new TCAttachSpec with the network namespace path set.
// If non-empty, attachment is performed in that network namespace.
func (s TCAttachSpec) WithNetns(netns string) TCAttachSpec {
	s.netns = netns
	return s
}

// TCXAttachSpec specifies how to attach TCX.
type TCXAttachSpec struct {
	programID kernel.ProgramID
	ifname    string
	ifindex   int
	direction TCDirection
	priority  int
	netns     string // optional network namespace path
}

// NewTCXAttachSpec creates a TCXAttachSpec with validated fields.
// direction must be a valid TCDirection (use ParseTCDirection to parse from strings).
func NewTCXAttachSpec(programID kernel.ProgramID, ifname string, ifindex int, direction TCDirection) (TCXAttachSpec, error) {
	if programID == 0 {
		return TCXAttachSpec{}, errors.New("programID is required")
	}
	if ifname == "" {
		return TCXAttachSpec{}, errors.New("ifname is required")
	}
	if ifindex <= 0 {
		return TCXAttachSpec{}, errors.New("ifindex must be positive")
	}
	if direction == (TCDirection{}) {
		return TCXAttachSpec{}, errors.New("direction is required")
	}
	return TCXAttachSpec{programID: programID, ifname: ifname, ifindex: ifindex, direction: direction}, nil
}

func (TCXAttachSpec) attachSpec()                   {}
func (s TCXAttachSpec) ProgramID() kernel.ProgramID { return s.programID }
func (s TCXAttachSpec) Ifname() string              { return s.ifname }
func (s TCXAttachSpec) Ifindex() int                { return s.ifindex }
func (s TCXAttachSpec) Direction() TCDirection      { return s.direction }
func (s TCXAttachSpec) Priority() int               { return s.priority }
func (s TCXAttachSpec) Netns() string               { return s.netns }

// WithPriority returns a new TCXAttachSpec with the priority set.
func (s TCXAttachSpec) WithPriority(p int) TCXAttachSpec {
	s.priority = p
	return s
}

// WithNetns returns a new TCXAttachSpec with the network namespace path set.
// If non-empty, attachment is performed in that network namespace.
func (s TCXAttachSpec) WithNetns(netns string) TCXAttachSpec {
	s.netns = netns
	return s
}
