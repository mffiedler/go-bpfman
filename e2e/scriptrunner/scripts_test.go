//go:build e2e

package scriptrunner

import (
	"bufio"
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
// discovery or PATH manipulation. Under sudo this means the
// invocation has to bypass secure_path itself (the make recipe
// uses `sudo env PATH=...`; a developer invoking the binary
// directly should arrange their PATH similarly).
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
// Per-script opt-out: a `.bpfman` script can declare itself
// non-parallel by putting `# NOPARALLEL` on a line within the
// first scriptHeaderLines lines of the file (the
// scriptOptsOutOfParallel helper pre-scans for it). Such a
// script's subtest runs sequentially in the parent's
// registration loop with no t.Parallel call, so it has the
// kernel surface to itself for the duration of its run; the
// other new/ subtests stay paused in the parallel queue
// until the parent body returns. The marker lives with the
// script, so the constraint is discoverable from the file
// that has it rather than from a central registry.
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
		// Pre-scan once per pass so a script's header is read
		// at most once even under stress repeats. The scan
		// fails loudly on any read error -- a script the
		// runner cannot open is not a script the harness can
		// honestly run, so propagate the open error rather
		// than silently treating the file as opt-in to
		// parallel.
		serial := make(map[string]bool, len(matches))
		if parallel {
			for _, abs := range matches {
				if scriptOptsOutOfParallel(t, abs) {
					serial[abs] = true
				}
			}
		}
		// Outer loop: repeat. Inner loop: scripts. Pass r=0 of
		// every script enters the dispatcher first, then r=1,
		// and so on; the t.Parallel queue under new/ therefore
		// holds [s1#0, s2#0, ..., sN#0, s1#1, ...] which
		// preserves wave diversity across repeats. Scripts
		// marked NOPARALLEL skip the repeat: they hold the
		// kernel surface to themselves for their run, so
		// extra registrations only burn wall-clock time
		// without tickling new race windows.
		for r := 0; r < repeats; r++ {
			for _, abs := range matches {
				if serial[abs] && r > 0 {
					continue
				}
				rel := filepath.Join(sub, filepath.Base(abs))
				// Suffix only when the script actually
				// participates in the repeat cycle. A
				// NOPARALLEL script runs exactly once even
				// under stress, so it keeps its unsuffixed
				// name; otherwise `-test.run 'Foo#3'` against
				// a NOPARALLEL script would silently match no
				// subtests.
				name := filepath.ToSlash(rel)
				if repeats > 1 && !serial[abs] {
					name = fmt.Sprintf("%s#%d", name, r)
				}
				runParallel := parallel && !serial[abs]
				t.Run(name, func(t *testing.T) {
					if runParallel {
						t.Parallel()
					}
					runBPFManScript(t, e2eDir, rel, timeout)
				})
			}
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

// scriptHeaderLines bounds the prescan window for the
// # NOPARALLEL marker so the helper does no more I/O than
// reading a small script header. Any marker has to appear in
// the file's first scriptHeaderLines lines; that keeps the
// convention explicit (the script header is where the
// constraint lives), avoids accidentally matching a comment
// or shell-quoted occurrence deep inside the body, and caps
// the per-script scan cost at a tiny bounded read.
const (
	scriptHeaderLines = 20
	scriptNoParallel  = "# NOPARALLEL"
)

// scriptOptsOutOfParallel reports whether the script at path
// declares itself non-parallel via a `# NOPARALLEL` line in
// its header. Match is a prefix on the trimmed line so
// trailing rationale is allowed (e.g. "# NOPARALLEL: shares
// pinned-map state"). Any read error is fatal -- a script
// the runner cannot even open is not a script the runner
// can run; better to surface the read error here than to
// silently default the missing-or-unreadable script into
// the parallel pool.
func scriptOptsOutOfParallel(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open script %s for NOPARALLEL prescan: %v", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for i := 0; i < scriptHeaderLines && scanner.Scan(); i++ {
		if strings.HasPrefix(strings.TrimSpace(scanner.Text()), scriptNoParallel) {
			return true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read script %s for NOPARALLEL prescan: %v", path, err)
	}
	return false
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
