package bpfmanfs

import (
	"fmt"
	"path/filepath"
)

// FSLayout is an immutable, validated filesystem layout. Fields are
// unexported; external packages cannot construct a non-zero FSLayout
// without calling New.
//
// FSLayout acts as a capability token following the same pattern as
// lock.WriterScope: possession of a valid FSLayout proves the base path
// has been validated.
//
// FSLayout is deliberately I/O free - it only computes and validates paths.
// Callers use bpfmanfs/runtime.Ensure() to create directories and mount
// bpffs before constructing a manager. This separation enables testing
// without root privileges or real filesystems.
type FSLayout struct {
	base string
}

// New creates an FSLayout for bpfman's runtime directory.
//
// The root path is used directly - callers must provide the full path
// including any "bpfman" suffix if desired.
//
// Examples:
//   - New("/run/bpfman") -> /run/bpfman
//   - New("/tmp/test/bpfman") -> /tmp/test/bpfman
//
// New rejects empty paths and relative paths.
func New(root string) (FSLayout, error) {
	if root == "" {
		return FSLayout{}, fmt.Errorf("bpfmanfs: root path cannot be empty")
	}
	if !filepath.IsAbs(root) {
		return FSLayout{}, fmt.Errorf("bpfmanfs: root path must be absolute, got %q", root)
	}
	return FSLayout{base: filepath.Clean(root)}, nil
}

// Valid reports whether l was constructed via New.
func (l FSLayout) Valid() bool {
	return l.base != ""
}

// String returns a string representation safe for logging.
// Unlike Base(), this never panics and can be used on zero-value FSLayouts.
func (l FSLayout) String() string {
	if !l.Valid() {
		return "bpfmanfs.FSLayout(<invalid>)"
	}
	return "bpfmanfs.FSLayout(" + l.base + ")"
}

// mustValid panics if l is a zero-value FSLayout.
// This catches programmer errors where an FSLayout is used without construction via New.
func (l FSLayout) mustValid() {
	if !l.Valid() {
		panic("bpfmanfs: zero FSLayout used; construct via bpfmanfs.New")
	}
}

// Base returns the runtime base path (e.g., /run/bpfman).
func (l FSLayout) Base() string {
	l.mustValid()
	return l.base
}

// BytecodeFS returns the regular-filesystem hierarchy domain for bytecode persistence.
func (l FSLayout) BytecodeFS() BytecodeFS {
	l.mustValid()
	return BytecodeFS{layout: l}
}

// BPFFS returns the bpffs hierarchy domain.
func (l FSLayout) BPFFS() BPFFS {
	l.mustValid()
	return BPFFS{layout: l}
}

// LockPath returns the global writer lock file path.
func (l FSLayout) LockPath() string {
	l.mustValid()
	return filepath.Join(l.base, ".lock")
}

// DBPath returns the full path to the SQLite database file.
func (l FSLayout) DBPath() string {
	l.mustValid()
	return filepath.Join(l.base, "db", "store.db")
}

// SocketPath returns the full path to the gRPC socket.
func (l FSLayout) SocketPath() string {
	l.mustValid()
	return filepath.Join(l.base, "sock", "bpfman.sock")
}

// CSISocketPath returns the full path to the CSI socket.
func (l FSLayout) CSISocketPath() string {
	l.mustValid()
	return filepath.Join(l.base, "csi", "csi.sock")
}

// BPFFSMountPoint returns the bpffs mount point path.
// Deprecated: use l.BPFFS().MountPoint() for consistency with the domain model.
func (l FSLayout) BPFFSMountPoint() string {
	return l.BPFFS().MountPoint()
}

// DBDir returns the directory containing the database file.
func (l FSLayout) DBDir() string {
	l.mustValid()
	return filepath.Join(l.base, "db")
}

// SocketDir returns the directory containing the gRPC socket.
func (l FSLayout) SocketDir() string {
	l.mustValid()
	return filepath.Join(l.base, "sock")
}

// CSIDir returns the CSI directory path.
func (l FSLayout) CSIDir() string {
	l.mustValid()
	return filepath.Join(l.base, "csi")
}

// CSIFSDir returns the CSI filesystem directory path.
func (l FSLayout) CSIFSDir() string {
	l.mustValid()
	return filepath.Join(l.base, "csi", "fs")
}

// RuntimeDirs returns the directories required for basic runtime operation.
// This includes Base() itself plus its required subdirectories.
// Callers should create these directories at startup.
func (l FSLayout) RuntimeDirs() []string {
	l.mustValid()
	return []string{l.base, l.DBDir(), l.SocketDir()}
}

// CSIDirs returns the directories required for CSI operation.
// Callers should create these directories only when CSI is enabled.
func (l FSLayout) CSIDirs() []string {
	l.mustValid()
	return []string{l.CSIDir(), l.CSIFSDir()}
}
