//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/image/oci"
	"github.com/frobware/go-bpfman/interpreter/image/verify"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/logging"
	"github.com/frobware/go-bpfman/manager"
)

// TestEnv provides an isolated test environment for e2e tests.
// Each test gets a fully isolated environment with unique directories,
// database, and socket, enabling t.Parallel() across all tests.
type TestEnv struct {
	T        *testing.T
	Root     fs.Root
	Manager  *manager.Manager
	Puller   interpreter.ImagePuller
	logger   *slog.Logger
	closeEnv func() error
}

// NewTestEnv creates an isolated test environment for e2e testing.
// The environment includes:
//   - A unique runtime directory in /tmp/bpfman-e2e-<pid>-<testname>/
//   - A fresh SQLite database
//   - A bpffs mount
//   - A manager instance for BPF operations
//
// The environment is automatically cleaned up via t.Cleanup().
func NewTestEnv(t *testing.T) *TestEnv {
	t.Helper()

	// Create unique directory for this test
	testName := sanitizeTestName(t.Name())
	baseDir := filepath.Join(os.TempDir(), fmt.Sprintf("bpfman-e2e-%d-%s", os.Getpid(), testName))

	root, err := fs.Open(baseDir)
	if err != nil {
		t.Fatalf("invalid runtime directory: %v", err)
	}

	// Set up logger based on BPFMAN_LOG environment variable.
	// Examples:
	//   BPFMAN_LOG=debug           - all components at debug
	//   BPFMAN_LOG=info,store=debug - default info, store (SQL) at debug
	var logger *slog.Logger
	if envSpec := os.Getenv("BPFMAN_LOG"); envSpec != "" {
		var err error
		logger, err = logging.New(logging.Options{
			EnvSpec: envSpec,
			Format:  logging.FormatText,
			Output:  os.Stderr,
		})
		if err != nil {
			t.Fatalf("invalid BPFMAN_LOG spec: %v", err)
		}
	} else {
		// Default: only errors
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelError,
		}))
	}

	// Set up runtime environment (ensures directories, opens store, creates manager)
	ctx := context.Background()
	mgr, cleanup, err := manager.SetupRuntimeEnv(ctx, root, logger)
	require.NoError(t, err, "failed to setup runtime environment")

	// Create signature verifier (disabled for tests)
	verifier := verify.NoSign()

	// Create image puller for OCI images
	puller, err := oci.NewPuller(
		oci.WithLogger(logger),
		oci.WithVerifier(verifier),
	)
	require.NoError(t, err, "failed to create image puller")

	env := &TestEnv{
		T:        t,
		Root:     root,
		Manager:  mgr,
		Puller:   puller,
		logger:   logger,
		closeEnv: cleanup,
	}

	// Register cleanup
	t.Cleanup(func() {
		env.cleanup()
	})

	return env
}

// cleanup releases resources and removes test directories.
func (e *TestEnv) cleanup() {
	if e.closeEnv != nil {
		e.closeEnv()
	}

	// Unmount bpffs if mounted
	bpffsMount := e.Root.BPFFSMountPoint()
	if isMounted(bpffsMount) {
		if err := unmount(bpffsMount); err != nil {
			e.T.Logf("warning: failed to unmount bpffs at %s: %v", bpffsMount, err)
		}
	}

	// Remove runtime directories
	if err := os.RemoveAll(e.Root.Base()); err != nil {
		e.T.Logf("warning: failed to remove %s: %v", e.Root.Base(), err)
	}
	sockDir := e.Root.Base() + "-sock"
	if err := os.RemoveAll(sockDir); err != nil {
		e.T.Logf("warning: failed to remove %s: %v", sockDir, err)
	}
}

// runWithLock executes a function under the writer lock.
func (e *TestEnv) runWithLock(ctx context.Context, fn func(context.Context) error) error {
	return lock.Run(ctx, e.Root.LockPath(), func(ctx context.Context, _ lock.WriterScope) error {
		return fn(ctx)
	})
}

// runWithLockAndScope executes a function under the writer lock with scope access.
func (e *TestEnv) runWithLockAndScope(ctx context.Context, fn func(context.Context, lock.WriterScope) error) error {
	return lock.Run(ctx, e.Root.LockPath(), fn)
}

// LoadImage loads BPF programs from an OCI image.
func (e *TestEnv) LoadImage(ctx context.Context, ref interpreter.ImageRef, programs []manager.ImageProgramSpec, opts manager.LoadImageOpts) ([]bpfman.Program, error) {
	var result []bpfman.Program
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		var loadErr error
		result, loadErr = e.Manager.LoadImage(ctx, e.Puller, ref, programs, opts)
		return loadErr
	})
	return result, err
}

// Load loads a BPF program from a file.
func (e *TestEnv) Load(ctx context.Context, spec bpfman.LoadSpec, opts manager.LoadOpts) (bpfman.Program, error) {
	var result bpfman.Program
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		prog, loadErr := e.Manager.Load(ctx, spec, opts)
		result = prog
		return loadErr
	})
	return result, err
}

// Unload unloads a BPF program.
func (e *TestEnv) Unload(ctx context.Context, kernelID uint32) error {
	return e.runWithLock(ctx, func(ctx context.Context) error {
		return e.Manager.Unload(ctx, kernelID)
	})
}

// List returns all managed programs.
func (e *TestEnv) List(ctx context.Context) ([]bpfman.Program, error) {
	result, err := e.Manager.ListPrograms(ctx)
	if err != nil {
		return nil, err
	}
	return result.Programs, nil
}

// Get returns detailed information about a program.
func (e *TestEnv) Get(ctx context.Context, kernelID uint32) (bpfman.Program, error) {
	return e.Manager.Get(ctx, kernelID)
}

// AttachTracepoint attaches a tracepoint program.
func (e *TestEnv) AttachTracepoint(ctx context.Context, spec bpfman.TracepointAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachTracepoint(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	// Fetch full link record for consistent return value
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindTracepoint}, nil
	}
	return record, nil
}

// AttachXDP attaches an XDP program.
func (e *TestEnv) AttachXDP(ctx context.Context, spec bpfman.XDPAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachXDP(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindXDP}, nil
	}
	return record, nil
}

// AttachTC attaches a TC program.
func (e *TestEnv) AttachTC(ctx context.Context, spec bpfman.TCAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachTC(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindTC}, nil
	}
	return record, nil
}

// AttachTCX attaches a TCX program.
func (e *TestEnv) AttachTCX(ctx context.Context, spec bpfman.TCXAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachTCX(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindTCX}, nil
	}
	return record, nil
}

// AttachKprobe attaches a kprobe/kretprobe program.
func (e *TestEnv) AttachKprobe(ctx context.Context, spec bpfman.KprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachKprobe(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindKprobe}, nil
	}
	return record, nil
}

// AttachUprobe attaches a uprobe/uretprobe program.
func (e *TestEnv) AttachUprobe(ctx context.Context, spec bpfman.UprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLockAndScope(ctx, func(ctx context.Context, scope lock.WriterScope) error {
		link, attachErr := e.Manager.AttachUprobe(ctx, scope, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindUprobe}, nil
	}
	return record, nil
}

// AttachFentry attaches a fentry program.
func (e *TestEnv) AttachFentry(ctx context.Context, spec bpfman.FentryAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachFentry(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindFentry}, nil
	}
	return record, nil
}

// AttachFexit attaches a fexit program.
func (e *TestEnv) AttachFexit(ctx context.Context, spec bpfman.FexitAttachSpec, opts bpfman.AttachOpts) (bpfman.LinkSpec, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		link, attachErr := e.Manager.AttachFexit(ctx, spec, opts)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkSpec{}, err
	}
	record, err := e.Manager.GetLink(ctx, result.Spec.ID)
	if err != nil {
		return bpfman.LinkSpec{ID: result.Spec.ID, Kind: bpfman.LinkKindFexit}, nil
	}
	return record, nil
}

// Detach detaches a link.
func (e *TestEnv) Detach(ctx context.Context, kernelLinkID uint32) error {
	return e.runWithLock(ctx, func(ctx context.Context) error {
		return e.Manager.Detach(ctx, bpfman.LinkID(kernelLinkID))
	})
}

// ListLinks returns all managed links.
func (e *TestEnv) ListLinks(ctx context.Context) ([]bpfman.LinkSpec, error) {
	return e.Manager.ListLinks(ctx)
}

// GetLink returns detailed information about a link.
func (e *TestEnv) GetLink(ctx context.Context, kernelLinkID uint32) (bpfman.LinkSpec, bpfman.LinkDetails, error) {
	record, err := e.Manager.GetLink(ctx, bpfman.LinkID(kernelLinkID))
	if err != nil {
		return bpfman.LinkSpec{}, nil, err
	}
	return record, record.Details, nil
}

// AssertCleanState verifies that no programs or links are managed.
func (e *TestEnv) AssertCleanState() {
	e.T.Helper()
	e.AssertProgramCount(0)
	e.AssertLinkCount(0)
}

// AssertProgramCount verifies the number of managed programs.
func (e *TestEnv) AssertProgramCount(expected int) {
	e.T.Helper()
	ctx := context.Background()

	programs, err := e.List(ctx)
	require.NoError(e.T, err, "failed to list programs")
	require.Len(e.T, programs, expected, "unexpected program count")
}

// AssertLinkCount verifies the total number of managed links.
func (e *TestEnv) AssertLinkCount(expected int) {
	e.T.Helper()
	ctx := context.Background()

	links, err := e.ListLinks(ctx)
	require.NoError(e.T, err, "failed to list links")
	require.Len(e.T, links, expected, "unexpected link count")
}

// AssertLinkCountByKind verifies the number of links of a specific kind.
func (e *TestEnv) AssertLinkCountByKind(linkKind bpfman.LinkKind, expected int) {
	e.T.Helper()
	ctx := context.Background()

	links, err := e.ListLinks(ctx)
	require.NoError(e.T, err, "failed to list links")

	count := 0
	for _, link := range links {
		if link.Kind == linkKind {
			count++
		}
	}
	require.Equal(e.T, expected, count, "unexpected link count for kind %s", linkKind)
}

// RequireRoot fails the test if not running as root.
func RequireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("test requires root privileges")
	}
}

// RequireBTF fails the test if kernel BTF is not available.
// BTF is required for fentry/fexit program types.
func RequireBTF(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); os.IsNotExist(err) {
		t.Fatal("test requires kernel BTF support (/sys/kernel/btf/vmlinux)")
	}
}

// RequireKernelFunction fails the test if the specified kernel function
// is not found in /proc/kallsyms.
func RequireKernelFunction(t *testing.T, fnName string) {
	t.Helper()

	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		t.Fatalf("cannot open /proc/kallsyms: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 {
			// Symbol name is the third field, may have module suffix
			sym := fields[2]
			if sym == fnName || strings.HasPrefix(sym, fnName+".") {
				return // Found it
			}
		}
	}

	t.Fatalf("kernel function %s not found in /proc/kallsyms", fnName)
}

// RequireKernelVersion fails the test if the kernel version is below the specified version.
// Useful for features like TCX which require kernel 6.6+.
func RequireKernelVersion(t *testing.T, major, minor int) {
	t.Helper()

	data, err := os.ReadFile("/proc/version")
	if err != nil {
		t.Fatalf("cannot read /proc/version: %v", err)
		return
	}

	// Parse kernel version from /proc/version
	// Format: "Linux version X.Y.Z-..."
	re := regexp.MustCompile(`Linux version (\d+)\.(\d+)`)
	matches := re.FindStringSubmatch(string(data))
	if len(matches) < 3 {
		t.Fatalf("cannot parse kernel version from /proc/version")
		return
	}

	kernelMajor, _ := strconv.Atoi(matches[1])
	kernelMinor, _ := strconv.Atoi(matches[2])

	if kernelMajor < major || (kernelMajor == major && kernelMinor < minor) {
		t.Fatalf("test requires kernel %d.%d+, have %d.%d", major, minor, kernelMajor, kernelMinor)
	}
}

// RequireTracepoint fails the test if the specified tracepoint doesn't exist.
func RequireTracepoint(t *testing.T, group, name string) {
	t.Helper()

	path := filepath.Join("/sys/kernel/debug/tracing/events", group, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("tracepoint %s/%s not found", group, name)
	}
}

// tcIngressFilters returns the TC filters attached to the ingress
// qdisc of the named network interface.
func tcIngressFilters(t *testing.T, ifaceName string) []netlink.Filter {
	t.Helper()
	link, err := netlink.LinkByName(ifaceName)
	require.NoError(t, err)
	filters, err := netlink.FilterList(link, netlink.HANDLE_MIN_INGRESS)
	require.NoError(t, err)
	return filters
}

// sanitizeTestName converts a test name to a safe directory name.
func sanitizeTestName(name string) string {
	// Replace characters that might be problematic in paths
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, " ", "_")
	// Limit length
	if len(name) > 50 {
		name = name[:50]
	}
	return name
}

// isMounted checks if a path is a mount point.
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

// unmount unmounts a filesystem.
func unmount(path string) error {
	// Use lazy unmount to avoid "device busy" errors
	cmd := fmt.Sprintf("umount -l %q 2>/dev/null", path)
	return runCommand(cmd)
}

// runCommand executes a shell command.
func runCommand(cmd string) error {
	c := []string{"sh", "-c", cmd}
	proc := os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	p, err := os.StartProcess("/bin/sh", c, &proc)
	if err != nil {
		return err
	}
	state, err := p.Wait()
	if err != nil {
		return err
	}
	if !state.Success() {
		return fmt.Errorf("command failed: %s", cmd)
	}
	return nil
}

// tcFilterCount returns the number of BPF tc filters on the given
// interface and direction by shelling out to tc(8). This matches the
// upstream Rust bpfman approach to verification.
func tcFilterCount(t *testing.T, iface, direction string) int {
	t.Helper()

	out, err := exec.Command("tc", "filter", "show", "dev", iface, direction).CombinedOutput()
	if err != nil {
		t.Logf("tc filter show dev %s %s: %v (output: %s)", iface, direction, err, out)
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "pref") {
			count++
		}
	}
	return count
}

// cleanupStaleTestDirs removes leftover test directories from previous runs.
func cleanupStaleTestDirs() {
	pattern := filepath.Join(os.TempDir(), "bpfman-e2e-*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	for _, path := range matches {
		// Check if the PID in the directory name is still running
		parts := strings.Split(filepath.Base(path), "-")
		if len(parts) >= 3 {
			pid, err := strconv.Atoi(parts[2])
			if err == nil {
				// Check if process exists
				if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
					// Process still running, skip
					continue
				}
			}
		}

		// Try to unmount bpffs if present
		fsPath := filepath.Join(path, "fs")
		if isMounted(fsPath) {
			unmount(fsPath)
		}

		// Remove the directory
		os.RemoveAll(path)
		// Also remove the -sock directory
		os.RemoveAll(path + "-sock")
	}
}
