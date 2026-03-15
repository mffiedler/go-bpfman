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

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
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
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, pos)

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

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
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
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, pos)

			assert.Equal(t, tt.wantReplace, replace)
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want)
			}
		})
	}
}

func TestArgTexts(t *testing.T) {
	args := []replang.Arg{
		replang.WordArg{Text: "show"},
		replang.WordArg{Text: "program"},
		replang.ScalarValueArg{Text: "42"},
	}
	got := argTexts(args)
	assert.Equal(t, []string{"show", "program", "42"}, got)
}

func TestArgTexts_Empty(t *testing.T) {
	got := argTexts(nil)
	assert.Empty(t, got)
}

func TestArgTexts_StructuredValueArg(t *testing.T) {
	args := []replang.Arg{
		replang.WordArg{Text: "show"},
		replang.WordArg{Text: "program"},
		replang.StructuredValueArg{Name: "prog", Value: replang.ValueFromMap(map[string]any{"id": "42"})},
	}
	got := argTexts(args)
	assert.Equal(t, []string{"show", "program", "$prog"}, got)
}

func TestArgTexts_QuotedArg(t *testing.T) {
	args := []replang.Arg{
		replang.WordArg{Text: "load"},
		replang.QuotedArg{Text: "my file.o"},
	}
	got := argTexts(args)
	assert.Equal(t, []string{"load", "my file.o"}, got)
}

func TestReplLoop_VarsEmpty(t *testing.T) {
	input := "vars\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "No variables defined")
}

func TestReplLoop_AssignmentToNonAssignable(t *testing.T) {
	// "help" returns no value, so assigning should produce an error.
	input := "let x = help\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "command produced no result to assign")
}

func TestReplLoop_UndefinedVariable(t *testing.T) {
	input := "show program $x.id\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
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

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	// The unknown command error should contain the hash character,
	// proving it was not stripped as a comment.
	assert.Contains(t, errBuf.String(), "bogus#notcomment")
}

func TestReplComplete_VarsCommand(t *testing.T) {
	// "vars" should appear in command completions.
	_, candidates := replComplete(context.Background(), nil, nil, "va", len("va"))
	assert.Contains(t, candidates, "vars ")
}

func TestReplLoop_Unset(t *testing.T) {
	session := replang.NewSession()
	session.Set("foo", replang.StringValue("42"))
	session.Set("bar", replang.StringValue("99"))

	input := "unset foo\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	_, ok := session.Get("foo")
	assert.False(t, ok, "foo should be unset")
	_, ok = session.Get("bar")
	assert.True(t, ok, "bar should still be set")
}

func TestReplLoop_UnsetMultiple(t *testing.T) {
	session := replang.NewSession()
	session.Set("a", replang.StringValue("1"))
	session.Set("b", replang.StringValue("2"))
	session.Set("c", replang.StringValue("3"))

	input := "unset a b\n"
	cli := &CLI{Out: io.Discard, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	_, ok := session.Get("a")
	assert.False(t, ok)
	_, ok = session.Get("b")
	assert.False(t, ok)
	_, ok = session.Get("c")
	assert.True(t, ok, "c should still be set")
}

func TestReplLoop_UnsetUndefined(t *testing.T) {
	input := "unset nosuch\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "undefined variable")
}

func TestReplLoop_UnsetNoArgs(t *testing.T) {
	input := "unset\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "requires at least one variable name")
}

func TestReplComplete_UnsetCompletion(t *testing.T) {
	session := replang.NewSession()
	session.Set("prog", replang.StringValue("42"))
	session.Set("prog2", replang.StringValue("99"))
	session.Set("other", replang.StringValue("1"))

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
	// Write a temp file containing "help" and source it.
	tmp := filepath.Join(t.TempDir(), "script.bpfman")
	require.NoError(t, os.WriteFile(tmp, []byte("help\n"), 0o644))

	input := "source " + tmp + "\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
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

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "nosuchcmd")
}

func TestReplLoop_SourceMissingFile(t *testing.T) {
	input := "source /nonexistent/path/script.bpfman\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
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

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "source cannot be used inside a sourced file")
}

func TestReplLoop_SourceNoArgs(t *testing.T) {
	input := "source\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "source requires exactly one file argument")
}

func TestResolveProgramIDArg(t *testing.T) {
	session := replang.NewSession()

	// Structured variable with .record.program_id
	structuredVal, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":42}}`))
	require.NoError(t, err)
	session.Set("prog", structuredVal)

	// Scalar variable
	session.Set("pid", replang.StringValue("99"))

	// Structured variable without .record.program_id
	noIDVal, err := replang.ValueFromJSON([]byte(`{"name":"test"}`))
	require.NoError(t, err)
	session.Set("noid", noIDVal)

	tests := []struct {
		name    string
		arg     string
		want    string
		wantErr string
	}{
		{
			name: "numeric ID passes through",
			arg:  "123",
			want: "123",
		},
		{
			name: "hex ID passes through",
			arg:  "0xff",
			want: "0xff",
		},
		{
			name: "$variable resolves record.program_id",
			arg:  "$prog",
			want: "42",
		},
		{
			name: "$variable with explicit path resolves to scalar",
			arg:  "$prog.record.program_id",
			want: "42",
		},
		{
			name: "$scalar variable resolves directly",
			arg:  "$pid",
			want: "99",
		},
		{
			name:    "$undefined variable returns error",
			arg:     "$nosuch",
			wantErr: "undefined variable",
		},
		{
			name:    "$structured variable without record.program_id returns error",
			arg:     "$noid",
			wantErr: "has no .record.program_id field",
		},
		{
			name:    "bare word without $ returns error",
			arg:     "prog",
			wantErr: "not a valid program ID or variable reference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveProgramIDArg(session, tt.arg)
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

func TestResolveProgramIDArgs(t *testing.T) {
	session := replang.NewSession()

	structuredVal, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":42}}`))
	require.NoError(t, err)
	session.Set("prog", structuredVal)
	session.Set("pid", replang.StringValue("99"))

	// Mixed numeric, $variable, and flags.
	got, err := resolveProgramIDArgs(session, []string{"123", "$prog", "$pid", "-r"})
	require.NoError(t, err)
	assert.Equal(t, []string{"123", "42", "99", "-r"}, got)
}

// TestResolveProgramIDArgs_ShowProgram verifies that the show-program
// resolution pattern only resolves the first positional argument,
// leaving sub-view names like "links" and "maps" untouched.
func TestResolveProgramIDArgs_ShowProgram(t *testing.T) {
	session := replang.NewSession()

	structuredVal, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":42}}`))
	require.NoError(t, err)
	session.Set("prog", structuredVal)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "$variable with links sub-view",
			args: []string{"$prog", "links"},
			want: []string{"42", "links"},
		},
		{
			name: "$variable with maps sub-view",
			args: []string{"$prog", "maps"},
			want: []string{"42", "maps"},
		},
		{
			name: "$variable with paths sub-view and output flag",
			args: []string{"$prog", "paths", "-o", "json"},
			want: []string{"42", "paths", "-o", "json"},
		},
		{
			name: "numeric ID with sub-view",
			args: []string{"123", "links"},
			want: []string{"123", "links"},
		},
		{
			name: "$variable alone",
			args: []string{"$prog"},
			want: []string{"42"},
		},
		{
			name: "output flag only",
			args: []string{"-o", "json"},
			want: []string{"-o", "json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the resolution pattern from replShowProgram:
			// resolve only the first non-flag argument.
			args := make([]string, len(tt.args))
			copy(args, tt.args)
			if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
				resolved, err := resolveProgramIDArg(session, args[0])
				require.NoError(t, err)
				args = append([]string{resolved}, args[1:]...)
			}
			assert.Equal(t, tt.want, args)
		})
	}
}

// TestResolveProgramIDArgs_DeleteProgram verifies that the delete
// pattern resolves all positional arguments as program IDs, including
// mixed numeric and variable forms, while leaving flags untouched.
func TestResolveProgramIDArgs_DeleteProgram(t *testing.T) {
	session := replang.NewSession()

	p1, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":10}}`))
	require.NoError(t, err)
	p2, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":20}}`))
	require.NoError(t, err)
	session.Set("a", p1)
	session.Set("b", p2)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "multiple $variables",
			args: []string{"$a", "$b"},
			want: []string{"10", "20"},
		},
		{
			name: "mixed numeric and $variables with flag",
			args: []string{"99", "$a", "$b", "-r"},
			want: []string{"99", "10", "20", "-r"},
		},
		{
			name: "single $variable with recursive flag",
			args: []string{"$a", "-r"},
			want: []string{"10", "-r"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveProgramIDArgs(session, tt.args)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReplCompleteVarPath(t *testing.T) {
	session := replang.NewSession()

	// Structured variable mimicking a loaded program.
	progVal, err := replang.ValueFromJSON([]byte(`{
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
	session.Set("pid", replang.StringValue("99"))

	// Second structured variable for name-matching tests.
	prog2Val, err := replang.ValueFromJSON([]byte(`{"id": 7}`))
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
			name:        "empty token dump lists all vars",
			token:       "",
			sigil:       false,
			wantAny:     []string{"pid ", "prog.", "prog2."},
			wantReplace: 0,
		},
		{
			name:        "empty $ sigil lists all $vars",
			token:       "$",
			sigil:       true,
			wantAny:     []string{"$pid ", "$prog.", "$prog2."},
			wantReplace: 1,
		},
		{
			name:        "partial variable name bare",
			token:       "pro",
			sigil:       false,
			wantAny:     []string{"prog.", "prog2."},
			wantNone:    []string{"pid "},
			wantReplace: 3,
		},
		{
			name:        "partial variable name sigil",
			token:       "$pro",
			sigil:       true,
			wantAny:     []string{"$prog.", "$prog2."},
			wantNone:    []string{"$pid "},
			wantReplace: 4,
		},
		{
			name:        "exact variable name bare structured",
			token:       "prog",
			sigil:       false,
			wantAny:     []string{"prog."},
			wantReplace: 4,
		},
		{
			name:        "top-level fields of structured var",
			token:       "prog.",
			sigil:       false,
			wantAny:     []string{"prog.record.", "prog.name ", "prog.maps["},
			wantReplace: 5,
		},
		{
			name:        "top-level fields with sigil",
			token:       "$prog.",
			sigil:       true,
			wantAny:     []string{"$prog.record.", "$prog.name ", "$prog.maps["},
			wantReplace: 6,
		},
		{
			name:        "partial top-level field",
			token:       "prog.re",
			sigil:       false,
			wantAny:     []string{"prog.record."},
			wantNone:    []string{"prog.name ", "prog.maps["},
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
			wantAny:     []string{"prog.maps[0].", "prog.maps[1]."},
			wantReplace: 10,
		},
		{
			name:        "array index completion with sigil",
			token:       "$prog.maps[",
			sigil:       true,
			wantAny:     []string{"$prog.maps[0].", "$prog.maps[1]."},
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
	candidates, replace := replCompleteVarPath(nil, "$prog.", true)
	assert.Empty(t, candidates)
	assert.Equal(t, 0, replace)
}

func TestReplComplete_DumpCompletion(t *testing.T) {
	session := replang.NewSession()
	v, err := replang.ValueFromJSON([]byte(`{"record": {"program_id": 42}, "name": "test"}`))
	require.NoError(t, err)
	session.Set("prog", v)
	session.Set("pid", replang.StringValue("99"))

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "dump with space lists all vars",
			line:        "dump ",
			wantAny:     []string{"pid ", "prog."},
			wantReplace: 0,
		},
		{
			name:        "dump with partial var name",
			line:        "dump pro",
			wantAny:     []string{"prog."},
			wantReplace: 3,
		},
		{
			name:        "dump with dotted path",
			line:        "dump prog.",
			wantAny:     []string{"prog.record.", "prog.name "},
			wantReplace: 5,
		},
		{
			name:        "dump with nested path",
			line:        "dump prog.record.",
			wantAny:     []string{"prog.record.program_id "},
			wantReplace: 12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replace, candidates := replComplete(context.Background(), nil, session, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestReplComplete_ProgramIDVarPathCompletion(t *testing.T) {
	session := replang.NewSession()
	v, err := replang.ValueFromJSON([]byte(`{"record": {"program_id": 42}, "name": "test"}`))
	require.NoError(t, err)
	session.Set("prog", v)

	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "show program $prog. completes fields",
			line:        "show program $prog.",
			wantAny:     []string{"$prog.record.", "$prog.name "},
			wantReplace: 6,
		},
		{
			name:        "show program $prog.record. completes nested",
			line:        "show program $prog.record.",
			wantAny:     []string{"$prog.record.program_id "},
			wantReplace: 13,
		},
		{
			name:        "program delete $prog. completes fields",
			line:        "program delete $prog.",
			wantAny:     []string{"$prog.record.", "$prog.name "},
			wantReplace: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replace, candidates := replComplete(context.Background(), nil, session, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestProgramDeleteCmd_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cmd     ProgramDeleteCmd
		wantErr string
	}{
		{
			name:    "neither --all nor IDs",
			cmd:     ProgramDeleteCmd{},
			wantErr: "provide at least one program ID or use --all",
		},
		{
			name: "both --all and IDs",
			cmd: ProgramDeleteCmd{
				All:        true,
				ProgramIDs: []ProgramID{{Value: 1}},
			},
			wantErr: "--all and explicit program IDs are mutually exclusive",
		},
		{
			name: "--all alone is valid",
			cmd:  ProgramDeleteCmd{All: true},
		},
		{
			name: "IDs alone is valid",
			cmd:  ProgramDeleteCmd{ProgramIDs: []ProgramID{{Value: 1}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cmd.Validate()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestReplComplete_ProgramDeleteAll(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "program delete offers --all",
			line:        "program delete ",
			wantAny:     []string{"--all "},
			wantReplace: 0,
		},
		{
			name:        "program delete partial --a",
			line:        "program delete --a",
			wantAny:     []string{"--all "},
			wantReplace: 3,
		},
		{
			name:        "program delete partial --al",
			line:        "program delete --al",
			wantAny:     []string{"--all "},
			wantReplace: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestReplComplete_ProgramGetNoAll(t *testing.T) {
	// "program get" should offer program IDs, not --all.
	replace, candidates := replComplete(context.Background(), nil, nil, "program get ", len("program get "))
	assert.Equal(t, 0, replace)
	for _, c := range candidates {
		assert.NotEqual(t, "--all ", c, "program get must not offer --all")
	}
}

func TestResolveVarRefs(t *testing.T) {
	session := replang.NewSession()

	structuredVal, err := replang.ValueFromJSON([]byte(`{"record":{"program_id":42}}`))
	require.NoError(t, err)
	session.Set("prog", structuredVal)

	// resolveVarRefs should pass non-$ tokens through unchanged,
	// including bare words that are not valid program IDs.
	got, err := resolveVarRefs(session, []string{"--iface", "eth0", "$prog", "123"})
	require.NoError(t, err)
	assert.Equal(t, []string{"--iface", "eth0", "42", "123"}, got)
}

func TestResolveVarRefs_UndefinedVar(t *testing.T) {
	session := replang.NewSession()
	_, err := resolveVarRefs(session, []string{"$nosuch"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined variable")
}

func TestResolveLinkIDArg(t *testing.T) {
	session := replang.NewSession()

	// Structured variable with .record.id
	linkVal, err := replang.ValueFromJSON([]byte(`{"record":{"id":77}}`))
	require.NoError(t, err)
	session.Set("lnk", linkVal)

	// Scalar variable
	session.Set("lid", replang.StringValue("88"))

	tests := []struct {
		name    string
		arg     string
		want    string
		wantErr string
	}{
		{
			name: "numeric ID passes through",
			arg:  "123",
			want: "123",
		},
		{
			name: "$variable resolves record.id",
			arg:  "$lnk",
			want: "77",
		},
		{
			name: "$scalar variable resolves directly",
			arg:  "$lid",
			want: "88",
		},
		{
			name:    "bare word returns error",
			arg:     "abc",
			wantErr: "not a valid link ID",
		},
		{
			name:    "undefined variable",
			arg:     "$nosuch",
			wantErr: "undefined variable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveLinkIDArg(session, tt.arg)
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

func TestResolveLinkIDArgs(t *testing.T) {
	session := replang.NewSession()

	linkVal, err := replang.ValueFromJSON([]byte(`{"record":{"id":77}}`))
	require.NoError(t, err)
	session.Set("lnk", linkVal)

	got, err := resolveLinkIDArgs(session, []string{"10", "$lnk", "-r"})
	require.NoError(t, err)
	assert.Equal(t, []string{"10", "77", "-r"}, got)
}

func TestReplLoop_Version(t *testing.T) {
	input := "version\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.NotEmpty(t, outBuf.String())
}

func TestReplComplete_NewCommands(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantAny     []string
		wantReplace int
	}{
		{
			name:        "link completes subcommands",
			line:        "link ",
			wantAny:     []string{"attach ", "detach ", "get ", "list ", "delete "},
			wantReplace: 0,
		},
		{
			name:        "link partial",
			line:        "link at",
			wantAny:     []string{"attach "},
			wantReplace: 2,
		},
		{
			name:        "dispatcher completes subcommands",
			line:        "dispatcher ",
			wantAny:     []string{"delete ", "get ", "list "},
			wantReplace: 0,
		},
		{
			name:        "doctor completes subcommands",
			line:        "doctor ",
			wantAny:     []string{"checkup ", "explain "},
			wantReplace: 0,
		},
		{
			name:        "program completes new subcommands",
			line:        "program ",
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
			name:        "gc in top-level completions",
			line:        "gc",
			wantAny:     []string{"gc "},
			wantReplace: 2,
		},
		{
			name:        "link attach completes types",
			line:        "link attach ",
			wantAny:     []string{"xdp ", "tc ", "tracepoint ", "kprobe "},
			wantReplace: 0,
		},
		{
			name:        "link attach partial type",
			line:        "link attach xd",
			wantAny:     []string{"xdp "},
			wantReplace: 2,
		},
		{
			name:        "load completes image subcommand",
			line:        "load ",
			wantAny:     []string{"file ", "image "},
			wantReplace: 0,
		},
		{
			name:        "program load completes file and image",
			line:        "program load ",
			wantAny:     []string{"file ", "image "},
			wantReplace: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, len(tt.line))
			assert.Equal(t, tt.wantReplace, replace, "replace")
			for _, want := range tt.wantAny {
				assert.Contains(t, candidates, want, "expected candidate %q", want)
			}
		})
	}
}

func TestReplLoop_DoctorExplain(t *testing.T) {
	// "doctor explain" without a rule should list all rules.
	input := "doctor explain\n"
	var outBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: io.Discard}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "Available coherency rules")
}

func TestReplLoop_DoctorExplainUnknown(t *testing.T) {
	input := "doctor explain nosuch-rule\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "unknown rule")
}

func TestReplLoop_DoctorUnknownSubcommand(t *testing.T) {
	input := "doctor bogus\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "unknown doctor subcommand")
}

func TestReplLoop_ProgramGetNoArgs(t *testing.T) {
	input := "program get\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "program get requires a program ID")
}

func TestReplLoop_ProgramUnloadNoArgs(t *testing.T) {
	input := "program unload\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "program unload requires at least one program ID")
}

func TestReplLoop_LinkAttachNoType(t *testing.T) {
	input := "link attach\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link attach requires a type")
}

func TestReplLoop_LinkAttachUnknownType(t *testing.T) {
	input := "link attach bogus\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "unknown attach type")
}

func TestReplLoop_LinkDetachNoArgs(t *testing.T) {
	input := "link detach\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link detach requires at least one link ID")
}

func TestReplLoop_LinkGetNoArgs(t *testing.T) {
	input := "link get\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link get requires a link ID")
}

func TestReplLoop_LinkDeleteNoArgs(t *testing.T) {
	input := "link delete\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "link delete requires at least one link ID")
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
			replace, candidates := replComplete(context.Background(), nil, nil, tt.line, pos)

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

func TestResolveProgramIDArg_RejectsLinkVariable(t *testing.T) {
	session := replang.NewSession()
	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        kernel.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	}
	v, err := replang.ValueFromStruct(link)
	require.NoError(t, err)
	session.Set("mylink", v)

	_, err = resolveProgramIDArg(session, "$mylink")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a program")
}

func TestResolveLinkIDArg_RejectsProgramVariable(t *testing.T) {
	session := replang.NewSession()
	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{
			ProgramID: kernel.ProgramID(42),
		},
	}
	v, err := replang.ValueFromStruct(prog)
	require.NoError(t, err)
	session.Set("myprog", v)

	_, err = resolveLinkIDArg(session, "$myprog")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a link")
}

func TestResolveProgramIDArg_ExplicitPathStillWorks(t *testing.T) {
	session := replang.NewSession()
	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        kernel.LinkID(10),
			ProgramID: kernel.ProgramID(42),
		},
	}
	v, err := replang.ValueFromStruct(link)
	require.NoError(t, err)
	session.Set("mylink", v)

	// Explicit path bypasses the type check.
	got, err := resolveProgramIDArg(session, "$mylink.record.program_id")
	require.NoError(t, err)
	assert.Equal(t, "42", got)
}

// ---- Assert/Require/Set tests ----

func TestAssertEqual(t *testing.T) {
	r, err := assertEqual([]string{"hello", "hello"})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertEqual([]string{"hello", "world"})
	require.NoError(t, err)
	assert.False(t, r.pass)
	assert.Contains(t, r.message, "equal")
}

func TestAssertEqual_WrongArgCount(t *testing.T) {
	_, err := assertEqual([]string{"one"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 arguments")
}

func TestAssertNe(t *testing.T) {
	r, err := assertNe([]string{"a", "b"})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertNe([]string{"a", "a"})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertNil(t *testing.T) {
	session := replang.NewSession()
	session.Set("n", replang.Value{}) // nil value
	session.Set("s", replang.StringValue("hello"))

	r, err := assertNil(session, []string{"n"})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertNil(session, []string{"s"})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertNil_Undefined(t *testing.T) {
	session := replang.NewSession()
	_, err := assertNil(session, []string{"novar"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined")
}

func TestAssertNotEmpty(t *testing.T) {
	r, err := assertNotEmpty([]string{"hello"})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertNotEmpty([]string{""})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertContains(t *testing.T) {
	r, err := assertContains([]string{"hello world", "world"})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertContains([]string{"hello", "xyz"})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertBool(t *testing.T) {
	r, err := assertBool([]string{"true"}, true)
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertBool([]string{"false"}, true)
	require.NoError(t, err)
	assert.False(t, r.pass)

	r, err = assertBool([]string{"false"}, false)
	require.NoError(t, err)
	assert.True(t, r.pass)
}

func TestAssertPath(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(existing, nil, 0o644))

	r, err := assertPath([]string{"exists", existing})
	require.NoError(t, err)
	assert.True(t, r.pass)

	r, err = assertPath([]string{"exists", filepath.Join(dir, "nope")})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestAssertPath_BadArgs(t *testing.T) {
	_, err := assertPath([]string{"nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path requires")
}

func TestAssertNumericCmp(t *testing.T) {
	tests := []struct {
		op   string
		a, b string
		pass bool
	}{
		{"lt", "1", "2", true},
		{"lt", "2", "1", false},
		{"lt", "1", "1", false},
		{"le", "1", "1", true},
		{"le", "2", "1", false},
		{"gt", "2", "1", true},
		{"gt", "1", "2", false},
		{"ge", "1", "1", true},
		{"ge", "0", "1", false},
	}
	for _, tt := range tests {
		t.Run(tt.op+"_"+tt.a+"_"+tt.b, func(t *testing.T) {
			r, err := assertNumericCmp([]string{tt.a, tt.b}, tt.op)
			require.NoError(t, err)
			assert.Equal(t, tt.pass, r.pass)
		})
	}
}

func TestAssertNumericCmp_NonNumeric(t *testing.T) {
	_, err := assertNumericCmp([]string{"abc", "2"}, "lt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a number")

	_, err = assertNumericCmp([]string{"1", "xyz"}, "gt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a number")
}

func TestAssertOk_UnknownCommand(t *testing.T) {
	cli := &CLI{Out: io.Discard, Err: io.Discard}
	session := replang.NewSession()
	// "bogus" is not a valid command, so it should fail.
	r, err := assertOk(context.Background(), cli, nil, session, []string{"bogus"})
	require.NoError(t, err)
	assert.False(t, r.pass)
	assert.Contains(t, r.message, "succeed")
}

func TestAssertFail_UnknownCommand(t *testing.T) {
	cli := &CLI{Out: io.Discard, Err: io.Discard}
	session := replang.NewSession()
	r, err := assertFail(context.Background(), cli, nil, session, []string{"bogus"})
	require.NoError(t, err)
	assert.True(t, r.pass)
}

func TestAssertOk_SuccessfulCommand(t *testing.T) {
	cli := &CLI{Out: io.Discard, Err: io.Discard}
	session := replang.NewSession()
	// "help" always succeeds.
	r, err := assertOk(context.Background(), cli, nil, session, []string{"help"})
	require.NoError(t, err)
	assert.True(t, r.pass)
}

func TestAssertFail_SuccessfulCommand(t *testing.T) {
	cli := &CLI{Out: io.Discard, Err: io.Discard}
	session := replang.NewSession()
	r, err := assertFail(context.Background(), cli, nil, session, []string{"help"})
	require.NoError(t, err)
	assert.False(t, r.pass)
}

func TestNegateMessage(t *testing.T) {
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
			assert.Equal(t, tt.want, negateMessage(tt.in))
		})
	}
}

func TestReplLoop_AssertEqualPass(t *testing.T) {
	input := "assert equal hello hello\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_AssertEqualFail(t *testing.T) {
	input := "assert equal hello world\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[assert] FAIL")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_AssertNotEqual(t *testing.T) {
	input := "assert not equal hello hello\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[assert] FAIL")
	assert.Contains(t, errBuf.String(), "not to equal")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_RequireHaltsExecution(t *testing.T) {
	// The second line should never run because require halts.
	input := strings.Join([]string{
		"require equal hello world",
		"assert equal a a",
	}, "\n")
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.Error(t, err)
	assert.Contains(t, errBuf.String(), "[require] FAIL")
	// The second line's assert should not have been evaluated.
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_MultipleAssertFailures(t *testing.T) {
	input := strings.Join([]string{
		"assert equal a b",
		"assert equal c d",
		"assert equal e e",
	}, "\n")
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Equal(t, 2, session.AssertFailures())
}

func TestReplLoop_SetAndAssert(t *testing.T) {
	input := strings.Join([]string{
		"set x = 42",
		"assert equal $x 42",
	}, "\n")
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())

	v, ok := session.Get("x")
	require.True(t, ok)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", s)
}

func TestReplLoop_SetMissingEquals(t *testing.T) {
	input := "set x 42 val\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "missing '='")
}

func TestReplLoop_SetTooFewArgs(t *testing.T) {
	input := "set x\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "set requires")
}

func TestReplLoop_SetTooManyArgs(t *testing.T) {
	input := "set x = a b\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "exactly one value")
}

func TestReplLoop_SetInvalidName(t *testing.T) {
	input := "set 0bad = val\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "invalid variable name")
}

func TestReplLoop_AssertNil(t *testing.T) {
	session := replang.NewSession()
	session.Set("n", replang.Value{}) // nil

	input := "assert nil n\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_AssertNotNil(t *testing.T) {
	session := replang.NewSession()
	session.Set("s", replang.StringValue("hello"))

	input := "assert not nil s\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_AssertContains(t *testing.T) {
	input := "assert contains \"hello world\" world\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertTrue(t *testing.T) {
	input := "assert true true\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertFalse(t *testing.T) {
	input := "assert false false\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertOkHelp(t *testing.T) {
	input := "assert ok help\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertFailBogus(t *testing.T) {
	input := "assert fail bogus_command\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertPathExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(f, nil, 0o644))

	input := "assert path exists " + f + "\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertPathNotExists(t *testing.T) {
	input := "assert not path exists /nonexistent/path/xyz\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertLt(t *testing.T) {
	input := "assert lt 1 2\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertGeFail(t *testing.T) {
	input := "assert ge 1 2\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)
	session := replang.NewSession()

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "[assert] FAIL")
	assert.Equal(t, 1, session.AssertFailures())
}

func TestReplLoop_AssertUnknownVerb(t *testing.T) {
	input := "assert bogusverb x y\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "unknown assertion verb")
}

func TestReplLoop_AssertNoVerb(t *testing.T) {
	input := "assert\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "expected a verb")
}

func TestReplLoop_AssertNotNoVerb(t *testing.T) {
	input := "assert not\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "expected a verb after")
}

func TestReplLoop_SetWithExpandedVar(t *testing.T) {
	session := replang.NewSession()
	session.Set("prog", replang.ValueFromMap(map[string]any{
		"record": map[string]any{
			"program_id": "199421",
		},
	}))

	input := "set pid = $prog.record.program_id\nassert equal $pid 199421\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, session, "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
	assert.Equal(t, 0, session.AssertFailures())
}

func TestReplLoop_RequireNotEqualPass(t *testing.T) {
	input := "require not equal a b\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertNe(t *testing.T) {
	input := "assert ne foo bar\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplLoop_AssertNotEmpty(t *testing.T) {
	input := "assert not-empty hello\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestReplComplete_AssertVerbs(t *testing.T) {
	// "assert " should offer verb completions.
	_, candidates := replComplete(context.Background(), nil, nil, "assert ", len("assert "))
	assert.Contains(t, candidates, "equal ")
	assert.Contains(t, candidates, "nil ")
	assert.Contains(t, candidates, "ok ")
	assert.Contains(t, candidates, "not ")
}

func TestReplComplete_RequireVerbs(t *testing.T) {
	_, candidates := replComplete(context.Background(), nil, nil, "require ", len("require "))
	assert.Contains(t, candidates, "equal ")
	assert.Contains(t, candidates, "fail ")
}

func TestReplComplete_SetInCommandNames(t *testing.T) {
	_, candidates := replComplete(context.Background(), nil, nil, "se", len("se"))
	assert.Contains(t, candidates, "set ")
}

func TestLookupBareVar(t *testing.T) {
	session := replang.NewSession()
	session.Set("x", replang.StringValue("hello"))
	session.Set("obj", replang.ValueFromMap(map[string]any{
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
	tests := []struct {
		name string
		loc  sourceLoc
		want string
	}{
		{"zero value", sourceLoc{}, ""},
		{"with file and line", sourceLoc{"test.bpfman", 5}, "test.bpfman:5: "},
		{"line one", sourceLoc{"script.bpfman", 1}, "script.bpfman:1: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.loc.String())
		})
	}
}

func TestReplLoop_ErrorWithFileIncludesLocation(t *testing.T) {
	// When a filename is provided, error messages should be
	// prefixed with file:line: for compilation-mode integration.
	input := "# line 1\n$undefined\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "test.bpfman")
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "test.bpfman:2: ")
}

func TestReplLoop_InteractiveModeOmitsLocationAndContinues(t *testing.T) {
	// Interactive mode (no file): errors have no location prefix
	// and execution continues to subsequent lines.
	input := "$undefined\nversion\n"
	var outBuf, errBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(errBuf.String(), "[repl]"), "expected error to start with [repl], got: %s", errBuf.String())
	assert.Contains(t, outBuf.String(), "Version:", "expected version output after error in interactive mode")
}

func TestReplLoop_RequireFailWithFileIncludesLocation(t *testing.T) {
	// require failures should also carry the file:line: prefix.
	input := "require equal a b\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "script.bpfman")
	require.Error(t, err)
	assert.Contains(t, errBuf.String(), "script.bpfman:1: [require] FAIL:")
}

func TestReplLoop_AssertFailWithFileIncludesLocation(t *testing.T) {
	input := "assert equal a b\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "script.bpfman")
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "script.bpfman:1: [assert] FAIL:")
}

func TestReplLoop_StdinIncludesLocation(t *testing.T) {
	// When the filename is "<stdin>" (piped input), errors should
	// carry a <stdin>:line: prefix and halt execution.
	input := "version\nx\nversion\n"
	var outBuf, errBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "<stdin>")
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "<stdin>:2: [repl] error:")
	// The third line should not have run.
	assert.Equal(t, 1, strings.Count(outBuf.String(), "Version:"), "expected only one version output before halt")
}

func TestReplLoop_ScriptModeHaltsOnError(t *testing.T) {
	// In script mode (file provided), an error on an early line
	// should prevent subsequent lines from executing.
	input := "x\nversion\n"
	var outBuf, errBuf bytes.Buffer
	cli := &CLI{Out: &outBuf, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "test.bpfman")
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "test.bpfman:1:")
	assert.Empty(t, outBuf.String(), "expected no output after error halted script")
}

func TestReplLoop_LineCounterIncrementsCorrectly(t *testing.T) {
	// Blank lines and comments still count towards the line number.
	input := "# comment\n\n# another\n$undefined\n"
	var errBuf bytes.Buffer
	cli := &CLI{Out: io.Discard, Err: &errBuf}
	lr := NewScannerReader(strings.NewReader(input), nil)

	err := replLoop(context.Background(), cli, nil, lr, replang.NewSession(), "test.bpfman")
	require.ErrorIs(t, err, errScriptError)
	assert.Contains(t, errBuf.String(), "test.bpfman:4: ")
}
