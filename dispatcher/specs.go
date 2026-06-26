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
	// DispatcherPinPath is the bpffs pin of the dispatcher program the
	// extension attaches into.
	DispatcherPinPath bpfman.ProgPinPath `json:"dispatcher_pin_path"`

	// ProgPinPath is the bpffs pin of the extension program; it is
	// loaded from this pin rather than re-read from the original ELF.
	ProgPinPath bpfman.ProgPinPath `json:"prog_pin_path"`

	// ProgramName is the extension program's name. The dispatcher slot
	// the extension attaches into is selected by Position (see
	// SlotName), not by this name.
	ProgramName string `json:"program_name"`

	// Position is the dispatcher slot to attach into, in the range
	// [0, MaxPrograms).
	Position int `json:"position"`

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
	// DispatcherPinPath is the bpffs pin of the dispatcher program the
	// extension attaches into.
	DispatcherPinPath bpfman.ProgPinPath `json:"dispatcher_pin_path"`

	// ProgPinPath is the bpffs pin of the extension program; it is
	// loaded from this pin rather than re-read from the original ELF.
	ProgPinPath bpfman.ProgPinPath `json:"prog_pin_path"`

	// ProgramName is the extension program's name. The dispatcher slot
	// the extension attaches into is selected by Position (see
	// SlotName), not by this name.
	ProgramName string `json:"program_name"`

	// Position is the dispatcher slot to attach into, in the range
	// [0, MaxPrograms).
	Position int `json:"position"`

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
