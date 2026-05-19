//go:build e2e

// Package testbpf provides helpers for working with embedded
// BPF objects in tests. Test binaries ship their .bpf.o files
// via go:embed so they are hermetic across the build/run
// boundary (e.g. binaries built inside a Fedora container run
// on an Ubuntu CI runner with no source tree present), but the
// daemon they drive lives in a separate process and has to open
// the bytecode through the kernel's filesystem syscalls. The
// helpers here materialise the embedded bytes out to a tmp
// directory so the daemon can read them.
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
// Callers pass their own embed.FS so the test binary only pays
// for the .bpf.o files it actually uses (Go's embed forbids
// ".." in patterns, so each package's go:embed pattern picks
// up only its own testdata/bpf subset).
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
