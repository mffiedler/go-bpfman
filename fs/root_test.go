package fs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/fs"
)

func TestOpen_ValidAbsolutePaths(t *testing.T) {
	for _, base := range []string{"/run/bpfman", "/tmp/test", "/var/run/bpfman"} {
		root, err := fs.Open(base)
		require.NoError(t, err, "Open(%q)", base)
		assert.Equal(t, base, root.Base())
	}
}

func TestOpen_RejectsEmpty(t *testing.T) {
	_, err := fs.Open("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestOpen_RejectsRelative(t *testing.T) {
	_, err := fs.Open("run/bpfman")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestOpen_RejectsRoot(t *testing.T) {
	_, err := fs.Open("/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
}

func TestZeroValueRoot(t *testing.T) {
	var root fs.Root
	assert.Equal(t, "", root.Base())

	err := root.EnsureDirectories()
	assert.ErrorIs(t, err, fs.ErrInvalidRoot)

	err = root.EnsureCSIDirectories()
	assert.ErrorIs(t, err, fs.ErrInvalidRoot)

	err = root.EnsureBPFFSMounted("/proc/self/mountinfo")
	assert.ErrorIs(t, err, fs.ErrInvalidRoot)
}

func TestEnsureDirectories(t *testing.T) {
	base := t.TempDir()
	root, err := fs.Open(base)
	require.NoError(t, err)

	// EnsureDirectories will fail on bpffs mount (expected in test).
	// We test that the regular directories are created despite that.
	_ = root.EnsureDirectories()

	// The directories that do not require bpffs should exist.
	assert.DirExists(t, base)
	assert.DirExists(t, base+"/db")
	assert.DirExists(t, base+"-sock")
}

func TestEnsureCSIDirectories(t *testing.T) {
	base := t.TempDir()
	root, err := fs.Open(base)
	require.NoError(t, err)

	err = root.EnsureCSIDirectories()
	require.NoError(t, err)

	assert.DirExists(t, base+"/csi")
	assert.DirExists(t, base+"/csi/fs")
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
