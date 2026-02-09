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
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
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
	Store         platform.Store
	Layout        bpfmanfs.FSLayout
	t             *testing.T
	bytecodeDir   string            // temp dir for dummy bytecode files
	bytecodeFiles map[string]string // name -> path cache
}

// newTestFixture creates a complete test fixture with accessible components.
func newTestFixture(t *testing.T) *testFixture {
	return newTestFixtureWithOptions(t, nil, nil)
}

// newTestFixtureWithDiscoverer creates a test fixture with a custom discoverer.
func newTestFixtureWithDiscoverer(t *testing.T, discoverer *fakeDiscoverer) *testFixture {
	return newTestFixtureWithOptions(t, discoverer, nil)
}

// newTestFixtureWithOptions creates a test fixture with optional overrides.
func newTestFixtureWithOptions(t *testing.T, discoverer *fakeDiscoverer, puller platform.ImagePuller) *testFixture {
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
	ensuredRuntime, err := runtime.New(layout, runtime.NoOpMounter{}, testLogger())
	require.NoError(t, err, "failed to ensure runtime")

	mgr, err := manager.New(ensuredRuntime, puller, store, kernel, discoverer, testLogger())
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

// Load is a convenience wrapper that loads a single program from a LoadSpec.
// It translates the LoadSpec into the LoadSource/ProgramSpec form expected
// by Manager.Load, ensures the fake discoverer knows about the program,
// and returns the single loaded program.
func (f *testFixture) Load(ctx context.Context, spec bpfman.LoadSpec, opts manager.LoadOpts) (bpfman.Program, error) {
	source := manager.LoadSource{FilePath: spec.ObjectPath()}
	programs := []manager.ProgramSpec{{
		Name:       spec.ProgramName(),
		Type:       spec.ProgramType(),
		AttachFunc: spec.AttachFunc(),
	}}
	if gd := spec.GlobalData(); gd != nil {
		programs[0].GlobalData = gd
	}
	if id := spec.MapOwnerID(); id != 0 {
		programs[0].MapOwnerID = id
	}
	// Ensure the discoverer knows about the program so validation passes.
	f.Discoverer.AddPrograms(spec.ObjectPath(), platform.DiscoveredProgram{
		Name:       spec.ProgramName(),
		Type:       spec.ProgramType(),
		AttachFunc: spec.AttachFunc(),
	})
	result, err := f.Manager.Load(ctx, source, programs, opts)
	if err != nil {
		return bpfman.Program{}, err
	}
	return result[0], nil
}

// Unload is a convenience wrapper that calls Manager.Unload directly.
func (f *testFixture) Unload(ctx context.Context, kernelID uint32) error {
	return f.Manager.Unload(ctx, kernelID)
}

// Attach is a convenience wrapper that calls Manager.Attach directly.
// For non-uprobe types, scope may be nil.
func (f *testFixture) Attach(ctx context.Context, scope lock.WriterScope, spec bpfman.AttachSpec) (bpfman.Link, error) {
	return f.Manager.Attach(ctx, scope, spec)
}

// Detach is a convenience wrapper that calls Manager.Detach directly.
func (f *testFixture) Detach(ctx context.Context, linkID bpfman.LinkID) error {
	return f.Manager.Detach(ctx, linkID)
}
