package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCheckInput wraps replCheckInput over a string source so tests
// can focus on which errors are reported and on which line.
func runCheckInput(t *testing.T, src string) (bool, string) {
	t.Helper()
	r := NewScannerReader(strings.NewReader(src), nil)
	var buf bytes.Buffer
	hadErrors := replCheckInput(r, &buf, "test.bpfman")
	return hadErrors, buf.String()
}

func TestReplCheck_CleanInput(t *testing.T) {
	t.Parallel()

	// --check now runs static analysis after parsing; every
	// $-reference must resolve to a previously-defined name,
	// matching how go vet and pylint catch undefined-name
	// typos. The 'if $x > 0' case defines $x first.
	clean := []string{
		"help",
		"let x = 1\nshow program",
		"let x = 1\nif $x > 0 {\n  bpfman program list\n}",
		"let y <- bpfman program list",
		"# a comment only",
		"",
	}
	for _, src := range clean {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			hadErrors, errOut := runCheckInput(t, src)
			assert.False(t, hadErrors, "unexpected errors: %s", errOut)
			assert.Empty(t, errOut)
		})
	}
}

func TestReplCheck_BrokenSnippets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		input       string
		wantContain string
	}{
		{
			name:        "second assign in let RHS",
			input:       "let x = 1 = 2",
			wantContain: "unexpected '='",
		},
		{
			name:        "bare ident-equals",
			input:       "prog = load",
			wantContain: "unexpected '='",
		},
		{
			name:        "if missing brace",
			input:       "if $x > 0 bpfman",
			wantContain: "expected '{'",
		},
		{
			name:        "unterminated quote",
			input:       `echo "hello`,
			wantContain: "unterminated",
		},
		{
			name:        "malformed varref",
			input:       "echo $prog.",
			wantContain: "expected identifier",
		},
		{
			name:        "bracket form rejected",
			input:       "let x = [foo]",
			wantContain: "brackets are not a DSL form",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hadErrors, errOut := runCheckInput(t, tc.input)
			assert.True(t, hadErrors, "expected errors; got clean output")
			assert.Contains(t, errOut, tc.wantContain)
			assert.Contains(t, errOut, "test.bpfman:")
		})
	}
}

func TestReplCheck_UnterminatedBlockAtEOF(t *testing.T) {
	t.Parallel()

	hadErrors, errOut := runCheckInput(t, "if $x > 0 {\n  let y = 1")
	assert.True(t, hadErrors)
	assert.Contains(t, errOut, "unterminated block")
}

func TestReplCheck_ReportsMultipleStaticIssues(t *testing.T) {
	t.Parallel()

	// Static analysis (Check) accumulates issues and reports
	// every undefined reference, not just the first. Parse
	// errors still bail on the first because the parser
	// cannot meaningfully continue past a syntax error.
	src := "print $a\nprint $b\n"
	hadErrors, errOut := runCheckInput(t, src)
	assert.True(t, hadErrors)
	assert.Contains(t, errOut, "undefined variable: a")
	assert.Contains(t, errOut, "undefined variable: b")
}

func TestReplCheck_LinePrefixTracksParserPosition(t *testing.T) {
	t.Parallel()

	// The parser emits errors with embedded LINE:COL prefixes;
	// replCheckInput strips them and uses the parser's line
	// for the file:line: rendering. A 'help' on line 1, a
	// blank line 2, then the offending 'let x = 1 = 2' on
	// line 3 should report at line 3, not line 1.
	src := "help\n\nlet x = 1 = 2\n"
	hadErrors, errOut := runCheckInput(t, src)
	assert.True(t, hadErrors)
	assert.Contains(t, errOut, "test.bpfman:3:")
}

// TestReplCheck_SyntaxGallery is a smoke test that the shipped
// emacs/syntax-gallery.bpfman example parses cleanly under --check.
// The gallery is the reference source for the REPL's surface syntax;
// if this regresses the refactor has lost coverage somewhere.
func TestReplCheck_SyntaxGallery(t *testing.T) {
	t.Parallel()

	path, err := filepath.Abs("../../emacs/syntax-gallery.bpfman")
	require.NoError(t, err)
	f, err := openScriptReader(path)
	require.NoError(t, err)
	defer f.Close()
	var buf bytes.Buffer
	hadErrors := replCheckInput(f, &buf, path)
	assert.False(t, hadErrors, "syntax gallery reports errors:\n%s", buf.String())
}
