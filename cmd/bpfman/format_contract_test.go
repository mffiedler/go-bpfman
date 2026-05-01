package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStdlibJSONMarshal_NoTrailingNewline pins the encoding/json
// "no trailing newline" contract that every marshaller-driven
// formatter in format.go relies on. The contract is documented in
// the encoding/json package godoc; we restate it here as an
// executable assertion so that a Go runtime upgrade or a stdlib
// behaviour change that introduces a trailing newline is caught
// before it leaks through to CLI consumers.
//
// See the file-level comment block in format.go for the broader
// CLI-output trailing-newline contract.
func TestStdlibJSONMarshal_NoTrailingNewline(t *testing.T) {
	t.Parallel()

	// A representative mix of value shapes: object, array of
	// objects, primitive, empty array, nested object. If any of
	// these grows a trailing newline in a future stdlib version,
	// the marshaller-driven formatters in format.go would emit
	// two trailing newlines instead of one.
	cases := []struct {
		name string
		v    any
	}{
		{"object", map[string]any{"k": "v"}},
		{"array", []map[string]any{{"id": 1}, {"id": 2}}},
		{"primitive", 42},
		{"empty array", []any{}},
		{"nested", map[string]any{"outer": map[string]any{"inner": 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plain, err := json.Marshal(tc.v)
			require.NoError(t, err)
			require.False(t, bytes.HasSuffix(plain, []byte("\n")),
				"json.Marshal(%v) unexpectedly ends with \\n: %q", tc.v, plain)

			indented, err := json.MarshalIndent(tc.v, "", "  ")
			require.NoError(t, err)
			require.False(t, bytes.HasSuffix(indented, []byte("\n")),
				"json.MarshalIndent(%v) unexpectedly ends with \\n: %q", tc.v, indented)
		})
	}
}

// TestExecuteJSONPath_OutputContract pins the executeJSONPath
// output contract: the returned string ends with exactly one "\n"
// regardless of whether the template's last token emits a newline.
// The contract underpins examples/tracepoint.sh and any downstream
// shell consumer; do not relax it without updating the file-level
// comment block in format.go and every shell example that relies
// on it.
func TestExecuteJSONPath_OutputContract(t *testing.T) {
	t.Parallel()

	type rec struct {
		ID int `json:"id"`
	}
	data := struct {
		Items []rec `json:"items"`
	}{
		Items: []rec{{ID: 1}, {ID: 2}, {ID: 3}},
	}

	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "single value, no template newline",
			expr: "{.items[0].id}",
			want: "1\n",
		},
		{
			name: "range with explicit newline",
			expr: `{range .items[*]}{.id}{"\n"}{end}`,
			want: "1\n2\n3\n",
		},
		{
			name: "range without separator",
			expr: `{range .items[*]}{.id}{end}`,
			want: "123\n",
		},
		{
			name: "range with space separator",
			expr: `{range .items[*]}{.id} {end}`,
			want: "1 2 3 \n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := executeJSONPath(data, tc.expr)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.True(t, strings.HasSuffix(got, "\n"))
			require.False(t, strings.HasSuffix(got, "\n\n"),
				"output must not end in two newlines: %q", got)
		})
	}
}
