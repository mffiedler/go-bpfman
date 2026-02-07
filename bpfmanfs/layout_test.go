package bpfmanfs_test

import (
	"path/filepath"
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
		{"/run/bpfman", "/run/bpfman"},
		{"/tmp/test/bpfman", "/tmp/test/bpfman"},
		{"/var/run/bpfman", "/var/run/bpfman"},
		{"/bpfman", "/bpfman"},
	}
	for _, tt := range tests {
		layout, err := bpfmanfs.New(tt.input)
		require.NoError(t, err, "New(%q)", tt.input)
		assert.Equal(t, tt.expected, layout.Base(), "New(%q)", tt.input)
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

func TestNew_CleansPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Multiple slashes are cleaned
		{"//run//bpfman//", "/run/bpfman"},
		// Dot navigation
		{"/run/./bpfman", "/run/bpfman"},
		{"/run/../run/bpfman", "/run/bpfman"},
		// Trailing slashes
		{"/run/bpfman/", "/run/bpfman"},
	}
	for _, tt := range tests {
		layout, err := bpfmanfs.New(tt.input)
		require.NoError(t, err, "New(%q)", tt.input)
		assert.Equal(t, tt.expected, layout.Base(), "New(%q)", tt.input)
	}
}

func TestZeroValueLayout(t *testing.T) {
	var layout bpfmanfs.FSLayout
	assert.False(t, layout.Valid(), "zero FSLayout should not be valid")

	// Methods on zero FSLayout should panic
	assert.Panics(t, func() { layout.Base() }, "Base() on zero FSLayout should panic")
	assert.Panics(t, func() { layout.DBPath() }, "DBPath() on zero FSLayout should panic")
	assert.Panics(t, func() { layout.SocketPath() }, "SocketPath() on zero FSLayout should panic")
	assert.Panics(t, func() { layout.RuntimeDirs() }, "RuntimeDirs() on zero FSLayout should panic")
}

func TestLayoutString(t *testing.T) {
	// String() on zero FSLayout should not panic and return a safe representation
	var zero bpfmanfs.FSLayout
	assert.Equal(t, "bpfmanfs.FSLayout(<invalid>)", zero.String())

	// String() on valid FSLayout should include the path
	layout, err := bpfmanfs.New("/run/bpfman")
	require.NoError(t, err)
	assert.Equal(t, "bpfmanfs.FSLayout(/run/bpfman)", layout.String())
}

func TestRuntimeDirs(t *testing.T) {
	parent := t.TempDir()
	layout, err := bpfmanfs.New(filepath.Join(parent, "bpfman"))
	require.NoError(t, err)

	dirs := layout.RuntimeDirs()
	require.Len(t, dirs, 3)
	assert.Equal(t, layout.Base(), dirs[0])
	assert.Equal(t, layout.DBDir(), dirs[1])
	assert.Equal(t, layout.SocketDir(), dirs[2])
}

func TestCSIDirs(t *testing.T) {
	parent := t.TempDir()
	layout, err := bpfmanfs.New(filepath.Join(parent, "bpfman"))
	require.NoError(t, err)

	dirs := layout.CSIDirs()
	require.Len(t, dirs, 2)
	assert.Equal(t, layout.CSIDir(), dirs[0])
	assert.Equal(t, layout.CSIFSDir(), dirs[1])
}

func TestRuntime_ZeroValue(t *testing.T) {
	var layout bpfmanfs.FSLayout
	// Calling Runtime() on zero FSLayout should panic
	assert.Panics(t, func() { layout.Runtime() }, "Runtime() on zero FSLayout should panic")
}

func TestBPFFS_ZeroValue(t *testing.T) {
	var layout bpfmanfs.FSLayout
	// Calling BPFFS() on zero FSLayout should panic
	assert.Panics(t, func() { layout.BPFFS() }, "BPFFS() on zero FSLayout should panic")
}
