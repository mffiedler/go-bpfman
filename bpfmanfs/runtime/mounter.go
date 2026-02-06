package runtime

import (
	"os"

	"github.com/frobware/go-bpfman/bpffs"
)

// Mounter handles bpffs mounting during initialisation.
type Mounter interface {
	EnsureMounted(mountPoint string) error
}

// RealMounter performs actual bpffs mounting using syscalls.
type RealMounter struct{}

func (RealMounter) EnsureMounted(mountPoint string) error {
	return bpffs.EnsureMounted(bpffs.DefaultMountInfoPath, mountPoint)
}

// NoOpMounter creates the mount point directory without mounting.
type NoOpMounter struct{}

func (NoOpMounter) EnsureMounted(mountPoint string) error {
	return os.MkdirAll(mountPoint, 0o755)
}
