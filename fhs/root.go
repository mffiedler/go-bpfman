package fhs

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
// Callers with appropriate context (e.g., manager.New with an injected
// BPFFSMounter) are responsible for creating directories and mount points.
// This separation enables testing without root privileges or real filesystems.
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
		return Root{}, fmt.Errorf("fhs: base path cannot be empty")
	}
	if !filepath.IsAbs(base) {
		return Root{}, fmt.Errorf("fhs: base path must be absolute, got %q", base)
	}
	base = filepath.Clean(base)
	return Root{base: filepath.Join(base, "bpfman")}, nil
}

// valid reports whether r was constructed via New.
func (r Root) valid() bool {
	return r.base != ""
}

// Valid reports whether r was constructed via New.
func (r Root) Valid() bool {
	return r.valid()
}

// Base returns the runtime root path (e.g., /run/bpfman).
func (r Root) Base() string { return r.base }

// Runtime returns the regular-filesystem domain rooted at base.
func (r Root) Runtime() Runtime {
	return Runtime{root: r}
}

// BPFFS returns the bpffs layout domain rooted at base.
func (r Root) BPFFS() BPFFS {
	return BPFFS{root: r}
}

// LockPath returns the global writer lock file path.
func (r Root) LockPath() string {
	return filepath.Join(r.base, ".lock")
}

// DBPath returns the full path to the SQLite database file.
func (r Root) DBPath() string {
	return filepath.Join(r.base, "db", "store.db")
}

// SocketPath returns the full path to the gRPC socket.
func (r Root) SocketPath() string {
	return filepath.Join(r.base+"-sock", "bpfman.sock")
}

// CSISocketPath returns the full path to the CSI socket.
func (r Root) CSISocketPath() string {
	return filepath.Join(r.base, "csi", "csi.sock")
}

// BPFFSMountPoint returns the bpffs mount point path.
func (r Root) BPFFSMountPoint() string {
	return filepath.Join(r.base, "fs")
}

// DBDir returns the directory containing the database file.
func (r Root) DBDir() string {
	return filepath.Join(r.base, "db")
}

// SocketDir returns the directory containing the gRPC socket.
func (r Root) SocketDir() string {
	return r.base + "-sock"
}

// CSIDir returns the CSI directory path.
func (r Root) CSIDir() string {
	return filepath.Join(r.base, "csi")
}

// CSIFSDir returns the CSI filesystem directory path.
func (r Root) CSIFSDir() string {
	return filepath.Join(r.base, "csi", "fs")
}

// RuntimeDirs returns the directories required for basic runtime operation.
// Callers should create these directories at startup.
func (r Root) RuntimeDirs() []string {
	return []string{
		r.Base(),
		r.DBDir(),
		r.SocketDir(),
	}
}

// CSIDirs returns the directories required for CSI operation.
// Callers should create these directories only when CSI is enabled.
func (r Root) CSIDirs() []string {
	return []string{
		r.CSIDir(),
		r.CSIFSDir(),
	}
}
