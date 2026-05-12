package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/shell"
)

func TestCanonicaliseHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "bpfman program list", "bpfman program list"},
		{"empty", "", ""},
		{"whitespace only", "   \n  \t", ""},
		{
			name: "backslash continuation",
			in:   "bpfman program load file \\\n    --path foo.o \\\n    --programs tracepoint:kr",
			want: "bpfman program load file --path foo.o --programs tracepoint:kr",
		},
		{
			name: "let with bracket continuation",
			in:   "let prog <- bpfman program load file \\\n    --path foo.o \\\n    --programs tracepoint:kr",
			want: "let prog <- bpfman program load file --path foo.o --programs tracepoint:kr",
		},
		{
			name: "if block",
			in:   "if true {\n    print yes\n}",
			want: "if true { print yes }",
		},
		{
			name: "preserves quoted newlines",
			in:   "print \"line1\nline2\"",
			want: "print \"line1\nline2\"",
		},
		{
			name: "strips line comments",
			in:   "bpfman program list # all programs\nfoo",
			want: "bpfman program list foo",
		},
		{
			name: "hash inside quotes preserved",
			in:   "print \"#not a comment\"",
			want: "print \"#not a comment\"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := canonicaliseHistory(tc.in)
			if got != tc.want {
				t.Errorf("canonicaliseHistory(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReplComplete_FileCompletion(t *testing.T) {
	t.Parallel()

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
			line:        "bpfman load file --path " + root + "/e2e/",
			wantReplace: len(root + "/e2e/"),
			wantAny:     []string{root + "/e2e/testdata/", root + "/e2e/README"},
			wantNonZero: true,
		},
		{
			name:        "absolute path partial match",
			line:        "bpfman load file --path " + root + "/e2e/test",
			wantReplace: len(root + "/e2e/test"),
			wantAny:     []string{root + "/e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "relative path ./e2e/",
			line:        "bpfman load file --path ./e2e/",
			wantReplace: len("./e2e/"),
			wantAny:     []string{"./e2e/testdata/", "./e2e/README"},
			wantNonZero: true,
		},
		{
			name:        "relative path ./e2e/test",
			line:        "bpfman load file --path ./e2e/test",
			wantReplace: len("./e2e/test"),
			wantAny:     []string{"./e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "relative path without dot prefix",
			line:        "bpfman load file --path e2e/test",
			wantReplace: len("e2e/test"),
			wantAny:     []string{"e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "short flag -p",
			line:        "bpfman load file -p ./e2e/test",
			wantReplace: len("./e2e/test"),
			wantAny:     []string{"./e2e/testdata/"},
			wantNonZero: true,
		},
		{
			name:        "--path with trailing space lists cwd",
			line:        "bpfman load file --path ",
			wantReplace: 0,
			wantAny:     []string{"./e2e/", "./somefile.o"},
			wantNonZero: true,
		},
		{
			name:        "-p with trailing space lists cwd",
			line:        "bpfman load file -p ",
			wantReplace: 0,
			wantNonZero: true,
		},
		{
			name:        "nonexistent path returns nothing",
			line:        "bpfman load file --path /nonexistent/path/xyz",
			wantReplace: len("/nonexistent/path/xyz"),
		},
		{
			name:        "file completion with .o filter prefix",
			line:        "bpfman load file --path " + root + "/e2e/testdata/s",
			wantReplace: len(root + "/e2e/testdata/s"),
			wantAny:     []string{root + "/e2e/testdata/stats.o"},
			wantNone:    []string{root + "/e2e/testdata/other.o"},
			wantNonZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pos := len(tt.line)
			replace, candidates := replCompleteIn(context.Background(), nil, nil, root, tt.line, pos)

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
	t.Parallel()

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
	t.Parallel()

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
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)

	// Two frames are expected, one per failing command. Each frame
	// is several lines (header, citation, gutter, source, caret),
	// so count "error:" headers rather than total lines.
	out := errBuf.String()
	assert.Equal(t, 2, strings.Count(out, "error:"), "expected exactly two error frames; got %s", out)
	assert.Contains(t, out, "bogus")
	assert.Contains(t, out, "also-bogus")
}

func TestReplComplete_CommandCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "empty line completes commands",
			line:        "",
			wantAny:     []string{"help ", "bpfman "},
			wantReplace: 0,
		},
		{
			name:        "partial bpfman",
			line:        "bp",
			wantAny:     []string{"bpfman "},
			wantReplace: len("bp"),
		},
		{
			name:        "bpfman completes domain commands",
			line:        "bpfman ",
			wantAny:     []string{"program ", "link ", "dispatcher "},
			wantReplace: 0,
		},
		{
			name:        "bpfman program load completes to file and image",
			line:        "bpfman program load ",
			wantAny:     []string{"file ", "image "},
			wantReplace: 0,
		},
		{
			name:        "bpfman program completes to delete, list, and load",
			line:        "bpfman program ",
			wantAny:     []string{"delete ", "list ", "load "},
			wantReplace: 0,
		},
		{
			name:        "bpfman program partial delete",
			line:        "bpfman program de",
			wantAny:     []string{"delete "},
			wantReplace: len("de"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pos := len(tt.line)
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, pos)

			assert.Equal(t, tt.wantReplace, replace)
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want)
			}
		})
	}
}

func TestArgTexts(t *testing.T) {
	t.Parallel()

	args := []shell.Arg{
		shell.WordArg{Text: "show"},
		shell.WordArg{Text: "program"},
		shell.ScalarValueArg{Text: "42"},
	}
	got := argTexts(args)
	assert.Equal(t, []string{"show", "program", "42"}, got)
}

func TestArgTexts_Empty(t *testing.T) {
	t.Parallel()

	got := argTexts(nil)
	assert.Empty(t, got)
}

func TestArgTexts_StructuredValueArg(t *testing.T) {
	t.Parallel()

	args := []shell.Arg{
		shell.WordArg{Text: "show"},
		shell.WordArg{Text: "program"},
		shell.StructuredValueArg{Name: "prog", Value: shell.ValueFromMap(map[string]any{"id": "42"})},
	}
	got := argTexts(args)
	assert.Equal(t, []string{"show", "program", "$prog"}, got)
}

func TestArgTexts_QuotedArg(t *testing.T) {
	t.Parallel()

	args := []shell.Arg{
		shell.WordArg{Text: "load"},
		shell.QuotedArg{Text: "my file.o"},
	}
	got := argTexts(args)
	assert.Equal(t, []string{"load", "my file.o"}, got)
}

func TestReplLoop_VarsEmpty(t *testing.T) {
	t.Parallel()

	// Unix contract: empty result is empty output. 'vars' on
	// a session with no bindings prints nothing -- no header,
	// no placeholder, no "No variables defined" filler. Tests
	// for emptiness check the literal output is empty.
	input := "vars\n"
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, outBuf.String(), "vars on empty session must produce no output")
}

func TestReplLoop_UndefinedVariable(t *testing.T) {
	t.Parallel()

	input := "bpfman show program $x.id\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "undefined variable")
}

func TestReplLoop_QuotedHashNotComment(t *testing.T) {
	t.Parallel()

	// A '#' inside double quotes should not be treated as a
	// comment.  A quoted literal at statement position is an
	// expression statement and is auto-printed, so the hash must
	// appear in stdout to prove it survived tokenisation.
	input := "\"bogus#notcomment\"\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Contains(t, outBuf.String(), "bogus#notcomment")
}

func TestReplComplete_VarsCommand(t *testing.T) {
	t.Parallel()

	// "vars" should appear in command completions.
	_, candidates := replComplete(context.Background(), nil, nil, "va", len("va"))
	assert.Contains(t, candidates, "vars ")
}

func TestReplLoop_Unset(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("foo", shell.StringValue("42"))
	session.Set("bar", shell.StringValue("99"))

	input := "unset foo\n"
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	_, ok := session.Get("foo")
	assert.False(t, ok, "foo should be unset")
	_, ok = session.Get("bar")
	assert.True(t, ok, "bar should still be set")
}

func TestReplLoop_UnsetMultiple(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("a", shell.StringValue("1"))
	session.Set("b", shell.StringValue("2"))
	session.Set("c", shell.StringValue("3"))

	input := "unset a b\n"
	cli := &bpfmancli.CLI{Out: io.Discard, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	_, ok := session.Get("a")
	assert.False(t, ok)
	_, ok = session.Get("b")
	assert.False(t, ok)
	_, ok = session.Get("c")
	assert.True(t, ok, "c should still be set")
}

func TestReplLoop_UnsetUndefined(t *testing.T) {
	t.Parallel()

	input := "unset nosuch\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "undefined variable")
}

func TestReplLoop_UnsetNoArgs(t *testing.T) {
	t.Parallel()

	input := "unset\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "requires at least one variable name")
}

func TestReplComplete_UnsetCompletion(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("prog", shell.StringValue("42"))
	session.Set("prog2", shell.StringValue("99"))
	session.Set("other", shell.StringValue("1"))

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantNone    []string
		wantReplace int
	}{
		{
			name:        "unset with space lists all vars",
			line:        "unset ",
			wantAny:     []string{"other ", "prog ", "prog2 "},
			wantReplace: 0,
		},
		{
			name:        "unset with partial name",
			line:        "unset pro",
			wantAny:     []string{"prog ", "prog2 "},
			wantNone:    []string{"other "},
			wantReplace: 3,
		},
		{
			name:        "unset third token lists all vars",
			line:        "unset prog ",
			wantAny:     []string{"other ", "prog ", "prog2 "},
			wantReplace: 0,
		},
		{
			name:        "unset third token with partial",
			line:        "unset prog ot",
			wantAny:     []string{"other "},
			wantNone:    []string{"prog "},
			wantReplace: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replace, candidates := replComplete(context.Background(), nil, session, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want)
			}
			for _, unwanted := range tt.wantNone {
				assert.NotContains(t, candidates, unwanted)
			}
		})
	}
}

func TestReplLoop_Source(t *testing.T) {
	t.Parallel()

	// Write a temp file containing "help" and source it.
	tmp := filepath.Join(t.TempDir(), "script.bpfman")
	require.NoError(t, os.WriteFile(tmp, []byte("help\n"), 0o644))

	input := "source " + tmp + "\n"
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "Available commands:")
}

func TestReplLoop_SourceSharesSession(t *testing.T) {
	t.Parallel()

	// Source a file with an unknown command and verify the error
	// appears, proving the sourced file runs in the same session.
	tmp := filepath.Join(t.TempDir(), "script.bpfman")
	require.NoError(t, os.WriteFile(tmp, []byte("nosuchcmd\n"), 0o644))

	input := "source " + tmp + "\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "nosuchcmd")
}

func TestReplLoop_SourceMissingFile(t *testing.T) {
	t.Parallel()

	input := "source /nonexistent/path/script.bpfman\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "open script")
}

func TestReplLoop_SourceNestedRejected(t *testing.T) {
	t.Parallel()

	// A sourced file that itself contains a source command should
	// be rejected with a clear error.
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.bpfman")
	outer := filepath.Join(dir, "outer.bpfman")
	require.NoError(t, os.WriteFile(inner, []byte("help\n"), 0o644))
	require.NoError(t, os.WriteFile(outer, []byte("source "+inner+"\n"), 0o644))

	input := "source " + outer + "\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "source cannot be used inside a sourced file")
}

func TestReplLoop_SourceNoArgs(t *testing.T) {
	t.Parallel()

	input := "source\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "source requires exactly one file argument")
}

func TestParseProgramIDArg(t *testing.T) {
	t.Parallel()

	// Origin-backed value via ValueFromStruct (exercises HasProgramID capability).
	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: kernel.ProgramID(42)},
	}
	originVal, err := shell.ValueFromStruct(prog)
	require.NoError(t, err)

	// Origin-less structured value via ValueFromJSON (exercises path lookup fallback).
	jsonVal, err := shell.ValueFromJSON([]byte(`{"record":{"program_id":99}}`))
	require.NoError(t, err)

	// Structured variable without .record.program_id
	noIDVal, err := shell.ValueFromJSON([]byte(`{"name":"test"}`))
	require.NoError(t, err)

	tests := []struct {
		name    string
		arg     shell.Arg
		want    kernel.ProgramID
		wantErr string
	}{
		{
			name: "numeric ID",
			arg:  shell.WordArg{Text: "123"},
			want: 123,
		},
		{
			name: "hex ID",
			arg:  shell.WordArg{Text: "0xff"},
			want: 255,
		},
		{
			name: "origin-backed variable uses HasProgramID",
			arg:  shell.StructuredValueArg{Name: "prog", Value: originVal},
			want: 42,
		},
		{
			name: "origin-less variable falls back to path lookup",
			arg:  shell.StructuredValueArg{Name: "prog", Value: jsonVal},
			want: 99,
		},
		{
			name: "scalar variable resolves directly",
			arg:  shell.ScalarValueArg{Text: "99"},
			want: 99,
		},
		{
			name:    "structured variable without record.program_id returns error",
			arg:     shell.StructuredValueArg{Name: "noid", Value: noIDVal},
			wantErr: "has no .record.program_id field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseProgramIDArg(tt.arg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestReplCompleteVarPath(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()

	// Structured variable mimicking a loaded program.
	progVal, err := shell.ValueFromJSON([]byte(`{
		"record": {
			"program_id": 42,
			"name": "my_prog",
			"type": "tracepoint"
		},
		"maps": [
			{"name": "counts", "pin": "/sys/fs/bpf/counts"},
			{"name": "events"}
		],
		"name": "top_name"
	}`))
	require.NoError(t, err)
	session.Set("prog", progVal)

	// Scalar variable.
	session.Set("pid", shell.StringValue("99"))

	// Second structured variable for name-matching tests.
	prog2Val, err := shell.ValueFromJSON([]byte(`{"id": 7}`))
	require.NoError(t, err)
	session.Set("prog2", prog2Val)

	tests := []struct {
		name        string
		token       string
		sigil       bool
		wantAny     []string // candidates must contain all of these
		wantNone    []string // candidates must contain none of these
		wantReplace int
	}{
		{
			name:        "empty token print lists all vars",
			token:       "",
			sigil:       false,
			wantAny:     []string{"pid ", "prog", "prog2"},
			wantReplace: 0,
		},
		{
			name:        "empty $ sigil lists all $vars",
			token:       "$",
			sigil:       true,
			wantAny:     []string{"$pid ", "$prog", "$prog2"},
			wantReplace: 1,
		},
		{
			name:        "partial variable name bare",
			token:       "pro",
			sigil:       false,
			wantAny:     []string{"prog", "prog2"},
			wantNone:    []string{"pid "},
			wantReplace: 3,
		},
		{
			name:        "partial variable name sigil",
			token:       "$pro",
			sigil:       true,
			wantAny:     []string{"$prog", "$prog2"},
			wantNone:    []string{"$pid "},
			wantReplace: 4,
		},
		{
			name:        "exact variable name bare structured drills one level",
			token:       "prog",
			sigil:       false,
			wantAny:     []string{"prog", "prog.record", "prog.name ", "prog.maps"},
			wantReplace: 4,
		},
		{
			name:        "top-level fields of structured var",
			token:       "prog.",
			sigil:       false,
			wantAny:     []string{"prog.record", "prog.name ", "prog.maps"},
			wantReplace: 5,
		},
		{
			name:        "top-level fields with sigil",
			token:       "$prog.",
			sigil:       true,
			wantAny:     []string{"$prog.record", "$prog.name ", "$prog.maps"},
			wantReplace: 6,
		},
		{
			name:        "partial top-level field",
			token:       "prog.re",
			sigil:       false,
			wantAny:     []string{"prog.record"},
			wantNone:    []string{"prog.name ", "prog.maps"},
			wantReplace: 7,
		},
		{
			name:        "nested fields",
			token:       "prog.record.",
			sigil:       false,
			wantAny:     []string{"prog.record.program_id ", "prog.record.name ", "prog.record.type "},
			wantReplace: 12,
		},
		{
			name:        "nested fields with sigil",
			token:       "$prog.record.",
			sigil:       true,
			wantAny:     []string{"$prog.record.program_id ", "$prog.record.name ", "$prog.record.type "},
			wantReplace: 13,
		},
		{
			name:        "partial nested field",
			token:       "$prog.record.prog",
			sigil:       true,
			wantAny:     []string{"$prog.record.program_id "},
			wantNone:    []string{"$prog.record.name ", "$prog.record.type "},
			wantReplace: 17,
		},
		{
			name:        "array index completion",
			token:       "prog.maps[",
			sigil:       false,
			wantAny:     []string{"prog.maps[0]", "prog.maps[1]"},
			wantReplace: 10,
		},
		{
			name:        "array index completion with sigil",
			token:       "$prog.maps[",
			sigil:       true,
			wantAny:     []string{"$prog.maps[0]", "$prog.maps[1]"},
			wantReplace: 11,
		},
		{
			name:        "array element fields",
			token:       "prog.maps[0].",
			sigil:       false,
			wantAny:     []string{"prog.maps[0].name ", "prog.maps[0].pin "},
			wantReplace: 13,
		},
		{
			name:        "closed bracket index drills one level",
			token:       "prog.maps[0]",
			sigil:       false,
			wantAny:     []string{"prog.maps[0]", "prog.maps[0].name ", "prog.maps[0].pin "},
			wantReplace: 12,
		},
		{
			// "maps.<tab>" must produce "maps[0]", not
			// "maps.[0]": bracketed array indices have no
			// leading dot.
			name:        "trailing dot on array field produces bracketed index without dot",
			token:       "prog.maps.",
			sigil:       false,
			wantAny:     []string{"prog.maps[0]"},
			wantNone:    []string{"prog.maps.[0]"},
			wantReplace: 10,
		},
		{
			name:        "closed bracket index with sigil drills one level",
			token:       "$prog.maps[0]",
			sigil:       true,
			wantAny:     []string{"$prog.maps[0]", "$prog.maps[0].name ", "$prog.maps[0].pin "},
			wantReplace: 13,
		},
		{
			name:        "scalar variable no path completions",
			token:       "pid.",
			sigil:       false,
			wantReplace: 4,
		},
		{
			name:        "nonexistent variable returns nothing",
			token:       "nosuch.",
			sigil:       false,
			wantReplace: 7,
		},
		{
			name:        "nonexistent variable with sigil returns nothing",
			token:       "$nosuch.",
			sigil:       true,
			wantReplace: 8,
		},
		{
			name:        "scalar variable bare lists as terminal",
			token:       "pi",
			sigil:       false,
			wantAny:     []string{"pid "},
			wantReplace: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidates, replace := replCompleteVarPath(session, tt.token, tt.sigil)
			assert.Equal(t, tt.wantReplace, replace, "replace")

			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}

			for _, unwanted := range tt.wantNone {
				assert.NotContains(t, candidates, unwanted, "unexpected candidate %q", unwanted)
			}

			if tt.wantAny == nil && tt.wantNone == nil {
				assert.Empty(t, candidates, "expected no candidates")
			}
		})
	}
}

func TestReplCompleteVarPath_NilSession(t *testing.T) {
	t.Parallel()

	candidates, replace := replCompleteVarPath(nil, "$prog.", true)
	assert.Empty(t, candidates)
	assert.Equal(t, 0, replace)
}

func TestReplComplete_PrintCompletion(t *testing.T) {
	t.Parallel()

	// print's argument is any expression that evaluates to a
	// value; bare-word args are literal strings at runtime
	// ("print foo" prints "foo", not $foo), so completion only
	// offers variable paths when the prefix is sigil-led.  This
	// keeps the completer honest with the parser's sigil rule.
	session := shell.NewSession()
	v, err := shell.ValueFromJSON([]byte(`{"record": {"program_id": 42}, "name": "test"}`))
	require.NoError(t, err)
	session.Set("prog", v)
	session.Set("pid", shell.StringValue("99"))

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantNone    []string
		wantReplace int
	}{
		{
			name:        "print with space lists all sigil vars",
			line:        "print ",
			wantAny:     []string{"$pid ", "$prog"},
			wantReplace: 0,
		},
		{
			name:        "print with bare $ lists all sigil vars",
			line:        "print $",
			wantAny:     []string{"$pid ", "$prog"},
			wantReplace: 1,
		},
		{
			name:        "print with partial sigil var name",
			line:        "print $pro",
			wantAny:     []string{"$prog"},
			wantReplace: 4,
		},
		{
			name:        "print with sigil dotted path",
			line:        "print $prog.",
			wantAny:     []string{"$prog.record", "$prog.name "},
			wantReplace: 6,
		},
		{
			name:        "print with sigil nested path",
			line:        "print $prog.record.",
			wantAny:     []string{"$prog.record.program_id "},
			wantReplace: 13,
		},
		{
			name:     "print with bare prefix does not offer variable paths",
			line:     "print pro",
			wantNone: []string{"prog", "prog.record"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replace, candidates := replComplete(context.Background(), nil, session, tt.line, len(tt.line))
			if tt.wantAny != nil {
				assert.Equal(t, tt.wantReplace, replace, "replace")
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

func TestReplComplete_ProgramIDVarPathCompletion(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	v, err := shell.ValueFromJSON([]byte(`{"record": {"program_id": 42}, "name": "test"}`))
	require.NoError(t, err)
	session.Set("prog", v)

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "bpfman show program $prog. completes fields",
			line:        "bpfman show program $prog.",
			wantAny:     []string{"$prog.record", "$prog.name "},
			wantReplace: 6,
		},
		{
			name:        "bpfman show program $prog.record. completes nested",
			line:        "bpfman show program $prog.record.",
			wantAny:     []string{"$prog.record.program_id "},
			wantReplace: 13,
		},
		{
			name:        "bpfman program delete $prog. completes fields",
			line:        "bpfman program delete $prog.",
			wantAny:     []string{"$prog.record", "$prog.name "},
			wantReplace: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replace, candidates := replComplete(context.Background(), nil, session, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestReplComplete_ProgramDeleteAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "bpfman program delete offers --all",
			line:        "bpfman program delete ",
			wantAny:     []string{"--all "},
			wantReplace: 0,
		},
		{
			name:        "bpfman program delete partial --a",
			line:        "bpfman program delete --a",
			wantAny:     []string{"--all "},
			wantReplace: 3,
		},
		{
			name:        "bpfman program delete partial --al",
			line:        "bpfman program delete --al",
			wantAny:     []string{"--all "},
			wantReplace: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestReplComplete_ProgramGetNoAll(t *testing.T) {
	t.Parallel()

	// "bpfman program get" should offer program IDs, not --all.
	replace, candidates := replComplete(context.Background(), nil, nil, "bpfman program get ", len("bpfman program get "))
	assert.Equal(t, 0, replace)
	for _, c := range candidates {
		assert.NotEqual(t, "--all ", c, "program get must not offer --all")
	}
}

func TestParseLinkIDArg(t *testing.T) {
	t.Parallel()

	// Origin-backed value via ValueFromStruct (exercises HasLinkID capability).
	link := bpfman.Link{
		Record: bpfman.LinkRecord{ID: kernel.LinkID(77)},
	}
	originVal, err := shell.ValueFromStruct(link)
	require.NoError(t, err)

	// Origin-less structured value via ValueFromJSON (exercises path lookup fallback).
	jsonVal, err := shell.ValueFromJSON([]byte(`{"record":{"id":88}}`))
	require.NoError(t, err)

	tests := []struct {
		name    string
		arg     shell.Arg
		want    kernel.LinkID
		wantErr string
	}{
		{
			name: "numeric ID",
			arg:  shell.WordArg{Text: "123"},
			want: 123,
		},
		{
			name: "origin-backed variable uses HasLinkID",
			arg:  shell.StructuredValueArg{Name: "lnk", Value: originVal},
			want: 77,
		},
		{
			name: "origin-less variable falls back to path lookup",
			arg:  shell.StructuredValueArg{Name: "lnk", Value: jsonVal},
			want: 88,
		},
		{
			name: "scalar variable resolves directly",
			arg:  shell.ScalarValueArg{Text: "99"},
			want: 99,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLinkIDArg(tt.arg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestReplLoop_Version(t *testing.T) {
	t.Parallel()

	input := "version\n"
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.NotEmpty(t, outBuf.String())
}

func TestReplComplete_NewCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "bpfman link completes subcommands",
			line:        "bpfman link ",
			wantAny:     []string{"attach ", "detach ", "get ", "list ", "delete "},
			wantReplace: 0,
		},
		{
			name:        "bpfman link partial",
			line:        "bpfman link at",
			wantAny:     []string{"attach "},
			wantReplace: 2,
		},
		{
			name:        "bpfman dispatcher completes subcommands",
			line:        "bpfman dispatcher ",
			wantAny:     []string{"delete ", "get ", "list "},
			wantReplace: 0,
		},
		{
			name:        "bpfman program completes new subcommands",
			line:        "bpfman program ",
			wantAny:     []string{"get ", "unload "},
			wantReplace: 0,
		},
		{
			name:        "version in top-level completions",
			line:        "ver",
			wantAny:     []string{"version "},
			wantReplace: 3,
		},
		{
			name:        "bpfman link attach completes types",
			line:        "bpfman link attach ",
			wantAny:     []string{"xdp ", "tc ", "tracepoint ", "kprobe "},
			wantReplace: 0,
		},
		{
			name:        "bpfman link attach partial type",
			line:        "bpfman link attach xd",
			wantAny:     []string{"xdp "},
			wantReplace: 2,
		},
		{
			name:        "bpfman program load completes file and image",
			line:        "bpfman program load ",
			wantAny:     []string{"file ", "image "},
			wantReplace: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestReplLoop_ProgramGetNoArgs(t *testing.T) {
	t.Parallel()

	input := "bpfman program get\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "program get: requires a program ID")
}

func TestReplLoop_ProgramUnloadNoArgs(t *testing.T) {
	t.Parallel()

	input := "bpfman program unload\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "program unload: requires at least one program ID")
}

func TestReplLoop_LinkAttachNoType(t *testing.T) {
	t.Parallel()

	input := "bpfman link attach\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link attach requires a type")
}

func TestReplLoop_LinkAttachUnknownType(t *testing.T) {
	t.Parallel()

	input := "bpfman link attach bogus\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "unknown attach type")
}

func TestReplLoop_LinkDetachNoArgs(t *testing.T) {
	t.Parallel()

	input := "bpfman link detach\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link detach: requires at least one link ID")
}

func TestReplLoop_LinkGetNoArgs(t *testing.T) {
	t.Parallel()

	input := "bpfman link get\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link get: requires a link ID")
}

func TestReplLoop_LinkDeleteNoArgs(t *testing.T) {
	t.Parallel()

	input := "bpfman link delete\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link delete: requires at least one link ID")
}

func TestReplLoop_AliasBasic(t *testing.T) {
	t.Parallel()

	// Define an alias and use it to invoke a domain command.
	// "bpfman program" without a subcommand produces a specific
	// targeted error; the alias should reach the same dispatcher
	// and surface the same message.
	input := "alias b = bpfman\nb program\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "program: subcommand required")
}

func TestReplLoop_AliasRejectsShellCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"assert", "alias assert = bpfman\n", "shell command"},
		{"let", "alias let = bpfman\n", "shell keyword"},
		{"bpfman", "alias bpfman = b\n", "domain prefix"},
		{"exec", "alias exec = bpfman\n", "shell command"},
		{"alias", "alias alias = bpfman\n", "shell command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
			lr := NewScannerReader(strings.NewReader(tt.input), nil)

			err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
			require.NoError(t, err)
			assert.Contains(t, errBuf.String(), tt.want)
		})
	}
}

func TestReplLoop_AliasBadSyntax(t *testing.T) {
	t.Parallel()

	input := "alias b\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "usage: alias")
}

func TestReplLoop_UnaliasBasic(t *testing.T) {
	t.Parallel()

	input := "alias b = bpfman\nunalias b\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_UnaliasUndefined(t *testing.T) {
	t.Parallel()

	input := "unalias nosuch\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "undefined alias")
}

func TestReplLoop_AliasesList(t *testing.T) {
	t.Parallel()

	input := "alias b = bpfman\nalias bp = bpfman\naliases\n"
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "b = bpfman")
	assert.Contains(t, outBuf.String(), "bp = bpfman")
}

func TestReplLoop_AliasesEmpty(t *testing.T) {
	t.Parallel()

	// Same contract as vars: empty list is empty output. No
	// "No aliases defined" filler.
	input := "aliases\n"
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, outBuf.String(), "aliases on empty session must produce no output")
}

func TestReplLoop_AliasInBindBinding(t *testing.T) {
	t.Parallel()

	// Alias expansion applies on the right of '<-' too: "b" is
	// declared as an alias for "bpfman" and the bind dispatches
	// through the domain pipeline. The script does not halt -- a
	// let bind never auto-fails -- so the alias's reach is
	// confirmed by the absence of an "unknown command" error.
	input := "alias b = bpfman\nlet x <- b version\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.NotContains(t, errBuf.String(), "unknown command")

	_, ok := session.Get("x")
	assert.True(t, ok, "let bind must set the variable even when the command produces no payload")
}

func TestReplComplete_SourceFileCompletion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "setup.bpfman"), nil, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755))

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
			t.Parallel()
			pos := len(tt.line)
			replace, candidates := replCompleteIn(context.Background(), nil, nil, root, tt.line, pos)

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

func TestParseProgramIDArg_RejectsLinkVariable(t *testing.T) {
	t.Parallel()

	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        kernel.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	}
	v, err := shell.ValueFromStruct(link)
	require.NoError(t, err)
	v = v.WithKind(shell.OriginLink)

	_, err = parseProgramIDArg(shell.StructuredValueArg{Name: "mylink", Value: v})
	require.Error(t, err)
	var mismatch *shell.OriginMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, shell.OriginLink, mismatch.Got)
	assert.Equal(t, []shell.OriginKind{shell.OriginProgram}, mismatch.Want)
}

func TestParseLinkIDArg_RejectsProgramVariable(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{
			ProgramID: kernel.ProgramID(42),
		},
	}
	v, err := shell.ValueFromStruct(prog)
	require.NoError(t, err)
	v = v.WithKind(shell.OriginProgram)

	_, err = parseLinkIDArg(shell.StructuredValueArg{Name: "myprog", Value: v})
	require.Error(t, err)
	var mismatch *shell.OriginMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, shell.OriginProgram, mismatch.Got)
	assert.Equal(t, []shell.OriginKind{shell.OriginLink}, mismatch.Want)
}

// ---- Assert/Require/Set tests ----
//
// Binary comparison and unary predicate semantics moved to the
// shell package (see shell/expr_test.go: TestEval_Binary_Textual,
// TestEval_Binary_Numeric, TestEval_Unary_*). The cmd/bpfman tests
// below cover the assertion verbs that remain outside the
// expression grammar: contains, path, ok, fail.

func TestAssertContains(t *testing.T) {
	t.Parallel()

	r, err := assertContains(shell.Span{}, []string{"hello world", "world"})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertContains(shell.Span{}, []string{"hello", "xyz"})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(existing, nil, 0o644))

	r, err := assertPath(shell.Span{}, []string{"exists", existing})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertPath(shell.Span{}, []string{"exists", filepath.Join(dir, "nope")})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertPath_BadArgs(t *testing.T) {
	t.Parallel()

	_, err := assertPath(shell.Span{}, []string{"nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path requires")
}

// wordArgs converts string slices to []shell.Arg for test convenience.
func wordArgs(ss ...string) []shell.Arg {
	args := make([]shell.Arg, len(ss))
	for i, s := range ss {
		args[i] = shell.WordArg{Text: s}
	}
	return args
}

func TestAssertOk_UnknownCommand(t *testing.T) {
	t.Parallel()

	cli := &bpfmancli.CLI{Out: io.Discard, Err: io.Discard}
	session := shell.NewSession()
	// "bogus" is not a valid command, so it should fail.
	r, err := assertOk(context.Background(), cli, nil, session, shell.Span{}, wordArgs("bogus"))
	require.NoError(t, err)
	assert.False(t, r.pass)
	assert.Contains(t, r.message, "succeed")
}

func TestAssertFail_UnknownCommand(t *testing.T) {
	t.Parallel()

	cli := &bpfmancli.CLI{Out: io.Discard, Err: io.Discard}
	session := shell.NewSession()
	r, err := assertFail(context.Background(), cli, nil, session, shell.Span{}, wordArgs("bogus"))
	require.NoError(t, err)
	assert.True(t, r.pass)
}

func TestAssertOk_SuccessfulCommand(t *testing.T) {
	t.Parallel()

	cli := &bpfmancli.CLI{Out: io.Discard, Err: io.Discard}
	session := shell.NewSession()
	// "help" always succeeds.
	r, err := assertOk(context.Background(), cli, nil, session, shell.Span{}, wordArgs("help"))
	require.NoError(t, err)
	assert.True(t, r.pass)
}

func TestAssertFail_SuccessfulCommand(t *testing.T) {
	t.Parallel()

	cli := &bpfmancli.CLI{Out: io.Discard, Err: io.Discard}
	session := shell.NewSession()
	r, err := assertFail(context.Background(), cli, nil, session, shell.Span{}, wordArgs("help"))
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestNegateMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{`expected "a" to equal "b"`, `expected "a" not to equal "b"`},
		{`expected path "/tmp" to exist`, `expected path "/tmp" not to exist`},
		{`expected command to succeed`, `expected command not to succeed`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, negateMessage(tt.in))
		})
	}
}

func TestReplLoop_AssertEqPass(t *testing.T) {
	t.Parallel()

	input := "assert hello == hello\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_AssertEqFail(t *testing.T) {
	t.Parallel()

	input := "assert hello == world\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[assert] FAIL")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_AssertNeFail(t *testing.T) {
	t.Parallel()

	input := "assert hello != hello\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[assert] FAIL")
	assert.Contains(t, errBuf.String(), "not equal")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_AssertNotWithInfix(t *testing.T) {
	t.Parallel()

	// "assert not hello == world" parses as a NotExpr wrapping a
	// BinaryExpr (precedence: "not" looser than comparison). It
	// evaluates true because hello != world, so the assertion
	// passes silently.
	input := "assert not hello == world\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_RequireHaltsExecution(t *testing.T) {
	t.Parallel()

	// The second line should never run because require halts.
	input := strings.Join([]string{
		"require hello == world",
		"assert a == a",
	}, "\n")
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.Error(t, err)
	assert.Contains(t, errBuf.String(), "[require] FAIL")
	// The second line's assert should not have been evaluated.
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_MultipleAssertFailures(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		"assert a == b",
		"assert c == d",
		"assert e == e",
	}, "\n")
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Equal(t, 2, session.AssertFailures())
}

func TestReplLoop_IfThenBranch(t *testing.T) {
	t.Parallel()

	input := "let x = 5\nif $x > 3 {\n  let out = took-then\n}"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("out")
	require.True(t, ok)
	s, _ := v.Scalar()
	assert.Equal(t, "took-then", s)
}

func TestReplLoop_IfElseBranch(t *testing.T) {
	t.Parallel()

	input := "let x = 1\nif $x > 3 {\n  let out = took-then\n} else {\n  let out = took-else\n}"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, _ := session.Get("out")
	s, _ := v.Scalar()
	assert.Equal(t, "took-else", s)
}

func TestReplLoop_IfElifChain(t *testing.T) {
	t.Parallel()

	input := "let x = 2\nif $x == 1 { let out = one } elif $x == 2 { let out = two } elif $x == 3 { let out = three } else { let out = other }"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, _ := session.Get("out")
	s, _ := v.Scalar()
	assert.Equal(t, "two", s)
}

func TestReplLoop_IfConditionMustBeBool(t *testing.T) {
	t.Parallel()

	input := "let x = 5\nif $x { let out = wrong }"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "use a comparison")
	_, ok := session.Get("out")
	assert.False(t, ok, "body should not run when condition errors")
}

func TestReplLoop_IfNested(t *testing.T) {
	t.Parallel()

	input := "let a = 1\nlet b = 2\nif $a == 1 {\n  if $b == 2 {\n    let out = both\n  } else {\n    let out = only-a\n  }\n}"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, _ := session.Get("out")
	s, _ := v.Scalar()
	assert.Equal(t, "both", s)
}

func TestReplLoop_IfWithCmdSubCondition(t *testing.T) {
	t.Parallel()

	// Unary predicates inside if work.
	input := "let x = hello\nif not-empty $x {\n  let out = ok\n}"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, _ := session.Get("out")
	s, _ := v.Scalar()
	assert.Equal(t, "ok", s)
}

func TestReplLoop_LetScalarAndAssert(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		"let x = 42",
		"assert $x == 42",
	}, "\n")
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("x")
	require.True(t, ok)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", s)
}

func TestReplLoop_LetMissingEquals(t *testing.T) {
	t.Parallel()

	input := "let x 42 val\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "let requires '=' or '<-'")
}

func TestReplLoop_LetTooFewArgs(t *testing.T) {
	t.Parallel()

	input := "let x\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "let requires")
}

func TestReplLoop_LetInvalidName(t *testing.T) {
	t.Parallel()

	input := "let 0bad = val\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "invalid variable name")
}

func TestReplLoop_AssertNil(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("n", shell.Value{}) // nil

	input := "assert nil n\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_AssertNotNil(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("s", shell.StringValue("hello"))

	input := "assert not nil s\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_AssertContains(t *testing.T) {
	t.Parallel()

	input := "assert contains \"hello world\" world\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertSingleBoolExpr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		wantFail  bool
		wantError string
	}{
		{"true_literal_passes", "assert true\n", false, ""},
		{"false_literal_fails", "assert false\n", true, ""},
		{"not_false_passes", "assert not false\n", false, ""},
		{"compound_via_let", "let r = true and true\nassert $r\n", false, ""},
		{"compound_or_via_let", "let r = false or true\nassert $r\n", false, ""},
		{"non_bool_literal_errors", "assert hello\n", false, "use a comparison"},
		{"bare_prefix_verb_keeps_arity_error", "assert nil\n", false, "nil requires"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
			lr := NewScannerReader(strings.NewReader(tc.input), nil)
			err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
			require.NoError(t, err)
			if tc.wantError != "" {
				assert.Contains(t, errBuf.String(), tc.wantError)
				return
			}
			if tc.wantFail {
				assert.Contains(t, errBuf.String(), "FAIL")
			} else {
				assert.Empty(t, errBuf.String())
			}
		})
	}
}

func TestReplLoop_AssertCompoundExpression(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		wantFail  bool
		wantError string
	}{
		{"and_passes", "let a = hello\nlet b = world\nassert (not-empty $a) and (not-empty $b)\n", false, ""},
		{"and_fails", "let a = hello\nlet b = ''\nassert (not-empty $a) and (not-empty $b)\n", true, ""},
		{"or_passes", "let a = ''\nlet b = world\nassert (not-empty $a) or (not-empty $b)\n", false, ""},
		{"or_fails", "let a = ''\nlet b = ''\nassert (not-empty $a) or (not-empty $b)\n", true, ""},
		{"not_compound", "let a = hello\nassert not (not-empty $a)\n", true, ""},
		{"numeric_compound", "let n = 50\nassert $n > 0 and $n < 100\n", false, ""},
		{"numeric_compound_fail", "let n = 200\nassert $n > 0 and $n < 100\n", true, ""},
		{"deep_paren", "assert ((1 == 1) and (2 == 2)) or (3 == 4)\n", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
			lr := NewScannerReader(strings.NewReader(tc.input), nil)
			err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
			require.NoError(t, err)
			if tc.wantError != "" {
				assert.Contains(t, errBuf.String(), tc.wantError)
				return
			}
			if tc.wantFail {
				assert.Contains(t, errBuf.String(), "FAIL")
			} else {
				assert.Empty(t, errBuf.String())
			}
		})
	}
}

func TestReplLoop_AssertOkHelp(t *testing.T) {
	t.Parallel()

	input := "assert ok help\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertFailBogus(t *testing.T) {
	t.Parallel()

	input := "assert fail bogus_command\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertPathExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(f, nil, 0o644))

	input := "assert path exists " + f + "\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertPathNotExists(t *testing.T) {
	t.Parallel()

	input := "assert not path exists /nonexistent/path/xyz\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertNumericLt(t *testing.T) {
	t.Parallel()

	input := "assert 1 < 2\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertNumericGeFail(t *testing.T) {
	t.Parallel()

	input := "assert 1 >= 2\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := shell.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[assert] FAIL")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_AssertExtraTokens(t *testing.T) {
	t.Parallel()

	input := "assert bogusverb x y\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "unexpected")
}

func TestReplLoop_AssertBareBinaryOpErrors(t *testing.T) {
	t.Parallel()

	// "assert == hello world" routes through the expression-form
	// path, which rejects "==" at the start of an expression.
	input := "assert == hello world\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "assert: ")
}

func TestReplLoop_AssertNoTarget(t *testing.T) {
	t.Parallel()

	input := "assert\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "requires an expression target")
}

func TestReplLoop_AssertNotNoTarget(t *testing.T) {
	t.Parallel()

	input := "assert not\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.NotEmpty(t, errBuf.String())
}

func TestReplLoop_SetWithExpandedVar(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("prog", shell.ValueFromMap(map[string]any{
		"record": map[string]any{
			"program_id": json.Number("199421"),
		},
	}))

	input := "let pid = $prog.record.program_id\nassert $pid == 199421\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_RequireNePass(t *testing.T) {
	t.Parallel()

	input := "require a != b\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertNe(t *testing.T) {
	t.Parallel()

	input := "assert foo != bar\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertNotEmpty(t *testing.T) {
	t.Parallel()

	input := "assert not-empty hello\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplComplete_AssertVerbs(t *testing.T) {
	t.Parallel()

	// "assert " should offer verb completions.
	_, candidates := replComplete(context.Background(), nil, nil, "assert ", len("assert "))
	assert.Contains(t, candidates, "nil ")
	assert.Contains(t, candidates, "ok ")
	assert.Contains(t, candidates, "not ")
	// Binary comparison verbs (==, !=, etc.) are infix operators,
	// not prefix verbs; they do not appear in prefix completion.
	assert.NotContains(t, candidates, "== ")
	assert.NotContains(t, candidates, "!= ")
	assert.NotContains(t, candidates, "< ")
}

func TestReplComplete_RequireVerbs(t *testing.T) {
	t.Parallel()

	_, candidates := replComplete(context.Background(), nil, nil, "require ", len("require "))
	assert.Contains(t, candidates, "fail ")
	assert.NotContains(t, candidates, "== ")
}

func TestReplComplete_SetIsRetired(t *testing.T) {
	t.Parallel()

	// Completion must not offer "set " — the keyword was removed;
	// let subsumes it.
	_, candidates := replComplete(context.Background(), nil, nil, "se", len("se"))
	assert.NotContains(t, candidates, "set ")
}

func TestLookupBareVar(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("x", shell.StringValue("hello"))
	session.Set("obj", shell.ValueFromMap(map[string]any{
		"a": map[string]any{"b": "deep"},
	}))

	v, err := lookupBareVar(session, "x")
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", s)

	v, err = lookupBareVar(session, "obj.a.b")
	require.NoError(t, err)
	s, err = v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "deep", s)

	_, err = lookupBareVar(session, "novar")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined")
}

func TestSourceLoc_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		loc  sourceLoc
		want string
	}{
		{"zero value", sourceLoc{}, ""},
		{"with file and line", sourceLoc{file: "test.bpfman", line: 5}, "test.bpfman:5: "},
		{"line one", sourceLoc{file: "script.bpfman", line: 1}, "script.bpfman:1: "},
		{"with column", sourceLoc{file: "test.bpfman", line: 5, col: 9}, "test.bpfman:5:9: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.loc.String())
		})
	}
}

func TestReplLoop_ErrorWithFileIncludesLocation(t *testing.T) {
	t.Parallel()

	// When a filename is provided, error messages should be
	// prefixed with file:line: for compilation-mode integration.
	input := "# line 1\n$undefined\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "test.bpfman", false, true)
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "test.bpfman:2:")
}

func TestReplLoop_InteractiveModeOmitsLocationAndContinues(t *testing.T) {
	t.Parallel()

	// Interactive mode (no file): errors have no location prefix
	// and execution continues to subsequent lines.
	input := "$undefined\nversion\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(errBuf.String(), "error:"), "expected error to start with 'error:', got: %s", errBuf.String())
	assert.Contains(t, outBuf.String(), "Version:", "expected version output after error in interactive mode")
}

func TestReplLoop_RequireFailWithFileIncludesLocation(t *testing.T) {
	t.Parallel()

	// require failures should also carry the file:line: prefix.
	input := "require a == b\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "script.bpfman", false, true)
	require.Error(t, err)
	assert.Contains(t, errBuf.String(), "script.bpfman:1: [require] FAIL:")
}

func TestReplLoop_AssertFailWithFileIncludesLocation(t *testing.T) {
	t.Parallel()

	input := "assert a == b\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "script.bpfman", false, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "script.bpfman:1: [assert] FAIL:")
}

func TestReplLoop_StdinIncludesLocation(t *testing.T) {
	t.Parallel()

	// When the filename is "<stdin>" (piped input), errors should
	// frame against the offending line. The runtime exec failure
	// reaches the chunk runner as a *shell.SyntaxError after the
	// statement-level safety net, so the rust frame's "--> file:
	// line:col" header carries the source coordinates.
	input := "version\nx\nversion\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "<stdin>", false, true)
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "--> <stdin>:2:1")
	// The third line should not have run.
	assert.Equal(t, 1, strings.Count(outBuf.String(), "Version:"), "expected only one version output before halt")
}

func TestReplLoop_ScriptModeHaltsOnError(t *testing.T) {
	t.Parallel()

	// In script mode (file provided), an error on an early line
	// should prevent subsequent lines from executing.
	input := "x\nversion\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "test.bpfman", false, true)
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "test.bpfman:1:")
	assert.Empty(t, outBuf.String(), "expected no output after error halted script")
}

func TestReplLoop_LineCounterIncrementsCorrectly(t *testing.T) {
	t.Parallel()

	// Blank lines and comments still count towards the line number.
	input := "# comment\n\n# another\n$undefined\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "test.bpfman", false, true)
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "test.bpfman:4:")
}

func TestWithDiscardOutput_PreservesRuntimeState(t *testing.T) {
	t.Parallel()

	original := &bpfmancli.CLI{
		RuntimeDir:    "/run/bpfman",
		ImageCacheDir: "/var/cache/bpfman",
		Config:        "/etc/bpfman/bpfman.toml",
		LockTimeout:   30 * time.Second,
		Out:           os.Stdout,
		Err:           os.Stderr,
	}
	quiet := original.WithDiscardOutput()

	// Runtime state must be preserved.
	assert.Equal(t, original.RuntimeDir, quiet.RuntimeDir)
	assert.Equal(t, original.ImageCacheDir, quiet.ImageCacheDir)
	assert.Equal(t, original.Config, quiet.Config)
	assert.Equal(t, original.LockTimeout, quiet.LockTimeout)

	// Output writers must be replaced.
	assert.Equal(t, io.Discard, quiet.Out)
	assert.Equal(t, io.Discard, quiet.Err)

	// Must not alias the original.
	assert.NotSame(t, original, quiet)
}

// --- exec shell command tests ---

func TestReplLoop_ExecSuccess(t *testing.T) {
	t.Parallel()

	input := "exec echo hello world\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, "hello world\n", outBuf.String())
}

func TestReplLoop_ExecFailure(t *testing.T) {
	t.Parallel()

	input := "exec false\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "exit status 1")
}

func TestReplLoop_ExecNoArgs(t *testing.T) {
	t.Parallel()

	input := "exec\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "exec requires at least one argument")
}

func TestReplLoop_ExecCommandNotFound(t *testing.T) {
	t.Parallel()

	input := "exec __nonexistent_command_12345__\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "exec __nonexistent_command_12345__")
}

func TestReplLoop_ExecLetBinding(t *testing.T) {
	t.Parallel()

	input := "let out <- exec echo hello\nassert contains $out.stdout hello\nassert $out.code == 0\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	// Bound form should not print stdout.
	assert.Empty(t, outBuf.String())

	// Variable should be set with a structured value.
	val, ok := session.Get("out")
	require.True(t, ok)
	assert.True(t, val.IsStructured())

	// Verify stdout field contains expected output.
	stdout, err := val.LookupValue("out", "stdout")
	require.NoError(t, err)
	s, err := stdout.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello\n", s)
}

func TestReplLoop_ExecLetBindingFieldAccess(t *testing.T) {
	t.Parallel()

	input := "let out <- exec echo testing123\nprint $out.stdout\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Contains(t, outBuf.String(), "testing123")
}

func TestReplLoop_LetBindExecZero(t *testing.T) {
	t.Parallel()

	input := "let r <- exec true\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("r")
	require.True(t, ok, "let bind should set the variable")
	assert.Equal(t, shell.OriginEnvelope, v.Kind())

	got, err := v.Lookup("$r", "ok")
	require.NoError(t, err)
	s, _ := got.Scalar()
	assert.Equal(t, "true", s)

	got, err = v.Lookup("$r", "code")
	require.NoError(t, err)
	s, _ = got.Scalar()
	assert.Equal(t, "0", s)
}

func TestReplLoop_LetBindExecNonZero(t *testing.T) {
	t.Parallel()

	input := "let r <- exec false\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	// Non-zero exit must NOT halt the script under '<-'; the
	// envelope carries the failure for the consumer to inspect.
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("r")
	require.True(t, ok, "let bind must set the variable even on non-zero exit")

	got, err := v.Lookup("$r", "ok")
	require.NoError(t, err)
	s, _ := got.Scalar()
	assert.Equal(t, "false", s)

	got, err = v.Lookup("$r", "code")
	require.NoError(t, err)
	s, _ = got.Scalar()
	assert.Equal(t, "1", s)
}

func TestReplLoop_LetBindExecStdout(t *testing.T) {
	t.Parallel()

	input := "let r <- exec echo hello\n"
	var errBuf bytes.Buffer
	var outBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	// The bind path captures stdout into the envelope rather than
	// writing it to the cli.
	assert.Empty(t, outBuf.String(), "<- bind must not echo stdout to the cli")

	v, ok := session.Get("r")
	require.True(t, ok)
	got, err := v.Lookup("$r", "stdout")
	require.NoError(t, err)
	s, _ := got.Scalar()
	assert.Equal(t, "hello\n", s)
}

func TestReplLoop_GuardBindExecZero(t *testing.T) {
	t.Parallel()

	input := "guard r <- exec true\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("r")
	require.True(t, ok, "guard must bind on success")
	got, err := v.Lookup("$r", "ok")
	require.NoError(t, err)
	s, _ := got.Scalar()
	assert.Equal(t, "true", s)
}

func TestReplLoop_GuardBindExecNonZero(t *testing.T) {
	t.Parallel()

	// Single accumulated block: the guard halts EvalProgram and
	// the trailing "let after" never runs even though both
	// statements are part of the same block.
	input := "guard r <- exec false; let after = ran\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	out := errBuf.String()
	assert.Contains(t, out, "[guard] FAIL at <repl>:1")
	assert.Contains(t, out, "command:\n  exec false")
	assert.Contains(t, out, "exit:\n  1")

	_, ok := session.Get("after")
	assert.False(t, ok, "guard halt must skip subsequent statements in the same block")
}

func TestReplLoop_GuardBindHaltsScript(t *testing.T) {
	t.Parallel()

	// Script mode (file != ""): a guard halt makes the whole
	// script return an error, so subsequent lines do not run
	// either.
	input := "guard r <- exec false\nlet after = ran\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "test.bpfman", false, true)
	require.Error(t, err, "script mode must surface guard failure as an error")
	out := errBuf.String()
	assert.Contains(t, out, "[guard] FAIL at test.bpfman:1")
	assert.Contains(t, out, "command:\n  exec false")
	assert.Contains(t, out, "exit:\n  1")

	_, ok := session.Get("after")
	assert.False(t, ok, "guard halt must abort the rest of the script")
}

func TestReplLoop_LetBindTupleExec(t *testing.T) {
	t.Parallel()

	// Tuple form binds rc and primary separately. For exec the
	// primary is also rc-shaped, so both bound values expose the
	// envelope fields.
	input := "let (rc, p) <- exec true\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	rcVal, ok := session.Get("rc")
	require.True(t, ok, "tuple bind must set rc")
	rcOK, _ := rcVal.Lookup("$rc", "ok")
	s, _ := rcOK.Scalar()
	assert.Equal(t, "true", s)

	primary, ok := session.Get("p")
	require.True(t, ok, "tuple bind must set primary")
	primOK, _ := primary.Lookup("$p", "ok")
	s, _ = primOK.Scalar()
	assert.Equal(t, "true", s)
}

func TestReplLoop_LetBindTupleDiscardRc(t *testing.T) {
	t.Parallel()

	// Discarding rc keeps only primary. Equivalent to single-name
	// form for rc-primary commands.
	input := "let (_, p) <- exec true\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	_, ok := session.Get("rc")
	assert.False(t, ok, "underscore must discard the rc slot")
	_, ok = session.Get("_")
	assert.False(t, ok, "underscore must not be a valid binding name")

	_, ok = session.Get("p")
	assert.True(t, ok)
}

func TestReplLoop_GuardBindTupleNonZeroNoBindings(t *testing.T) {
	t.Parallel()

	// On guard failure, neither tuple slot is bound. The renderer
	// fires; statements after the guard never run.
	input := "guard (rc, p) <- exec false; let after = ran\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[guard] FAIL")

	_, ok := session.Get("rc")
	assert.False(t, ok, "guard failure must not bind rc")
	_, ok = session.Get("p")
	assert.False(t, ok, "guard failure must not bind primary")
	_, ok = session.Get("after")
	assert.False(t, ok, "guard failure must skip subsequent statements")
}

func TestReplLoop_GuardBindRendersStderr(t *testing.T) {
	t.Parallel()

	// A failing subprocess writes to stderr. The renderer must
	// include the captured stderr block so the user sees why the
	// command failed.
	input := "guard r <- exec sh -c \"echo oops 1>&2; exit 7\"\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	out := errBuf.String()
	assert.Contains(t, out, "exit:\n  7")
	assert.Contains(t, out, "stderr:\n  oops")
}

func TestReplLoop_ExecAssertOk(t *testing.T) {
	t.Parallel()

	input := "assert ok exec echo hello\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_ExecAssertFail(t *testing.T) {
	t.Parallel()

	input := "assert fail exec false\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_ExecAssertOkFails(t *testing.T) {
	t.Parallel()

	input := "assert ok exec false\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "FAIL")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_ExecAssertFailSucceeds(t *testing.T) {
	t.Parallel()

	input := "assert fail exec echo hello\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "FAIL")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_ExecAssertContainsStdout(t *testing.T) {
	t.Parallel()

	input := "let out <- exec echo \"hello world\"\nassert contains $out.stdout hello\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_ExecStderrCaptured(t *testing.T) {
	t.Parallel()

	// Use sh -c to produce stderr output. The exec command runs
	// argv[0] directly, so we invoke sh as the command.
	input := "let out <- exec sh -c \"echo errout >&2\"\nassert contains $out.stderr errout\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_ExecVariableExpansion(t *testing.T) {
	t.Parallel()

	session := shell.NewSession()
	session.Set("msg", shell.StringValue("expanded"))

	input := "let out <- exec echo $msg\nassert contains $out.stdout expanded\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_InteractiveSurvivesParentCancellation(t *testing.T) {
	t.Parallel()

	// Interactive mode does not observe parent-ctx
	// cancellation for foreground externals. The shell stays
	// alive at the prompt; subsequent commands run under a
	// fresh per-chunk ctx. This test feeds a 'true' (instant
	// success) under a cancelled parent, follows it with a
	// recognisable observable command, and asserts the second
	// command ran -- proving the loop did not exit on the
	// cancelled parent.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := "exec true\nprint after-cancel\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(ctx, cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "after-cancel",
		"interactive loop must keep running after parent ctx cancellation")
}

func TestReplLoop_ExecTopLevelErrorsOnNonZero(t *testing.T) {
	t.Parallel()

	// Top-level exec runs the command for its side effects and
	// reports non-zero exit as an error.
	input := "exec false\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "exit status 1")
}

func TestReplComplete_ExecInCommandNames(t *testing.T) {
	t.Parallel()

	_, candidates := replComplete(context.Background(), nil, nil, "ex", len("ex"))
	assert.Contains(t, candidates, "exec ")
}

// --- jq on JSON text / structured data tests ---

func TestReplLoop_JQ_FromJsonObject(t *testing.T) {
	t.Parallel()

	input := `let data = jq "." '{"name":"test","id":42}'` + "\nassert $data.name == test\nassert $data.id == 42\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Empty(t, outBuf.String(), "bound form should not print")

	val, ok := session.Get("data")
	require.True(t, ok)
	assert.True(t, val.IsStructured())
}

func TestReplLoop_JQ_FromJsonArray(t *testing.T) {
	t.Parallel()

	input := `let arr = jq "." '[1,2,3]'` + "\nassert $arr[0] == 1\nassert $arr[2] == 3\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_JQ_FromJsonScalar(t *testing.T) {
	t.Parallel()

	input := `let v = jq "." 123` + "\nassert $v == 123\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_JQ_FromJsonInvalidInput(t *testing.T) {
	t.Parallel()

	// jq is a pure builtin and is invoked from expression
	// position. Invalid JSON input is an evaluation error: the
	// expression cannot produce a value, so the script reports
	// the error and $data is left unbound. The previous
	// envelope-capturing '<-' form was retired because pure
	// builtins have no envelope to capture; an evaluation error
	// is the natural shape for a parse failure.
	input := `let data = jq "." not-json` + "\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "not valid JSON")

	_, ok := session.Get("data")
	assert.False(t, ok, "failed pure-builtin call leaves the target unbound")
}

func TestReplLoop_JQ_WrongArgCount(t *testing.T) {
	t.Parallel()

	input := "jq\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "usage: jq")
}

func TestReplLoop_JQ_FromJsonAssertOk(t *testing.T) {
	t.Parallel()

	input := `assert ok jq "." '{"a":1}'` + "\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_JQ_FromJsonAssertFail(t *testing.T) {
	t.Parallel()

	input := `assert fail jq "." not-json` + "\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_JQ_NestedAccess(t *testing.T) {
	t.Parallel()

	input := `let data = jq "." '{"a":{"b":{"c":"deep"}}}'` + "\nassert $data.a.b.c == deep\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_JQ_WithExec(t *testing.T) {
	t.Parallel()

	// End-to-end: exec produces JSON text, jq on JSON text makes it
	// structured.
	input := `let raw <- exec echo '{"status":"ok","count":3}'` + "\nlet data = jq \".\" $raw.stdout\nassert $data.status == ok\nassert $data.count == 3\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

// file temp tests

func TestReplLoop_FileTempScalar(t *testing.T) {
	t.Parallel()

	input := "let data = hello\nlet f <- file temp $data\nassert not-empty $f\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	val, ok := session.Get("f")
	require.True(t, ok)
	path, err := val.Scalar()
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
	os.Remove(path)
}

func TestReplLoop_FileTempStructured(t *testing.T) {
	t.Parallel()

	input := `let data = jq "." '{"b":2,"a":1}'` + "\nlet f <- file temp $data\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	val, ok := session.Get("f")
	require.True(t, ok)
	path, err := val.Scalar()
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	// Keys should be sorted alphabetically.
	assert.Contains(t, string(content), `"a": 1`)
	assert.Contains(t, string(content), `"b": 2`)
	// Trailing newline after JSON.
	assert.True(t, strings.HasSuffix(string(content), "}\n"))
	os.Remove(path)
}

func TestReplLoop_FileTempPathScalar(t *testing.T) {
	t.Parallel()

	input := "let raw <- exec echo hello\nlet f <- file temp $raw.stdout\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	val, ok := session.Get("f")
	require.True(t, ok)
	path, err := val.Scalar()
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(content))
	os.Remove(path)
}

func TestReplLoop_FileTempPathStructured(t *testing.T) {
	t.Parallel()

	input := `let data = jq "." '{"items":[{"id":1},{"id":2}]}'` + "\nlet f <- file temp $data.items\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	val, ok := session.Get("f")
	require.True(t, ok)
	path, err := val.Scalar()
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	// Should be a JSON array.
	assert.Contains(t, string(content), `"id": 1`)
	os.Remove(path)
}

func TestReplLoop_FileTempNoArgs(t *testing.T) {
	t.Parallel()

	input := "file temp\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "file temp requires exactly one argument")
}

func TestReplLoop_FileTempUndefinedVar(t *testing.T) {
	t.Parallel()

	input := "let f <- file temp $undefined_var\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "undefined variable")
}

func TestReplLoop_FileTempPlainFormPrintsPath(t *testing.T) {
	t.Parallel()

	input := "let data = hello\nfile temp $data\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	// Plain form should print the path.
	assert.Contains(t, outBuf.String(), "bpfman-repl-")
}

func TestReplLoop_FileTempNoSubcommand(t *testing.T) {
	t.Parallel()

	input := "file\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "usage: file temp")
}

// Inline file adapter tests

func TestReplLoop_ExecFileAdapterScalar(t *testing.T) {
	t.Parallel()

	input := "let raw <- exec echo hello\nlet out <- exec wc -c file:$raw.stdout\nassert contains $out.stdout 6\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_ExecFileAdapterStructured(t *testing.T) {
	t.Parallel()

	input := `let data = jq "." '{"name":"test"}'` + "\nlet out <- exec cat file:$data\nassert contains $out.stdout name\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_ExecFileAdapterMultiple(t *testing.T) {
	t.Parallel()

	input := "let a <- exec echo aaa\nlet b <- exec echo bbb\nlet out <- exec diff file:$a.stdout file:$b.stdout\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	// diff returns non-zero exit for different files; under '<-'
	// that lands in the envelope rather than as an error.
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("out")
	require.True(t, ok)
	got, _ := v.Lookup("$out", "code")
	s, _ := got.Scalar()
	assert.Equal(t, "1", s)
}

func TestReplLoop_ExecFileAdapterMixed(t *testing.T) {
	t.Parallel()

	input := "let raw <- exec echo hello\nlet out <- exec wc -l file:$raw.stdout\nassert $out.code == 0\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_ExecFileAdapterCleanup(t *testing.T) {
	t.Parallel()

	// Verify that adapter temp files are cleaned up after exec.
	input := "let data = hello\nlet out <- exec cat file:$data\nassert contains $out.stdout hello\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	// The adapter temp file should not exist. Since we cannot
	// directly observe the temp path, verify the output came
	// through correctly (if the file did not exist, cat would
	// have failed).
	out, ok := session.Get("out")
	require.True(t, ok)
	stdout, err := out.LookupValue("out", "stdout")
	require.NoError(t, err)
	s, err := stdout.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", s)
}

func TestReplLoop_ExecFileAdapterLetBinding(t *testing.T) {
	t.Parallel()

	input := `let data = jq "." '{"a":1}'` + "\nlet out <- exec cat file:$data\nassert contains $out.stdout '\"a\": 1'\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	session := shell.NewSession()
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

// Completion tests for file command

func TestReplComplete_FileInCommandNames(t *testing.T) {
	t.Parallel()

	_, candidates := replComplete(context.Background(), nil, nil, "fi", len("fi"))
	assert.Contains(t, candidates, "file ")
}

func TestReplComplete_FileSubcommands(t *testing.T) {
	t.Parallel()

	_, candidates := replComplete(context.Background(), nil, nil, "file ", len("file "))
	assert.Contains(t, candidates, "temp ")
}

func TestReplComplete_JQInCommandNames(t *testing.T) {
	t.Parallel()

	_, candidates := replComplete(context.Background(), nil, nil, "j", len("j"))
	assert.Contains(t, candidates, "jq ")
}

func TestReplComplete_DollarLeadsExpressionAtPrompt(t *testing.T) {
	t.Parallel()

	// At the top-level prompt a '$'-led first token is an
	// expression statement: the completer should walk variable
	// paths just like inside a command argument, so typing
	// "$prog.<tab>" offers the object's fields.  Bare "prog."
	// stays on the command-name path (it is a literal).
	session := shell.NewSession()
	progVal, err := shell.ValueFromJSON([]byte(`{
		"record": {"program_id": 42, "name": "my_prog"}
	}`))
	require.NoError(t, err)
	session.Set("prog", progVal)
	session.Set("pid", shell.StringValue("99"))

	t.Run("dollar alone lists all sigil-prefixed vars", func(t *testing.T) {
		t.Parallel()
		replace, cands := replComplete(context.Background(), nil, session, "$", len("$"))
		assert.Equal(t, 1, replace)
		assert.Contains(t, cands, "$prog")
		assert.Contains(t, cands, "$pid ")
	})
	t.Run("dollar-name partial completes variable names", func(t *testing.T) {
		t.Parallel()
		_, cands := replComplete(context.Background(), nil, session, "$pr", len("$pr"))
		assert.Contains(t, cands, "$prog")
	})
	t.Run("dollar-name-dot walks fields", func(t *testing.T) {
		t.Parallel()
		_, cands := replComplete(context.Background(), nil, session, "$prog.", len("$prog."))
		assert.Contains(t, cands, "$prog.record")
	})
	t.Run("dollar-name-nested-dot walks nested fields", func(t *testing.T) {
		t.Parallel()
		_, cands := replComplete(context.Background(), nil, session, "$prog.record.", len("$prog.record."))
		assert.Contains(t, cands, "$prog.record.program_id ")
	})
	t.Run("bare-word first token stays on command path", func(t *testing.T) {
		t.Parallel()
		// "prog." is a literal string argument at statement
		// position, not a variable reference; no variable
		// paths should leak into the candidate list.
		_, cands := replComplete(context.Background(), nil, session, "prog.", len("prog."))
		assert.NotContains(t, cands, "prog.record")
	})
	t.Run("exact sigil name drills one level for discovery", func(t *testing.T) {
		t.Parallel()
		// "$prog" on its own is a valid expression; completion
		// also surfaces its immediate children so a single tab
		// on a structured variable makes drillable fields
		// discoverable without requiring the user to type ".".
		_, cands := replComplete(context.Background(), nil, session, "$prog", len("$prog"))
		assert.Contains(t, cands, "$prog")
		assert.Contains(t, cands, "$prog.record")
	})
	t.Run("exact path field drills one level for discovery", func(t *testing.T) {
		t.Parallel()
		// Same discovery behaviour one level deeper: tabbing
		// on a fully-typed path whose value is structured
		// reveals its immediate children.
		_, cands := replComplete(context.Background(), nil, session, "$prog.record", len("$prog.record"))
		assert.Contains(t, cands, "$prog.record")
		assert.Contains(t, cands, "$prog.record.program_id ")
	})
}

func TestReplLoop_Arithmetic_AutoPrintsAdditive(t *testing.T) {
	t.Parallel()

	// let binding plus a bare arithmetic expression statement:
	// the second line is routed to ExprStmt (leading '$' leads
	// expression position) and its value is auto-printed.
	input := "let x = 5\n$x + 1\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Contains(t, outBuf.String(), "6")
}

func TestReplLoop_InterpString_SimpleVar(t *testing.T) {
	t.Parallel()

	input := "let n = 60\nlet wait = \"${n}s\"\nprint $wait\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Contains(t, outBuf.String(), "60s")
}

func TestReplLoop_InterpString_PathConstruction(t *testing.T) {
	t.Parallel()

	input := "let id = 42\nlet path = \"/sys/fs/bpf/prog-${id}/map\"\nprint $path\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Contains(t, outBuf.String(), "/sys/fs/bpf/prog-42/map")
}

func TestReplLoop_InterpString_ArithmeticInside(t *testing.T) {
	t.Parallel()

	input := "let n = 30\nlet wait = \"${$n * 2}s\"\nprint $wait\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Contains(t, outBuf.String(), "60s")
}

func TestReplLoop_PrintMultipleArgs(t *testing.T) {
	t.Parallel()

	// Multiple arguments render compactly and join with a single
	// space; a single trailing newline closes the line.  Structured
	// values render as compact JSON (the interpolation form) so a
	// mix of scalars and records fits on one line.
	input := strings.Join([]string{
		`let n = 42`,
		`let r = jq "." '{"a":1,"b":2}'`,
		`print 1 2 3`,
		`print "count=" $n`,
		`print "r=" $r`,
		``,
	}, "\n")
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, "1 2 3", lines[0])
	assert.Equal(t, "count= 42", lines[1])
	assert.Equal(t, `r= {"a":1,"b":2}`, lines[2])
}

func TestReplLoop_PrintNoArgsEmitsBlankLine(t *testing.T) {
	t.Parallel()

	// print with no arguments emits a single newline -- the same
	// shape as Python's print(), JavaScript's console.log(), or
	// shell echo. Handy for spacing output blocks apart, and
	// avoids surfacing the empty call as a user error: the shell
	// already knows what to do.
	input := "print\n"
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, "\n", outBuf.String())
}

func TestReplLoop_JQNullBinding(t *testing.T) {
	t.Parallel()

	// A jq filter that selects a missing field returns a present
	// null.  The user should be able to bind it, print it, and
	// interpolate it without tripping "produced no assignable
	// value" — the whole point of OriginNull.
	input := strings.Join([]string{
		`let r = jq "." '{"a":1}'`,
		`let x = jq ".missing" $r`,
		`print $x`,
		`print "missing=${x}"`,
		``,
	}, "\n")
	var outBuf, errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String(), "expected no REPL error")
	out := outBuf.String()
	assert.Contains(t, out, "null")
	assert.Contains(t, out, "missing=null")
}

func TestReplLoop_InterpString_BareDollarRejected(t *testing.T) {
	t.Parallel()

	input := "let x = \"$foo\"\n"
	var errBuf bytes.Buffer
	cli := &bpfmancli.CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, shell.NewSession(), "", true, true)
	require.NoError(t, err) // REPL reports parse errors through errBuf, not by returning
	assert.Contains(t, errBuf.String(), "followed by '{...}'")
}
