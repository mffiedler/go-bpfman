//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
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

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/frobware/go-bpfman"
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

// TestEnv provides an isolated test environment for e2e tests.
// Each test gets a fully isolated environment with unique directories,
// database, and socket, enabling t.Parallel() across all tests.
type TestEnv struct {
	T           *testing.T
	Layout      fs.Layout
	Manager     *manager.Manager
	ImagePuller platform.ImagePuller
	logger      *slog.Logger
	baseDir     string // parent directory containing layout and cache
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
func (e *TestEnv) runWithLock(ctx context.Context, fn func(context.Context) error) error {
	return lock.Run(ctx, e.Layout.LockPath(), func(ctx context.Context, _ lock.WriterScope) error {
		return fn(ctx)
	})
}

// runWithLockAndScope executes a function under the writer lock with scope access.
func (e *TestEnv) runWithLockAndScope(ctx context.Context, fn func(context.Context, lock.WriterScope) error) error {
	return lock.Run(ctx, e.Layout.LockPath(), fn)
}

// LoadImage loads BPF programs from an OCI image.
func (e *TestEnv) LoadImage(ctx context.Context, ref platform.ImageRef, programs []manager.ProgramSpec, opts manager.LoadOpts) ([]bpfman.Program, error) {
	var result []bpfman.Program
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		var loadErr error
		result, loadErr = e.Manager.Load(ctx, manager.LoadSource{
			Image: &ref,
		}, programs, opts)
		return loadErr
	})
	return result, err
}

// LoadFile loads BPF programs from a local object file.
func (e *TestEnv) LoadFile(ctx context.Context, filePath string, programs []manager.ProgramSpec, opts manager.LoadOpts) ([]bpfman.Program, error) {
	var result []bpfman.Program
	err := e.runWithLock(ctx, func(ctx context.Context) error {
		var loadErr error
		result, loadErr = e.Manager.Load(ctx, manager.LoadSource{
			FilePath: filePath,
		}, programs, opts)
		return loadErr
	})
	return result, err
}

// Unload unloads a BPF program.
func (e *TestEnv) Unload(ctx context.Context, programID kernel.ProgramID) error {
	return e.runWithLock(ctx, func(ctx context.Context) error {
		return e.Manager.Unload(ctx, programID)
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

// Attach attaches a program using the given spec.  The lock scope is
// acquired automatically and passed to the manager.
func (e *TestEnv) Attach(ctx context.Context, spec bpfman.AttachSpec) (bpfman.LinkRecord, error) {
	var result bpfman.Link
	err := e.runWithLockAndScope(ctx, func(ctx context.Context, scope lock.WriterScope) error {
		link, attachErr := e.Manager.Attach(ctx, scope, spec)
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
	return e.runWithLock(ctx, func(ctx context.Context) error {
		return e.Manager.Detach(ctx, linkID)
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
// the root namespace's routing table.
func vethAddrs() (addrA, addrB, pingTarget string) {
	return vethAddrsForIndex(vethAddrSeq.Add(1))
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
	ipA, ipB, pingTarget := vethAddrs()

	// Configure A in root namespace with a peer route to B.
	linkA, err := netlink.LinkByName(nameA)
	if err != nil {
		t.Fatalf("failed to find interface %s: %v", nameA, err)
	}
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

	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", v.Netns,
		"ping", "-c", strconv.Itoa(count), "-i", "0.1", "-W", "1", v.PingTarget)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ping failed: %v\n%s", err, out)
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
