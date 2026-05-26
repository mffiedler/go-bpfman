//go:build e2e

package scriptrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBPFManScripts discovers every .bpfman script under
// e2e/scripts/ and e2e/new/ and runs each one as a subtest by
// invoking bpfman-shell from PATH. The caller is responsible
// for putting bpfman-shell on PATH; the test does no binary
// discovery or validation of its own.
//
// BIN_DIR is the convenience escape hatch for sudo, which
// strips the caller's PATH via secure_path. When set, BIN_DIR
// is prepended to the child's PATH; no other check happens.
// The caller can equivalently use `sudo -E` or
// `sudo --preserve-env=PATH` instead.
//
// The package's TestMain already enforces root and takes the
// suite-wide flock, so each subtest just runs the script
// directly with no further elevation.
//
// Subtests under e2e/new/ call t.Parallel(): those scripts
// use the address-pool-backed `net veth-pair` builtin (see
// cmd/bpfman-shell/shell/netpool.go) and are safe to run
// concurrently by construction. Subtests under e2e/scripts/
// stay serial because the legacy corpus hardcodes
// 198.51.100.1/24 via raw `ip addr add` and would collide.
//
// Subtest names are the script's path relative to e2e/, so
// `go test -run 'TestBPFManScripts/new/Foo\.bpfman$'` selects
// one script (escape the dot and anchor the end to avoid
// prefix matches against longer names).
//
// BPFMAN_E2E_SCRIPT_REPEATS turns the corpus into a stress
// run: each script is registered N times as
// `<corpus>/<name>.bpfman#<r>` (r in [0, N)), with the outer
// loop being repeat and the inner loop being the script list.
// Registering in that order means consecutive subtests in the
// t.Parallel queue are different scripts, which gives the
// address pool's `net veth-pair` builtin maximum name diversity
// per dispatched wave -- the same load shape the legacy
// e2e/parallel-scripts.sh -r N option produced. Unset or N=1
// keeps the default one-pass behaviour and the unsuffixed
// subtest names.
func TestBPFManScripts(t *testing.T) {
	timeout := scriptTimeout()
	repeats := scriptRepeats()
	e2eDir := e2ePackageDir(t)

	for _, sub := range []string{"scripts", "new"} {
		absSub := filepath.Join(e2eDir, sub)
		matches, err := filepath.Glob(filepath.Join(absSub, "*.bpfman"))
		if err != nil {
			t.Fatalf("glob %s: %v", absSub, err)
		}
		if len(matches) == 0 {
			t.Fatalf("no .bpfman scripts found under %s/", absSub)
		}
		parallel := sub == "new"
		// Outer loop: repeat. Inner loop: scripts. Pass r=0 of
		// every script enters the dispatcher first, then r=1,
		// and so on; the t.Parallel queue under new/ therefore
		// holds [s1#0, s2#0, ..., sN#0, s1#1, ...] which
		// preserves wave diversity across repeats.
		for r := 0; r < repeats; r++ {
			for _, abs := range matches {
				rel := filepath.Join(sub, filepath.Base(abs))
				t.Run(scriptSubtestName(rel, r, repeats), func(t *testing.T) {
					if parallel {
						t.Parallel()
					}
					runBPFManScript(t, e2eDir, rel, timeout)
				})
			}
		}
	}
}

// scriptSubtestName produces the subtest path for one
// registration. When repeats <= 1 the bare relative path is
// used (so the default one-pass shape keeps clean names like
// "new/Foo.bpfman"); when stressing the suffix "#<r>" makes
// each registration unique under Go's t.Run dedup rules.
func scriptSubtestName(rel string, repeat, repeats int) string {
	name := filepath.ToSlash(rel)
	if repeats <= 1 {
		return name
	}
	return fmt.Sprintf("%s#%d", name, repeat)
}

// e2ePackageDir returns the absolute path of the e2e directory
// where the .bpfman corpora and testdata live. Resolution order:
//
//  1. BPFMAN_E2E_DIR if set. This is the authoritative form and
//     is what the Makefile recipe passes (set to $(abspath e2e)),
//     so CI builds that ship the test binary in one filesystem
//     and run it in another always get the right path.
//
//  2. The runtime.Caller-derived path otherwise. Convenient for
//     direct local invocations of bin/e2e-scripts.test from any
//     cwd, but only works when the binary still has access to
//     its build-time source tree -- which is true for local
//     `go test` / `go build` runs and false for static binaries
//     built inside a container and extracted to a different host.
func e2ePackageDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("BPFMAN_E2E_DIR"); d != "" {
		return d
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed and BPFMAN_E2E_DIR is unset; cannot anchor e2e dir")
	}
	// file is .../e2e/scriptrunner/scripts_test.go; walk up
	// two levels to reach the e2e/ directory where the script
	// corpora and testdata live.
	dir := filepath.Dir(filepath.Dir(file))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("runtime.Caller-derived e2e dir %s is not accessible (set BPFMAN_E2E_DIR for cross-filesystem builds): %v", dir, err)
	}
	return dir
}

// bpfmanShellTimeoutEnv overrides the per-script deadline.
// Without it each script gets bpfmanShellTimeoutDefault to
// complete; a deadline-exceeded run fails that one subtest
// cleanly rather than hanging the whole binary against
// go test's outer -timeout.
//
// bpfmanShellRepeatsEnv is the stress knob: each script is
// registered N times so the t.Parallel queue under new/ holds
// a wave-diverse mix the dispatcher can fan out at the
// configured -test.parallel concurrency. Unset or N<=1 keeps
// the default one-pass behaviour.
const (
	bpfmanShellTimeoutEnv     = "BPFMAN_E2E_SCRIPT_TIMEOUT"
	bpfmanShellTimeoutDefault = 5 * time.Minute
	bpfmanShellRepeatsEnv     = "BPFMAN_E2E_SCRIPT_REPEATS"
	bpfmanShellRepeatsDefault = 1
)

func scriptTimeout() time.Duration {
	raw := os.Getenv(bpfmanShellTimeoutEnv)
	if raw == "" {
		return bpfmanShellTimeoutDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return bpfmanShellTimeoutDefault
	}
	return d
}

func scriptRepeats() int {
	raw := os.Getenv(bpfmanShellRepeatsEnv)
	if raw == "" {
		return bpfmanShellRepeatsDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return bpfmanShellRepeatsDefault
	}
	return n
}

// runBPFManScript executes one .bpfman script under a per-script
// deadline. Output goes to a single combined buffer (matching
// the Bash runners' user-visible shape) and is dumped via
// t.Fatalf on failure or via t.Logf on success; either way the
// Go test framework owns the framing so go test -json picks it
// up cleanly.
func runBPFManScript(t *testing.T, e2eDir, script string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bpfman-shell", script)
	// Script paths inside the corpus reference testdata
	// relative to e2e/, so the child has to run with cwd at
	// the package dir regardless of where the test binary was
	// invoked from. PATH is already set up at TestMain (BIN_DIR
	// prepended once, before any exec.Command).
	cmd.Dir = e2eDir
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("%s timed out after %s\n\n%s", script, timeout, out)
	}
	if err != nil {
		t.Fatalf("%s failed: %v\n\n%s", script, err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Logf("%s", s)
	}
}

