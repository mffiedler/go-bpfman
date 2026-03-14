package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/replang"
)

func TestReplComplete_FileCompletion(t *testing.T) {
	// Create a temp directory tree to complete against.
	root := t.TempDir()

	// Create: root/e2e/testdata/stats.o
	//         root/e2e/testdata/other.o
	//         root/e2e/README
	//         root/somefile.o
	require.NoError(t, os.MkdirAll(filepath.Join(root, "e2e", "testdata"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "e2e", "testdata", "stats.o"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "e2e", "testdata", "other.o"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "e2e", "README"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "somefile.o"), nil, 0o644))

	// Run tests from the temp directory so relative paths resolve.
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { os.Chdir(orig) })

	tests := []struct {
		name        string
		line        string
		wantReplace int
		wantAny     []string // candidates must contain all of these
		wantNone    []string // candidates must contain none of these
		wantNonZero bool     // at least one candidate expected
	}{
		{
			name:        "absolute path to directory",
			line:        "load file --path " + root + "/e2e/",
			wantReplace: len(root + "/e2e/"),
			wantAny:     []string{root + "/e2e/testdata/", root + "/e2e/README"},
			wantNonZero: true,
		},
		{
			name:        "absolute path partial match",
			line:        "load file --path " + root + "/e2e/test",
			wantReplace: len(root + "/e2e/test"),
			wantAny:     []string{root + "/e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "relative path ./e2e/",
			line:        "load file --path ./e2e/",
			wantReplace: len("./e2e/"),
			wantAny:     []string{"./e2e/testdata/", "./e2e/README"},
			wantNonZero: true,
		},
		{
			name:        "relative path ./e2e/test",
			line:        "load file --path ./e2e/test",
			wantReplace: len("./e2e/test"),
			wantAny:     []string{"./e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "relative path without dot prefix",
			line:        "load file --path e2e/test",
			wantReplace: len("e2e/test"),
			wantAny:     []string{"e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "short flag -p",
			line:        "load file -p ./e2e/test",
			wantReplace: len("./e2e/test"),
			wantAny:     []string{"./e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "--path with trailing space lists cwd",
			line:        "load file --path ",
			wantReplace: 0,
			wantAny:     []string{"./e2e/", "./somefile.o"},
			wantNonZero: true,
		},
		{
			name:        "-p with trailing space lists cwd",
			line:        "load file -p ",
			wantReplace: 0,
			wantNonZero: true,
		},
		{
			name:        "nonexistent path returns nothing",
			line:        "load file --path /nonexistent/path/xyz",
			wantReplace: len("/nonexistent/path/xyz"),
		},
		{
			name:        "file completion with .o filter prefix",
			line:        "load file --path " + root + "/e2e/testdata/s",
			wantReplace: len(root + "/e2e/testdata/s"),
			wantAny:     []string{root + "/e2e/testdata/stats.o"},
			wantNone:    []string{root + "/e2e/testdata/other.o"},
			wantNonZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos := len(tt.line)
			replace, candidates := replComplete(context.Background(), nil, tt.line, pos)

			assert.Equal(t, tt.wantReplace, replace, "replace")

			if tt.wantNonZero {
				assert.NotEmpty(t, candidates, "expected at least one candidate")
			}

			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}

			for _, unwanted := range tt.wantNone {
				assert.NotContains(t, candidates, unwanted, "unexpected candidate %q", unwanted)
			}
		})
	}
}

func TestScannerReader(t *testing.T) {
	input := "line one\nline two\n"
	lr := NewScannerReader(strings.NewReader(input), nil)
	defer lr.Close()

	s, err := lr.Readline()
	require.NoError(t, err)
	assert.Equal(t, "line one", s)

	s, err = lr.Readline()
	require.NoError(t, err)
	assert.Equal(t, "line two", s)

	_, err = lr.Readline()
	assert.ErrorIs(t, err, io.EOF)
}

func TestReplLoop_CommentsAndBlanks(t *testing.T) {
	// Feed the loop lines that include comments, blank lines, and
	// an unknown command so we can verify only real commands are
	// dispatched. The only side effect we can easily observe
	// without a real manager is the error output for unknown
	// commands.
	input := strings.Join([]string{
		"# full line comment",
		"",
		"   ",
		"bogus # inline comment stripped",
		"  # indented comment",
		"also-bogus",
	}, "\n")

	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)

	// We expect exactly two error lines: one for "bogus", one for
	// "also-bogus".
	lines := strings.Split(strings.TrimSpace(errBuf.String()), "\n")
	require.Len(t, lines, 2)
	assert.True(t, strings.HasPrefix(lines[0], "[repl] "), "expected [repl] prefix: %s", lines[0])
	assert.Contains(t, lines[0], "bogus")
	assert.Contains(t, lines[1], "also-bogus")
}

func TestReplComplete_CommandCompletion(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "empty line completes commands",
			line:        "",
			wantAny:     []string{"help ", "load ", "list "},
			wantReplace: 0,
		},
		{
			name:        "partial load",
			line:        "lo",
			wantAny:     []string{"load "},
			wantReplace: len("lo"),
		},
		{
			name:        "load completes to file",
			line:        "load ",
			wantAny:     []string{"file "},
			wantReplace: 0,
		},
		{
			name:        "list completes to programs",
			line:        "list ",
			wantAny:     []string{"programs "},
			wantReplace: 0,
		},
		{
			name:        "program completes to delete, list, and load",
			line:        "program ",
			wantAny:     []string{"delete ", "list ", "load "},
			wantReplace: 0,
		},
		{
			name:        "program partial delete",
			line:        "program de",
			wantAny:     []string{"delete "},
			wantReplace: len("de"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos := len(tt.line)
			replace, candidates := replComplete(context.Background(), nil, tt.line, pos)

			assert.Equal(t, tt.wantReplace, replace)
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want)
			}
		})
	}
}

func TestTokenTexts(t *testing.T) {
	tokens := []replang.Token{
		{Kind: replang.TokenWord, Text: "show"},
		{Kind: replang.TokenWord, Text: "program"},
		{Kind: replang.TokenWord, Text: "42"},
	}
	got := tokenTexts(tokens)
	assert.Equal(t, []string{"show", "program", "42"}, got)
}

func TestTokenTexts_Empty(t *testing.T) {
	got := tokenTexts(nil)
	assert.Empty(t, got)
}

func TestReplLoop_VarsEmpty(t *testing.T) {
	input := "vars\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "No variables defined")
}

func TestReplLoop_AssignmentToNonAssignable(t *testing.T) {
	// "help" returns no value, so assigning should produce an error.
	input := "x = help\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "command produced no result to assign")
}

func TestReplLoop_UndefinedVariable(t *testing.T) {
	input := "show program $x.id\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "undefined variable")
}

func TestReplLoop_QuotedHashNotComment(t *testing.T) {
	// A '#' inside double quotes should not be treated as a
	// comment. The tokeniser preserves it, so the dispatched
	// command should include the hash.
	input := "\"bogus#notcomment\"\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	// The unknown command error should contain the hash character,
	// proving it was not stripped as a comment.
	assert.Contains(t, errBuf.String(), "bogus#notcomment")
}

func TestReplComplete_VarsCommand(t *testing.T) {
	// "vars" should appear in command completions.
	_, candidates := replComplete(context.Background(), nil, "va", len("va"))
	assert.Contains(t, candidates, "vars ")
}

func TestReplLoop_Source(t *testing.T) {
	// Write a temp file containing "help" and source it.
	tmp := filepath.Join(t.TempDir(), "script.bpfman")
	require.NoError(t, os.WriteFile(tmp, []byte("help\n"), 0o644))

	input := "source " + tmp + "\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "Available commands:")
}

func TestReplLoop_SourceSharesSession(t *testing.T) {
	// Source a file with an unknown command and verify the error
	// appears, proving the sourced file runs in the same session.
	tmp := filepath.Join(t.TempDir(), "script.bpfman")
	require.NoError(t, os.WriteFile(tmp, []byte("nosuchcmd\n"), 0o644))

	input := "source " + tmp + "\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "nosuchcmd")
}

func TestReplLoop_SourceMissingFile(t *testing.T) {
	input := "source /nonexistent/path/script.bpfman\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "open script")
}

func TestReplLoop_SourceNestedRejected(t *testing.T) {
	// A sourced file that itself contains a source command should
	// be rejected with a clear error.
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.bpfman")
	outer := filepath.Join(dir, "outer.bpfman")
	require.NoError(t, os.WriteFile(inner, []byte("help\n"), 0o644))
	require.NoError(t, os.WriteFile(outer, []byte("source "+inner+"\n"), 0o644))

	input := "source " + outer + "\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "source cannot be used inside a sourced file")
}

func TestReplLoop_SourceNoArgs(t *testing.T) {
	input := "source\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "source requires exactly one file argument")
}

func TestReplComplete_SourceFileCompletion(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "setup.bpfman"), nil, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755))

	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { os.Chdir(orig) })

	tests := []struct {
		name        string
		line        string
		wantReplace int
		wantNonZero bool
		wantAny     []string
	}{
		{
			name:        "source with trailing space lists files",
			line:        "source ",
			wantReplace: 0,
			wantNonZero: true,
			wantAny:     []string{"./setup.bpfman", "./scripts/"},
		},
		{
			name:        "source with partial path",
			line:        "source ./set",
			wantReplace: len("./set"),
			wantNonZero: true,
			wantAny:     []string{"./setup.bpfman"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos := len(tt.line)
			replace, candidates := replComplete(context.Background(), nil, tt.line, pos)

			assert.Equal(t, tt.wantReplace, replace)
			if tt.wantNonZero {
				assert.NotEmpty(t, candidates)
			}
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want)
			}
		})
	}
}
