package replang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStmt(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []Token
		want    Stmt
		wantErr string
	}{
		{
			name:   "empty",
			tokens: nil,
			want:   nil,
		},
		{
			name:   "single word command",
			tokens: []Token{{Kind: TokenWord, Text: "help"}},
			want:   &CommandStmt{Tokens: []Token{{Kind: TokenWord, Text: "help"}}},
		},
		{
			name: "plain command",
			tokens: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenWord, Text: "123"},
			},
			want: &CommandStmt{
				Tokens: []Token{
					{Kind: TokenWord, Text: "show"},
					{Kind: TokenWord, Text: "program"},
					{Kind: TokenWord, Text: "123"},
				},
			},
		},
		{
			name: "let assignment",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenWord, Text: "prog"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "file"},
			},
			want: &LetStmt{
				Name: "prog",
				Command: []Token{
					{Kind: TokenWord, Text: "load"},
					{Kind: TokenWord, Text: "file"},
				},
			},
		},
		{
			name: "let assignment with varrefs in command",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenWord, Text: "link"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "attach"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: &LetStmt{
				Name: "link",
				Command: []Token{
					{Kind: TokenWord, Text: "attach"},
					{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
				},
			},
		},
		{
			name: "let no command after equals",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenWord, Text: "x"},
				{Kind: TokenAssign, Text: "="},
			},
			wantErr: "let requires",
		},
		{
			name: "let too few tokens",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenWord, Text: "x"},
			},
			wantErr: "let requires",
		},
		{
			name: "let missing equals",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenWord, Text: "x"},
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "file"},
			},
			wantErr: "missing '='",
		},
		{
			name: "let non-identifier LHS",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenVarRef, Text: "$x", VarName: "x"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			wantErr: "let requires an identifier",
		},
		{
			name: "let invalid identifier",
			tokens: []Token{
				{Kind: TokenWord, Text: "let"},
				{Kind: TokenWord, Text: "0bad"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			wantErr: "invalid variable name",
		},
		{
			name: "set binding",
			tokens: []Token{
				{Kind: TokenWord, Text: "set"},
				{Kind: TokenWord, Text: "pid"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "42"},
			},
			want: &SetStmt{
				Name:  "pid",
				Value: Token{Kind: TokenWord, Text: "42"},
			},
		},
		{
			name: "set rejects multiple values",
			tokens: []Token{
				{Kind: TokenWord, Text: "set"},
				{Kind: TokenWord, Text: "x"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "a"},
				{Kind: TokenWord, Text: "b"},
			},
			wantErr: "exactly one value",
		},
		{
			name: "set too few tokens",
			tokens: []Token{
				{Kind: TokenWord, Text: "set"},
				{Kind: TokenWord, Text: "x"},
			},
			wantErr: "set requires",
		},
		{
			name: "set missing equals",
			tokens: []Token{
				{Kind: TokenWord, Text: "set"},
				{Kind: TokenWord, Text: "x"},
				{Kind: TokenWord, Text: "42"},
				{Kind: TokenWord, Text: "extra"},
			},
			wantErr: "missing '='",
		},
		{
			name: "set invalid identifier",
			tokens: []Token{
				{Kind: TokenWord, Text: "set"},
				{Kind: TokenWord, Text: "0bad"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "val"},
			},
			wantErr: "invalid variable name",
		},
		{
			name: "bare ident-equals is a parse error",
			tokens: []Token{
				{Kind: TokenWord, Text: "prog"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "file"},
			},
			wantErr: "unexpected '='",
		},
		{
			name: "varref then equals is a parse error",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$x", VarName: "x"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			wantErr: "unexpected '='",
		},
		{
			name: "quoted then equals is a parse error",
			tokens: []Token{
				{Kind: TokenQuoted, Text: "name"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			wantErr: "unexpected '='",
		},
		{
			name: "leading equals is a parse error",
			tokens: []Token{
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			wantErr: "unexpected '='",
		},
		{
			name: "command with only varref",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: &CommandStmt{
				Tokens: []Token{
					{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
				},
			},
		},
		{
			name: "word without = is plain command",
			tokens: []Token{
				{Kind: TokenWord, Text: "prog"},
				{Kind: TokenWord, Text: "load"},
			},
			want: &CommandStmt{
				Tokens: []Token{
					{Kind: TokenWord, Text: "prog"},
					{Kind: TokenWord, Text: "load"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStmt(tt.tokens)
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
