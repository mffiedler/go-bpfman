package bpfmanfs

import (
	"fmt"
	"path/filepath"
)

// Root is an immutable, validated filesystem root. Fields are
// unexported; external packages cannot construct a non-zero Root
// without calling New.
//
// Root acts as a capability token following the same pattern as
// lock.WriterScope: possession of a valid Root proves the base path
// has been validated.
//
// Root is deliberately I/O free - it only computes and validates paths.
// Callers use bpfmanfs/runtime.Ensure() to create directories and mount
// bpffs before constructing a manager. This separation enables testing
// without root privileges or real filesystems.
type Root struct {
	base string
}

// New creates a Root for bpfman's runtime directory.
//
// The base path specifies the parent directory; New always appends
// "bpfman" to create the actual runtime root. This ensures bpfman
// operates in a controlled subdirectory regardless of what base is
// provided, preventing accidental operations on system directories.
//
// Examples:
//   - New("/run") -> /run/bpfman
//   - New("/tmp/test") -> /tmp/test/bpfman
//   - New("/") -> /bpfman
//
// New rejects empty paths and relative paths.
func New(base string) (Root, error) {
	if base == "" {
		return Root{}, fmt.Errorf("bpfmanfs: base path cannot be empty")
	}
	if !filepath.IsAbs(base) {
		return Root{}, fmt.Errorf("bpfmanfs: base path must be absolute, got %q", base)
	}
	base = filepath.Clean(base)
	return Root{base: filepath.Join(base, "bpfman")}, nil
}

// Valid reports whether r was constructed via New.
func (r Root) Valid() bool {
	return r.base != ""
}

// String returns a string representation safe for logging.
// Unlike Base(), this never panics and can be used on zero-value Roots.
func (r Root) String() string {
	if !r.Valid() {
		return "bpfmanfs.Root(<invalid>)"
	}
	return "bpfmanfs.Root(" + r.base + ")"
}

// mustValid panics if r is a zero-value Root.
// This catches programmer errors where a Root is used without construction via New.
func (r Root) mustValid() {
	if !r.Valid() {
		panic("bpfmanfs: zero Root used; construct via bpfmanfs.New")
	}
}

// Base returns the runtime root path (e.g., /run/bpfman).
func (r Root) Base() string {
	r.mustValid()
	return r.base
}

// Runtime returns the regular-filesystem hierarchy domain.
func (r Root) Runtime() Runtime {
	r.mustValid()
	return Runtime{root: r}
}

// BPFFS returns the bpffs hierarchy domain.
func (r Root) BPFFS() BPFFS {
	r.mustValid()
	return BPFFS{root: r}
}

// LockPath returns the global writer lock file path.
func (r Root) LockPath() string {
	r.mustValid()
	return filepath.Join(r.base, ".lock")
}

// DBPath returns the full path to the SQLite database file.
func (r Root) DBPath() string {
	r.mustValid()
	return filepath.Join(r.base, "db", "store.db")
}

// SocketPath returns the full path to the gRPC socket.
func (r Root) SocketPath() string {
	r.mustValid()
	return filepath.Join(r.base, "sock", "bpfman.sock")
}

// CSISocketPath returns the full path to the CSI socket.
func (r Root) CSISocketPath() string {
	r.mustValid()
	return filepath.Join(r.base, "csi", "csi.sock")
}

// BPFFSMountPoint returns the bpffs mount point path.
// Deprecated: use r.BPFFS().MountPoint() for consistency with the domain model.
func (r Root) BPFFSMountPoint() string {
	return r.BPFFS().MountPoint()
}

// DBDir returns the directory containing the database file.
func (r Root) DBDir() string {
	r.mustValid()
	return filepath.Join(r.base, "db")
}

// SocketDir returns the directory containing the gRPC socket.
func (r Root) SocketDir() string {
	r.mustValid()
	return filepath.Join(r.base, "sock")
}

// CSIDir returns the CSI directory path.
func (r Root) CSIDir() string {
	r.mustValid()
	return filepath.Join(r.base, "csi")
}

// CSIFSDir returns the CSI filesystem directory path.
func (r Root) CSIFSDir() string {
	r.mustValid()
	return filepath.Join(r.base, "csi", "fs")
}

// RuntimeDirs returns the directories required for basic runtime operation.
// This includes Base() itself plus its required subdirectories.
// Callers should create these directories at startup.
func (r Root) RuntimeDirs() []string {
	r.mustValid()
	return []string{r.base, r.DBDir(), r.SocketDir()}
}

// CSIDirs returns the directories required for CSI operation.
// Callers should create these directories only when CSI is enabled.
func (r Root) CSIDirs() []string {
	r.mustValid()
	return []string{r.CSIDir(), r.CSIFSDir()}
}
