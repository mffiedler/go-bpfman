//go:build e2e

package scriptrunner

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

// prependBINDIRToPath puts $BIN_DIR at the front of the test
// process's own PATH. exec.Command's name resolution runs
// LookPath against os.Environ() at construction time, not
// against the cmd.Env we hand the child, so an
// already-built bpfman-shell sitting in BIN_DIR has to be
// reachable from the parent's environment for `exec.Command(
// "bpfman-shell", ...)` to find it. sudo strips PATH via
// secure_path; this restores enough of it that the test
// process can resolve the binary. Children inherit the
// augmented PATH via os.Environ() once exec.Command runs, so
// no per-cmd plumbing is needed downstream.
func prependBINDIRToPath() {
	bin := os.Getenv("BIN_DIR")
	if bin == "" {
		return
	}
	path := os.Getenv("PATH")
	if path == "" {
		_ = os.Setenv("PATH", bin)
		return
	}
	_ = os.Setenv("PATH", bin+string(os.PathListSeparator)+path)
}

// e2eSuiteLockPath is the same system-wide flock the workload
// e2e binary holds. Sharing the path means the two test
// binaries are mutually exclusive on a single host: a
// developer can run `make test-e2e` and `make test-e2e-scripts`
// back-to-back without contention, but if they fire both in
// parallel the second one fails fast with a clear message
// instead of racing into shared kernel state. CI runs the two
// binaries on separate runners so the flock never contends
// there.
//
// Kept byte-identical to the workload binary's constant so a
// future shared-helper extraction is a mechanical move.
const e2eSuiteLockPath = "/tmp/bpfman-e2e.lock"

// suiteLock is package-scoped so the open file (and thus the
// flock) lives for the full test process lifetime; closing
// the fd drops the lock. The OS releases everything on
// process exit.
var suiteLock *os.File

func acquireSuiteLock() {
	f, err := os.OpenFile(e2eSuiteLockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scriptrunner: open suite lock %s: %v\n", e2eSuiteLockPath, err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr,
			"scriptrunner: another bpfman e2e test binary holds %s -- refusing to start.\n"+
				"    pid likely visible via: lsof %s   or   fuser %s\n"+
				"    if no such process exists, remove the lock file and retry.\n",
			e2eSuiteLockPath, e2eSuiteLockPath, e2eSuiteLockPath)
		f.Close()
		os.Exit(1)
	}
	suiteLock = f
}

// TestMain is the script runner's package-level setup. Smaller
// than the workload binary's TestMain because the scripts side
// has no helper-mode re-exec, no shared-runtime initialisation,
// no stale-dir cleanup (the address-pool builtin and short
// bpfman-shell lifetimes leave nothing of the kind the workload
// suite accumulates), and no self-exec discovery. Three load-
// bearing steps remain: refuse to run without root, take the
// suite-wide flock, and prepend BIN_DIR to PATH so exec.Command
// can resolve bpfman-shell under sudo's secure_path.
func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "scriptrunner: tests require root privileges")
		os.Exit(1)
	}
	acquireSuiteLock()
	prependBINDIRToPath()
	os.Exit(m.Run())
}
