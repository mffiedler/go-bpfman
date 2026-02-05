package fs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/fs"
)

func TestOpen_ValidAbsolutePaths(t *testing.T) {
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
		root, err := fs.Open(tt.input)
		require.NoError(t, err, "Open(%q)", tt.input)
		assert.Equal(t, tt.expected, root.Base(), "Open(%q)", tt.input)
	}
}

func TestOpen_RejectsEmpty(t *testing.T) {
	_, err := fs.Open("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestOpen_RejectsRelative(t *testing.T) {
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
		_, err := fs.Open(path)
		require.Error(t, err, "Open(%q) should fail", path)
		assert.Contains(t, err.Error(), "absolute", "Open(%q)", path)
	}
}

func TestOpen_HandlesRootSafely(t *testing.T) {
	// Even "/" is safe because we append /bpfman
	root, err := fs.Open("/")
	require.NoError(t, err)
	assert.Equal(t, "/bpfman", root.Base())
}

func TestOpen_CleansPathVariants(t *testing.T) {
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
		root, err := fs.Open(tt.input)
		require.NoError(t, err, "Open(%q)", tt.input)
		assert.Equal(t, tt.expected, root.Base(), "Open(%q)", tt.input)
	}
}

func TestZeroValueRoot(t *testing.T) {
	var root fs.Root
	assert.Equal(t, "", root.Base())
	assert.False(t, root.Valid())
}

func TestRuntimeDirs(t *testing.T) {
	parent := t.TempDir()
	root, err := fs.Open(parent)
	require.NoError(t, err)

	dirs := root.RuntimeDirs()
	require.Len(t, dirs, 3)
	assert.Equal(t, root.Base(), dirs[0])
	assert.Equal(t, root.DBDir(), dirs[1])
	assert.Equal(t, root.SocketDir(), dirs[2])
}

func TestCSIDirs(t *testing.T) {
	parent := t.TempDir()
	root, err := fs.Open(parent)
	require.NoError(t, err)

	dirs := root.CSIDirs()
	require.Len(t, dirs, 2)
	assert.Equal(t, root.CSIDir(), dirs[0])
	assert.Equal(t, root.CSIFSDir(), dirs[1])
}

func TestRuntime_ZeroValue(t *testing.T) {
	var root fs.Root
	rt := root.Runtime()
	assert.False(t, rt.Valid())
}

func TestBPFFS_ZeroValue(t *testing.T) {
	var root fs.Root
	b := root.BPFFS()
	assert.False(t, b.Valid())
}
