package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenise(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []Token
		wantErr string
	}{
		{
			name:  "empty input",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only",
			input: "   \t  ",
			want:  nil,
		},
		{
			name:  "single word",
			input: "help",
			want:  []Token{{Kind: TokenWord, Text: "help"}},
		},
		{
			name:  "multiple words",
			input: "show program 123",
			want: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenWord, Text: "123"},
			},
		},
		{
			name:  "flags",
			input: "load file --path foo.o -m app=test",
			want: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "file"},
				{Kind: TokenWord, Text: "--path"},
				{Kind: TokenWord, Text: "foo.o"},
				{Kind: TokenWord, Text: "-m"},
				{Kind: TokenWord, Text: "app=test"},
			},
		},
		{
			name:  "equals embedded in word stays part of word",
			input: "load KEY=VALUE",
			want: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "KEY=VALUE"},
			},
		},
		{
			name:  "standalone equals after identifier",
			input: "prog = load file",
			want: []Token{
				{Kind: TokenWord, Text: "prog"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "file"},
			},
		},
		{
			name:  "bare varref simple",
			input: "show $prog",
			want: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenVarRef, Text: "$prog", VarName: "prog"},
			},
		},
		{
			name:  "bare varref with dotted path",
			input: "show $prog.id",
			want: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
		},
		{
			name:  "bare varref with nested dotted path",
			input: "--program-id $prog.details.kernel_id",
			want: []Token{
				{Kind: TokenWord, Text: "--program-id"},
				{Kind: TokenVarRef, Text: "$prog.details.kernel_id", VarName: "prog", VarPath: "details.kernel_id"},
			},
		},
		{
			name:  "bare varref with array index",
			input: "$prog.maps[0].name",
			want: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps[0].name", VarName: "prog", VarPath: "maps[0].name"},
			},
		},
		{
			name:  "braced varref simple",
			input: "${prog}",
			want: []Token{
				{Kind: TokenVarRef, Text: "${prog}", VarName: "prog"},
			},
		},
		{
			name:  "braced varref with path",
			input: "${prog.id}",
			want: []Token{
				{Kind: TokenVarRef, Text: "${prog.id}", VarName: "prog", VarPath: "id"},
			},
		},
		{
			name:  "braced varref with index",
			input: "${prog.maps[0].name}",
			want: []Token{
				{Kind: TokenVarRef, Text: "${prog.maps[0].name}", VarName: "prog", VarPath: "maps[0].name"},
			},
		},
		{
			name:  "double-quoted string",
			input: `load "hello world"`,
			want: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenQuoted, Text: "hello world"},
			},
		},
		{
			name:  "single-quoted string",
			input: "load 'hello world'",
			want: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenQuoted, Text: "hello world"},
			},
		},
		{
			name:  "dollar is literal inside quotes",
			input: `"$prog.id"`,
			want: []Token{
				{Kind: TokenQuoted, Text: "$prog.id"},
			},
		},
		{
			name:  "comment strips trailing text",
			input: "show program 123 # this is a comment",
			want: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenWord, Text: "123"},
			},
		},
		{
			name:  "hash inside quotes is not a comment",
			input: `load "path#with#hash"`,
			want: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenQuoted, Text: "path#with#hash"},
			},
		},
		{
			name:  "comment only",
			input: "# just a comment",
			want:  nil,
		},
		{
			name:  "mixed line with assignment and varrefs",
			input: "link = link attach --program-id $prog.id",
			want: []Token{
				{Kind: TokenWord, Text: "link"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "link"},
				{Kind: TokenWord, Text: "attach"},
				{Kind: TokenWord, Text: "--program-id"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
		},
		{
			name:  "varref adjacent to word",
			input: "prefix$var",
			want: []Token{
				{Kind: TokenWord, Text: "prefix"},
				{Kind: TokenVarRef, Text: "$var", VarName: "var"},
			},
		},
		{
			name:    "unterminated double quote",
			input:   `"hello`,
			wantErr: `unterminated "-quoted string`,
		},
		{
			name:    "unterminated single quote",
			input:   `'hello`,
			wantErr: `unterminated '-quoted string`,
		},
		{
			name:    "unterminated braced varref",
			input:   "${prog.id",
			wantErr: "unterminated variable reference: missing }",
		},
		{
			name:    "empty dollar",
			input:   "$ ",
			wantErr: "unexpected end of input after $",
		},
		{
			name:    "empty braced varref",
			input:   "${}",
			wantErr: "empty variable reference: ${}",
		},
		{
			name:    "dollar followed by digit",
			input:   "$123",
			wantErr: "invalid variable reference: expected identifier after $",
		},
		{
			name:  "varref with underscore",
			input: "$my_var.field_name",
			want: []Token{
				{Kind: TokenVarRef, Text: "$my_var.field_name", VarName: "my_var", VarPath: "field_name"},
			},
		},

		// Malformed variable reference tests (Step 6: tighten
		// tokeniser invariants).

		// Bare form: trailing dot.
		{
			name:    "bare varref trailing dot at end of input",
			input:   "$prog.",
			wantErr: "expected identifier after '.'",
		},
		{
			name:    "bare varref trailing dot before space",
			input:   "$prog. foo",
			wantErr: "expected identifier after '.'",
		},
		{
			name:    "bare varref dot followed by digit",
			input:   "$prog.123",
			wantErr: "expected identifier after '.'",
		},
		{
			name:    "bare varref trailing dot after path",
			input:   "$prog.maps[0].",
			wantErr: "expected identifier after '.'",
		},

		// Bare form: malformed index.
		{
			name:    "bare varref empty index",
			input:   "$prog[]",
			wantErr: "expected digits inside '[]'",
		},
		{
			name:    "bare varref non-numeric index",
			input:   "$prog[abc]",
			wantErr: "expected digits inside '[]'",
		},
		{
			name:    "bare varref unclosed index",
			input:   "$prog[0",
			wantErr: "expected ']' after index",
		},
		{
			name:    "bare varref unclosed index no digits",
			input:   "$prog[",
			wantErr: "expected digits inside '[]'",
		},

		// Braced form: trailing dot.
		{
			name:    "braced varref trailing dot",
			input:   "${prog.}",
			wantErr: "expected identifier after '.' in ${...}",
		},
		{
			name:    "braced varref empty segment (double dot)",
			input:   "${prog..id}",
			wantErr: "expected identifier after '.' in ${...}",
		},

		// Braced form: malformed index.
		{
			name:    "braced varref non-numeric index",
			input:   "${prog[abc]}",
			wantErr: "expected digits inside '[]' in ${...}",
		},
		{
			name:    "braced varref empty index",
			input:   "${prog[]}",
			wantErr: "expected digits inside '[]' in ${...}",
		},
		{
			name:    "braced varref unclosed index",
			input:   "${prog[0}",
			wantErr: "expected ']' after index in ${...}",
		},

		// Braced form: unexpected characters.
		{
			name:    "braced varref unexpected character in path",
			input:   "${prog!id}",
			wantErr: "unexpected character",
		},
		{
			name:    "braced varref space in path",
			input:   "${prog id}",
			wantErr: "unexpected character",
		},

		// Adapter reference tests.

		{
			name:  "adapter ref bare",
			input: "exec diff file:$x file:$y",
			want: []Token{
				{Kind: TokenWord, Text: "exec"},
				{Kind: TokenWord, Text: "diff"},
				{Kind: TokenAdapterRef, Text: "file:$x", Adapter: "file", VarName: "x"},
				{Kind: TokenAdapterRef, Text: "file:$y", Adapter: "file", VarName: "y"},
			},
		},
		{
			name:  "adapter ref with dotted path",
			input: "file:$raw.stdout",
			want: []Token{
				{Kind: TokenAdapterRef, Text: "file:$raw.stdout", Adapter: "file", VarName: "raw", VarPath: "stdout"},
			},
		},
		{
			name:  "adapter ref with index",
			input: "file:$snap[2]",
			want: []Token{
				{Kind: TokenAdapterRef, Text: "file:$snap[2]", Adapter: "file", VarName: "snap", VarPath: "[2]"},
			},
		},
		{
			name:  "adapter ref braced form",
			input: "file:${data.items[0]}",
			want: []Token{
				{Kind: TokenAdapterRef, Text: "file:${data.items[0]}", Adapter: "file", VarName: "data", VarPath: "items[0]"},
			},
		},
		{
			name:  "file colon without dollar is plain word",
			input: "file:something",
			want: []Token{
				{Kind: TokenWord, Text: "file:something"},
			},
		},
		{
			name:  "file colon with space before dollar is two tokens",
			input: "file: $var",
			want: []Token{
				{Kind: TokenWord, Text: "file:"},
				{Kind: TokenVarRef, Text: "$var", VarName: "var"},
			},
		},
		// Comparison operator tokenisation (Phase 2 infix assertions).
		{
			name:  "== is a single word token, not two assigns",
			input: "assert $a == 1",
			want: []Token{
				{Kind: TokenWord, Text: "assert"},
				{Kind: TokenVarRef, Text: "$a", VarName: "a"},
				{Kind: TokenWord, Text: "=="},
				{Kind: TokenWord, Text: "1"},
			},
		},
		{
			name:  "!= is a single word token",
			input: "assert $a != 1",
			want: []Token{
				{Kind: TokenWord, Text: "assert"},
				{Kind: TokenVarRef, Text: "$a", VarName: "a"},
				{Kind: TokenWord, Text: "!="},
				{Kind: TokenWord, Text: "1"},
			},
		},
		{
			name:  "< and > are word tokens",
			input: "assert $a < $b",
			want: []Token{
				{Kind: TokenWord, Text: "assert"},
				{Kind: TokenVarRef, Text: "$a", VarName: "a"},
				{Kind: TokenWord, Text: "<"},
				{Kind: TokenVarRef, Text: "$b", VarName: "b"},
			},
		},
		{
			name:  "<= is a single word token",
			input: "assert $a <= $b",
			want: []Token{
				{Kind: TokenWord, Text: "assert"},
				{Kind: TokenVarRef, Text: "$a", VarName: "a"},
				{Kind: TokenWord, Text: "<="},
				{Kind: TokenVarRef, Text: "$b", VarName: "b"},
			},
		},
		{
			name:  ">= is a single word token",
			input: "assert $a >= $b",
			want: []Token{
				{Kind: TokenWord, Text: "assert"},
				{Kind: TokenVarRef, Text: "$a", VarName: "a"},
				{Kind: TokenWord, Text: ">="},
				{Kind: TokenVarRef, Text: "$b", VarName: "b"},
			},
		},
		{
			name:  "= remains assignment when not followed by =",
			input: "x = 42",
			want: []Token{
				{Kind: TokenWord, Text: "x"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "42"},
			},
		},

		{
			name:  "unknown adapter prefix is word plus varref",
			input: "notanadapter:$var",
			want: []Token{
				{Kind: TokenWord, Text: "notanadapter:"},
				{Kind: TokenVarRef, Text: "$var", VarName: "var"},
			},
		},
		{
			name:  "adapter ref mixed with normal args",
			input: "exec wc -l file:$raw.stdout",
			want: []Token{
				{Kind: TokenWord, Text: "exec"},
				{Kind: TokenWord, Text: "wc"},
				{Kind: TokenWord, Text: "-l"},
				{Kind: TokenAdapterRef, Text: "file:$raw.stdout", Adapter: "file", VarName: "raw", VarPath: "stdout"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tokenise(tt.input)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsIdent(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"prog", true},
		{"_private", true},
		{"myVar2", true},
		{"MY_CONST", true},
		{"a", true},
		{"", false},
		{"123", false},
		{"1abc", false},
		{"my-var", false},
		{"my.var", false},
		{"my var", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, IsIdent(tt.input))
		})
	}
}
