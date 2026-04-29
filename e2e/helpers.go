//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	iofs "io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/ebpf"
	"github.com/frobware/go-bpfman/platform/image/oci"
	"github.com/frobware/go-bpfman/platform/image/verify"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/logging"
	"github.com/frobware/go-bpfman/manager"
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

// TestEnv provides an isolated test environment for e2e tests.
// Each test gets a fully isolated environment with unique directories,
// database, and socket, enabling t.Parallel() across all tests.
type TestEnv struct {
	T           *testing.T
	Layout      fs.Layout
	Manager     *manager.Manager
	ImagePuller platform.ImagePuller
	logger      *slog.Logger
	baseDir     string // parent directory containing layout, cache, testdata
	closeEnv    func() error
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
func (e *TestEnv) cleanup() {
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

// runWithLock executes a function under the writer lock.
func (e *TestEnv) runWithLock(ctx context.Context, fn func(context.Context, lock.WriterScope) error) error {
	return lock.Run(ctx, e.Layout.LockPath(), fn)
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
	return e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		return e.Manager.Unload(ctx, writeLock, programID)
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
func (e *TestEnv) Get(ctx context.Context, programID kernel.ProgramID) (bpfman.Program, error) {
	return e.Manager.Get(ctx, programID)
}

// Attach attaches a program using the given spec.  The writer lock is
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
	record, err := e.Manager.GetLink(ctx, result.Record.ID)
	if err != nil {
		return bpfman.LinkRecord{ID: result.Record.ID}, nil
	}
	return record, nil
}

// Detach detaches a link.
func (e *TestEnv) Detach(ctx context.Context, linkID kernel.LinkID) error {
	return e.runWithLock(ctx, func(ctx context.Context, writeLock lock.WriterScope) error {
		return e.Manager.Detach(ctx, writeLock, linkID)
	})
}

// ListLinks returns all managed links.
func (e *TestEnv) ListLinks(ctx context.Context) ([]bpfman.LinkRecord, error) {
	return e.Manager.ListLinks(ctx)
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

	out, err := exec.CommandContext(ctx, "tc", "filter", "show", "dev", iface, direction).CombinedOutput()
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

var vethAddrSeq atomic.Uint32

// vethAddrs allocates a unique pair of /32 addresses from the RFC
// 5737 TEST-NET-2 range (198.51.100.0/24) for a veth pair. Each
// call returns addresses that won't conflict with other pairs in
// the root namespace's routing table. The returned pairIndex is
// the allocation index, which callers must use for any derived
// identifiers (e.g., MAC addresses) to avoid races between the
// atomic increment and a separate Load.
func vethAddrs() (addrA, addrB, pingTarget string, pairIndex uint32) {
	idx := vethAddrSeq.Add(1)
	addrA, addrB, pingTarget = vethAddrsForIndex(idx)
	return addrA, addrB, pingTarget, idx
}

// vethAddrsForIndex returns unique /32 addresses for the given pair
// index. The index must be in [1, 127].
func vethAddrsForIndex(n uint32) (addrA, addrB, pingTarget string) {
	if n < 1 || n > 127 {
		panic(fmt.Sprintf("veth pair index %d out of range [1, 127]", n))
	}
	hostA := n*2 + 1 // 3, 5, 7, ...
	hostB := n * 2    // 2, 4, 6, ...
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

	// Create named network namespace. NewNamed switches the calling
	// thread's netns, so we must lock the OS thread and restore.
	runtime.LockOSThread()
	origNs, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("failed to get current network namespace: %v", err)
	}
	newNs, err := netns.NewNamed(nsName)
	if err != nil {
		origNs.Close()
		runtime.UnlockOSThread()
		t.Fatalf("failed to create network namespace %s: %v", nsName, err)
	}
	newNs.Close()
	if err := netns.Set(origNs); err != nil {
		origNs.Close()
		runtime.UnlockOSThread()
		t.Fatalf("failed to restore network namespace: %v", err)
	}
	origNs.Close()
	runtime.UnlockOSThread()

	t.Cleanup(func() {
		netns.DeleteNamed(nsName)
	})

	t.Logf("creating veth pair %s/%s in namespace %s", nameA, nameB, nsName)

	// Create veth pair in root namespace.
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: nameA, TxQLen: 1000},
		PeerName:  nameB,
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

	// Allocate unique /32 addresses from TEST-NET-2 so that
	// multiple veth pairs in the root namespace never conflict.
	ipA, ipB, pingTarget, pairIdx := vethAddrs()

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
	// index returned by vethAddrs (ensuring uniqueness within a
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
	// Switching to explicit MACs reduced the failure rate from
	// ~40% to ~5% but did not eliminate it. Disabling IPv6 on
	// both veth ends (see disableIPv6/disableIPv6InNs calls
	// below) eliminated the remaining failures entirely (50/50
	// under stress).
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
	// When IPv6 is disabled, addrconf_notify returns early
	// without processing the event, breaking the chain. We did
	// not identify the exact kernel function that rewrites the
	// MAC, but the empirical evidence is unambiguous: disabling
	// IPv6 prevents it. The tests only need IPv4, so this has
	// no functional impact. Observed on kernel 6.12.74.
	pid := os.Getpid()
	macA, _ := net.ParseMAC(fmt.Sprintf("02:%02x:%02x:00:%02x:01", (pid>>8)&0xff, pid&0xff, pairIdx))
	macB, _ := net.ParseMAC(fmt.Sprintf("02:%02x:%02x:00:%02x:02", (pid>>8)&0xff, pid&0xff, pairIdx))
	if err := netlink.LinkSetHardwareAddr(linkA, macA); err != nil {
		t.Fatalf("failed to set MAC on %s: %v", nameA, err)
	}

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
	if err := nlh.LinkSetHardwareAddr(nsLinkB, macB); err != nil {
		t.Fatalf("failed to set MAC on %s: %v", nameB, err)
	}

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
	monCmd := exec.Command("ip", "monitor", "link", "address", "route", "neigh")
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

// Ping sends count ICMP echo requests from the veth pair's B
// interface (inside the test namespace) to A's IP address. This
// generates real ingress traffic on A, triggering any attached TC
// programs.
func (v TestVethPair) Ping(t *testing.T, count int) {
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

	// Re-verify connectivity before the test burst. Under heavy
	// parallel load, concurrent veth cleanup from other tests
	// can disrupt link state. This re-establishes ARP entries.
	waitConnectivity(t, v.Netns, v.PingTarget, 30*time.Second)

	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", v.Netns,
		"ping", "-c", strconv.Itoa(count), "-i", "0.1", "-W", "1", v.PingTarget)
	out, err := cmd.CombinedOutput()
	if err != nil {
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
		// Safety: verify path is under tempDir and has expected prefix
		if err := validateStaleTestDir(path, tempDir); err != nil {
			return fmt.Errorf("refusing to remove %s: %w", path, err)
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
