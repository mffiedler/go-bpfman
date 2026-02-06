// Package bpfmanfs models bpfman's runtime filesystem hierarchy.
//
// The package provides capability tokens for safe path construction:
//
//   - Root: validated runtime root, constructed via New
//   - Runtime: bytecode persistence operations, obtained via Root.Runtime()
//   - BPFFS: bpffs pin path conventions, obtained via Root.BPFFS()
//
// All types enforce validity: methods panic on zero-value receivers to
// catch programmer errors. This prevents accidentally obtaining paths
// from uninitialised values.
package bpfmanfs

import (
	"errors"
	"fmt"
)

// ErrInvalidRoot is returned when an operation is called on a
// zero-value Root, Runtime, or BPFFS.
var ErrInvalidRoot = errors.New("bpfmanfs: invalid root (zero value)")

// ErrFinalExists is returned by PublishBytecode when the final
// directory already exists. This is an invariant violation: GC
// should have removed orphan directories before the load path
// executes.
var ErrFinalExists = errors.New("bpfmanfs: final directory already exists")

// ErrOutsideRoot is returned when a path safety check fails.
type ErrOutsideRoot struct {
	Parent string
	Target string
}

func (e ErrOutsideRoot) Error() string {
	return fmt.Sprintf("bpfmanfs: target %q is outside parent %q", e.Target, e.Parent)
}

// PathError wraps a filesystem error with the operation and path.
type PathError struct {
	Op   string
	Path string
	Err  error
}

func (e *PathError) Error() string {
	return fmt.Sprintf("bpfmanfs: %s %s: %v", e.Op, e.Path, e.Err)
}

func (e *PathError) Unwrap() error {
	return e.Err
}
