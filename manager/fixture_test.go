package manager_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/bpfmanfs/runtime"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store/sqlite"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
)

// testLogger returns a logger for tests. By default it discards all output.
// Set BPFMAN_TEST_VERBOSE=1 to enable logging.
func testLogger() *slog.Logger {
	if os.Getenv("BPFMAN_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testFixture provides access to all components for verification.
type testFixture struct {
	Manager       *manager.Manager
	Kernel        *fakeKernel
	Discoverer    *fakeDiscoverer
	Store         interpreter.Store
	Layout        bpfmanfs.FSLayout
	t             *testing.T
	bytecodeDir   string            // temp dir for dummy bytecode files
	bytecodeFiles map[string]string // name -> path cache
}

// newTestFixture creates a complete test fixture with accessible components.
func newTestFixture(t *testing.T) *testFixture {
	return newTestFixtureWithDiscoverer(t, nil)
}

// newTestFixtureWithDiscoverer creates a test fixture with a custom discoverer.
func newTestFixtureWithDiscoverer(t *testing.T, discoverer *fakeDiscoverer) *testFixture {
	t.Helper()
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	t.Cleanup(func() { store.Close() })
	layout, err := bpfmanfs.New(filepath.Join(t.TempDir(), "bpfman"))
	require.NoError(t, err, "failed to create fs layout")
	kernel := newFakeKernel()
	if discoverer == nil {
		discoverer = newFakeDiscoverer()
	}

	// Centralised ensure call in fixture
	require.NoError(t, runtime.Ensure(layout, runtime.NoOpMounter{}, testLogger()))

	mgr, err := manager.New(layout, store, kernel, discoverer, testLogger())
	require.NoError(t, err, "failed to create manager")
	bcDir := t.TempDir()
	return &testFixture{
		Manager:       mgr,
		Kernel:        kernel,
		Discoverer:    discoverer,
		Store:         store,
		Layout:        layout,
		t:             t,
		bytecodeDir:   bcDir,
		bytecodeFiles: make(map[string]string),
	}
}

// BytecodeFile returns the path to a dummy bytecode file with the
// given name. The file is created on first request and reused for
// subsequent calls with the same name. Tests should use this instead
// of hard-coded paths like "/path/to/prog.o".
func (f *testFixture) BytecodeFile(name string) string {
	f.t.Helper()
	if p, ok := f.bytecodeFiles[name]; ok {
		return p
	}
	p := filepath.Join(f.bytecodeDir, name)
	dir := filepath.Dir(p)
	require.NoError(f.t, os.MkdirAll(dir, 0755))
	require.NoError(f.t, os.WriteFile(p, []byte("ELF dummy bytecode"), 0644))
	f.bytecodeFiles[name] = p
	return p
}

// AssertKernelEmpty verifies no programs remain in the kernel.
func (f *testFixture) AssertKernelEmpty() {
	f.t.Helper()
	assert.Equal(f.t, 0, f.Kernel.ProgramCount(), "expected no programs in kernel")
}

// AssertDatabaseEmpty verifies no programs remain in the database.
func (f *testFixture) AssertDatabaseEmpty() {
	f.t.Helper()
	programs, err := f.Store.List(context.Background())
	require.NoError(f.t, err, "failed to list programs from store")
	assert.Empty(f.t, programs, "expected no programs in database")
}

// AssertCleanState verifies both kernel and database are empty.
func (f *testFixture) AssertCleanState() {
	f.t.Helper()
	f.AssertKernelEmpty()
	f.AssertDatabaseEmpty()
}

// AssertKernelOps verifies the sequence of kernel operations.
func (f *testFixture) AssertKernelOps(expected []string) {
	f.t.Helper()
	ops := f.Kernel.Operations()
	actual := make([]string, len(ops))
	for i, op := range ops {
		if op.Err != nil {
			actual[i] = fmt.Sprintf("%s:%s:error", op.Op, op.Name)
		} else {
			actual[i] = fmt.Sprintf("%s:%s:ok", op.Op, op.Name)
		}
	}
	assert.Equal(f.t, expected, actual, "kernel operations mismatch")
}

// RunWithLock executes fn while holding the global writer lock.
// Use this for operations that require a WriterScope (e.g., AttachUprobe).
func (f *testFixture) RunWithLock(ctx context.Context, fn func(ctx context.Context, scope lock.WriterScope) error) error {
	f.t.Helper()
	return lock.Run(ctx, f.Layout.LockPath(), fn)
}

// Load is a convenience wrapper that calls Manager.Load directly.
func (f *testFixture) Load(ctx context.Context, spec bpfman.LoadSpec, opts manager.LoadOpts) (bpfman.Program, error) {
	return f.Manager.Load(ctx, spec, opts)
}

// Unload is a convenience wrapper that calls Manager.Unload directly.
func (f *testFixture) Unload(ctx context.Context, kernelID uint32) error {
	return f.Manager.Unload(ctx, kernelID)
}

// AttachTracepoint is a convenience wrapper that calls Manager.AttachTracepoint directly.
func (f *testFixture) AttachTracepoint(ctx context.Context, spec bpfman.TracepointAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachTracepoint(ctx, spec, opts)
}

// AttachKprobe is a convenience wrapper that calls Manager.AttachKprobe directly.
func (f *testFixture) AttachKprobe(ctx context.Context, spec bpfman.KprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachKprobe(ctx, spec, opts)
}

// AttachUprobe is a convenience wrapper that calls Manager.AttachUprobe directly.
func (f *testFixture) AttachUprobe(ctx context.Context, scope lock.WriterScope, spec bpfman.UprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachUprobe(ctx, scope, spec, opts)
}

// AttachFentry is a convenience wrapper that calls Manager.AttachFentry directly.
func (f *testFixture) AttachFentry(ctx context.Context, spec bpfman.FentryAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachFentry(ctx, spec, opts)
}

// AttachFexit is a convenience wrapper that calls Manager.AttachFexit directly.
func (f *testFixture) AttachFexit(ctx context.Context, spec bpfman.FexitAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachFexit(ctx, spec, opts)
}

// AttachXDP is a convenience wrapper that calls Manager.AttachXDP directly.
func (f *testFixture) AttachXDP(ctx context.Context, spec bpfman.XDPAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachXDP(ctx, spec, opts)
}

// AttachTC is a convenience wrapper that calls Manager.AttachTC directly.
func (f *testFixture) AttachTC(ctx context.Context, spec bpfman.TCAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachTC(ctx, spec, opts)
}

// AttachTCX is a convenience wrapper that calls Manager.AttachTCX directly.
func (f *testFixture) AttachTCX(ctx context.Context, spec bpfman.TCXAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return f.Manager.AttachTCX(ctx, spec, opts)
}

// Detach is a convenience wrapper that calls Manager.Detach directly.
func (f *testFixture) Detach(ctx context.Context, linkID bpfman.LinkID) error {
	return f.Manager.Detach(ctx, linkID)
}
