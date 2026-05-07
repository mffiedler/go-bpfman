//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	iofs "io/fs"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ciliumebpf "github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	fsruntime "github.com/frobware/go-bpfman/fs/runtime"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/logging"
	"github.com/frobware/go-bpfman/manager"
	bpfnetns "github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/image/oci"
	"github.com/frobware/go-bpfman/platform/image/verify"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
)

// bpfFS embeds the compiled BPF objects under testdata/bpf/ so the
// e2e.test binary is self-contained: at runtime the bytes are
// materialised under each test's baseDir (see materialiseBPFFS),
// and LoadFile resolves relative paths against that directory.
// The embed pattern resolves at `go test -c` time, so the Makefile
// rule for $(E2E_BPF_OBJECTS) is still a real build prereq for
// e2e.test.
//
//go:embed testdata/bpf/*.bpf.o
var bpfFS embed.FS

// TestEnv provides a test environment for e2e tests.
//
// By default (BPFMAN_E2E_ISOLATED_RUNTIME unset) NewTestEnv hands
// back a view onto the suite-wide runtime; the per-test cleanup
// (unmount bpffs, remove temp dir, close store) is then a no-op and
// the suite owns those operations end-to-end via teardownSharedRuntime
// in TestMain. Setting BPFMAN_E2E_ISOLATED_RUNTIME=1 opts each test
// out and gives it its own runtime, enabling fully-isolated parallel
// runs at the cost of cross-test concurrency coverage. See
// shared_runtime_test.go for the rationale.
type TestEnv struct {
	T           *testing.T
	Layout      fs.Layout
	Manager     *manager.Manager
	ImagePuller platform.ImagePuller
	logger      *slog.Logger
	baseDir     string // parent directory containing layout, cache, testdata
	closeEnv    func() error
	// shared is true when this TestEnv is a view onto the suite-wide
	// runtime rather than a per-test runtime; cleanup() is a no-op
	// in that case and the suite-end teardown owns global teardown.
	shared bool

	// scopeMu guards the per-test scope sets below. In shared mode
	// concurrent tests run against the same manager, so the
	// TestEnv's bookkeeping has to be safe against the test's own
	// callers parallelising helpers (none today, but cheap to keep
	// correct).
	scopeMu       sync.Mutex
	scopePrograms map[kernel.ProgramID]struct{}
	scopeLinks    map[kernel.LinkID]struct{}
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

	// Shared-runtime mode: hand back a view onto the suite-wide
	// runtime that TestMain stood up. The per-test bpffs mount,
	// store, and manager are skipped; cleanup is a no-op. Note
	// that AssertCleanState and friends still operate on global
	// state in this mode -- phase 2 of the shared-runtime work
	// makes them scope-aware. Today, running multiple tests
	// concurrently in shared mode will trip the global checks.
	if sharedRuntimeMode() {
		rt := requireSharedRuntimeForTest(t)
		env := &TestEnv{
			T:             t,
			Layout:        rt.layout,
			Manager:       rt.manager,
			ImagePuller:   rt.imagePuller,
			logger:        rt.logger.With("test", t.Name()),
			baseDir:       rt.baseDir,
			closeEnv:      nil,
			shared:        true,
			scopePrograms: make(map[kernel.ProgramID]struct{}),
			scopeLinks:    make(map[kernel.LinkID]struct{}),
		}
		t.Cleanup(env.cleanup)
		return env
	}

	// Create unique directory for this test
	baseDir, err := os.MkdirTemp("", fmt.Sprintf("bpfman-e2e-%d-", os.Getpid()))
	if err != nil {
		t.Fatalf("failed to create temp directory: %v", err)
	}

	if err := materialiseBPFFS(baseDir); err != nil {
		t.Fatalf("materialise embedded BPF objects: %v", err)
	}

	layout, err := fs.New(baseDir)
	if err != nil {
		t.Fatalf("invalid runtime directory: %v", err)
	}

	imageCacheBase, err := fs.NewImageCache(filepath.Join(layout.Base(), "cache", "image"))
	if err != nil {
		t.Fatalf("invalid image cache directory: %v", err)
	}
	imageCache, err := fs.EnsureCache(imageCacheBase)
	if err != nil {
		t.Fatalf("failed to ensure image cache: %v", err)
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
	} else if false {
		// Disabled diagnostic path: route every record through
		// t.Logf at Info level so the verify: extension link
		// lines from the dispatcher rebuild paths surface in
		// test output on failure. Re-enable by changing
		// `if false` to a true condition. Disabled because
		// the per-record t.Logf call alters timing enough to
		// hide the chain race we are currently hunting; we
		// want the original stderr-only path for repro.
		logger = slog.New(newTLogHandler(t, slog.LevelInfo))
	} else {
		// Default: only errors
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelError,
		}))
	}

	// Create store
	ctx := context.Background()
	store, err := sqlite.New(ctx, layout.DBPath(), logger)
	require.NoError(t, err, "failed to create store")

	// Create kernel adapter
	kernel := ebpf.New(ebpf.WithLogger(logger))

	// Ensure runtime directories and bpffs mount
	ensuredRuntime, err := fsruntime.New(layout, fsruntime.RealMounter{}, logger)
	require.NoError(t, err, "failed to ensure runtime")

	// Create signature verifier (disabled for tests)
	verifier := verify.NoSign()

	// Create image puller for OCI images
	puller, err := oci.NewPuller(
		imageCache,
		oci.WithLogger(logger),
		oci.WithVerifier(verifier),
	)
	require.NoError(t, err, "failed to create image puller")

	// Create manager
	mgr, err := manager.New(ensuredRuntime, puller, store, kernel, ebpf.NewProgramDiscoverer(), logger)
	require.NoError(t, err, "failed to create manager")

	cleanup := func() error {
		return store.Close()
	}

	env := &TestEnv{
		T:           t,
		Layout:      layout,
		Manager:     mgr,
		ImagePuller: puller,
		logger:      logger,
		baseDir:     baseDir,
		closeEnv:    cleanup,
	}

	// Register cleanup
	t.Cleanup(func() {
		env.cleanup()
	})

	return env
}

// cleanup releases resources and removes test directories.
// Failures are reported and cause the test to fail.
//
// In shared-runtime mode this is a no-op: the bpffs mount, store,
// and base directory belong to the suite, not to any single test,
// and teardownSharedRuntime in TestMain owns those operations.
// Per-test resource lifecycle (programs loaded, links attached) is
// still managed by the test's own t.Cleanup callbacks via
// env.Detach / env.Unload, exactly as in isolated mode.
func (e *TestEnv) cleanup() {
	if e.shared {
		return
	}
	if e.closeEnv != nil {
		if err := e.closeEnv(); err != nil {
			e.T.Errorf("failed to close environment: %v", err)
		}
	}

	// Unmount bpffs that was mounted by NewTestEnv
	bpffsMount := e.Layout.BPFFSMountPoint()
	e.T.Logf("unmounting bpffs at %s", bpffsMount)
	if err := unmount(bpffsMount); err != nil {
		e.T.Errorf("failed to unmount bpffs: %v", err)
	}

	// Remove the test directory
	e.T.Logf("removing test directory %s", e.baseDir)
	if err := os.RemoveAll(e.baseDir); err != nil {
		e.T.Errorf("failed to remove %s: %v", e.baseDir, err)
	}
}

// runWithLock executes a function under the writer lock. Routes
// through lock.RunWithTiming so wait_ms / held_ms appear in the
// log stream tagged component=lock; emit at Debug level, so the
// default e2e logger (Error level) still drops them and the
// instrumentation is free unless explicitly opted into via
// BPFMAN_LOG=lock=debug (or a coarser BPFMAN_LOG=debug).
//
// Tags every entry with op=<calling-method> via runtime.Caller and
// test=<t.Name()> via the env logger, so a shared-runtime BPFMAN_LOG
// run can be slice-and-diced by operation (LoadFile, Attach, ...)
// or by test to find the outliers behind shared-mode wall-clock.
func (e *TestEnv) runWithLock(ctx context.Context, fn func(context.Context, lock.WriterScope) error) error {
	op := callerOp()
	return lock.RunWithTiming(ctx, e.Layout.LockPath(), e.logger.With("op", op), fn)
}

// callerOp returns the unqualified name of the function that
// called runWithLock -- e.g. "LoadFile", "Attach". Used to tag
// the lock-timing log entries so shared-runtime BPFMAN_LOG runs
// can be aggregated by operation type. Returns "?" if the stack
// inspection fails; callers must not rely on this for control
// flow.
func callerOp() string {
	pc, _, _, ok := runtime.Caller(2)
	if !ok {
		return "?"
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "?"
	}
	name := fn.Name()
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// LoadImage loads BPF programs from an OCI image.
func (e *TestEnv) LoadImage(ctx context.Context, ref platform.ImageRef, programs []manager.ProgramSpec, opts manager.LoadOpts) ([]bpfman.Program, error) {
	var result []bpfman.Program
	err := e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		var loadErr error
		result, loadErr = e.Manager.Load(ctx, writeLock, manager.LoadSource{
			Image: &ref,
		}, programs, opts)
		return loadErr
	})
	if err == nil {
		e.trackPrograms(result)
	}
	return result, err
}

// LoadFile loads BPF programs from a local object file.
//
// Relative paths are resolved against the per-test baseDir, into
// which the embedded testdata/bpf/ tree is materialised at
// NewTestEnv. This lets call sites keep their historical
// "testdata/bpf/foo.bpf.o" form regardless of cwd.
func (e *TestEnv) LoadFile(ctx context.Context, filePath string, programs []manager.ProgramSpec, opts manager.LoadOpts) ([]bpfman.Program, error) {
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(e.baseDir, filePath)
	}
	var result []bpfman.Program
	err := e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		var loadErr error
		result, loadErr = e.Manager.Load(ctx, writeLock, manager.LoadSource{
			FilePath: filePath,
		}, programs, opts)
		return loadErr
	})
	if err == nil {
		e.trackPrograms(result)
	}
	return result, err
}

// materialiseBPFFS writes every file in the embedded BPF filesystem
// out under root, preserving the embed.FS layout (testdata/bpf/...).
// Called once per TestEnv so the Manager.Load file-path machinery
// can open real files even though the binary ships its own copy.
func materialiseBPFFS(root string) error {
	return iofs.WalkDir(bpfFS, ".", func(name string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(root, name)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := bpfFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		return nil
	})
}

// Unload unloads a BPF program.
func (e *TestEnv) Unload(ctx context.Context, programID kernel.ProgramID) error {
	err := e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		return e.Manager.Unload(ctx, writeLock, programID)
	})
	if err == nil {
		e.untrackProgram(programID)
	}
	return err
}

// List returns the managed programs visible to this TestEnv.
//
// In isolated mode (the default) this is everything the manager
// knows about, since the manager is per-test. In shared-runtime
// mode the result is filtered to programs this TestEnv created via
// LoadFile / LoadImage and not yet Unloaded -- callers that wrote
// against the historical "shows my programs" expectation continue
// to work without scope-awareness leaking into every test.
func (e *TestEnv) List(ctx context.Context) ([]bpfman.Program, error) {
	result, err := e.Manager.ListPrograms(ctx)
	if err != nil {
		return nil, err
	}
	if !e.shared {
		return result.Programs, nil
	}
	mine := make([]bpfman.Program, 0, e.scopeProgramCount())
	for _, p := range result.Programs {
		if p.Status.Kernel == nil {
			continue
		}
		e.scopeMu.Lock()
		_, ok := e.scopePrograms[p.Status.Kernel.ID]
		e.scopeMu.Unlock()
		if ok {
			mine = append(mine, p)
		}
	}
	return mine, nil
}

// Get returns detailed information about a program.
func (e *TestEnv) Get(ctx context.Context, programID kernel.ProgramID) (bpfman.Program, error) {
	return e.Manager.Get(ctx, programID)
}

// Attach attaches a program using the given spec. The writer lock is
// acquired automatically and passed to the manager.
func (e *TestEnv) Attach(ctx context.Context, spec bpfman.AttachSpec) (bpfman.LinkRecord, error) {
	var result bpfman.Link
	err := e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		link, attachErr := e.Manager.Attach(ctx, writeLock, spec)
		result = link
		return attachErr
	})
	if err != nil {
		return bpfman.LinkRecord{}, err
	}
	e.trackLink(result.Record.ID)
	record, err := e.Manager.GetLink(ctx, result.Record.ID)
	if err != nil {
		return bpfman.LinkRecord{ID: result.Record.ID}, nil
	}
	return record, nil
}

// Detach detaches a link.
func (e *TestEnv) Detach(ctx context.Context, linkID kernel.LinkID) error {
	err := e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		return e.Manager.Detach(ctx, writeLock, linkID)
	})
	if err == nil {
		e.untrackLink(linkID)
	}
	return err
}

// trackPrograms records every successfully loaded program in the
// TestEnv's local set so AssertProgramCount and AssertCleanState
// can return scope-local answers under shared mode. Cheap in
// isolated mode (the assertion helpers ignore the set there).
func (e *TestEnv) trackPrograms(progs []bpfman.Program) {
	if len(progs) == 0 {
		return
	}
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	if e.scopePrograms == nil {
		e.scopePrograms = make(map[kernel.ProgramID]struct{}, len(progs))
	}
	for _, p := range progs {
		if p.Status.Kernel == nil {
			continue
		}
		e.scopePrograms[p.Status.Kernel.ID] = struct{}{}
	}
}

func (e *TestEnv) untrackProgram(id kernel.ProgramID) {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	delete(e.scopePrograms, id)
}

func (e *TestEnv) trackLink(id kernel.LinkID) {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	if e.scopeLinks == nil {
		e.scopeLinks = make(map[kernel.LinkID]struct{})
	}
	e.scopeLinks[id] = struct{}{}
}

func (e *TestEnv) untrackLink(id kernel.LinkID) {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	delete(e.scopeLinks, id)
}

func (e *TestEnv) scopeProgramCount() int {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return len(e.scopePrograms)
}

func (e *TestEnv) scopeLinkCount() int {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return len(e.scopeLinks)
}

func (e *TestEnv) scopeContainsLink(id kernel.LinkID) bool {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	_, ok := e.scopeLinks[id]
	return ok
}

// ListLinks returns the managed links visible to this TestEnv.
// Scope-aware in shared-runtime mode; see List for the rationale.
func (e *TestEnv) ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error) {
	all, err := e.Manager.ListLinks(ctx)
	if err != nil {
		return nil, err
	}
	if !e.shared {
		return all, nil
	}
	mine := make([]bpfman.LinkRecord, 0, e.scopeLinkCount())
	for _, l := range all {
		if e.scopeContainsLink(l.ID) {
			mine = append(mine, l)
		}
	}
	return mine, nil
}

// GetLink returns detailed information about a link.
func (e *TestEnv) GetLink(ctx context.Context, linkID kernel.LinkID) (bpfman.LinkRecord, bpfman.LinkDetails, error) {
	record, err := e.Manager.GetLink(ctx, linkID)
	if err != nil {
		return bpfman.LinkRecord{}, nil, err
	}
	return record, record.Details, nil
}

// GetDispatcherSnapshot retrieves the full dispatcher snapshot for the
// given type, namespace, and interface.
func (e *TestEnv) GetDispatcherSnapshot(ctx context.Context, key dispatcher.Key) (platform.DispatcherSnapshot, error) {
	return e.Manager.GetDispatcherSnapshot(ctx, key)
}

// GC runs garbage collection, removing stale store entries that no
// longer correspond to kernel objects. This mirrors the GC that the
// daemon runs before each gRPC RPC.
func (e *TestEnv) GC(ctx context.Context) (manager.GCResult, error) {
	var result manager.GCResult
	err := e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		var gcErr error
		result, gcErr = e.Manager.GC(ctx, writeLock)
		return gcErr
	})
	return result, err
}

// AssertCleanState verifies that no programs or links are managed.
func (e *TestEnv) AssertCleanState() {
	e.T.Helper()
	e.AssertProgramCount(0)
	e.AssertLinkCount(0)
}

// AssertProgramCount verifies the number of managed programs.
//
// In shared-runtime mode the assertion is scoped to programs this
// TestEnv created (via env.LoadFile / env.LoadImage and not yet
// Unloaded), since the manager's global view also contains other
// concurrent tests' programs. In isolated mode the per-test
// manager has only this test's programs, so a global list is
// equivalent; we keep the global path for that mode unchanged.
func (e *TestEnv) AssertProgramCount(expected int) {
	e.T.Helper()
	if e.shared {
		got := e.scopeProgramCount()
		require.Equal(e.T, expected, got,
			"unexpected scope-local program count (shared runtime mode); want=%d got=%d",
			expected, got)
		return
	}
	ctx := context.Background()
	programs, err := e.List(ctx)
	require.NoError(e.T, err, "failed to list programs")
	require.Len(e.T, programs, expected, "unexpected program count")
}

// AssertLinkCount verifies the total number of managed links.
// Scope-aware in shared-runtime mode; see AssertProgramCount.
func (e *TestEnv) AssertLinkCount(expected int) {
	e.T.Helper()
	if e.shared {
		got := e.scopeLinkCount()
		require.Equal(e.T, expected, got,
			"unexpected scope-local link count (shared runtime mode); want=%d got=%d",
			expected, got)
		return
	}
	ctx := context.Background()
	links, err := e.ListLinks(ctx)
	require.NoError(e.T, err, "failed to list links")
	require.Len(e.T, links, expected, "unexpected link count")
}

// AssertLinkCountByKind verifies the number of links of a specific kind.
// Scope-aware in shared-runtime mode; iterates the global list and
// keeps only links this TestEnv created.
func (e *TestEnv) AssertLinkCountByKind(linkKind bpfman.LinkKind, expected int) {
	e.T.Helper()
	ctx := context.Background()

	links, err := e.ListLinks(ctx)
	require.NoError(e.T, err, "failed to list links")

	count := 0
	for _, link := range links {
		if link.Kind != linkKind {
			continue
		}
		if e.shared && !e.scopeContainsLink(link.ID) {
			continue
		}
		count++
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

// RequireIsolatedRuntime skips the test under the shared-runtime
// default (BPFMAN_E2E_ISOLATED_RUNTIME unset). Use it for tests
// whose assertions are globally scoped -- e.g. "after my last
// unload the shared bpffs pin is removed" -- which are correct in
// the per-test runtime where this test owns the entire bpffs, but
// cannot hold under shared mode where concurrent tests legitimately
// keep the same shared resources alive. The skip carries a reason
// so a shared-mode run still reports clearly that something was
// deliberately not exercised.
func RequireIsolatedRuntime(t *testing.T, reason string) {
	t.Helper()
	if sharedRuntimeMode() {
		t.Skipf("skipped under shared runtime (set BPFMAN_E2E_ISOLATED_RUNTIME=1 to exercise): %s", reason)
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
// Checks both tracefs locations: /sys/kernel/tracing (modern) and
// /sys/kernel/debug/tracing (legacy debugfs mount).
func RequireTracepoint(t *testing.T, group, name string) {
	t.Helper()

	// Modern kernels mount tracefs at /sys/kernel/tracing
	// Older systems use /sys/kernel/debug/tracing via debugfs
	paths := []string{
		filepath.Join("/sys/kernel/tracing/events", group, name),
		filepath.Join("/sys/kernel/debug/tracing/events", group, name),
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}

	t.Fatalf("tracepoint %s/%s not found (checked %v)", group, name, paths)
}

// RequireTC fails the test if the tc command is not available.
func RequireTC(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("tc"); err != nil {
		t.Fatal("test requires tc command (iproute2)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Wrapped via `ip netns exec root` so the netns is
	// determined by the bind-mount at /run/netns/root rather
	// than by whichever Go thread happens to perform the fork.
	out, err := exec.CommandContext(ctx, "ip", "netns", "exec", rootNetnsName, "tc", "filter", "show", "dev", iface, direction).CombinedOutput()
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

// TestInterface holds information about a test network interface.
type TestInterface struct {
	Name    string
	Ifindex int
}

var testNameSeq atomic.Uint64

// uniqueTestName generates a unique name for test network interfaces
// and namespaces. The name starts with "b" and ends with "n" (the
// first and last letters of "bpfman"), with 12 hex characters
// between them derived from hashing the PID and an atomic counter.
// The result is 14 characters, leaving room for a single veth
// suffix within the IFNAMSIZ limit of 15.
func uniqueTestName() string {
	n := testNameSeq.Add(1)
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%d", os.Getpid(), n)
	return fmt.Sprintf("B%012xN", h.Sum64()&0xffffffffffff)
}

// slotProvenance records the most recent occupant of a pair
// index slot so a stale-state check on the next acquire can
// attribute any leaked kernel state to a specific test.
type slotProvenance struct {
	testName   string
	nsName     string
	linkAName  string // name of A-side veth in root namespace
	releasedAt time.Time
}

// vethAddrPool tracks which pair indices in [1, 127] are
// currently allocated. Indices map to /32 addresses inside RFC
// 5737 TEST-NET-2 (198.51.100.0/24) via vethAddrsForIndex. The
// pool is sized for peak concurrent veth pairs across parallel
// tests, not the cumulative total over the lifetime of the
// process: NewTestVethPair acquires an index, the t.Cleanup
// releases it. Pre-release the counter was monotonic and a
// `-test.count=N` run quickly exceeded the 127 ceiling even
// though peak concurrency stayed well under it.
//
// Allocation order is FIFO: the oldest released index is handed
// out first. Each pair index pins a (deterministic MAC, IP)
// pair; reusing the most recently released index immediately
// would put a fresh test on top of kernel state (ARP entries,
// refcount-pending XDP links on the just-deleted veth's
// ifindex) that has not finished tearing down. FIFO maximises
// the cooldown each freed index gets before reuse without
// growing the pool.
//
// last[idx] keeps the forensic breadcrumb of the slot's most
// recent occupant so acquire-time stale-state checks can name
// the test that leaked.
var vethAddrPool = struct {
	mu   sync.Mutex
	used [128]bool // index 0 unused; valid range is [1, 127]
	free []uint32  // FIFO queue of free indices; head = oldest
	last [128]slotProvenance
}{
	free: func() []uint32 {
		q := make([]uint32, 0, 127)
		for i := uint32(1); i <= 127; i++ {
			q = append(q, i)
		}
		return q
	}(),
}

// acquireVethAddrs takes the oldest free index from
// vethAddrPool, asserts that no kernel state from the slot's
// previous occupant is still present, and returns the slot's
// /32 addresses. Panics if the pool is exhausted -- that means
// more than 127 veth pairs are alive concurrently, which is well
// past expected parallelism and indicates either a leak
// (releaseVethAddrs not called) or genuinely too many parallel
// tests for this address range.
//
// nsName and linkAName are the names the caller is about to
// bind to this slot. They are recorded so that the *next*
// acquire of this slot can verify the previous occupant
// actually deleted them. testName is t.Name() captured before
// any failure path so attribution in panics or fatals is always
// accurate.
func acquireVethAddrs(t *testing.T, nsName, linkAName string) (addrA, addrB, pingTarget string, pairIndex uint32) {
	t.Helper()
	vethAddrPool.mu.Lock()
	defer vethAddrPool.mu.Unlock()
	if len(vethAddrPool.free) == 0 {
		panic("veth address pool exhausted: more than 127 concurrent veth pairs in flight (leak or excessive parallelism)")
	}
	pairIndex = vethAddrPool.free[0]
	vethAddrPool.free = vethAddrPool.free[1:]

	addrA, addrB, pingTarget = vethAddrsForIndex(pairIndex)

	if err := assertSlotClean(pairIndex, vethAddrPool.last[pairIndex]); err != nil {
		t.Fatalf("acquireVethAddrs: %v", err)
	}

	vethAddrPool.used[pairIndex] = true
	vethAddrPool.last[pairIndex] = slotProvenance{
		testName:  t.Name(),
		nsName:    nsName,
		linkAName: linkAName,
	}
	return addrA, addrB, pingTarget, pairIndex
}

// assertSlotClean verifies that no kernel state attributable to
// the slot's previous occupant is still present. Checks the two
// resources the previous occupant owned by name:
//
//   - the A-side veth in root namespace (LinkByName)
//   - the netns the B-side lived in (netns.GetFromName)
//
// Both must be absent; if either remains, the previous
// occupant's t.Cleanup did not finish. On a leak the error
// names the test that previously held the slot, the kind of
// leak, and how long ago the slot was released. The caller
// raises this via t.Fatalf so the leak fails the *next* test
// as a canary, surfaced loudly with attribution rather than
// silently propagating into mysterious EBUSY/EEXIST further
// down the line.
//
// Targeted lookups (rather than dumping the whole link table)
// avoid NLM_F_DUMP_INTR under heavy parallel churn and answer
// the exact "did the previous tenant clean up?" question.
func assertSlotClean(idx uint32, prev slotProvenance) error {
	if prev.linkAName != "" {
		if lnk, err := netlink.LinkByName(prev.linkAName); err == nil {
			attrs := lnk.Attrs()
			return fmt.Errorf("pair index %d: root-ns interface %q (ifindex %d) from previous tenant test=%q still exists (released %s ago); cleanup did not delete it",
				idx, attrs.Name, attrs.Index, prev.testName,
				time.Since(prev.releasedAt))
		}
	}
	if prev.nsName != "" {
		if h, err := netns.GetFromName(prev.nsName); err == nil {
			h.Close()
			return fmt.Errorf("pair index %d: netns %q from previous tenant test=%q still exists (released %s ago); cleanup did not delete it",
				idx, prev.nsName, prev.testName,
				time.Since(prev.releasedAt))
		}
	}
	return nil
}

// releaseVethAddrs returns an index to vethAddrPool so the next
// acquireVethAddrs can reuse its addresses. Released indices go
// to the tail of the FIFO queue, ensuring maximum cooldown
// before reuse. Idempotent for the no-op case (already-free
// index) so a double-cleanup doesn't crash; the index becoming
// free twice is benign and the second release is dropped.
func releaseVethAddrs(pairIndex uint32) {
	vethAddrPool.mu.Lock()
	defer vethAddrPool.mu.Unlock()
	if pairIndex < 1 || pairIndex > 127 {
		return
	}
	if !vethAddrPool.used[pairIndex] {
		return
	}
	vethAddrPool.used[pairIndex] = false
	vethAddrPool.last[pairIndex].releasedAt = time.Now()
	vethAddrPool.free = append(vethAddrPool.free, pairIndex)
}

// vethAddrsForIndex returns unique /32 addresses for the given pair
// index. The index must be in [1, 127].
func vethAddrsForIndex(n uint32) (addrA, addrB, pingTarget string) {
	if n < 1 || n > 127 {
		panic(fmt.Sprintf("veth pair index %d out of range [1, 127]", n))
	}
	hostA := n*2 + 1 // 3, 5, 7, ...
	hostB := n * 2   // 2, 4, 6, ...
	addrA = fmt.Sprintf("198.51.100.%d/32", hostA)
	addrB = fmt.Sprintf("198.51.100.%d/32", hostB)
	pingTarget = fmt.Sprintf("198.51.100.%d", hostA)
	return
}

// NewTestInterface creates a dummy network interface for testing.
// The interface is automatically deleted via t.Cleanup().
// Each test gets a unique interface, enabling parallel execution.
func NewTestInterface(t *testing.T) TestInterface {
	t.Helper()

	name := uniqueTestName()

	t.Logf("creating interface %s", name)

	// Fail if interface already exists - indicates a leak from a previous test.
	if _, err := netlink.LinkByName(name); err == nil {
		t.Fatalf("interface %s already exists (leaked from previous test?)", name)
	}

	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: name, TxQLen: 1000},
	}

	if err := netlink.LinkAdd(dummy); err != nil {
		t.Fatalf("failed to create dummy interface %s: %v", name, err)
	}

	t.Cleanup(func() {
		// Best effort cleanup - interface may already be gone
		if link, err := netlink.LinkByName(name); err == nil {
			netlink.LinkDel(link)
		}
	})

	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("failed to find dummy interface %s: %v", name, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("failed to bring up interface %s: %v", name, err)
	}

	return TestInterface{
		Name:    name,
		Ifindex: link.Attrs().Index,
	}
}

// TestVethPair holds information about a veth pair where one end is
// in the root namespace and the other is in a test network namespace.
// Programs are attached to interface A (root namespace); traffic is
// generated from interface B (test namespace).
type TestVethPair struct {
	A          TestInterface // root namespace, attach programs here
	B          TestInterface // test namespace, generate traffic here
	Netns      string        // network namespace name
	PingTarget string        // A's IP address (ping destination from B)
}

// NewTestVethPair creates a veth pair with one end in a dedicated
// network namespace for generating real traffic through TC hooks.
//
// A unique name and unique /32 addresses from RFC 5737 TEST-NET-2
// (198.51.100.0/24) are generated automatically. Interface A stays
// in the root namespace; interface B is moved to a new network
// namespace. Peer routes ensure each pair has its own distinct
// routing entry, avoiding conflicts when multiple pairs coexist.
//
// Both interfaces and the namespace are cleaned up via t.Cleanup().
func NewTestVethPair(t *testing.T) TestVethPair {
	t.Helper()

	base := uniqueTestName()
	nameA := base + "a"
	nameB := base + "b"
	nsName := base

	// Fail if interfaces already exist.
	for _, name := range []string{nameA, nameB} {
		if _, err := netlink.LinkByName(name); err == nil {
			t.Fatalf("interface %s already exists (leaked from previous test?)", name)
		}
	}

	// Fail if namespace already exists.
	if _, err := netns.GetFromName(nsName); err == nil {
		t.Fatalf("namespace %s already exists (leaked from previous test?)", nsName)
	}

	// Acquire the address index first and register its release
	// cleanup before any kernel artefacts (namespace, veth) so
	// LIFO cleanup order is: delete veth -> delete namespace ->
	// release index. The release must run *after* the kernel
	// has removed the interface (and with it the addresses and
	// routes) so a concurrent acquirer reusing the same index
	// doesn't try to add the still-present /32 to a new veth.
	ipA, ipB, pingTarget, pairIdx := acquireVethAddrs(t, nsName, nameA)
	t.Cleanup(func() { releaseVethAddrs(pairIdx) })

	// Create the named netns. CreateNamed handles the
	// LockOSThread / NewNamed / restore / UnlockOSThread
	// dance correctly: on any error after the new netns is
	// created the OS thread is left locked so that t.Fatalf
	// retires it rather than returning a poisoned thread
	// (still in the named netns) to Go's scheduler.
	if err := bpfnetns.CreateNamed(nsName); err != nil {
		t.Fatalf("%v", err)
	}

	t.Cleanup(func() {
		netns.DeleteNamed(nsName)
	})

	t.Logf("creating veth pair %s/%s in namespace %s", nameA, nameB, nsName)

	// Compute deterministic MACs and pass them at create time
	// rather than overwriting after creation. See the long
	// comment further down for the format and history. Setting
	// the address at LinkAdd time (via LinkAttrs.HardwareAddr /
	// PeerHardwareAddr) means the kernel never assigns a random
	// MAC in the first place; subsequent NETDEV_CHANGE storms
	// from sibling subtests' teardown cannot regenerate an
	// address that wasn't kernel-generated.
	pid := os.Getpid()
	macA, _ := net.ParseMAC(fmt.Sprintf("02:%02x:%02x:00:%02x:01", (pid>>8)&0xff, pid&0xff, pairIdx))
	macB, _ := net.ParseMAC(fmt.Sprintf("02:%02x:%02x:00:%02x:02", (pid>>8)&0xff, pid&0xff, pairIdx))

	// Create veth pair in root namespace with MACs baked in.
	veth := &netlink.Veth{
		LinkAttrs:        netlink.LinkAttrs{Name: nameA, TxQLen: 1000, HardwareAddr: macA},
		PeerName:         nameB,
		PeerHardwareAddr: macB,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("failed to create veth pair %s/%s: %v", nameA, nameB, err)
	}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(nameA); err == nil {
			netlink.LinkDel(link)
		}
	})

	// Set TxQLen on peer before moving it into the namespace.
	linkB, err := netlink.LinkByName(nameB)
	if err != nil {
		t.Fatalf("failed to find interface %s: %v", nameB, err)
	}
	if err := netlink.LinkSetTxQLen(linkB, 1000); err != nil {
		t.Fatalf("failed to set txqlen on %s: %v", nameB, err)
	}

	// Move B into the namespace via netlink.
	nsHandleForMove, err := netns.GetFromName(nsName)
	if err != nil {
		t.Fatalf("failed to get ns handle for %s: %v", nsName, err)
	}
	if err := netlink.LinkSetNsFd(linkB, int(nsHandleForMove)); err != nil {
		nsHandleForMove.Close()
		t.Fatalf("failed to move %s to namespace %s: %v", nameB, nsName, err)
	}
	nsHandleForMove.Close()

	// Configure A in root namespace with a peer route to B.
	linkA, err := netlink.LinkByName(nameA)
	if err != nil {
		t.Fatalf("failed to find interface %s: %v", nameA, err)
	}

	// Set deterministic, locally administered MAC addresses on
	// both veth ends. This is essential for parallel test
	// stability.
	//
	// The kernel auto-assigns random MACs at veth creation time
	// and marks them NET_ADDR_RANDOM internally. Under concurrent
	// veth creation and deletion (as happens with parallel
	// subtests whose t.Cleanup tears down finished pairs while
	// other pairs are still live), the kernel can regenerate MACs
	// marked NET_ADDR_RANDOM on unrelated interfaces. This
	// invalidates ARP caches and causes 100% ping packet loss.
	//
	// Explicitly setting the MAC via LinkSetHardwareAddr changes
	// the kernel's addr_assign_type to NET_ADDR_SET, which the
	// kernel treats as sacrosanct and never regenerates.
	//
	// We use the IEEE 802 locally administered address space.
	// The first octet's two least-significant bits control the
	// address type:
	//
	//   First octet: 0x02
	//
	//     bit 7   bit 0
	//      |       |
	//      v       v
	//      0 0 0 0 0 0 1 0
	//                  | |
	//                  | +-- 0 = unicast (vs 1 = multicast)
	//                  +---- 1 = locally administered (vs 0 = OUI/global)
	//
	// The "locally administered" bit (bit 1) indicates the
	// address is not from a globally unique OUI allocation but
	// a locally scoped assignment, analogous to RFC 1918 for IP
	// addresses. Any MAC with this bit set is guaranteed never
	// to collide with a manufacturer-assigned address.
	//
	// The full format is:
	//
	//   02:<pid_hi>:<pid_lo>:00:<pair>:<end>
	//
	// where <pid_hi>:<pid_lo> are the two least-significant
	// bytes of the process ID (ensuring uniqueness across
	// concurrent stress test processes), <pair> is the veth pair
	// index returned by acquireVethAddrs (ensuring uniqueness within a
	// process and avoiding a race between Add and Load on the
	// atomic counter), and <end> is 01 for the A side (root
	// namespace) or 02 for the B side (test namespace).
	//
	// History: we originally relied on kernel-assigned random
	// MACs, which worked reliably for sequential tests. When we
	// made subtests parallel for speed (32s down to ~4s), one
	// subtest per run would fail intermittently with 100% ping
	// packet loss.
	//
	// Using ip monitor we captured the root cause: a live veth
	// interface's MAC would change mid-test (same ifindex,
	// different MAC), immediately followed by the kernel
	// flushing ARP neighbour entries and regenerating the IPv6
	// link-local address (derived via EUI-64 from the new MAC).
	// The MAC change was always correlated with another
	// subtest's t.Cleanup deleting its own (unrelated) veth
	// pair. Serialising setup and teardown under a mutex did
	// not help because the MAC regeneration is kernel-internal
	// and asynchronous.
	//
	// First mitigation was to call LinkSetHardwareAddr after
	// LinkAdd, which reduced the failure rate from ~40% to ~5%
	// but did not eliminate it. Layering disableIPv6 on both
	// veth ends (see calls below) drove it to 0% in our
	// original stress runs.
	//
	// Examining the kernel 6.12 source revealed the likely
	// chain of events: when a veth peer is deleted,
	// veth_dellink triggers carrier loss on the surviving end
	// via netif_carrier_off, which fires a NETDEV_CHANGE
	// notification through the linkwatch subsystem. The IPv6
	// addrconf_notify handler (net/ipv6/addrconf.c) processes
	// NETDEV_CHANGE and calls addrconf_dev_config, which
	// triggers EUI-64 link-local address generation from the
	// device's MAC. This processing chain appears to cause MAC
	// regeneration on other veth interfaces as a side effect.
	//
	// On aarch64 (Asahi, kernel 6.12.x) we observed the
	// LinkSetHardwareAddr-then-disableIPv6 combination still
	// flaking: A's MAC reverted to a kernel-random value
	// between LinkSetHardwareAddr and the post-warmup ARP
	// check, even with IPv6 disabled. The current strategy
	// passes the deterministic MAC at LinkAdd time via
	// LinkAttrs.HardwareAddr and PeerHardwareAddr, so the
	// kernel never assigns a random MAC in the first place.
	// There is no "original" address for any later code path
	// to regenerate back to. disableIPv6 is retained as a
	// secondary defence and to avoid wasting cycles on
	// IPv6 link-local plumbing the tests do not use.

	// Disable IPv6 on A before link-up. We only need IPv4 for
	// the ping traffic, and IPv6 link-local address generation
	// triggers kernel code paths that can regenerate MAC
	// addresses on veth interfaces under concurrent load.
	disableIPv6(t, nameA)

	addrA, _ := netlink.ParseAddr(ipA)
	peerOfA, _ := netlink.ParseAddr(ipB)
	addrA.Peer = peerOfA.IPNet
	if err := netlink.AddrAdd(linkA, addrA); err != nil {
		t.Fatalf("failed to add address to %s: %v", nameA, err)
	}
	if err := netlink.LinkSetUp(linkA); err != nil {
		t.Fatalf("failed to bring up %s: %v", nameA, err)
	}

	// Configure B inside the namespace via a netlink handle.
	nsHandleForConfig, err := netns.GetFromName(nsName)
	if err != nil {
		t.Fatalf("failed to get ns handle for config: %v", err)
	}
	nlh, err := netlink.NewHandleAt(nsHandleForConfig)
	nsHandleForConfig.Close()
	if err != nil {
		t.Fatalf("failed to create netlink handle in namespace %s: %v", nsName, err)
	}
	defer nlh.Close()

	nsLinkB, err := nlh.LinkByName(nameB)
	if err != nil {
		t.Fatalf("failed to find %s in namespace: %v", nameB, err)
	}
	// B's MAC was set at LinkAdd time via PeerHardwareAddr.

	// Disable IPv6 on B inside the namespace.
	disableIPv6InNs(t, nsName, nameB)

	addrB, _ := netlink.ParseAddr(ipB)
	peerOfB, _ := netlink.ParseAddr(ipA)
	addrB.Peer = peerOfB.IPNet
	if err := nlh.AddrAdd(nsLinkB, addrB); err != nil {
		t.Fatalf("failed to add address to %s: %v", nameB, err)
	}
	if err := nlh.LinkSetUp(nsLinkB); err != nil {
		t.Fatalf("failed to bring up %s: %v", nameB, err)
	}

	// Bring up loopback in the namespace.
	lo, err := nlh.LinkByName("lo")
	if err != nil {
		t.Fatalf("failed to find lo in namespace: %v", err)
	}
	if err := nlh.LinkSetUp(lo); err != nil {
		t.Fatalf("failed to bring up lo in namespace: %v", err)
	}

	// Wait for both veth ends to reach OperUp. Veth interfaces
	// transition to OperUp once both peers are up, but there can
	// be a brief kernel event propagation delay under load.
	waitLinkOperUp(t, nil, nameA, 5*time.Second)
	waitLinkOperUp(t, nlh, nameB, 5*time.Second)

	// Verify end-to-end connectivity with a warmup ping. Under
	// heavy parallel load ARP resolution can lag behind link-up.
	waitConnectivity(t, nsName, pingTarget, 30*time.Second)

	// Verify ARP consistency: B's cached MAC for A must match
	// A's actual MAC. Log both for debugging intermittent failures.
	linkARefresh, _ := netlink.LinkByName(nameA)
	aMac := linkARefresh.Attrs().HardwareAddr.String()
	aIdx := linkARefresh.Attrs().Index
	arpOut, _ := exec.Command("ip", "netns", "exec", nsName,
		"ip", "neigh", "show", "dev", nameB, pingTarget).CombinedOutput()
	t.Logf("post-warmup: A=%s ifindex=%d MAC=%s, B's ARP: %s",
		nameA, aIdx, aMac, strings.TrimSpace(string(arpOut)))

	// Start ip monitor to capture link state events for this
	// veth pair. Output is logged on test completion.
	var monBuf bytes.Buffer
	// Wrapped via `ip netns exec root` so monitor is bound
	// to the bind-mounted root netns regardless of caller
	// thread state.
	monCmd := exec.Command("ip", "netns", "exec", rootNetnsName, "ip", "monitor", "link", "address", "route", "neigh")
	monCmd.Stdout = &monBuf
	monCmd.Stderr = &monBuf
	if err := monCmd.Start(); err != nil {
		t.Logf("ip monitor failed to start: %v", err)
	} else {
		t.Cleanup(func() {
			monCmd.Process.Kill()
			monCmd.Wait()
			// Filter output to only show events for our interfaces.
			for _, line := range strings.Split(monBuf.String(), "\n") {
				if strings.Contains(line, nameA) || strings.Contains(line, nameB) || strings.Contains(line, nsName) {
					t.Logf("[ip-monitor %s] %s", base, line)
				}
			}
		})
	}

	return TestVethPair{
		A: TestInterface{
			Name:    nameA,
			Ifindex: linkA.Attrs().Index,
		},
		B: TestInterface{
			Name: nameB,
		},
		Netns:      nsName,
		PingTarget: pingTarget,
	}
}

// pingMode selects ping behaviour for the private ping helper.
// The three Ping* public methods each pick one mode; the modes
// aren't combinatorial flags, they're discrete choices, so an
// enum reads better than a pair of bools at the helper's call
// sites.
type pingMode int

const (
	// pingDefensive re-verifies connectivity before the burst
	// (one extra pre-burst ping) and fails on any reply loss.
	// Default for non-exact-equality tests under heavy parallel
	// load -- the re-verify re-warms ARP that other tests'
	// cleanup may have evicted.
	pingDefensive pingMode = iota

	// pingExactCount skips the re-verify so the burst is
	// exactly N ICMP echo requests on A's ingress, and fails on
	// any reply loss. For exact-equality counter assertions.
	pingExactCount

	// pingExpectDrop skips the re-verify and tolerates 100%
	// reply loss. For chain-stops tests where an attached BPF
	// program drops packets at A's ingress (e.g. a multi-prog
	// XDP chain whose middle program returns XDP_DROP).
	pingExpectDrop
)

// Ping sends count ICMP echo requests from the veth pair's B
// interface (inside the test namespace) to A's IP address. This
// generates real ingress traffic on A, triggering any attached TC
// programs. Does a defensive re-verify before the burst, which adds
// one extra echo request to A's ingress -- non-exact tests use this
// form; tests that assert exact counts should use PingExact.
func (v TestVethPair) Ping(t *testing.T, count int) {
	t.Helper()
	v.ping(t, count, pingDefensive)
}

// PingExact is Ping without the pre-burst re-verify. Use for
// exact-equality counter assertions, where the extra ping inflates
// the count by one. Relies on the initial waitConnectivity at veth
// creation having warmed ARP; do not call across long pauses where
// concurrent cleanup elsewhere could disrupt link state.
func (v TestVethPair) PingExact(t *testing.T, count int) {
	t.Helper()
	v.ping(t, count, pingExactCount)
}

// PingExpectDrop fires count ICMP echo requests but tolerates
// 100% reply loss. Use when an attached BPF program is expected
// to drop packets at A's ingress (e.g. a multi-program XDP chain
// where the middle program returns XDP_DROP to terminate the
// chain). The kernel still sends the N requests from B; the
// counter at A's BPF program advances exactly N times even though
// the kernel ICMP responder never gets them.
func (v TestVethPair) PingExpectDrop(t *testing.T, count int) {
	t.Helper()
	v.ping(t, count, pingExpectDrop)
}

func (v TestVethPair) ping(t *testing.T, count int, mode pingMode) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pre-ping MAC/ifindex check for debugging.
	linkA, err := netlink.LinkByName(v.A.Name)
	if err != nil {
		t.Fatalf("pre-ping: cannot find %s: %v", v.A.Name, err)
	}
	t.Logf("pre-ping: A=%s ifindex=%d MAC=%s (expected ifindex=%d)",
		v.A.Name, linkA.Attrs().Index, linkA.Attrs().HardwareAddr, v.A.Ifindex)
	if linkA.Attrs().Index != v.A.Ifindex {
		t.Errorf("IFINDEX CHANGED: was %d at creation, now %d -- interface was recreated!",
			v.A.Ifindex, linkA.Attrs().Index)
	}

	if mode == pingDefensive {
		// Re-verify connectivity before the test burst. Under
		// heavy parallel load, concurrent veth cleanup from
		// other tests can disrupt link state. This re-
		// establishes ARP entries.
		waitConnectivity(t, v.Netns, v.PingTarget, 30*time.Second)
	}

	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", v.Netns,
		"ping", "-c", strconv.Itoa(count), "-i", "0.1", "-W", "1", v.PingTarget)
	out, err := cmd.CombinedOutput()
	if err != nil && mode != pingExpectDrop {
		v.dumpNetworkState(t, "ping-failure")
		t.Fatalf("ping failed: %v\n%s", err, out)
	}
}

// dumpNetworkState logs diagnostic information about the veth pair
// to help debug connectivity failures.
func (v TestVethPair) dumpNetworkState(t *testing.T, label string) {
	t.Helper()

	// Root namespace: interface A state, addresses, routes, ARP, TC filters.
	for _, args := range [][]string{
		{"ip", "link", "show", v.A.Name},
		{"ip", "addr", "show", v.A.Name},
		{"ip", "route", "show", "dev", v.A.Name},
		{"ip", "neigh", "show", "dev", v.A.Name},
		{"tc", "filter", "show", "dev", v.A.Name, "ingress"},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Logf("[%s root] %s: error: %v", label, strings.Join(args, " "), err)
		} else {
			t.Logf("[%s root] %s:\n%s", label, strings.Join(args, " "), out)
		}
	}

	// Test namespace: interface B state, addresses, routes, ARP.
	for _, args := range [][]string{
		{"ip", "netns", "exec", v.Netns, "ip", "link", "show", v.B.Name},
		{"ip", "netns", "exec", v.Netns, "ip", "addr", "show", v.B.Name},
		{"ip", "netns", "exec", v.Netns, "ip", "route", "show", "dev", v.B.Name},
		{"ip", "netns", "exec", v.Netns, "ip", "neigh", "show", "dev", v.B.Name},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Logf("[%s ns %s] %s: error: %v", label, v.Netns, strings.Join(args, " "), err)
		} else {
			t.Logf("[%s ns %s] %s:\n%s", label, v.Netns, strings.Join(args, " "), out)
		}
	}
}

// waitLinkOperUp polls until the named interface reports OperUp. Pass
// a nil handle to query the root network namespace.
func waitLinkOperUp(t *testing.T, h *netlink.Handle, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var link netlink.Link
		var err error
		if h != nil {
			link, err = h.LinkByName(name)
		} else {
			link, err = netlink.LinkByName(name)
		}
		if err == nil && link.Attrs().OperState == netlink.OperUp {
			return
		}
		if time.Now().After(deadline) {
			state := "unknown"
			if err == nil {
				state = link.Attrs().OperState.String()
			}
			t.Fatalf("interface %s did not reach OperUp within %v (current state: %s)", name, timeout, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitConnectivity sends single pings with retries until one
// succeeds, proving the veth path is ready for traffic.
func waitConnectivity(t *testing.T, nsName, target string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		cmd := exec.CommandContext(ctx, "ip", "netns", "exec", nsName,
			"ping", "-c", "1", "-W", "1", target)
		err := cmd.Run()
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("veth pair connectivity not established within %v", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// disableIPv6 disables IPv6 on an interface in the root namespace
// via sysctl. This must be called before LinkSetUp to prevent the
// kernel from generating an IPv6 link-local address.
func disableIPv6(t *testing.T, ifaceName string) {
	t.Helper()
	path := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", ifaceName)
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		t.Fatalf("failed to disable IPv6 on %s: %v", ifaceName, err)
	}
}

// disableIPv6InNs disables IPv6 on an interface inside a named
// network namespace via ip netns exec.
func disableIPv6InNs(t *testing.T, nsName, ifaceName string) {
	t.Helper()
	sysctl := fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", ifaceName)
	out, err := exec.Command("ip", "netns", "exec", nsName, "sysctl", "-w", sysctl).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to disable IPv6 on %s in ns %s: %v\n%s", ifaceName, nsName, err, out)
	}
}

const staleTestDirPrefix = "bpfman-e2e-"

// staleTestIfaceRe matches interface and namespace names generated
// by uniqueTestName: "B", 12 hex characters, "N", optionally with
// an "a" or "b" veth suffix.
var staleTestIfaceRe = regexp.MustCompile(`^B[0-9a-f]{12}N[ab]?$`)

// cleanupStaleTestArtifacts removes leftover test interfaces,
// namespaces, and directories from previous runs.
func cleanupStaleTestDirs() error {
	if err := cleanupStaleInterfaces(); err != nil {
		return err
	}

	tempDir := os.TempDir()
	pattern := filepath.Join(tempDir, staleTestDirPrefix+"*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob %s: %w", pattern, err)
	}

	for _, path := range matches {
		// Safety: verify path is under tempDir and has expected prefix.
		if err := validateStaleTestDir(path, tempDir); err != nil {
			return fmt.Errorf("refusing to remove %s: %w", path, err)
		}

		// Defensive: only stale TEST DIRECTORIES are in scope here.
		// Files matching the glob (notably the suite-lock file the
		// e2eSuiteLockPath now lives outside the prefix on principle,
		// but anything else that ends up in /tmp under the prefix
		// without being a directory is not ours to remove) are left
		// alone.
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() {
			continue
		}

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

		// Attempt to unmount bpffs; ignore errors as it may already
		// be unmounted or never mounted.
		layout, err := fs.New(path)
		if err == nil {
			unmount(layout.BPFFSMountPoint())
		}

		// Remove the entire test directory
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	return nil
}

// validateStaleTestDir ensures path is safe to remove.
func validateStaleTestDir(path, tempDir string) error {
	// Must be absolute
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path is not absolute")
	}

	// Must be under tempDir
	cleanPath := filepath.Clean(path)
	cleanTempDir := filepath.Clean(tempDir)
	if !strings.HasPrefix(cleanPath, cleanTempDir+string(filepath.Separator)) {
		return fmt.Errorf("path %q is not under temp dir %q", cleanPath, cleanTempDir)
	}

	// Must have the expected prefix
	base := filepath.Base(cleanPath)
	if !strings.HasPrefix(base, staleTestDirPrefix) {
		return fmt.Errorf("path %q does not have prefix %q", base, staleTestDirPrefix)
	}

	// Must not be a top-level directory (sanity check)
	if cleanPath == "/" || strings.Count(cleanPath, string(filepath.Separator)) < 2 {
		return fmt.Errorf("path %q is too short", cleanPath)
	}

	return nil
}

// cleanupStaleInterfaces removes leftover test interfaces and
// namespaces from crashed runs. Names are matched by the specific
// pattern generated by uniqueTestName (b + 13 hex chars).
func cleanupStaleInterfaces() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list interfaces: %w", err)
	}
	for _, link := range links {
		if staleTestIfaceRe.MatchString(link.Attrs().Name) {
			netlink.LinkDel(link)
		}
	}

	entries, err := os.ReadDir("/run/netns")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read /run/netns: %w", err)
	}
	for _, entry := range entries {
		if staleTestIfaceRe.MatchString(entry.Name()) {
			netns.DeleteNamed(entry.Name())
		}
	}

	return nil
}

// readPerCPUCounter loads a pinned BPF_MAP_TYPE_PERCPU_ARRAY map and
// returns the sum of the uint64 values across all CPUs for the given
// key. This is used to verify that BPF programs with simple counter
// maps have actually executed.
func readPerCPUCounter(t *testing.T, mapPinPath string, key uint32) uint64 {
	t.Helper()

	m, err := ciliumebpf.LoadPinnedMap(mapPinPath, nil)
	require.NoError(t, err, "load pinned map at %s", mapPinPath)
	defer m.Close()

	var perCPU []uint64
	err = m.Lookup(key, &perCPU)
	require.NoError(t, err, "lookup key %d in map at %s", key, mapPinPath)

	var total uint64
	for _, v := range perCPU {
		total += v
	}
	return total
}

// mapIDByName resolves a program's kernel map by name and returns its
// kernel-assigned ID. Useful for tests that need to read a specific
// named counter map without depending on the kernel's MapIDs ordering
// (which is not contractually stable when a program owns multiple
// maps).
func mapIDByName(t *testing.T, prog bpfman.Program, name string) kernel.MapID {
	t.Helper()
	require.NotNil(t, prog.Status.Kernel, "program has no kernel info")
	for _, m := range prog.Status.Maps {
		if m.Name == name {
			return m.ID
		}
	}
	t.Fatalf("program %d has no map named %q (maps: %+v)", prog.Status.Kernel.ID, name, prog.Status.Maps)
	return 0
}

// readArrayCounterByID opens a BPF_MAP_TYPE_ARRAY map by its
// kernel-assigned ID and returns the uint64 value at key 0. Used by
// tests where the BPF program filters in-kernel and writes to a
// single counter slot.
func readArrayCounterByID(t *testing.T, mapID kernel.MapID) uint64 {
	t.Helper()

	m, err := ciliumebpf.NewMapFromID(ciliumebpf.MapID(mapID))
	require.NoError(t, err, "open map ID %d", mapID)
	defer m.Close()

	var key uint32 = 0
	var val uint64
	err = m.Lookup(key, &val)
	require.NoError(t, err, "lookup key 0 in map ID %d", mapID)
	return val
}

// assertCounterQuiet drives `fire` and asserts that the named
// counter map on `prog` does not advance. Use after Detach to
// prove that detach actually stopped the BPF program firing, not
// just removed bpfman's link record. The original perf-link
// detach bug (commit 1459c0b) would surface here as a non-zero
// delta even though `events * weight == count` passed pre-detach,
// because the singleton tests never fire post-detach traffic by
// themselves. Applies to every program type -- the bug is
// perf-link specific but the property "detach stopped it" is
// worth pinning down uniformly.
func assertCounterQuiet(t *testing.T, prog bpfman.Program, mapName string, fire func()) {
	t.Helper()
	mapID := mapIDByName(t, prog, mapName)
	before := readArrayCounterByID(t, mapID)
	fire()
	after := readArrayCounterByID(t, mapID)
	requireCounterEqual(t, before, after,
		"counter %q should be quiet after detach", mapName)
}

// requireCounterEqual asserts want == got and, on mismatch, prints
// both sides plus the delta in decimal. testify's require.Equal
// renders uint64 mismatches through spew which prefixes them with
// 0x; the surrounding test diagnostics (events, weights, before/
// after counts) are decimal, so the hex/decimal split makes failure
// triage harder than it needs to be. Using a plain t.Fatalf keeps
// every number in one base.
func requireCounterEqual(t *testing.T, want, got uint64, format string, args ...any) {
	t.Helper()
	if want == got {
		return
	}
	prefix := fmt.Sprintf(format, args...)
	t.Fatalf("%s: want=%d got=%d delta=%d", prefix, want, got, int64(got)-int64(want))
}

// ActiveResult is what waitProgramActive reports back. Mirror of
// QuiescenceResult on the attach side.
type ActiveResult struct {
	// Probes is the total number of single-event firings driven
	// before the just-attached program counted its first event.
	// Each probe still flows through the kernel hook, so siblings
	// of the just-attached program (which are already attached)
	// count every probe; callers must add Probes to each sibling's
	// expected counter to keep assertions exact.
	Probes int
	// EventsCounted is how many probes the just-attached program
	// counted. Always 1 on success (the first observed increment is
	// the exit condition); included for symmetry with
	// QuiescenceResult and so callers add it uniformly to expected.
	EventsCounted int
	// LostProbes is Probes - EventsCounted, the number of probes
	// fired before the program became active. Telemetry: 0 on
	// synchronous attach, > 0 on racy/contended attach paths
	// (notably bpf_trampoline rebuild for fentry/fexit when
	// concurrent attaches contend on the same target function).
	LostProbes int
	// Latency is the wall-clock time from first probe to first
	// observed increment.
	Latency time.Duration
}

// AttachActiveProbe configures waitProgramActive.
type AttachActiveProbe struct {
	// AttachedMap is the counter map of the just-attached program.
	AttachedMap    kernel.MapID
	AttachedWeight uint64
	// FireOne drives exactly one workload event that should hit the
	// program's hook now that it's attached.
	FireOne func()
	// Deadline is the upper bound on the entire wait. Default 500ms.
	Deadline time.Duration
}

// waitProgramActive fires single workload events one at a time after
// env.Attach returns and waits for the just-attached program's counter
// to register its first event -- proving the attach has taken effect
// kernel-side. This is the symmetric attach-side counterpart of
// waitDetachQuiescent.
//
// Why it's needed: for fentry/fexit (and to a lesser extent any
// multi-program-on-one-hook attach), env.Attach can return before the
// kernel-side machinery is fully active. For fentry/fexit specifically
// the attach goes through bpf_trampoline_update -- a JITed image is
// rebuilt and text_poke_bp swaps the function-entry patch. Under
// concurrent attach/detach contention on the same target function (the
// e2e suite has many tests sharing do_unlinkat) the rebuild can be
// slow enough that a workload event fired immediately after Attach
// lands on the OLD image, where our program isn't yet present, and the
// event is lost from our counter. Same class of bug as the detach
// deferral, symmetrically placed.
//
// kprobe/uprobe/tracepoint attach is essentially synchronous (a single
// rcu_assign_pointer publish onto tp_event->prog_array), so this
// helper typically returns on the first probe for those types -- but
// using it uniformly costs ~one extra probe per attach and gains
// uniform telemetry plus defence in depth.
func waitProgramActive(t *testing.T, p AttachActiveProbe) ActiveResult {
	t.Helper()
	deadline := p.Deadline
	if deadline <= 0 {
		deadline = 500 * time.Millisecond
	}

	start := time.Now()
	initial := readArrayCounterByID(t, p.AttachedMap)
	probes := 0

	for time.Since(start) < deadline {
		p.FireOne()
		probes++
		now := readArrayCounterByID(t, p.AttachedMap)
		if now != initial {
			eventsCounted := int((now - initial) / p.AttachedWeight)
			return ActiveResult{
				Probes:        probes,
				EventsCounted: eventsCounted,
				LostProbes:    probes - eventsCounted,
				Latency:       time.Since(start),
			}
		}
	}
	t.Fatalf("waitProgramActive: counter never incremented after %d probes in %s -- attach not effective",
		probes, time.Since(start))
	return ActiveResult{}
}

// QuiescenceResult is what waitDetachQuiescent reports back so callers
// can both check that the barrier was reached and fold the probe events
// into their expected counts.
type QuiescenceResult struct {
	// Probes is the total number of single-event workload firings
	// driven during the wait. Each probe still flows through the
	// kernel hook, so siblings of the just-detached program (which
	// remain attached) WILL count it; callers must add Probes to
	// the expected counter for any sibling that is still attached.
	Probes int
	// EventsCounted is the number of probes the detached program
	// itself counted before quiescence. Telemetry: under perfect
	// synchronous detach this is 0; values > 0 measure the kernel
	// deferral (RCU GP + workqueue) in events.
	EventsCounted int
	// Latency is the wall-clock time from first probe to declaring
	// quiescence. Telemetry only.
	Latency time.Duration
}

// QuiescenceProbe configures waitDetachQuiescent.
type QuiescenceProbe struct {
	// DetachedMap is the counter map of the just-detached program.
	// The barrier waits until this counter has been stable across
	// StableProbes consecutive probes.
	DetachedMap    kernel.MapID
	DetachedWeight uint64

	// ControlMap, if non-zero, is the counter map of a still-
	// attached sibling on the same hook. After the barrier, the
	// helper asserts ControlMap advanced by exactly
	// Probes*ControlWeight, catching the "workload is broken so the
	// hook never fires" false-negative case (where a never-firing
	// counter looks identical to a successfully detached one). Pass
	// 0 to skip -- typically only for singleton tests where the
	// pre-detach pass already proved the workload reaches the hook.
	ControlMap    kernel.MapID
	ControlWeight uint64

	// FireOne drives exactly one workload event that would hit the
	// program's hook if it were still attached.
	FireOne func()

	// StableProbes is how many consecutive non-moving counter reads
	// declare quiescence. Default 3.
	StableProbes int
	// Deadline is the upper bound on the entire wait. Default 500ms.
	Deadline time.Duration
}

// waitDetachQuiescent fires single workload events one at a time and
// reads the just-detached program's counter after each, returning when
// the counter has been stable for StableProbes consecutive probes
// (the detach has demonstrably taken effect kernel-side) or fails the
// test if the deadline expires while the counter is still moving.
//
// This is the only correct primitive for proving "this BPF program
// stopped firing" on bpf-perf-link types (kprobe, kretprobe, uprobe,
// uretprobe, tracepoint, fentry, fexit). Per
// docs/PERF-LINK-DETACH-IS-ASYNC.md the kernel exposes no synchronous
// teardown for those: pin removal is RCU-deferred via the bpffs
// inode's free_inode super_op, and the FD close path can only free
// when it drops the LAST ref, which it never does while the pin is
// alive. The deferred bpf_link_free is what eventually runs
// perf_event_detach_bpf_prog and removes the program from the
// trace_event's prog_array. Any fixed sleep is wrong on slow runners
// eventually; polling the program's own counter is the lagging
// observable that adapts to actual kernel timing.
//
// FireOne must drive exactly one workload event that would hit the
// program's hook if it were still attached. For tests that share a
// hook between several programs, every probe also lands on the still-
// attached siblings -- the caller adds the returned Probes to each
// sibling's expected counter to keep assertions exact.
//
// If ControlMap is set, the helper sanity-checks that ControlMap
// advanced by exactly Probes*ControlWeight after the barrier; this
// catches the false-negative where the workload no longer reaches the
// hook (counter never increments, looks like a clean detach).
func waitDetachQuiescent(t *testing.T, p QuiescenceProbe) QuiescenceResult {
	t.Helper()
	stableProbes := p.StableProbes
	if stableProbes <= 0 {
		stableProbes = 3
	}
	deadline := p.Deadline
	if deadline <= 0 {
		deadline = 500 * time.Millisecond
	}

	start := time.Now()
	initial := readArrayCounterByID(t, p.DetachedMap)
	var controlInitial uint64
	if p.ControlMap != 0 {
		controlInitial = readArrayCounterByID(t, p.ControlMap)
	}
	last := initial
	stable := 0
	probes := 0

	for time.Since(start) < deadline {
		p.FireOne()
		probes++
		now := readArrayCounterByID(t, p.DetachedMap)
		if now == last {
			stable++
			if stable >= stableProbes {
				result := QuiescenceResult{
					Probes:        probes,
					EventsCounted: int((now - initial) / p.DetachedWeight),
					Latency:       time.Since(start),
				}
				if p.ControlMap != 0 {
					controlDelta := readArrayCounterByID(t, p.ControlMap) - controlInitial
					expected := uint64(probes) * p.ControlWeight
					require.Equal(t, expected, controlDelta,
						"control sibling counter delta should equal probes(%d) * weight(%d) = %d after barrier; got %d. Workload likely not hitting the hook -- 'quiescence' would be a false positive",
						probes, p.ControlWeight, expected, controlDelta)
				}
				return result
			}
		} else {
			stable = 0
			last = now
		}
	}
	t.Fatalf("waitDetachQuiescent: counter still moving after %s and %d probes (delta=%d, weight=%d)",
		deadline, probes, last-initial, p.DetachedWeight)
	return QuiescenceResult{}
}

// uniqueWeights returns n distinct random uint64 weights derived
// from a fresh test-scoped seed. Used by tests that pass per-program
// weights as global data so the BPF program's counter is a verifiable
// function of (events × weight) rather than a bare event tally.
//
// Weights are forced to differ from each other so that a "wrong map"
// or "swapped indices" bug is detectable. The high bit is cleared so
// that events × weight cannot overflow for realistic event counts.
func uniqueWeights(t *testing.T, n int) []uint64 {
	t.Helper()
	r := newTestRand(t)
	seen := make(map[uint64]struct{}, n)
	out := make([]uint64, 0, n)
	for len(out) < n {
		w := r.Uint64() & ((1 << 40) - 1)
		if w == 0 {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

// newTestRand returns a math/rand source seeded uniquely per test.
// The seed is logged so a failing test is reproducible when the
// random weights matter.
func newTestRand(t *testing.T) *rand.Rand {
	t.Helper()
	var seedBytes [8]byte
	_, err := cryptorand.Read(seedBytes[:])
	require.NoError(t, err, "read crypto/rand seed")
	seed := int64(binary.LittleEndian.Uint64(seedBytes[:]))
	t.Logf("test rand seed: %d", seed)
	return rand.New(rand.NewSource(seed))
}

// uint32LE encodes v as 4 little-endian bytes, the form bpfman
// global-data injection expects for `volatile const __u32`.
func uint32LE(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}

// uint64LE encodes v as 8 little-endian bytes, the form bpfman
// global-data injection expects for `volatile const __u64`.
func uint64LE(v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return b[:]
}

// readHashCounterByID opens a BPF_MAP_TYPE_HASH map by its
// kernel-assigned ID and returns the uint64 value at the given key,
// or 0 if the key is not present. Used by tests that count events
// per-PID in a hash map keyed by bpf_get_current_pid_tgid() >> 32,
// so they can assert exact counts without being polluted by ambient
// system activity on the same attach surface.
func readHashCounterByID(t *testing.T, mapID kernel.MapID, key uint32) uint64 {
	t.Helper()

	m, err := ciliumebpf.NewMapFromID(ciliumebpf.MapID(mapID))
	require.NoError(t, err, "open map ID %d", mapID)
	defer m.Close()

	var val uint64
	err = m.Lookup(key, &val)
	if err != nil {
		if errors.Is(err, ciliumebpf.ErrKeyNotExist) {
			return 0
		}
		t.Fatalf("lookup key %d in map ID %d: %v", key, mapID, err)
	}
	return val
}

// readPerCPUCounterByID opens a BPF_MAP_TYPE_PERCPU_ARRAY map by its
// kernel-assigned ID and returns the sum of uint64 values across all
// CPUs for the given key. Used by tests that load programs from
// LIBBPF_PIN_NONE objects, where the maps are not on the filesystem
// and must be resolved via Program.Status.Kernel.MapIDs.
func readPerCPUCounterByID(t *testing.T, mapID kernel.MapID, key uint32) uint64 {
	t.Helper()

	m, err := ciliumebpf.NewMapFromID(ciliumebpf.MapID(mapID))
	require.NoError(t, err, "open map ID %d", mapID)
	defer m.Close()

	var perCPU []uint64
	err = m.Lookup(key, &perCPU)
	require.NoError(t, err, "lookup key %d in map ID %d", key, mapID)

	var total uint64
	for _, v := range perCPU {
		total += v
	}
	return total
}

// tLogHandler is an slog.Handler that emits records via t.Logf
// at the configured minimum level. Go's testing framework
// buffers t.Logf output per test and shows it only when the
// test fails (or is run with -v), so wiring bpfman's logger
// through it surfaces diagnostic lines exactly when they help
// without spamming successful runs. Each NewTestEnv call wires
// its own handler bound to its own *testing.T so logs are
// attributed to the right test even under -test.parallel.
type tLogHandler struct {
	t     *testing.T
	level slog.Leveler
	attrs []slog.Attr
	group string
}

func newTLogHandler(t *testing.T, level slog.Level) *tLogHandler {
	return &tLogHandler{t: t, level: level}
}

func (h *tLogHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level.Level()
}

func (h *tLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.t.Helper()
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s", r.Level, r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	h.t.Logf("%s", b.String())
	return nil
}

func (h *tLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *tLogHandler) WithGroup(name string) slog.Handler {
	nh := *h
	if h.group != "" {
		nh.group = h.group + "." + name
	} else {
		nh.group = name
	}
	return &nh
}

// doUnlinkAtHookMu serialises every test that attaches anything --
// fentry, fexit, kprobe, or kretprobe -- to do_unlinkat. All four
// attach types ride on the kernel's __fentry__ patch site for the
// target function: ftrace owns that site and re-merges its callback
// list (kprobe handlers + the BPF trampoline + ...) on every
// attach and detach. Under concurrent rebuilds there is a window
// during which an in-flight call lands in the gap and the BPF
// trampoline misses an event, dropping it from our exact-equality
// counters.
//
// Empirically a narrower mutex covering only fentry/fexit was
// insufficient: a parallel kprobe attach on do_unlinkat can still
// kick the ftrace rebuild and drop one of our quiescence probes
// (the off-by-one mfx_a control-sibling failure).
//
// Holding this mutex from before the first Attach until the test
// ends keeps only one do_unlinkat-attaching test active at a time.
// Tests that share the hook with non-suite attachers (system
// observability tools, an interactive bpftrace) remain vulnerable;
// the long-term fix is the private kmod design in
// docs/HERMETIC-FENTRY-FEXIT-KMOD.md.
var doUnlinkAtHookMu sync.Mutex

// lockDoUnlinkAtHook blocks until the suite-wide do_unlinkat hook
// mutex is acquired, then registers a t.Cleanup that releases it.
// Call this at the top of any test that attaches a fentry, fexit,
// kprobe, or kretprobe program to do_unlinkat, after the Require*
// gates.
func lockDoUnlinkAtHook(t *testing.T) {
	t.Helper()
	doUnlinkAtHookMu.Lock()
	t.Cleanup(doUnlinkAtHookMu.Unlock)
}

// kmodTargetsRoot is the debugfs directory the bpfman_e2e_targets
// kernel module exposes once loaded. Each entry trigger_NNN under
// it, when written, invokes bpfman_e2e_target_N once.
const kmodTargetsRoot = "/sys/kernel/debug/bpfman_e2e"

// kmodSlotPoolSize is the number of slots the bpfman_e2e_targets
// module exports. Must match BPFMAN_E2E_NUM_SLOTS in the module
// source.
const kmodSlotPoolSize = 32

var (
	kmodSlotPool     chan int
	kmodSlotPoolOnce sync.Once
)

func initKmodSlotPool() {
	kmodSlotPool = make(chan int, kmodSlotPoolSize)
	for i := 0; i < kmodSlotPoolSize; i++ {
		kmodSlotPool <- i
	}
}

// KmodSlot identifies one slot in the bpfman_e2e_targets module.
// Each fentry/fexit test that uses a slot owns it for its lifetime;
// no two tests share a slot, so attach/detach against the slot's
// function does not contend with sibling tests on the trampoline.
type KmodSlot struct {
	// Index is the slot number, 0 <= Index < kmodSlotPoolSize.
	Index int
	// Func is the kernel-resolvable name of the function this
	// slot exports. Use as bpfman ProgramSpec.AttachFunc when
	// loading a fentry/fexit program against this slot.
	Func string
	// TriggerPath is the debugfs file whose write(2) invokes
	// Func once. Test code uses this to drive events.
	TriggerPath string
}

// RequireKmodTargets skips the test if the bpfman_e2e_targets
// kernel module is not loaded. The module must be loaded once
// per host (typically via the NixOS module that ships its .ko)
// before any kmod-targeting test can run.
func RequireKmodTargets(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(kmodTargetsRoot); err != nil {
		t.Skipf("bpfman_e2e_targets kmod not available at %s: %v (load the module first)",
			kmodTargetsRoot, err)
	}
}

// acquireKmodSlot reserves an unused slot from the
// bpfman_e2e_targets module and registers a t.Cleanup that
// returns the slot to the pool when the test ends. Blocks if
// every slot is in use; in practice the pool size exceeds the
// concurrent count of kmod-targeting tests by a wide margin so
// this is effectively non-blocking.
func acquireKmodSlot(t *testing.T) KmodSlot {
	t.Helper()
	kmodSlotPoolOnce.Do(initKmodSlotPool)

	idx := <-kmodSlotPool
	t.Cleanup(func() { kmodSlotPool <- idx })

	return KmodSlot{
		Index:       idx,
		Func:        fmt.Sprintf("bpfman_e2e_target_%d", idx),
		TriggerPath: fmt.Sprintf("%s/trigger_%03d", kmodTargetsRoot, idx),
	}
}

// Fire issues n write(2) calls to the slot's trigger file, each
// invoking the slot's function once. Buffer contents are
// ignored by the kernel module; callers control event count by
// the number of writes.
func (s KmodSlot) Fire(t *testing.T, n int) {
	t.Helper()
	f, err := os.OpenFile(s.TriggerPath, os.O_WRONLY, 0)
	require.NoError(t, err, "open trigger %s", s.TriggerPath)
	defer f.Close()
	for i := 0; i < n; i++ {
		if _, err := f.Write([]byte{0}); err != nil {
			t.Fatalf("write trigger %s (event %d/%d): %v", s.TriggerPath, i+1, n, err)
		}
	}
}
