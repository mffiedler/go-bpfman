package bpfman

import (
	"fmt"

	"github.com/frobware/go-bpfman/kernel"
)

// ErrLinkNotManaged is returned when attempting to operate on a link
// that exists in the kernel but is not managed by bpfman.
type ErrLinkNotManaged struct {
	LinkID kernel.LinkID `json:"link_id"`
}

func (e ErrLinkNotManaged) Error() string {
	return fmt.Sprintf("link %d exists in kernel but is not managed by bpfman", e.LinkID)
}

// ErrLinkNotFound is returned when attempting to operate on a link
// that does not exist in either the kernel or bpfman's store.
type ErrLinkNotFound struct {
	LinkID kernel.LinkID `json:"link_id"`
}

func (e ErrLinkNotFound) Error() string {
	return fmt.Sprintf("link %d does not exist", e.LinkID)
}

// ErrProgramNotManaged is returned when attempting to operate on a program
// that exists in the kernel but is not managed by bpfman.
type ErrProgramNotManaged struct {
	ID kernel.ProgramID `json:"id"`
}

func (e ErrProgramNotManaged) Error() string {
	return fmt.Sprintf("program %d exists in kernel but is not managed by bpfman", e.ID)
}

// ErrProgramNotFound is returned when attempting to operate on a program
// that does not exist in either the kernel or bpfman's store.
type ErrProgramNotFound struct {
	ID kernel.ProgramID `json:"id"`
}

func (e ErrProgramNotFound) Error() string {
	return fmt.Sprintf("program %d does not exist", e.ID)
}
