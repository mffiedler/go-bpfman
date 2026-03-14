package replang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLine(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []Token
		want    Line
		wantErr string
	}{
		{
			name:   "empty",
			tokens: nil,
			want:   Line{},
		},
		{
			name:   "single word command",
			tokens: []Token{{Kind: TokenWord, Text: "help"}},
			want:   Line{Command: []Token{{Kind: TokenWord, Text: "help"}}},
		},
		{
			name: "plain command",
			tokens: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenWord, Text: "123"},
			},
			want: Line{
				Command: []Token{
					{Kind: TokenWord, Text: "show"},
					{Kind: TokenWord, Text: "program"},
					{Kind: TokenWord, Text: "123"},
				},
			},
		},
		{
			name: "assignment",
			tokens: []Token{
				{Kind: TokenWord, Text: "prog"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenWord, Text: "file"},
			},
			want: Line{
				VarName: "prog",
				Command: []Token{
					{Kind: TokenWord, Text: "load"},
					{Kind: TokenWord, Text: "file"},
				},
			},
		},
		{
			name: "assignment with varrefs in command",
			tokens: []Token{
				{Kind: TokenWord, Text: "link"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "attach"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: Line{
				VarName: "link",
				Command: []Token{
					{Kind: TokenWord, Text: "attach"},
					{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
				},
			},
		},
		{
			name: "no command after equals",
			tokens: []Token{
				{Kind: TokenWord, Text: "x"},
				{Kind: TokenAssign, Text: "="},
			},
			wantErr: "expected command after =",
		},
		{
			name: "varref as LHS is not assignment",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$x", VarName: "x"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			want: Line{
				Command: []Token{
					{Kind: TokenVarRef, Text: "$x", VarName: "x"},
					{Kind: TokenAssign, Text: "="},
					{Kind: TokenWord, Text: "foo"},
				},
			},
		},
		{
			name: "quoted as LHS is not assignment",
			tokens: []Token{
				{Kind: TokenQuoted, Text: "name"},
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			want: Line{
				Command: []Token{
					{Kind: TokenQuoted, Text: "name"},
					{Kind: TokenAssign, Text: "="},
					{Kind: TokenWord, Text: "foo"},
				},
			},
		},
		{
			name: "assign as LHS is not assignment",
			tokens: []Token{
				{Kind: TokenAssign, Text: "="},
				{Kind: TokenWord, Text: "foo"},
			},
			want: Line{
				Command: []Token{
					{Kind: TokenAssign, Text: "="},
					{Kind: TokenWord, Text: "foo"},
				},
			},
		},
		{
			name: "command with only varref",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: Line{
				Command: []Token{
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
			want: Line{
				Command: []Token{
					{Kind: TokenWord, Text: "prog"},
					{Kind: TokenWord, Text: "load"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLine(tt.tokens)
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
