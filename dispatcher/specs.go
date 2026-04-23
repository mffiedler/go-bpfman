package dispatcher

import (
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
)

// XDPDispatcherAttachSpec contains parameters for creating an XDP dispatcher.
type XDPDispatcherAttachSpec struct {
	Target      bpfman.AttachTarget `json:"target"`
	ProgPinPath string              `json:"prog_pin_path"` // where to pin dispatcher program
	LinkPinPath string              `json:"link_pin_path"` // where to pin dispatcher link
	NumProgs    int                 `json:"num_progs"`     // extension slot count
	ProceedOn   uint32              `json:"proceed_on"`    // bitmask of actions to proceed on; 0 means "none"
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
	if s.NumProgs < 1 || s.NumProgs > MaxPrograms {
		return fmt.Errorf("NumProgs %d out of range [1, %d]", s.NumProgs, MaxPrograms)
	}
	return nil
}

// TCDispatcherAttachSpec contains parameters for creating a TC dispatcher.
// Note: TC legacy uses netlink, not BPF links, so no LinkPinPath.
// TC netlink requires interface name; manager resolves and supplies it.
type TCDispatcherAttachSpec struct {
	Target      bpfman.AttachTarget `json:"target"`
	IfName      string              `json:"ifname"` // needed for netlink operations
	ProgPinPath string              `json:"prog_pin_path"`
	Direction   bpfman.TCDirection  `json:"direction"`
	NumProgs    int                 `json:"num_progs"`
	ProceedOn   uint32              `json:"proceed_on"` // bitmask of actions to proceed on; 0 means "none"
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
	if s.Direction == (bpfman.TCDirection{}) {
		return errors.New("direction is required")
	}
	if s.NumProgs < 1 || s.NumProgs > MaxPrograms {
		return fmt.Errorf("NumProgs %d out of range [1, %d]", s.NumProgs, MaxPrograms)
	}
	return nil
}

// XDPExtensionAttachSpec contains parameters for attaching an XDP extension
// program to a dispatcher slot. The extension program is loaded from its
// bpffs pin rather than re-read from the original ELF file.
type XDPExtensionAttachSpec struct {
	DispatcherPinPath string `json:"dispatcher_pin_path"` // pinned dispatcher program
	ProgPinPath       string `json:"prog_pin_path"`       // pinned extension program
	ProgramName       string `json:"program_name"`        // program name for slot derivation
	Position          int    `json:"position"`            // dispatcher slot [0, MaxPrograms)
	// LinkPinPath empty means the extension link is ephemeral (not pinned); the
	// empty string is the discriminator for ephemeral versus pinned extensions.
	LinkPinPath string `json:"link_pin_path,omitempty"`
}

// Validate checks the spec for invalid or missing values.
func (s XDPExtensionAttachSpec) Validate() error {
	if s.DispatcherPinPath == "" {
		return errors.New("XDP extension: DispatcherPinPath is required")
	}
	if s.ProgPinPath == "" {
		return errors.New("XDP extension: ProgPinPath is required")
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
// program to a dispatcher slot. The extension program is loaded from its
// bpffs pin rather than re-read from the original ELF file.
type TCExtensionAttachSpec struct {
	DispatcherPinPath string `json:"dispatcher_pin_path"` // pinned dispatcher program
	ProgPinPath       string `json:"prog_pin_path"`       // pinned extension program
	ProgramName       string `json:"program_name"`        // program name for slot derivation
	Position          int    `json:"position"`            // dispatcher slot [0, MaxPrograms)
	// LinkPinPath empty means the extension link is ephemeral (not pinned); the
	// empty string is the discriminator for ephemeral versus pinned extensions.
	LinkPinPath string `json:"link_pin_path,omitempty"`
}

// Validate checks the spec for invalid or missing values.
func (s TCExtensionAttachSpec) Validate() error {
	if s.DispatcherPinPath == "" {
		return errors.New("TC extension: DispatcherPinPath is required")
	}
	if s.ProgPinPath == "" {
		return errors.New("TC extension: ProgPinPath is required")
	}
	if s.ProgramName == "" {
		return errors.New("TC extension: ProgramName is required")
	}
	if s.Position < 0 || s.Position >= MaxPrograms {
		return fmt.Errorf("TC extension: Position %d out of range [0, %d)", s.Position, MaxPrograms)
	}
	return nil
}
