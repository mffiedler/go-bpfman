//go:build nsenter

package nsenter_test

import (
	"testing"

	"github.com/frobware/go-bpfman/ns/nsenter"
)

// TestConstructorWithSelfNamespace is the strongest proof that the
// constructor runs and the full setns path executes.
//
// The subprocess is launched with _BPFMAN_MNT_NS pointing at its
// own mount namespace (/proc/self/ns/mnt). This is a no-op
// namespace switch (same namespace) but it exercises the real code
// path: open the namespace file, call setns, clear the environment
// variable. The test asserts that the variable was cleared. If the
// constructor did not run the variable would still be present.
//
// Requires CAP_SYS_ADMIN; fails if not root. Skipped under QEMU
// user-mode emulation where setns is not supported.
//
// Build tag: nsenter. Run via "make test-nsenter" which adds
// -tags=nsenter and sudo.
func TestConstructorWithSelfNamespace(t *testing.T) {
	result := runHelper(t, []string{
		nsenter.MntNsEnvVar + "=/proc/self/ns/mnt",
	})
	if result.mntNsEnv != "cleared" {
		t.Fatalf("%s was not cleared by the constructor: env is %q",
			nsenter.MntNsEnvVar, result.mntNsEnv)
	}
	t.Logf("subprocess inode: %d (constructor cleared %s)",
		result.inode, nsenter.MntNsEnvVar)
}
