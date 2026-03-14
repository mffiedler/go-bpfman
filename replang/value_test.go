package replang

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValueFromJSON(t *testing.T) {
	t.Run("object", func(t *testing.T) {
		v, err := ValueFromJSON([]byte(`{"id": 42, "name": "test"}`))
		require.NoError(t, err)
		assert.True(t, v.IsStructured())
		assert.False(t, v.IsScalar())
		assert.False(t, v.IsNil())
	})

	t.Run("string", func(t *testing.T) {
		v, err := ValueFromJSON([]byte(`"hello"`))
		require.NoError(t, err)
		assert.True(t, v.IsScalar())
		assert.False(t, v.IsStructured())
		s, err := v.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "hello", s)
	})

	t.Run("number preserved as json.Number", func(t *testing.T) {
		v, err := ValueFromJSON([]byte(`42`))
		require.NoError(t, err)
		_, ok := v.Raw().(json.Number)
		assert.True(t, ok, "expected json.Number, got %T", v.Raw())
	})

	t.Run("null", func(t *testing.T) {
		v, err := ValueFromJSON([]byte(`null`))
		require.NoError(t, err)
		assert.True(t, v.IsNil())
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, err := ValueFromJSON([]byte(`{invalid`))
		require.Error(t, err)
	})
}

func TestValueFromStruct(t *testing.T) {
	type Result struct {
		ID   uint32 `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}

	v, err := ValueFromStruct(Result{ID: 123, Name: "my_prog", Type: "tracepoint"})
	require.NoError(t, err)
	assert.True(t, v.IsStructured())

	// Verify fields accessible via Lookup.
	id, err := v.Lookup("result", "id")
	require.NoError(t, err)
	s, err := id.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "123", s)

	name, err := v.Lookup("result", "name")
	require.NoError(t, err)
	s, err = name.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "my_prog", s)
}

func TestValueConvenience(t *testing.T) {
	t.Run("StringValue", func(t *testing.T) {
		v := StringValue("hello")
		assert.True(t, v.IsScalar())
		s, err := v.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "hello", s)
	})

	t.Run("BoolValue", func(t *testing.T) {
		v := BoolValue(true)
		assert.True(t, v.IsScalar())
		s, err := v.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "true", s)
	})
}

func TestValueScalar(t *testing.T) {
	tests := []struct {
		name    string
		value   Value
		want    string
		wantErr string
	}{
		{
			name:  "string",
			value: StringValue("hello"),
			want:  "hello",
		},
		{
			name:  "json.Number int",
			value: Value{v: json.Number("42")},
			want:  "42",
		},
		{
			name:  "json.Number float",
			value: Value{v: json.Number("3.14")},
			want:  "3.14",
		},
		{
			name:  "bool true",
			value: BoolValue(true),
			want:  "true",
		},
		{
			name:  "bool false",
			value: BoolValue(false),
			want:  "false",
		},
		{
			name:  "float64",
			value: Value{v: float64(2.5)},
			want:  "2.5",
		},
		{
			name:    "nil",
			value:   Value{v: nil},
			wantErr: "value is null",
		},
		{
			name:    "map",
			value:   ValueFromMap(map[string]any{"a": 1}),
			wantErr: "value is not a scalar",
		},
		{
			name:    "slice",
			value:   Value{v: []any{1, 2, 3}},
			wantErr: "value is not a scalar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.value.Scalar()
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

func TestValueLookup(t *testing.T) {
	// Build a structured value with nested fields and arrays.
	data := map[string]any{
		"id":   json.Number("42"),
		"name": "test_prog",
		"maps": []any{
			map[string]any{"name": "counts", "pin": "/sys/fs/bpf/counts"},
			map[string]any{"name": "events", "pin": "/sys/fs/bpf/events"},
		},
		"details": map[string]any{
			"kernel_id": json.Number("99"),
			"type":      "tracepoint",
		},
		"nullable": nil,
		"nested_arr": []any{
			[]any{"a", "b"},
		},
	}
	v := ValueFromMap(data)

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{
			name: "simple field",
			path: "name",
			want: "test_prog",
		},
		{
			name: "numeric field",
			path: "id",
			want: "42",
		},
		{
			name: "nested field",
			path: "details.kernel_id",
			want: "99",
		},
		{
			name: "nested dotted field",
			path: "details.type",
			want: "tracepoint",
		},
		{
			name: "array index then field",
			path: "maps[0].name",
			want: "counts",
		},
		{
			name: "array second element",
			path: "maps[1].pin",
			want: "/sys/fs/bpf/events",
		},
		{
			name:    "missing field",
			path:    "nonexistent",
			wantErr: "field nonexistent not found in variable v",
		},
		{
			name:    "missing nested field",
			path:    "details.missing",
			wantErr: "field missing not found in variable v.details",
		},
		{
			name:    "index out of range",
			path:    "maps[5]",
			wantErr: "index 5 out of range for variable v.maps (length 2)",
		},
		{
			name:    "null field",
			path:    "nullable",
			wantErr: "variable v.nullable is null",
		},
		{
			name:    "object leaf",
			path:    "details",
			wantErr: "variable v.details is an object; use field access to reach a scalar value",
		},
		{
			name:    "array leaf",
			path:    "maps",
			wantErr: "variable v.maps is an array; use indexing to reach a scalar value",
		},
		{
			name:    "index on non-array",
			path:    "name[0]",
			wantErr: "cannot index non-array in variable v.name",
		},
		{
			name:    "field on non-object",
			path:    "id.sub",
			wantErr: "cannot access field sub on non-object in variable v.id",
		},
		{
			name: "empty path returns value itself (structured triggers error)",
			path: "",
			// Empty path returns the value, which is a map,
			// so Lookup returns it without error -- but the
			// caller would get an error from Scalar().
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := v.Lookup("v", tt.path)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.want != "" {
				s, err := got.Scalar()
				require.NoError(t, err)
				assert.Equal(t, tt.want, s)
			}
		})
	}
}

func TestValueLookupEmptyPath(t *testing.T) {
	v := StringValue("hello")
	got, err := v.Lookup("v", "")
	require.NoError(t, err)
	s, err := got.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", s)
}

func TestValuePrecision(t *testing.T) {
	t.Run("large uint32", func(t *testing.T) {
		v, err := ValueFromJSON([]byte(`{"id": 4294967295}`))
		require.NoError(t, err)
		id, err := v.Lookup("v", "id")
		require.NoError(t, err)
		s, err := id.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "4294967295", s)
	})

	t.Run("2^53+1 via json.Number", func(t *testing.T) {
		v, err := ValueFromJSON([]byte(`{"big": 9007199254740993}`))
		require.NoError(t, err)
		big, err := v.Lookup("v", "big")
		require.NoError(t, err)
		s, err := big.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "9007199254740993", s)
	})
}

func TestValueLookupValue(t *testing.T) {
	data := map[string]any{
		"id":   json.Number("42"),
		"name": "test_prog",
		"maps": []any{
			map[string]any{"name": "counts", "pin": "/sys/fs/bpf/counts"},
			map[string]any{"name": "events", "pin": "/sys/fs/bpf/events"},
		},
		"details": map[string]any{
			"kernel_id": json.Number("99"),
			"type":      "tracepoint",
		},
		"nullable": nil,
	}
	v := ValueFromMap(data)

	t.Run("returns structured map", func(t *testing.T) {
		got, err := v.LookupValue("v", "details")
		require.NoError(t, err)
		assert.True(t, got.IsStructured())
		m, ok := got.Raw().(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "tracepoint", m["type"])
	})

	t.Run("returns structured array", func(t *testing.T) {
		got, err := v.LookupValue("v", "maps")
		require.NoError(t, err)
		assert.True(t, got.IsStructured())
		arr, ok := got.Raw().([]any)
		require.True(t, ok)
		assert.Len(t, arr, 2)
	})

	t.Run("returns nil value", func(t *testing.T) {
		got, err := v.LookupValue("v", "nullable")
		require.NoError(t, err)
		assert.True(t, got.IsNil())
	})

	t.Run("returns scalar", func(t *testing.T) {
		got, err := v.LookupValue("v", "name")
		require.NoError(t, err)
		assert.True(t, got.IsScalar())
		s, err := got.Scalar()
		require.NoError(t, err)
		assert.Equal(t, "test_prog", s)
	})

	t.Run("nested array element", func(t *testing.T) {
		got, err := v.LookupValue("v", "maps[0]")
		require.NoError(t, err)
		assert.True(t, got.IsStructured())
		m, ok := got.Raw().(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "counts", m["name"])
	})

	t.Run("empty path returns whole value", func(t *testing.T) {
		got, err := v.LookupValue("v", "")
		require.NoError(t, err)
		assert.True(t, got.IsStructured())
	})

	t.Run("missing field errors", func(t *testing.T) {
		_, err := v.LookupValue("v", "nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "field nonexistent not found")
	})

	t.Run("index out of range errors", func(t *testing.T) {
		_, err := v.LookupValue("v", "maps[5]")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "index 5 out of range")
	})
}

func TestValueKeys(t *testing.T) {
	t.Run("map returns sorted keys", func(t *testing.T) {
		v := ValueFromMap(map[string]any{
			"zebra": "z",
			"alpha": "a",
			"mid":   "m",
		})
		assert.Equal(t, []string{"alpha", "mid", "zebra"}, v.Keys())
	})

	t.Run("array returns index strings", func(t *testing.T) {
		v := Value{v: []any{"a", "b", "c"}}
		assert.Equal(t, []string{"[0]", "[1]", "[2]"}, v.Keys())
	})

	t.Run("empty map returns empty slice", func(t *testing.T) {
		v := ValueFromMap(map[string]any{})
		assert.Equal(t, []string{}, v.Keys())
	})

	t.Run("empty array returns empty slice", func(t *testing.T) {
		v := Value{v: []any{}}
		assert.Equal(t, []string{}, v.Keys())
	})

	t.Run("scalar returns nil", func(t *testing.T) {
		assert.Nil(t, StringValue("hello").Keys())
	})

	t.Run("nil returns nil", func(t *testing.T) {
		assert.Nil(t, Value{}.Keys())
	})

	t.Run("number returns nil", func(t *testing.T) {
		v := Value{v: json.Number("42")}
		assert.Nil(t, v.Keys())
	})

	t.Run("bool returns nil", func(t *testing.T) {
		assert.Nil(t, BoolValue(true).Keys())
	})
}

func TestValueIsPredicates(t *testing.T) {
	assert.True(t, Value{}.IsNil())
	assert.False(t, Value{}.IsScalar())
	assert.False(t, Value{}.IsStructured())

	assert.False(t, StringValue("x").IsNil())
	assert.True(t, StringValue("x").IsScalar())
	assert.False(t, StringValue("x").IsStructured())

	m := ValueFromMap(map[string]any{})
	assert.False(t, m.IsNil())
	assert.False(t, m.IsScalar())
	assert.True(t, m.IsStructured())
}
