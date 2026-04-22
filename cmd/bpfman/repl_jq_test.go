package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman/shell"
)

// replJQ is the "jq FILTER VALUE" shell builtin.  Scalars pass
// through, structured values are walked, and aggregation filters
// (add, length, map, select, group_by) all reduce to a Value.

func TestReplJQ_IdentityOnJSONScalar(t *testing.T) {
	// A bare scalar input is parsed as JSON — matching bash jq
	// semantics — so a JSON-quoted string flows through as a
	// string and the identity filter returns it intact.
	v, err := replJQ([]shell.Arg{
		shell.WordArg{Text: "."},
		shell.QuotedArg{Text: `"hello"`},
	})
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", s)
}

func TestReplJQ_IdentityOnJSONNumber(t *testing.T) {
	v, err := replJQ([]shell.Arg{
		shell.WordArg{Text: "."},
		shell.WordArg{Text: "42"},
	})
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", s)
}

func TestReplJQ_ScalarNotValidJSONIsError(t *testing.T) {
	// 'hello' on its own isn't JSON — users who want a string
	// wrap it in JSON quotes.  Matches the error the standalone
	// jq CLI produces on non-JSON stdin.
	_, err := replJQ([]shell.Arg{
		shell.WordArg{Text: "."},
		shell.ScalarValueArg{Text: "hello"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

func TestReplJQ_PathOnStructured(t *testing.T) {
	input := shell.ValueFromMap(map[string]any{"a": "apple", "b": "banana"})
	v, err := replJQ([]shell.Arg{
		shell.WordArg{Text: ".a"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "apple", s)
}

func TestReplJQ_AggregateSum(t *testing.T) {
	input := shell.ValueFromMap(map[string]any{
		"items": []any{
			map[string]any{"v": json.Number("1")},
			map[string]any{"v": json.Number("2")},
			map[string]any{"v": json.Number("3")},
		},
	})
	v, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: "[.items[].v] | add"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "6", s)
}

func TestReplJQ_Length(t *testing.T) {
	input := shell.ValueFromMap(map[string]any{
		"items": []any{"a", "b", "c"},
	})
	v, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: ".items | length"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "3", s)
}

func TestReplJQ_Map(t *testing.T) {
	input := shell.ValueFromMap(map[string]any{
		"items": []any{
			map[string]any{"name": "foo"},
			map[string]any{"name": "bar"},
		},
	})
	v, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: ".items | map(.name)"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	require.True(t, v.IsStructured())
	raw, ok := v.Raw().([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"foo", "bar"}, raw)
}

func TestReplJQ_MultiResultCollected(t *testing.T) {
	// jq ".items[]" emits one value per element.  Our builtin
	// collects a multi-emission into a list Value so the caller
	// can use it as a single bindable result.
	input := shell.ValueFromMap(map[string]any{
		"items": []any{"a", "b", "c"},
	})
	v, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: ".items[]"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	require.True(t, v.IsStructured())
	raw, ok := v.Raw().([]any)
	require.True(t, ok)
	assert.Equal(t, []any{"a", "b", "c"}, raw)
}

func TestReplJQ_BooleanResultIsOriginBool(t *testing.T) {
	input := shell.ValueFromMap(map[string]any{"a": json.Number("5")})
	v, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: ".a > 3"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	b, err := shell.AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)
}

func TestReplJQ_NullResultIsNilValue(t *testing.T) {
	input := shell.ValueFromMap(map[string]any{"a": "apple"})
	v, err := replJQ([]shell.Arg{
		shell.WordArg{Text: ".missing"},
		shell.StructuredValueArg{Value: input},
	})
	require.NoError(t, err)
	assert.True(t, v.IsNil())
}

func TestReplJQ_InvalidFilter(t *testing.T) {
	_, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: "{{{ not valid"},
		shell.ScalarValueArg{Text: "x"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jq")
}

func TestReplJQ_WrongArgCount(t *testing.T) {
	_, err := replJQ(nil)
	require.Error(t, err)
	_, err = replJQ([]shell.Arg{shell.WordArg{Text: "."}})
	require.Error(t, err)
	_, err = replJQ([]shell.Arg{
		shell.WordArg{Text: "."},
		shell.ScalarValueArg{Text: "x"},
		shell.ScalarValueArg{Text: "y"},
	})
	require.Error(t, err)
}

func TestReplJQ_FlagArgGetsHint(t *testing.T) {
	// Passing a standalone-jq style flag like "-c" should not land
	// the user with a bare "usage" message; it should explain that
	// output formatting is a consumer concern ("${...}" for compact,
	// auto-print for pretty, shell-out for the real thing).
	_, err := replJQ([]shell.Arg{
		shell.WordArg{Text: "-c"},
		shell.WordArg{Text: "."},
		shell.ScalarValueArg{Text: "1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `-c`)
	assert.Contains(t, err.Error(), "filter-only")
	assert.Contains(t, err.Error(), "compact JSON")
}

func TestReplJQ_NormalisesIntsToJSONNumber(t *testing.T) {
	// gojq produces int for integer arithmetic.  We want
	// downstream Value.Scalar() to render the result as digits,
	// not fall through to the "not a scalar" branch.
	listValue, err := shell.ValueFromJSON([]byte(`[10,20,12]`))
	require.NoError(t, err)
	v, err := replJQ([]shell.Arg{
		shell.QuotedArg{Text: "add"},
		shell.StructuredValueArg{Value: listValue},
	})
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", s)
}
