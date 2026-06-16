package dispatcher

import (
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
)

// XDPExtensionAttachSpec contains parameters for attaching an XDP extension
// program to a dispatcher slot. The extension program is loaded from its
// bpffs pin rather than re-read from the original ELF file.
type XDPExtensionAttachSpec struct {
	DispatcherPinPath bpfman.ProgPinPath `json:"dispatcher_pin_path"` // pinned dispatcher program
	ProgPinPath       bpfman.ProgPinPath `json:"prog_pin_path"`       // pinned extension program
	ProgramName       string             `json:"program_name"`        // program name for slot derivation
	Position          int                `json:"position"`            // dispatcher slot [0, MaxPrograms)
	// LinkPinPath empty means the extension link is ephemeral (not pinned); the
	// empty string is the discriminator for ephemeral versus pinned extensions.
	LinkPinPath bpfman.LinkPath `json:"link_pin_path,omitempty"`
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
	DispatcherPinPath bpfman.ProgPinPath `json:"dispatcher_pin_path"` // pinned dispatcher program
	ProgPinPath       bpfman.ProgPinPath `json:"prog_pin_path"`       // pinned extension program
	ProgramName       string             `json:"program_name"`        // program name for slot derivation
	Position          int                `json:"position"`            // dispatcher slot [0, MaxPrograms)
	// LinkPinPath empty means the extension link is ephemeral (not pinned); the
	// empty string is the discriminator for ephemeral versus pinned extensions.
	LinkPinPath bpfman.LinkPath `json:"link_pin_path,omitempty"`
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
