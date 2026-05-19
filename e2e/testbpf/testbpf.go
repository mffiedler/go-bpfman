//go:build e2e

// Package testbpf provides Materialise, a helper that writes an
// embedded BPF object tree out to disk. The shared embed.FS
// itself lives in the e2e package (e2e.BpfFS): keeping the
// embed declaration in the e2e package directory means the
// pattern can reach the canonical testdata/bpf tree directly
// without each consumer carrying its own copy.
//
// Test binaries ship the .bpf.o files via go:embed so they are
// hermetic across the build/run boundary (e.g. binaries built
// inside a Fedora container run on an Ubuntu CI runner with no
// source tree present), but the daemon they drive lives in a
// separate process and has to open the bytecode through the
// kernel's filesystem syscalls. Materialise writes the embedded
// bytes out to a tmp directory so the daemon can read them.
package testbpf

import (
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
)

// Materialise writes every file in fsys to disk under root,
// preserving the source FS layout. Intermediate directories
// are created with 0o755; regular files are written with 0o600
// since BPF object bytes are not executable.
//
// Callers pass the embed.FS they want materialised. The
// canonical FS for the e2e suite is e2e.BpfFS; consumers (e2e
// itself and e2e/grpc) pass that one through here.
func Materialise(fsys iofs.FS, root string) error {
	return iofs.WalkDir(fsys, ".", func(name string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(root, name)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := iofs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		return nil
	})
}
