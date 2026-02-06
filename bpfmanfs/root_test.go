package bpfmanfs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/bpfmanfs"
)

func TestNew_ValidAbsolutePaths(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/run", "/run/bpfman"},
		{"/tmp/test", "/tmp/test/bpfman"},
		{"/var/run", "/var/run/bpfman"},
		{"/", "/bpfman"},
	}
	for _, tt := range tests {
		root, err := bpfmanfs.New(tt.input)
		require.NoError(t, err, "New(%q)", tt.input)
		assert.Equal(t, tt.expected, root.Base(), "New(%q)", tt.input)
	}
}

func TestNew_RejectsEmpty(t *testing.T) {
	_, err := bpfmanfs.New("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestNew_RejectsRelative(t *testing.T) {
	relativePaths := []string{
		"run/bpfman",
		"./",
		"../",
		".",
		"..",
		"./foo",
		"../foo",
		"foo/bar",
	}
	for _, path := range relativePaths {
		_, err := bpfmanfs.New(path)
		require.Error(t, err, "New(%q) should fail", path)
		assert.Contains(t, err.Error(), "absolute", "New(%q)", path)
	}
}

func TestNew_HandlesRootSafely(t *testing.T) {
	// Even "/" is safe because we append /bpfman
	root, err := bpfmanfs.New("/")
	require.NoError(t, err)
	assert.Equal(t, "/bpfman", root.Base())
}

func TestNew_CleansPathVariants(t *testing.T) {
	// All these paths that would be dangerous if used directly
	// are safe because we append /bpfman after cleaning.
	tests := []struct {
		input    string
		expected string
	}{
		// Multiple slashes
		{"//////", "/bpfman"},
		{"//", "/bpfman"},
		{"//run//", "/run/bpfman"},
		// Dot navigation
		{"/./", "/bpfman"},
		{"/././.", "/bpfman"},
		{"/../", "/bpfman"},
		{"/../../", "/bpfman"},
		{"/../../../../../../../", "/bpfman"},
		// Mixed
		{"//./", "/bpfman"},
		{"//..", "/bpfman"},
		{"/..//", "/bpfman"},
		{"/.//../", "/bpfman"},
		// Trailing components that resolve away
		{"/tmp/..", "/bpfman"},
		{"/run/../..", "/bpfman"},
		{"/a/b/c/../../..", "/bpfman"},
		{"/foo/bar/baz/../../../..", "/bpfman"},
		// Normal paths with extra slashes
		{"//run//test//", "/run/test/bpfman"},
	}
	for _, tt := range tests {
		root, err := bpfmanfs.New(tt.input)
		require.NoError(t, err, "New(%q)", tt.input)
		assert.Equal(t, tt.expected, root.Base(), "New(%q)", tt.input)
	}
}

func TestZeroValueRoot(t *testing.T) {
	var root bpfmanfs.Root
	assert.False(t, root.Valid(), "zero Root should not be valid")

	// Methods on zero Root should panic
	assert.Panics(t, func() { root.Base() }, "Base() on zero Root should panic")
	assert.Panics(t, func() { root.DBPath() }, "DBPath() on zero Root should panic")
	assert.Panics(t, func() { root.SocketPath() }, "SocketPath() on zero Root should panic")
	assert.Panics(t, func() { root.RuntimeDirs() }, "RuntimeDirs() on zero Root should panic")
}

func TestRuntimeDirs(t *testing.T) {
	parent := t.TempDir()
	root, err := bpfmanfs.New(parent)
	require.NoError(t, err)

	dirs := root.RuntimeDirs()
	require.Len(t, dirs, 3)
	assert.Equal(t, root.Base(), dirs[0])
	assert.Equal(t, root.DBDir(), dirs[1])
	assert.Equal(t, root.SocketDir(), dirs[2])
}

func TestCSIDirs(t *testing.T) {
	parent := t.TempDir()
	root, err := bpfmanfs.New(parent)
	require.NoError(t, err)

	dirs := root.CSIDirs()
	require.Len(t, dirs, 2)
	assert.Equal(t, root.CSIDir(), dirs[0])
	assert.Equal(t, root.CSIFSDir(), dirs[1])
}

func TestRuntime_ZeroValue(t *testing.T) {
	var root bpfmanfs.Root
	// Calling Runtime() on zero Root should panic
	assert.Panics(t, func() { root.Runtime() }, "Runtime() on zero Root should panic")
}

func TestBPFFS_ZeroValue(t *testing.T) {
	var root bpfmanfs.Root
	// Calling BPFFS() on zero Root should panic
	assert.Panics(t, func() { root.BPFFS() }, "BPFFS() on zero Root should panic")
}
