//go:build e2e

package scriptrunner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
func TestBPFManScripts(t *testing.T) {
	timeout := scriptTimeout()
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
		for _, abs := range matches {
			// Subtest name is the script's path relative to
			// e2eDir (e.g. "new/Foo.bpfman"); keeps -run
			// filters portable.
			rel := filepath.Join(sub, filepath.Base(abs))
			t.Run(filepath.ToSlash(rel), func(t *testing.T) {
				if parallel {
					t.Parallel()
				}
				runBPFManScript(t, e2eDir, rel, timeout)
			})
		}
	}
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
const (
	bpfmanShellTimeoutEnv     = "BPFMAN_E2E_SCRIPT_TIMEOUT"
	bpfmanShellTimeoutDefault = 5 * time.Minute
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
	// invoked from.
	cmd.Dir = e2eDir
	if bin := os.Getenv("BIN_DIR"); bin != "" {
		cmd.Env = augmentPath(os.Environ(), bin)
	}
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

// augmentPath returns a copy of env with binDir prepended to
// the PATH entry. Used only when BIN_DIR is explicitly set, to
// work around sudo's secure_path stripping the caller's PATH.
func augmentPath(env []string, binDir string) []string {
	out := make([]string, len(env))
	copy(out, env)
	for i, kv := range out {
		if strings.HasPrefix(kv, "PATH=") {
			out[i] = "PATH=" + binDir + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return out
		}
	}
	return append(out, "PATH="+binDir)
}
