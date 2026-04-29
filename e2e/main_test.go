//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
)

// e2eModeEnv selects an alternative process role. When set, the
// binary skips the Go test framework entirely and runs the named
// helper before exiting, so the same e2e.test binary serves as
// both the test driver and the uprobe attach target.
//
// Mode values are <verb>-<specifier>: the verb names the high-
// level behaviour (e.g. uprobe-trigger), the specifier names
// which sibling helper to run (e.g. call-malloc). Picking a name
// per helper avoids retrofitting a "_2" or "_target" suffix once
// a second uprobe-firing path is needed.
const (
	e2eModeEnv                     = "BPFMAN_E2E_MODE"
	e2eModeUprobeTriggerCallMalloc = "uprobe-trigger-call-malloc"
)

// selfExe is the absolute path of the running e2e.test binary,
// resolved once at TestMain time. Used by the uprobe tests both
// as the kernel's attach target (kernel resolves to inode +
// symbol-offset) and as the binary they re-exec via os/exec to
// fire the probe.
var selfExe string

func TestMain(m *testing.M) {
	// Helper-mode dispatch must run before the root check and
	// stale-dir cleanup: the parent test process invokes us via
	// exec.Command(os.Executable()) inheriting BPFMAN_E2E_MODE,
	// and the helper has nothing to clean up.
	switch os.Getenv(e2eModeEnv) {
	case e2eModeUprobeTriggerCallMalloc:
		invokeUprobeCallMalloc()
		os.Exit(0)
	case "":
		// normal test driver mode
	default:
		fmt.Fprintf(os.Stderr, "unknown %s=%q\n", e2eModeEnv, os.Getenv(e2eModeEnv))
		os.Exit(2)
	}

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "e2e tests require root privileges")
		os.Exit(1)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "os.Executable: %v\n", err)
		os.Exit(1)
	}
	selfExe = exe

	if err := cleanupStaleTestDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to clean stale test dirs: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}
