package fs

import (
	"errors"
	"fmt"
)

// ErrInvalidLayout is returned when an operation is called on a
// zero-value Layout, Runtime, or BPFFS.
var ErrInvalidLayout = errors.New("fs: invalid layout (zero value)")

// ErrFinalExists is returned by PublishBytecode when the final
// directory already exists. This is an invariant violation: GC
// should have removed orphan directories before the load path
// executes.
var ErrFinalExists = errors.New("fs: final directory already exists")

// ErrOutsideLayout is returned when a path safety check fails.
type ErrOutsideLayout struct {
	Parent string
	Target string
}

func (e ErrOutsideLayout) Error() string {
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
