package fs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/config"
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

func TestFromRuntimeDirs_PathEquivalence(t *testing.T) {
	dirs := config.DefaultRuntimeDirs()
	root := fs.FromRuntimeDirs(dirs)
	bpffsView := root.BPFFS()

	assert.Equal(t, dirs.Base(), root.Base())
	assert.Equal(t, dirs.Lock(), root.LockPath())
	assert.Equal(t, dirs.DBPath(), root.DBPath())
	assert.Equal(t, dirs.SocketPath(), root.SocketPath())
	assert.Equal(t, dirs.CSISocketPath(), root.CSISocketPath())
	assert.Equal(t, dirs.FS(), root.BPFFSMountPoint())

	assert.Equal(t, dirs.FS(), bpffsView.FS())
	assert.Equal(t, dirs.FS_XDP(), bpffsView.XDP())
	assert.Equal(t, dirs.FS_TC_INGRESS(), bpffsView.TCIngress())
	assert.Equal(t, dirs.FS_TC_EGRESS(), bpffsView.TCEgress())
	assert.Equal(t, dirs.FS_MAPS(), bpffsView.Maps())
	assert.Equal(t, dirs.FS_LINKS(), bpffsView.Links())

	// Test program-specific paths with various IDs.
	for _, id := range []uint32{0, 1, 42, 1000, 4294967295} {
		assert.Equal(t, dirs.ProgPinPath(id), bpffsView.ProgPinPath(id), "ProgPinPath(%d)", id)
		assert.Equal(t, dirs.MapPinDir(id), bpffsView.MapPinDir(id), "MapPinDir(%d)", id)
		assert.Equal(t, dirs.LinkPinDir(id), bpffsView.LinkPinDir(id), "LinkPinDir(%d)", id)
	}

	// ScannerDirs equivalence.
	assert.Equal(t, dirs.ScannerDirs(), bpffsView.ScannerDirs())
}

func TestFromRuntimeDirs_CustomBase(t *testing.T) {
	dirs, err := config.NewRuntimeDirs("/tmp/custom-bpfman")
	require.NoError(t, err)
	root := fs.FromRuntimeDirs(dirs)

	assert.Equal(t, "/tmp/custom-bpfman", root.Base())
	assert.Equal(t, "/tmp/custom-bpfman/.lock", root.LockPath())
	assert.Equal(t, "/tmp/custom-bpfman/db/store.db", root.DBPath())
	assert.Equal(t, "/tmp/custom-bpfman-sock/bpfman.sock", root.SocketPath())
	assert.Equal(t, "/tmp/custom-bpfman/csi/csi.sock", root.CSISocketPath())
	assert.Equal(t, "/tmp/custom-bpfman/fs", root.BPFFSMountPoint())
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
