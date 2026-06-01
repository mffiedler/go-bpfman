package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/driver"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
)

func runWholeProgramStdin(t *testing.T, script string) (string, string, error) {
	t.Helper()

	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := driver.NewScannerReader(strings.NewReader(script), nil)

	err := driver.Run(context.Background(), driver.Config{
		CLI:          cli,
		Mgr:          nil,
		LineReader:   lr,
		Session:      runtime.NewSession(),
		File:         "<stdin>",
		NoCheck:      false,
		Fallback:     commandFallback,
		BindFallback: bindCommandFallback,
		MakeAssertIR: makeExecAssertIR,
	})
	return outBuf.String(), errBuf.String(), err
}

func TestScriptRun_StdinWholeProgram_ForwardDefVisible(t *testing.T) {
	t.Parallel()

	script := "hello\ndef hello() { print from-stdin }\n"
	stdout, stderr, err := runWholeProgramStdin(t, script)
	require.NoError(t, err)
	assert.Equal(t, []string{"from-stdin"}, nonEmptyOutputLines(stdout))
	assert.Empty(t, stderr)
}

//nolint:paralleltest // This test changes process cwd via t.Chdir to verify stdin import resolution semantics.
func TestScriptRun_StdinWholeProgram_ImportResolvesFromCwd(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.bpfman"), []byte("def hi() { print cwd-lib }\n"), 0o644))

	t.Chdir(dir)

	script := "import ./lib.bpfman\nhi\n"
	stdout, stderr, err := runWholeProgramStdin(t, script)
	require.NoError(t, err)
	assert.Equal(t, []string{"cwd-lib"}, nonEmptyOutputLines(stdout))
	assert.Empty(t, stderr)
}
