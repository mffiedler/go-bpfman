package manager

import (
	"os"

	"github.com/frobware/go-bpfman/bpffs"
)

// BPFFSMounter handles bpffs mounting during manager initialisation.
// This interface allows tests to skip actual bpffs mounting.
type BPFFSMounter interface {
	// EnsureMounted ensures a bpffs is mounted at the given mount point.
	// For production, this performs an actual mount syscall.
	// For tests, this typically just creates the directory.
	EnsureMounted(mountPoint string) error
}

// RealMounter performs actual bpffs mounting using syscalls.
// Use this for production code.
type RealMounter struct{}

// EnsureMounted ensures a bpffs is mounted at mountPoint.
func (RealMounter) EnsureMounted(mountPoint string) error {
	return bpffs.EnsureMounted(bpffs.DefaultMountInfoPath, mountPoint)
}

// NoOpMounter creates the mount point directory without mounting.
// Use this for tests that don't need actual bpffs.
type NoOpMounter struct{}

// EnsureMounted creates the mount point directory without mounting bpffs.
func (NoOpMounter) EnsureMounted(mountPoint string) error {
	return os.MkdirAll(mountPoint, 0755)
}
