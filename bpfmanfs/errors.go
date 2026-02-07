// Package bpfmanfs models bpfman's runtime filesystem hierarchy.
//
// The package provides capability tokens for safe path construction:
//
//   - FSLayout: validated filesystem layout, constructed via New
//   - FilesystemContext: capability token from runtime.New() proving directories and bpffs are ready
//   - BytecodeFS: bytecode persistence operations, obtained via Layout.BytecodeFS()
//   - BPFFS: bpffs pin path conventions, obtained via Layout.BPFFS()
//
// All types enforce validity: methods panic on zero-value receivers to
// catch programmer errors. This prevents accidentally obtaining paths
// from uninitialised values.
package bpfmanfs

import (
	"errors"
	"fmt"
)

// ErrInvalidLayout is returned when an operation is called on a
// zero-value Layout, Runtime, or BPFFS.
var ErrInvalidLayout = errors.New("bpfmanfs: invalid layout (zero value)")

// ErrFinalExists is returned by PublishBytecode when the final
// directory already exists. This is an invariant violation: GC
// should have removed orphan directories before the load path
// executes.
var ErrFinalExists = errors.New("bpfmanfs: final directory already exists")

// ErrOutsideLayout is returned when a path safety check fails.
type ErrOutsideLayout struct {
	Parent string
	Target string
}

func (e ErrOutsideLayout) Error() string {
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
