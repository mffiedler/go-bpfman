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
