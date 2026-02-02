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
