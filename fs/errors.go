// Package fs centralises all bpfman filesystem layout and mutations.
//
// Callers express intent (publish, remove, ensure) rather than
// computing paths or calling os.* directly. The package provides
// three capability types:
//
//   - Root: validated filesystem root with infrastructure path methods
//   - Runtime: regular-filesystem operations (bytecode persistence)
//   - BPFFS: bpffs layout conventions (pin paths, scanner dirs)
//
// All types have unexported fields and are constructible only via Open.
package fs

import (
	"errors"
	"fmt"
)

// ErrInvalidRoot is returned when an operation is called on a
// zero-value Root, Runtime, or BPFFS.
var ErrInvalidRoot = errors.New("fs: invalid root (zero value)")

// ErrFinalExists is returned by PublishBytecode when the final
// directory already exists. This is an invariant violation: GC
// should have removed orphan directories before the load path
// executes.
var ErrFinalExists = errors.New("fs: final directory already exists")

// ErrOutsideRoot is returned when a path safety check fails.
type ErrOutsideRoot struct {
	Parent string
	Target string
}

func (e ErrOutsideRoot) Error() string {
	return fmt.Sprintf("fs: target %q is outside parent %q", e.Target, e.Parent)
}

// PathError wraps a filesystem error with the operation and path.
type PathError struct {
	Op   string
	Path string
	Err  error
}

func (e *PathError) Error() string {
	return fmt.Sprintf("fs: %s %s: %v", e.Op, e.Path, e.Err)
}

func (e *PathError) Unwrap() error {
	return e.Err
}
