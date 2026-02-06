package bpfmanfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeRemoveAll_UnderParent(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "child")
	require.NoError(t, os.MkdirAll(target, 0755))

	err := safeRemoveAll(parent, target)
	require.NoError(t, err)
	assert.NoDirExists(t, target)
}

func TestSafeRemoveAll_RejectsEscape(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()

	err := safeRemoveAll(parent, outside)
	assert.Error(t, err)
	var errOutside ErrOutsideRoot
	assert.ErrorAs(t, err, &errOutside)
}

func TestSafeRemoveAll_RejectsDotDot(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "..")

	err := safeRemoveAll(parent, target)
	assert.Error(t, err)
	var errOutside ErrOutsideRoot
	assert.ErrorAs(t, err, &errOutside)
}

func TestSafeRemoveAll_PrefixFalsePositive(t *testing.T) {
	// /tmp/programs vs /tmp/programsX -- must not match.
	base := t.TempDir()
	parent := filepath.Join(base, "programs")
	falsePositive := filepath.Join(base, "programsX")

	require.NoError(t, os.MkdirAll(parent, 0755))
	require.NoError(t, os.MkdirAll(falsePositive, 0755))

	err := safeRemoveAll(parent, falsePositive)
	assert.Error(t, err)
	var errOutside ErrOutsideRoot
	assert.ErrorAs(t, err, &errOutside)

	// The false positive target should still exist.
	assert.DirExists(t, falsePositive)
}
