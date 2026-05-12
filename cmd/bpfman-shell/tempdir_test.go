package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/shell"
)

// handleTempdir is the "tempdir PREFIX" shell builtin. It creates
// a private directory under the OS default temp dir and returns a
// structured value carrying the absolute path on .path.

// tempdirCtx is a minimal builtinCtx for testing the handler.
// Only Ctx and Args are read by handleTempdir; the other fields
// stay at their zero values.
func tempdirCtx(args ...shell.Arg) builtinCtx {
	return builtinCtx{Ctx: context.Background(), Args: args}
}

func TestHandleTempdir_CreatesUniqueDirectory(t *testing.T) {
	t.Parallel()

	v, err := handleTempdir(tempdirCtx(shell.WordArg{Text: "bpfman-test"}))
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

func TestHandleTempdir_DistinctInvocationsAreUnique(t *testing.T) {
	t.Parallel()

	v1, err := handleTempdir(tempdirCtx(shell.WordArg{Text: "bpfman-test"}))
	require.NoError(t, err)
	p1, err := v1.LookupValue("wd1", "path")
	require.NoError(t, err)
	s1, err := p1.Scalar()
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(s1) })

	v2, err := handleTempdir(tempdirCtx(shell.WordArg{Text: "bpfman-test"}))
	require.NoError(t, err)
	p2, err := v2.LookupValue("wd2", "path")
	require.NoError(t, err)
	s2, err := p2.Scalar()
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(s2) })

	assert.NotEqual(t, s1, s2, "concurrent invocations must produce distinct paths")
}

func TestHandleTempdir_RejectsMissingPrefix(t *testing.T) {
	t.Parallel()

	_, err := handleTempdir(tempdirCtx())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PREFIX")
}

func TestHandleTempdir_RejectsEmptyPrefix(t *testing.T) {
	t.Parallel()

	_, err := handleTempdir(tempdirCtx(shell.QuotedArg{Text: ""}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be empty")
}

func TestHandleTempdir_RejectsExtraArgs(t *testing.T) {
	t.Parallel()

	_, err := handleTempdir(tempdirCtx(
		shell.WordArg{Text: "bpfman-test"},
		shell.WordArg{Text: "extra"},
	))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}
