package bpfman

import (
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/kernel"
)

// ErrLinkNotFound is returned when attempting to operate on a link
// that does not exist in either the kernel or bpfman's store.
type ErrLinkNotFound struct {
	LinkID LinkID `json:"link_id"`
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

// ErrTracepointNotFound is returned when an attach targets a kernel
// tracepoint that is not present in /sys/kernel/tracing/events/.
// Suggestions holds up to a few nearest-match tracepoints computed by
// the manager; empty when nothing close enough was found or when the
// kernel could not be consulted.
type ErrTracepointNotFound struct {
	Group       string   `json:"group"`
	Name        string   `json:"name"`
	Suggestions []string `json:"suggestions,omitempty"`
}

func (e ErrTracepointNotFound) Error() string {
	msg := fmt.Sprintf("tracepoint %q not found", e.Group+"/"+e.Name)
	if len(e.Suggestions) == 0 {
		return msg
	}
	return msg + "; did you mean: " + strings.Join(e.Suggestions, ", ") + "?"
}
