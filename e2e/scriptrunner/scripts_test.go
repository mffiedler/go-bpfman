//go:build e2e

package scriptrunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBPFManScripts discovers every .bpfman script under
// e2e/scripts/ and runs each one as a subtest by invoking
// bpfman-shell from PATH. The caller is responsible for
// putting bpfman-shell on PATH; the test does no binary
// discovery or PATH manipulation. Under sudo this means the
// invocation has to bypass secure_path itself (the make recipe
// uses `sudo env PATH=...`; a developer invoking the binary
// directly should arrange their PATH similarly).
//
// The package's TestMain already enforces root and takes the
// suite-wide flock, so each subtest just runs the script
// directly with no further elevation.
//
// Subtests call t.Parallel() by default: scripts use the
// address-pool-backed `net veth-pair` builtin (see
// cmd/bpfman-shell/internal/builtins/netpool.go) and are safe
// to run concurrently by construction.
//
// Per-script serial lane: a `.bpfman` script can declare
// itself serial by putting `# SERIAL` on a line within the
// first scriptHeaderLines lines of the file. Such a script
// still enters the Go parallel queue so registration can
// complete and the parallel-eligible scripts can start, but
// it takes a package-local serial mutex before executing. That
// means SERIAL scripts run one-at-a-time with other SERIAL
// scripts while parallel-eligible scripts continue to run
// beside them.
//
// Per-script exclusive lane: a `.bpfman` script can declare
// itself exclusive by putting `# EXCLUSIVE` in the same header
// window. Exclusive scripts do not call t.Parallel, so they run
// during registration while parallel subtests are still queued.
// Use this only for scripts that mutate global bpfman state,
// such as `bpfman program delete --all`.
//
// Subtest names are the script's path relative to e2e/, so
// `go test -run 'TestBPFManScripts/scripts/Foo\.bpfman$'`
// selects one script (escape the dot and anchor the end to
// avoid prefix matches against longer names). Under stress mode
// (BPFMAN_E2E_SCRIPT_REPEATS>1, see below) the registered name
// for a parallel-eligible script gains a `#<r>` suffix, so the
// $-anchored form silently matches no subtests. The portable
// shapes are:
//
//   - one script, one run:
//     go test -run 'TestBPFManScripts/scripts/Foo\.bpfman$'
//   - one script, all repetitions:
//     BPFMAN_E2E_SCRIPT_REPEATS=10 \
//     go test -run 'TestBPFManScripts/scripts/Foo\.bpfman(#\d+)?$'
//   - one script, either mode (drop the anchor; prefix-matches
//     longer-named scripts that share the prefix):
//     go test -run 'TestBPFManScripts/scripts/Foo\.bpfman'
//
// BPFMAN_E2E_SCRIPT_REPEATS turns the corpus into a stress
// run: each script is registered N times as
// `scripts/<name>.bpfman#<r>` (r in [0, N)), with the outer
// loop being repeat and the inner loop being the script list.
// Registering in that order means consecutive subtests in the
// t.Parallel queue are different scripts, which gives the
// address pool's `net veth-pair` builtin maximum name
// diversity per dispatched wave -- the same load shape the
// legacy e2e/parallel-scripts.sh -r N option produced. Unset
// or N=1 keeps the default one-pass behaviour and the
// unsuffixed subtest names.
func TestBPFManScripts(t *testing.T) {
	timeout := scriptTimeout()
	repeats := scriptRepeats()
	e2eDir := e2ePackageDir(t)

	const sub = "scripts"
	absSub := filepath.Join(e2eDir, sub)
	matches, err := filepath.Glob(filepath.Join(absSub, "*.bpfman"))
	if err != nil {
		t.Fatalf("glob %s: %v", absSub, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no .bpfman scripts found under %s/", absSub)
	}
	// Pre-scan once per pass so a script's header is read at
	// most once even under stress repeats. The scan fails
	// loudly on any read error -- a script the runner cannot
	// open is not a script the harness can honestly run, so
	// propagate the open error rather than silently treating
	// the file as opt-in to parallel.
	serial := make(map[string]bool, len(matches))
	exclusive := make(map[string]bool, len(matches))
	for _, abs := range matches {
		mode := scriptExecutionMode(t, abs)
		if mode.serial {
			serial[abs] = true
		}
		if mode.exclusive {
			exclusive[abs] = true
		}
	}
	// Outer loop: repeat. Inner loop: scripts. Pass r=0 of
	// every script enters the dispatcher first, then r=1,
	// and so on; the t.Parallel queue therefore holds
	// [s1#0, s2#0, ..., sN#0, s1#1, ...] which preserves
	// wave diversity across repeats. Scripts marked SERIAL
	// skip the repeat: they are serialized with other SERIAL
	// scripts by scriptSerialMu, so extra registrations only
	// burn wall-clock time without tickling new race windows.
	for r := 0; r < repeats; r++ {
		for _, abs := range matches {
			if (serial[abs] || exclusive[abs]) && r > 0 {
				continue
			}
			rel := filepath.Join(sub, filepath.Base(abs))
			// Suffix only when the script actually
			// participates in the repeat cycle. A
			// SERIAL script runs exactly once even under
			// stress, so it keeps its unsuffixed name;
			// otherwise `-test.run 'Foo#3'` against a
			// SERIAL script would silently match no
			// subtests.
			name := filepath.ToSlash(rel)
			if repeats > 1 && !serial[abs] && !exclusive[abs] {
				name = fmt.Sprintf("%s#%d", name, r)
			}
			runSerial := serial[abs]
			runExclusive := exclusive[abs]
			t.Run(name, func(t *testing.T) {
				if !runExclusive {
					t.Parallel()
				}
				if runSerial {
					scriptSerialMu.Lock()
					defer scriptSerialMu.Unlock()
				}
				if err := emitScriptTimelineMarker("script_start", t.Name()); err != nil {
					t.Fatalf("write script timeline start marker: %v", err)
				}
				defer func() {
					if err := emitScriptTimelineMarker("script_end", t.Name()); err != nil {
						t.Errorf("write script timeline end marker: %v", err)
					}
				}()
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
//
// bpfmanShellRepeatsEnv is the stress knob: each script is
// registered N times so the t.Parallel queue holds a wave-
// diverse mix the dispatcher can fan out at the configured
// -test.parallel concurrency. Unset or N<=1 keeps the default
// one-pass behaviour.
const (
	bpfmanShellTimeoutEnv     = "BPFMAN_E2E_SCRIPT_TIMEOUT"
	bpfmanShellTimeoutDefault = 5 * time.Minute
	bpfmanShellRepeatsEnv     = "BPFMAN_E2E_SCRIPT_REPEATS"
	bpfmanShellRepeatsDefault = 1
	bpfmanShellTimelineEnv    = "BPFMAN_E2E_SCRIPT_TIMELINE"
	bpfmanShellTestPackage    = "github.com/frobware/go-bpfman/e2e/scriptrunner"
)

var scriptTimelineMu sync.Mutex
var scriptSerialMu sync.Mutex

type scriptTimelineMarker struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
}

func emitScriptTimelineMarker(action, testName string) error {
	path := os.Getenv(bpfmanShellTimelineEnv)
	if path == "" {
		return nil
	}
	scriptTimelineMu.Lock()
	defer scriptTimelineMu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if err := json.NewEncoder(f).Encode(scriptTimelineMarker{
		Time:    time.Now(),
		Action:  action,
		Package: bpfmanShellTestPackage,
		Test:    testName,
	}); err != nil {
		return err
	}
	return nil
}

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
// # SERIAL marker so the helper does no more I/O than
// reading a small script header. Any marker has to appear in
// the file's first scriptHeaderLines lines; that keeps the
// convention explicit (the script header is where the
// constraint lives), avoids accidentally matching a comment
// or shell-quoted occurrence deep inside the body, and caps
// the per-script scan cost at a tiny bounded read.
const (
	scriptHeaderLines = 20
	scriptSerial      = "# SERIAL"
	scriptExclusive   = "# EXCLUSIVE"
)

type scriptMode struct {
	serial    bool
	exclusive bool
}

// scriptExecutionMode reports whether the script at path declares a
// special execution mode in its header. Matches are prefixes on the
// trimmed line so trailing rationale is allowed (e.g. "# SERIAL:
// shares pinned-map state"). Any read error is fatal -- a script the
// runner cannot even open is not a script the runner can run; better
// to surface the read error here than to silently default the
// missing-or-unreadable script into the parallel pool.
func scriptExecutionMode(t *testing.T, path string) scriptMode {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open script %s for mode-marker prescan: %v", path, err)
	}
	defer f.Close()
	var mode scriptMode
	scanner := bufio.NewScanner(f)
	for i := 0; i < scriptHeaderLines && scanner.Scan(); i++ {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, scriptSerial) {
			mode.serial = true
		}
		if strings.HasPrefix(line, scriptExclusive) {
			mode.exclusive = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read script %s for serial-marker prescan: %v", path, err)
	}
	return mode
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
