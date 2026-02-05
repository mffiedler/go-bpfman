package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/config"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store/sqlite"
	"github.com/frobware/go-bpfman/server"
)

// testLogger returns a logger for tests. By default it discards all output.
// Set BPFMAN_TEST_VERBOSE=1 to enable logging.
func testLogger() *slog.Logger {
	if os.Getenv("BPFMAN_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeNetIfaceResolver implements server.NetIfaceResolver for testing.
// It returns fake interface data without requiring real network interfaces.
type fakeNetIfaceResolver struct {
	interfaces map[string]*net.Interface
}

func newFakeNetIfaceResolver() *fakeNetIfaceResolver {
	return &fakeNetIfaceResolver{
		interfaces: map[string]*net.Interface{
			"lo":   {Index: 1, Name: "lo"},
			"eth0": {Index: 2, Name: "eth0"},
		},
	}
}

func (f *fakeNetIfaceResolver) InterfaceByName(name string) (*net.Interface, error) {
	iface, ok := f.interfaces[name]
	if !ok {
		return nil, fmt.Errorf("interface %q not found", name)
	}
	return iface, nil
}

// testFixture provides access to all components for verification.
type testFixture struct {
	Server        *server.Server
	Kernel        *fakeKernel
	Store         interpreter.Store
	Dirs          *config.RuntimeDirs
	t             *testing.T
	bytecodeDir   string
	bytecodeFiles map[string]string
}

// newTestFixture creates a complete test fixture with accessible components.
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()
	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err, "failed to create store")
	t.Cleanup(func() { store.Close() })
	dirs, err := config.NewRuntimeDirs(t.TempDir())
	require.NoError(t, err, "failed to create runtime dirs")
	kernel := newFakeKernel()
	netIface := newFakeNetIfaceResolver()
	srv := server.New(dirs, store, kernel, nil, netIface, testLogger())
	return &testFixture{
		Server:        srv,
		Kernel:        kernel,
		Store:         store,
		Dirs:          &dirs,
		t:             t,
		bytecodeDir:   t.TempDir(),
		bytecodeFiles: make(map[string]string),
	}
}

// BytecodeFile returns the path to a dummy bytecode file with the
// given name. The file is created on first request and reused for
// subsequent calls with the same name.
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

// newTestServer creates a server with fake kernel and real in-memory SQLite.
func newTestServer(t *testing.T) *server.Server {
	t.Helper()
	return newTestFixture(t).Server
}

// stringPtr returns a pointer to a string value.
// Useful for optional string fields in protobuf messages.
func stringPtr(s string) *string {
	return &s
}
