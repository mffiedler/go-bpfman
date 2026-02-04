package manager_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/config"
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
	Manager    *manager.Manager
	Kernel     *fakeKernel
	Discoverer *fakeDiscoverer
	Store      interpreter.Store
	Dirs       *config.RuntimeDirs
	t          *testing.T
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
	dirs, err := config.NewRuntimeDirs(t.TempDir())
	require.NoError(t, err, "failed to create runtime dirs")
	kernel := newFakeKernel()
	if discoverer == nil {
		discoverer = newFakeDiscoverer()
	}
	mgr := manager.New(dirs, store, kernel, discoverer, testLogger())
	return &testFixture{
		Manager:    mgr,
		Kernel:     kernel,
		Discoverer: discoverer,
		Store:      store,
		Dirs:       &dirs,
		t:          t,
	}
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
	return lock.Run(ctx, f.Dirs.Lock(), fn)
}

// Load is a convenience wrapper that returns just the Program.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) Load(ctx context.Context, spec bpfman.LoadSpec, opts manager.LoadOpts) (bpfman.Program, error) {
	result, err := f.Manager.Load(ctx, spec, opts)
	return result.Program, err
}

// Unload is a convenience wrapper that returns just the error.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) Unload(ctx context.Context, kernelID uint32) error {
	_, err := f.Manager.Unload(ctx, kernelID)
	return err
}

// AttachTracepoint is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachTracepoint(ctx context.Context, spec bpfman.TracepointAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachTracepoint(ctx, spec, opts)
	return result.Link, err
}

// AttachKprobe is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachKprobe(ctx context.Context, spec bpfman.KprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachKprobe(ctx, spec, opts)
	return result.Link, err
}

// AttachUprobe is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachUprobe(ctx context.Context, scope lock.WriterScope, spec bpfman.UprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachUprobe(ctx, scope, spec, opts)
	return result.Link, err
}

// AttachFentry is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachFentry(ctx context.Context, spec bpfman.FentryAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachFentry(ctx, spec, opts)
	return result.Link, err
}

// AttachFexit is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachFexit(ctx context.Context, spec bpfman.FexitAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachFexit(ctx, spec, opts)
	return result.Link, err
}

// AttachXDP is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachXDP(ctx context.Context, spec bpfman.XDPAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachXDP(ctx, spec, opts)
	return result.Link, err
}

// AttachTC is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachTC(ctx context.Context, spec bpfman.TCAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachTC(ctx, spec, opts)
	return result.Link, err
}

// AttachTCX is a convenience wrapper that returns just the Link.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) AttachTCX(ctx context.Context, spec bpfman.TCXAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	result, err := f.Manager.AttachTCX(ctx, spec, opts)
	return result.Link, err
}

// Detach is a convenience wrapper that returns just the error.
// Use this for tests that don't need to inspect the outcome.
func (f *testFixture) Detach(ctx context.Context, linkID bpfman.LinkID) error {
	_, err := f.Manager.Detach(ctx, linkID)
	return err
}
