package shell

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionSetGetDelete(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	s := NewSession()
	s.Set("beta", StringValue("b"))
	s.Set("alpha", StringValue("a"))
	s.Set("gamma", StringValue("g"))
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, s.Names())
}

func TestSessionExpand(t *testing.T) {
	t.Parallel()

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
		want    []Arg
		wantErr string
	}{
		{
			name: "passthrough no varrefs",
			tokens: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
			},
			want: []Arg{
				WordArg{Text: "show"},
				WordArg{Text: "program"},
			},
		},
		{
			name: "simple scalar variable",
			tokens: []Token{
				{Kind: TokenWord, Text: "echo"},
				{Kind: TokenVarRef, Text: "$simple", VarName: "simple"},
			},
			want: []Arg{
				WordArg{Text: "echo"},
				ScalarValueArg{Text: "hello"},
			},
		},
		{
			name: "field access",
			tokens: []Token{
				{Kind: TokenWord, Text: "show"},
				{Kind: TokenWord, Text: "program"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: []Arg{
				WordArg{Text: "show"},
				WordArg{Text: "program"},
				ScalarValueArg{Text: "42"},
			},
		},
		{
			name: "nested path",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.details.kernel_id", VarName: "prog", VarPath: "details.kernel_id"},
			},
			want: []Arg{
				ScalarValueArg{Text: "99"},
			},
		},
		{
			name: "array index",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps[0].name", VarName: "prog", VarPath: "maps[0].name"},
			},
			want: []Arg{
				ScalarValueArg{Text: "counts"},
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
			want: []Arg{
				WordArg{Text: "--id"},
				ScalarValueArg{Text: "42"},
				WordArg{Text: "--name"},
				ScalarValueArg{Text: "test_prog"},
			},
		},
		{
			name: "mixed token types preserved",
			tokens: []Token{
				{Kind: TokenWord, Text: "load"},
				{Kind: TokenQuoted, Text: "my file.o"},
				{Kind: TokenVarRef, Text: "$prog.id", VarName: "prog", VarPath: "id"},
			},
			want: []Arg{
				WordArg{Text: "load"},
				QuotedArg{Text: "my file.o"},
				ScalarValueArg{Text: "42"},
			},
		},
		{
			name: "bool field",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.active", VarName: "prog", VarPath: "active"},
			},
			want: []Arg{
				ScalarValueArg{Text: "true"},
			},
		},
		{
			name: "bare scalar variable (bool)",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$flag", VarName: "flag"},
			},
			want: []Arg{
				ScalarValueArg{Text: "true"},
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
			name: "bare structured variable preserved as typed arg",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog", VarName: "prog"},
			},
			want: []Arg{
				StructuredValueArg{Name: "prog", Value: ValueFromMap(progData)},
			},
		},
		{
			name: "missing field surfaces as MissingArg",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.nonexistent", VarName: "prog", VarPath: "nonexistent"},
			},
			want: []Arg{MissingArg{Name: "prog", Path: "nonexistent"}},
		},
		{
			name: "index out of range",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps[5]", VarName: "prog", VarPath: "maps[5]"},
			},
			wantErr: "index 5 out of range for variable prog.maps (length 2)",
		},
		{
			name: "null field surfaces as NilArg",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.extra", VarName: "prog", VarPath: "extra"},
			},
			want: []Arg{NilArg{}},
		},
		{
			name: "non-scalar leaf (object) preserved as structured arg",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.details", VarName: "prog", VarPath: "details"},
			},
			want: []Arg{
				StructuredValueArg{
					Name: "prog",
					Value: ValueFromMap(map[string]any{
						"kernel_id": json.Number("99"),
					}),
				},
			},
		},
		{
			name: "non-scalar leaf (array) preserved as structured arg",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.maps", VarName: "prog", VarPath: "maps"},
			},
			want: []Arg{
				StructuredValueArg{
					Name: "prog",
					Value: ValueFromAny([]any{
						map[string]any{"name": "counts", "pin": "/sys/fs/bpf/counts"},
						map[string]any{"name": "events", "pin": "/sys/fs/bpf/events"},
					}),
				},
			},
		},
		{
			name: "string field from struct",
			tokens: []Token{
				{Kind: TokenVarRef, Text: "$prog.type", VarName: "prog", VarPath: "type"},
			},
			want: []Arg{
				ScalarValueArg{Text: "tracepoint"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSession()
			got, err := evalArgsForTest(s, tt.tokens)
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

// evalArgsForTest turns a token slice into evaluated []Arg by
// building a primary expression per token and running them
// through EvalArgs. Tests use this to keep their table-driven
// shape; the production pipeline goes tokens -> Parse ->
// EvalArgs, which exercises the same helpers.
func evalArgsForTest(s *Session, tokens []Token) ([]Arg, error) {
	exprs := make([]Expr, 0, len(tokens))
	for _, tok := range tokens {
		e, err := parsePrimary(tok)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
	}
	env := &Env{Session: s}
	return EvalArgs(exprs, env)
}

func TestSessionAssertFailures(t *testing.T) {
	t.Parallel()

	s := NewSession()
	assert.Equal(t, 0, s.AssertFailures())

	s.RecordAssertFailure()
	assert.Equal(t, 1, s.AssertFailures())

	s.RecordAssertFailure()
	s.RecordAssertFailure()
	assert.Equal(t, 3, s.AssertFailures())
}

func TestSessionExpandAdapterRef(t *testing.T) {
	t.Parallel()

	progData := map[string]any{
		"id":   json.Number("42"),
		"name": "test_prog",
		"maps": []any{
			map[string]any{"name": "counts"},
			map[string]any{"name": "events"},
		},
		"details": map[string]any{
			"kernel_id": json.Number("99"),
		},
	}

	newSession := func() *Session {
		s := NewSession()
		s.Set("prog", ValueFromMap(progData))
		s.Set("simple", StringValue("hello"))
		return s
	}

	t.Run("adapter ref with scalar value", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		got, err := evalArgsForTest(s, []Token{
			{Kind: TokenAdapterRef, Text: "file:$simple", Adapter: "file", VarName: "simple"},
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		aa, ok := got[0].(AdapterArg)
		require.True(t, ok)
		assert.Equal(t, "file", aa.Adapter)
		assert.Equal(t, "simple", aa.Name)
		assert.True(t, aa.Value.IsScalar())
		str, err := aa.Value.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "hello", str)
	})

	t.Run("adapter ref with bare structured value", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		got, err := evalArgsForTest(s, []Token{
			{Kind: TokenAdapterRef, Text: "file:$prog", Adapter: "file", VarName: "prog"},
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		aa, ok := got[0].(AdapterArg)
		require.True(t, ok)
		assert.True(t, aa.Value.IsStructured())
	})

	t.Run("adapter ref with pathed structured subtree", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		got, err := evalArgsForTest(s, []Token{
			{Kind: TokenAdapterRef, Text: "file:$prog.details", Adapter: "file", VarName: "prog", VarPath: "details"},
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		aa, ok := got[0].(AdapterArg)
		require.True(t, ok)
		assert.True(t, aa.Value.IsStructured())
	})

	t.Run("adapter ref with pathed scalar leaf", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		got, err := evalArgsForTest(s, []Token{
			{Kind: TokenAdapterRef, Text: "file:$prog.name", Adapter: "file", VarName: "prog", VarPath: "name"},
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		aa, ok := got[0].(AdapterArg)
		require.True(t, ok)
		assert.True(t, aa.Value.IsScalar())
		str, err := aa.Value.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "test_prog", str)
	})

	t.Run("adapter ref with undefined variable", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		_, err := evalArgsForTest(s, []Token{
			{Kind: TokenAdapterRef, Text: "file:$unknown", Adapter: "file", VarName: "unknown"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "undefined variable: unknown")
	})

	t.Run("adapter ref with null value", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		s.Set("n", Value{})
		_, err := evalArgsForTest(s, []Token{
			{Kind: TokenAdapterRef, Text: "file:$n", Adapter: "file", VarName: "n"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "null")
	})

	t.Run("adapter ref mixed with normal tokens", func(t *testing.T) {
		t.Parallel()
		s := newSession()
		got, err := evalArgsForTest(s, []Token{
			{Kind: TokenWord, Text: "diff"},
			{Kind: TokenAdapterRef, Text: "file:$prog.name", Adapter: "file", VarName: "prog", VarPath: "name"},
			{Kind: TokenAdapterRef, Text: "file:$simple", Adapter: "file", VarName: "simple"},
		})
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.IsType(t, WordArg{}, got[0])
		assert.IsType(t, AdapterArg{}, got[1])
		assert.IsType(t, AdapterArg{}, got[2])
	})
}

func TestSessionExpandNilVariable(t *testing.T) {
	t.Parallel()

	// Expanding a nil variable now produces a NilArg at the
	// argument boundary rather than erroring out at expansion
	// time. Downstream commands decide whether null is meaningful
	// for their semantics (jq treats it as JSON null; print
	// renders the JSON token "null"; predicates that care -- nil,
	// present, not-empty -- inspect NilArg explicitly).
	s := NewSession()
	s.Set("n", Value{}) // nil value

	got, err := evalArgsForTest(s, []Token{
		{Kind: TokenVarRef, Text: "$n", VarName: "n"},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, isNil := got[0].(NilArg)
	assert.True(t, isNil, "nil variable expansion produces NilArg")
}
