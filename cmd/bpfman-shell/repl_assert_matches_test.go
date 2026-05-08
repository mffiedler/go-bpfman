package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/shell"
)

// runMatchesScript runs script against a session in which $prog is
// pre-bound to record. It returns stdout, stderr, and the recorded
// REPL error (if any). Tests use this to drive `assert ... matches`
// end-to-end through the parser, evaluator, and assert handler.
func runMatchesScript(t *testing.T, record map[string]any, script string) (out, errOut string, retErr error) {
	t.Helper()
	session := shell.NewSession()
	session.Set("prog", shell.ValueFromMap(record))

	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(script), nil)
	err := replLoop(context.Background(), cli, nil, lr, session, "")
	return outBuf.String(), errBuf.String(), err
}

func sampleProgram() map[string]any {
	return map[string]any{
		"record": map[string]any{
			"meta": map[string]any{
				"name": "tracepoint_kill_recorder",
			},
			"handles": map[string]any{
				"pin_path": "/sys/fs/bpf/prog/42",
			},
		},
		"status": map[string]any{
			"kernel": map[string]any{
				"id":  "42",
				"tag": "abc123",
			},
		},
	}
}

func TestAssertMatches_AllPass(t *testing.T) {
	t.Parallel()

	script := `assert $prog matches {
    record.meta.name: tracepoint_kill_recorder
    status.kernel.id: 42
    status.kernel.tag: not-empty
    record.handles.pin_path: not-empty
}
`
	_, errOut, err := runMatchesScript(t, sampleProgram(), script)
	require.NoError(t, err)
	assert.Empty(t, errOut, "unexpected stderr: %s", errOut)
}

func TestAssertMatches_ForwardCompatibleIgnoresExtraFields(t *testing.T) {
	t.Parallel()

	rec := sampleProgram()
	// Add a new field that the matches block does not mention; the
	// match must still pass — that's the load-bearing property of
	// subset semantics.
	rec["record"].(map[string]any)["meta"].(map[string]any)["labels"] = map[string]any{"env": "prod"}

	script := `assert $prog matches { record.meta.name: tracepoint_kill_recorder }` + "\n"
	_, errOut, err := runMatchesScript(t, rec, script)
	require.NoError(t, err)
	assert.Empty(t, errOut)
}

func TestAssertMatches_LiteralMismatch(t *testing.T) {
	t.Parallel()

	script := `assert $prog matches { record.meta.name: wrong_name }` + "\n"
	_, errOut, err := runMatchesScript(t, sampleProgram(), script)
	require.NoError(t, err)
	assert.Contains(t, errOut, "FAIL")
	assert.Contains(t, errOut, "record.meta.name")
	assert.Contains(t, errOut, "tracepoint_kill_recorder")
	assert.Contains(t, errOut, "wrong_name")
}

func TestAssertMatches_NotEmptyFailsOnEmpty(t *testing.T) {
	t.Parallel()

	rec := sampleProgram()
	rec["status"].(map[string]any)["kernel"].(map[string]any)["tag"] = ""

	script := `assert $prog matches { status.kernel.tag: not-empty }` + "\n"
	_, errOut, err := runMatchesScript(t, rec, script)
	require.NoError(t, err)
	assert.Contains(t, errOut, "FAIL")
	assert.Contains(t, errOut, "status.kernel.tag")
	assert.Contains(t, errOut, "non-empty")
}

func TestAssertMatches_MultipleMismatchesAllReported(t *testing.T) {
	t.Parallel()

	rec := sampleProgram()
	rec["status"].(map[string]any)["kernel"].(map[string]any)["tag"] = ""
	rec["record"].(map[string]any)["handles"].(map[string]any)["pin_path"] = ""

	script := `assert $prog matches {
    record.meta.name: tracepoint_kill_recorder
    status.kernel.tag: not-empty
    record.handles.pin_path: not-empty
}
`
	_, errOut, err := runMatchesScript(t, rec, script)
	require.NoError(t, err)
	assert.Contains(t, errOut, "FAIL")
	assert.Contains(t, errOut, "2 mismatches")
	assert.Contains(t, errOut, "status.kernel.tag")
	assert.Contains(t, errOut, "record.handles.pin_path")
	assert.NotContains(t, errOut, "record.meta.name", "passing entry should not appear in failure output")
}

func TestAssertMatches_VarPattern(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("prog", shell.ValueFromMap(sampleProgram()))
	session.Set("expected_id", shell.StringValue("42"))

	script := `assert $prog matches { status.kernel.id: $expected_id }` + "\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(script), nil)
	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestAssertMatches_MissingPath(t *testing.T) {
	t.Parallel()

	script := `assert $prog matches { record.meta.does_not_exist: foo }` + "\n"
	_, errOut, err := runMatchesScript(t, sampleProgram(), script)
	require.NoError(t, err)
	assert.Contains(t, errOut, "FAIL")
	assert.Contains(t, errOut, "record.meta.does_not_exist")
}

func TestRequireMatches_HaltsOnFailure(t *testing.T) {
	t.Parallel()

	// `require` semantics: on failure the script halts. After
	// `require ... matches` fails, the `print after` line below
	// must not run.
	script := `require $prog matches { record.meta.name: wrong }
print after
`
	out, errOut, _ := runMatchesScript(t, sampleProgram(), script)
	assert.Contains(t, errOut, "FAIL")
	assert.NotContains(t, out, "after", "require failure must halt subsequent statements")
}

func TestAssertMatches_FailureLineNumbers_ChunkRelative(t *testing.T) {
	t.Parallel()

	// Without a file context, locations in the failure message
	// should be chunk-relative: line numbers count from the start
	// of the accumulated REPL chunk rather than from any file.
	// The script begins with the `assert` on chunk-line 1, so the
	// three failing entries fall on chunk-lines 2, 4, and 5.
	script := `assert $prog matches {
    record.meta.name:        wrong_name
    status.kernel.id:        42
    status.kernel.tag:       not-empty
    record.handles.pin_path: ""
}
`
	rec := sampleProgram()
	rec["status"].(map[string]any)["kernel"].(map[string]any)["tag"] = ""
	_, errOut, err := runMatchesScript(t, rec, script)
	require.NoError(t, err)
	require.Contains(t, errOut, "FAIL")
	require.Contains(t, errOut, "3 mismatches")

	// Each mismatch carries its own line:col prefix from the
	// matches block, not the location of the assert keyword.
	assert.Contains(t, errOut, "2:5: record.meta.name:")
	assert.Contains(t, errOut, "4:5: status.kernel.tag:")
	assert.Contains(t, errOut, "5:5: record.handles.pin_path:")
}

func TestAssertMatches_FailureLineNumbers_AbsoluteFile(t *testing.T) {
	t.Parallel()

	// With a file context, the locations must be absolute file
	// lines: the chunk start-line is added to each entry's
	// chunk-relative line. The script is preceded by a comment
	// and a blank line, so the assert keyword sits on file line 3
	// and the three failing entries land on file lines 4, 6, and 7.
	// (status.kernel.id=42 on line 5 passes, so it does not
	// surface in the failure output.)
	script := `# matches-line-numbers test

assert $prog matches {
    record.meta.name:        wrong_name
    status.kernel.id:        42
    record.handles.pin_path: ""
    status.kernel.tag:       not-empty
}
`
	rec := sampleProgram()
	rec["status"].(map[string]any)["kernel"].(map[string]any)["tag"] = ""

	session := shell.NewSession()
	session.Set("prog", shell.ValueFromMap(rec))
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(script), nil)
	require.NoError(t, replLoop(context.Background(), cli, nil, lr, session, "fake.bpfman"))

	errOut := errBuf.String()
	require.Contains(t, errOut, "FAIL")
	require.Contains(t, errOut, "3 mismatches")
	assert.Contains(t, errOut, "fake.bpfman:4:5: record.meta.name:")
	assert.Contains(t, errOut, "fake.bpfman:6:5: record.handles.pin_path:")
	assert.Contains(t, errOut, "fake.bpfman:7:5: status.kernel.tag:")
}

func TestAssertMatches_PassingEntriesNotInOutput(t *testing.T) {
	t.Parallel()

	// The failure message must list only the diverging entries;
	// passing entries do not appear and do not consume a line in
	// the report.
	script := `assert $prog matches {
    record.meta.name:    tracepoint_kill_recorder
    status.kernel.id:    nope
}
`
	_, errOut, err := runMatchesScript(t, sampleProgram(), script)
	require.NoError(t, err)
	assert.Contains(t, errOut, "1 mismatch\n")
	assert.Contains(t, errOut, "status.kernel.id:")
	assert.NotContains(t, errOut, "record.meta.name:")
}

func TestAssertMatches_NotWithMatchesIsError(t *testing.T) {
	t.Parallel()

	script := `assert not $prog matches { record.meta.name: x }` + "\n"
	_, errOut, _ := runMatchesScript(t, sampleProgram(), script)
	assert.Contains(t, strings.ToLower(errOut), "not")
}

// Avoid "declared but not used" if io is not referenced by other
// helpers added later in the file.
var _ = io.Discard
