package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/shell"
)

// replTempdir is the "tempdir PREFIX" shell builtin. It creates a
// private directory under the OS default temp dir and returns a
// structured value carrying the absolute path on .path.

func TestReplTempdir_CreatesUniqueDirectory(t *testing.T) {
	t.Parallel()

	v, err := replTempdir([]shell.Arg{shell.WordArg{Text: "bpfman-test"}})
	require.NoError(t, err)

	path, err := v.LookupValue("wd", "path")
	require.NoError(t, err)
	p, err := path.Scalar()
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(p) })

	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "tempdir result must be a directory")
	assert.True(t, strings.HasPrefix(filepath.Base(p), "bpfman-test."),
		"tempdir result %q must start with the requested prefix", p)
}

func TestReplTempdir_DistinctInvocationsAreUnique(t *testing.T) {
	t.Parallel()

	v1, err := replTempdir([]shell.Arg{shell.WordArg{Text: "bpfman-test"}})
	require.NoError(t, err)
	p1, err := v1.LookupValue("wd1", "path")
	require.NoError(t, err)
	s1, err := p1.Scalar()
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(s1) })

	v2, err := replTempdir([]shell.Arg{shell.WordArg{Text: "bpfman-test"}})
	require.NoError(t, err)
	p2, err := v2.LookupValue("wd2", "path")
	require.NoError(t, err)
	s2, err := p2.Scalar()
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(s2) })

	assert.NotEqual(t, s1, s2, "concurrent invocations must produce distinct paths")
}

func TestReplTempdir_RejectsMissingPrefix(t *testing.T) {
	t.Parallel()

	_, err := replTempdir(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PREFIX")
}

func TestReplTempdir_RejectsEmptyPrefix(t *testing.T) {
	t.Parallel()

	_, err := replTempdir([]shell.Arg{shell.QuotedArg{Text: ""}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be empty")
}

func TestReplTempdir_RejectsExtraArgs(t *testing.T) {
	t.Parallel()

	_, err := replTempdir([]shell.Arg{
		shell.WordArg{Text: "bpfman-test"},
		shell.WordArg{Text: "extra"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}
