package dispatcher

import (
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
)

// XDPDispatcherAttachSpec contains parameters for creating an XDP dispatcher.
type XDPDispatcherAttachSpec struct {
	Target      bpfman.AttachTarget
	ProgPinPath string // where to pin dispatcher program
	LinkPinPath string // where to pin dispatcher link
	NumProgs    int    // extension slot count
	ProceedOn   uint32 // XDP action bitmask
}

// Validate checks the spec for invalid or missing values.
func (s XDPDispatcherAttachSpec) Validate() error {
	if s.Target.IfIndex <= 0 {
		return errors.New("IfIndex must be positive")
	}
	if s.ProgPinPath == "" {
		return errors.New("ProgPinPath is required")
	}
	if s.LinkPinPath == "" {
		return errors.New("LinkPinPath is required")
	}
	if s.NumProgs <= 0 {
		return errors.New("NumProgs must be positive")
	}
	return nil
}

// TCDispatcherAttachSpec contains parameters for creating a TC dispatcher.
// Note: TC legacy uses netlink, not BPF links, so no LinkPinPath.
// TC netlink requires interface name; manager resolves and supplies it.
type TCDispatcherAttachSpec struct {
	Target      bpfman.AttachTarget
	IfName      string             // needed for netlink operations
	ProgPinPath string             // where to pin dispatcher program
	Direction   bpfman.TCDirection // ingress or egress
	NumProgs    int                // extension slot count
	ProceedOn   uint32             // TC action bitmask
}

// Validate checks the spec for invalid or missing values.
func (s TCDispatcherAttachSpec) Validate() error {
	if s.Target.IfIndex <= 0 {
		return errors.New("IfIndex must be positive")
	}
	if s.IfName == "" {
		return errors.New("IfName is required")
	}
	if s.ProgPinPath == "" {
		return errors.New("ProgPinPath is required")
	}
	if s.Direction != bpfman.TCDirectionIngress &&
		s.Direction != bpfman.TCDirectionEgress {
		return errors.New("Direction must be ingress or egress")
	}
	if s.NumProgs <= 0 {
		return errors.New("NumProgs must be positive")
	}
	return nil
}

// XDPExtensionAttachSpec contains parameters for attaching an XDP extension
// program to a dispatcher slot.
type XDPExtensionAttachSpec struct {
	DispatcherPinPath string // pinned dispatcher program
	ObjectPath        string // ELF file containing extension
	ProgramName       string // program name within ELF
	Position          int    // dispatcher slot [0, MaxPrograms)
	LinkPinPath       string // optional - empty for ephemeral
	MapPinDir         string // optional - empty if no maps
}

// Validate checks the spec for invalid or missing values.
func (s XDPExtensionAttachSpec) Validate() error {
	if s.DispatcherPinPath == "" {
		return errors.New("XDP extension: DispatcherPinPath is required")
	}
	if s.ObjectPath == "" {
		return errors.New("XDP extension: ObjectPath is required")
	}
	if s.ProgramName == "" {
		return errors.New("XDP extension: ProgramName is required")
	}
	if s.Position < 0 || s.Position >= MaxPrograms {
		return fmt.Errorf("XDP extension: Position %d out of range [0, %d)", s.Position, MaxPrograms)
	}
	return nil
}

// TCExtensionAttachSpec contains parameters for attaching a TC extension
// program to a dispatcher slot.
type TCExtensionAttachSpec struct {
	DispatcherPinPath string // pinned dispatcher program
	ObjectPath        string // ELF file containing extension
	ProgramName       string // program name within ELF
	Position          int    // dispatcher slot [0, MaxPrograms)
	LinkPinPath       string // optional - empty for ephemeral
	MapPinDir         string // optional - empty if no maps
}

// Validate checks the spec for invalid or missing values.
func (s TCExtensionAttachSpec) Validate() error {
	if s.DispatcherPinPath == "" {
		return errors.New("TC extension: DispatcherPinPath is required")
	}
	if s.ObjectPath == "" {
		return errors.New("TC extension: ObjectPath is required")
	}
	if s.ProgramName == "" {
		return errors.New("TC extension: ProgramName is required")
	}
	if s.Position < 0 || s.Position >= MaxPrograms {
		return fmt.Errorf("TC extension: Position %d out of range [0, %d)", s.Position, MaxPrograms)
	}
	return nil
}
