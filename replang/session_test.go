package replang

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionSetGetDelete(t *testing.T) {
	s := NewSession()

	// Initially empty.
	_, ok := s.Get("x")
	assert.False(t, ok)
	assert.Empty(t, s.Names())

	// Set and get.
	s.Set("x", StringValue("hello"))
	v, ok := s.Get("x")
	assert.True(t, ok)
	str, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", str)

	// Overwrite.
	s.Set("x", StringValue("world"))
	v, ok = s.Get("x")
	assert.True(t, ok)
	str, err = v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "world", str)

	// Delete.
	s.Delete("x")
	_, ok = s.Get("x")
	assert.False(t, ok)

	// Delete non-existent is fine.
	s.Delete("nonexistent")
}

func TestSessionNames(t *testing.T) {
	s := NewSession()
	s.Set("beta", StringValue("b"))
	s.Set("alpha", StringValue("a"))
	s.Set("gamma", StringValue("g"))
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, s.Names())
}

func TestSessionExpand(t *testing.T) {
	progData := map[string]any{
		"id":   json.Number("42"),
		"name": "test_prog",
		"type": "tracepoint",
		"maps": []any{
			map[string]any{"name": "counts", "pin": "/sys/fs/bpf/counts"},
			map[string]any{"name": "events", "pin": "/sys/fs/bpf/events"},
		},
		"details": map[string]any{
			"kernel_id": json.Number("99"),
		},
		"active": true,
		"extra":  nil,
	}

	newSession := func() *Session {
		s := NewSession()
		s.Set("prog", ValueFromMap(progData))
		s.Set("simple", StringValue("hello"))
		s.Set("flag", BoolValue(true))
		return s
	}

	tests := []struct {
		name    string
		tokens  []Token
		want    []Token
		wantErr string
	}{
		{
			name: "passthrough no varrefs",
			tokens: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
			},
		},
		{
			name: "simple scalar variable",
			tokens: []Token{
				{Kind: TokenWord, Text: "echo"},
				{Kind: TokenVarRef, Text: "$simple", VarName: "simple"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "echo"},
				{Kind: TokenWord, Text: "hello"},
			},
		},
		{
			name: "field access",
			tokens: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenWord, Text: "42"},
			},
		},
		{
			name: "nested path",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.details.kernel_id", VarName: "prog", VarPath: "details.kernel_id"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "99"},
			},
		},
		{
			name: "array index",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps[0].name", VarName: "prog", VarPath: "maps[0].name"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "counts"},
			},
		},
		{
			name: "multiple varrefs",
			tokens: []Token{
				{Kind: TokenWord, Text: "--id"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
				{Kind: TokenWord, Text: "--name"},
				{Kind: TokenVarRef, Text: "$prog.name", VarName: "prog", VarPath: "name"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "--id"},
				{Kind: TokenWord, Text: "42"},
				{Kind: TokenWord, Text: "--name"},
				{Kind: TokenWord, Text: "test_prog"},
			},
		},
		{
			name: "mixed token types preserved",
			tokens: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenQuoted, Text: "my file.o"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenQuoted, Text: "my file.o"},
				{Kind: TokenWord, Text: "42"},
			},
		},
		{
			name: "bool field",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.active", VarName: "prog", VarPath: "active"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "true"},
			},
		},
		{
			name: "bare scalar variable (bool)",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$flag", VarName: "flag"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "true"},
			},
		},
		{
			name: "undefined variable",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$unknown", VarName: "unknown"},
			},
			wantErr: "undefined variable: unknown",
		},
		{
			name: "bare structured variable passes through",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog", VarName: "prog"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "$prog"},
			},
		},
		{
			name: "missing field",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.nonexistent", VarName: "prog", VarPath: "nonexistent"},
			},
			wantErr: "field nonexistent not found in variable prog",
		},
		{
			name: "index out of range",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps[5]", VarName: "prog", VarPath: "maps[5]"},
			},
			wantErr: "index 5 out of range for variable prog.maps (length 2)",
		},
		{
			name: "null field",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.extra", VarName: "prog", VarPath: "extra"},
			},
			wantErr: "variable prog.extra is null",
		},
		{
			name: "non-scalar leaf (object)",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.details", VarName: "prog", VarPath: "details"},
			},
			wantErr: "variable prog.details is an object; use field access to reach a scalar value",
		},
		{
			name: "non-scalar leaf (array)",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps", VarName: "prog", VarPath: "maps"},
			},
			wantErr: "variable prog.maps is an array; use indexing to reach a scalar value",
		},
		{
			name: "string field from struct",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.type", VarName: "prog", VarPath: "type"},
			},
			want: []Token{
				{Kind: TokenWord, Text: "tracepoint"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSession()
			got, err := s.Expand(tt.tokens)
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

func TestSessionExpandNilVariable(t *testing.T) {
	s := NewSession()
	s.Set("n", Value{}) // nil value

	_, err := s.Expand([]Token{
		{Kind: TokenVarRef, Text: "$n", VarName: "n"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "variable n is null")
}
