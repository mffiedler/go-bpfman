package fs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/frobware/go-bpfman/bpffs"
)

// Root is an immutable, validated filesystem root. Fields are
// unexported; external packages cannot construct a non-zero Root
// without calling Open.
//
// Root acts as a capability token following the same pattern as
// lock.WriterScope: possession of a valid Root proves the base path
// has been validated.
type Root struct {
	base string
}

// Open creates a Root rooted at the given base path.
//
// Open rejects empty paths, relative paths, and "/" (the filesystem
// root is never a valid bpfman base).
func Open(base string) (Root, error) {
	if base == "" {
		return Root{}, fmt.Errorf("fs: base path cannot be empty")
	}
	if !filepath.IsAbs(base) {
		return Root{}, fmt.Errorf("fs: base path must be absolute, got %q", base)
	}
	if base == "/" {
		return Root{}, fmt.Errorf("fs: base path cannot be filesystem root")
	}
	return Root{base: base}, nil
}

// valid reports whether r was constructed via Open.
func (r Root) valid() bool {
	return r.base != ""
}

// Valid reports whether r was constructed via Open.
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

// EnsureDirectories creates core runtime directories and ensures
// bpffs is mounted. Call this at startup to fail fast on permission
// or configuration issues.
//
// Creates these directories (on regular filesystem):
//   - {base}/
//   - {base}/db/
//   - {base}-sock/
//
// Mounts bpffs at {base}/fs/ if not already mounted.
func (r Root) EnsureDirectories() error {
	if !r.valid() {
		return ErrInvalidRoot
	}
	for _, dir := range []string{
		r.base,
		filepath.Join(r.base, "db"),
		r.base + "-sock",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return r.EnsureBPFFSMounted(bpffs.DefaultMountInfoPath)
}

// EnsureBPFFSMounted ensures bpffs is mounted at the mount point.
func (r Root) EnsureBPFFSMounted(mountInfoPath string) error {
	if !r.valid() {
		return ErrInvalidRoot
	}
	mp := r.BPFFSMountPoint()
	if err := bpffs.EnsureMounted(mountInfoPath, mp); err != nil {
		return fmt.Errorf("failed to ensure bpffs at %s: %w", mp, err)
	}
	return nil
}

// EnsureCSIDirectories creates CSI-specific directories.
// Call this only when CSI functionality is enabled.
func (r Root) EnsureCSIDirectories() error {
	if !r.valid() {
		return ErrInvalidRoot
	}
	csi := filepath.Join(r.base, "csi")
	csiFS := filepath.Join(csi, "fs")
	for _, dir := range []string{csi, csiFS} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return nil
}
