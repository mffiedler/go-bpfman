package cliformat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// programWithID is a minimal helper for building a Program value that
// round-trips through json.Marshal with a recognisable program_id.
// The CLI format tests do not exercise the rest of the Program shape
// (that is the manager's job), so all other fields stay zero.
func programWithID(id kernel.ProgramID, name string) bpfman.Program {
	return bpfman.Program{
		Record: bpfman.ProgramRecord{
			ProgramID: id,
			Meta:      bpfman.ProgramMeta{Name: name},
		},
	}
}

// TestRenderLoadedProgramsJSON_WrapsWithProgramsKey asserts that
// RenderLoadedPrograms emits a top-level object whose `programs`
// key carries the slice in slice order. The contract is shared with
// RenderProgramList (program list) so jsonpath consumers can
// write {.programs[i]...} regardless of which command produced the
// output.
func TestRenderLoadedProgramsJSON_WrapsWithProgramsKey(t *testing.T) {
	t.Parallel()

	programs := []bpfman.Program{
		programWithID(7, "tp_c"),
		programWithID(3, "tp_a"),
		programWithID(5, "tp_b"),
	}

	var buf bytes.Buffer
	require.NoError(t, RenderLoadedPrograms(&buf, LoadedProgramsView{Programs: programs}, &OutputFlags{Output: OutputValue{Value: "json"}}))
	got := buf.String()

	// Decode into a generic shape so we test the wire format
	// without depending on bpfman.Program's strict unmarshaller
	// (which rejects empty program type, irrelevant to the
	// wrapper-key contract).
	var raw struct {
		Programs []map[string]any `json:"programs"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(got)).Decode(&raw))
	require.Len(t, raw.Programs, len(programs), "programs count")
	for i := range programs {
		record, ok := raw.Programs[i]["record"].(map[string]any)
		require.True(t, ok, "programs[%d].record missing", i)
		require.EqualValues(t, programs[i].Record.ProgramID, record["program_id"],
			"slice order preserved at index %d", i)
	}
}

// TestRenderLoadedProgramsJSON_EmptySlice asserts that an empty
// load result still produces a valid object with `programs: []`,
// not `programs: null`. Consumers that use jsonpath ranges
// ({.programs[*]}) on the empty case must see a list, not a null.
func TestRenderLoadedProgramsJSON_EmptySlice(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"nil", "empty"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var input []bpfman.Program
			if name == "empty" {
				input = []bpfman.Program{}
			}
			var buf bytes.Buffer
			require.NoError(t, RenderLoadedPrograms(&buf, LoadedProgramsView{Programs: input}, &OutputFlags{Output: OutputValue{Value: "json"}}))
			got := buf.String()

			var raw map[string]json.RawMessage
			require.NoError(t, json.NewDecoder(strings.NewReader(got)).Decode(&raw))
			require.Contains(t, raw, "programs", "top-level key 'programs' must always be present")
			require.Equal(t, "[]", string(raw["programs"]),
				"empty load result must marshal as `programs: []`, not null")
		})
	}
}

// TestRenderLoadedProgramsJSONPath_TrailingNewline asserts that
// the formatter normalises trailing newlines to exactly one,
// regardless of whether the user's template ends with {"\n"}. Shell
// consumers should never have to strip a stray blank line at the
// end of `bpfman ... -o jsonpath=...` output.
func TestRenderLoadedProgramsJSONPath_TrailingNewline(t *testing.T) {
	t.Parallel()

	programs := []bpfman.Program{
		programWithID(11, "tp_a"),
		programWithID(13, "tp_b"),
		programWithID(17, "tp_c"),
	}

	tests := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "single value gets one trailing newline",
			expr: "{.programs[0].record.program_id}",
			want: "11\n",
		},
		{
			name: "range with explicit newline does not double",
			expr: `{range .programs[*]}{.record.program_id}{"\n"}{end}`,
			want: "11\n13\n17\n",
		},
		{
			name: "range with space separator gets one trailing newline",
			expr: `{range .programs[*]}{.record.program_id} {end}`,
			want: "11 13 17 \n",
		},
		{
			name: "range with no trailing separator gets one trailing newline",
			expr: `{range .programs[*]}{.record.program_id}{end}`,
			want: "111317\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			require.NoError(t, RenderLoadedPrograms(&buf, LoadedProgramsView{Programs: programs}, &OutputFlags{Output: OutputValue{Value: "jsonpath=" + tc.expr}}))
			require.Equal(t, tc.want, buf.String())
		})
	}
}

// TestRenderLoadedProgramsJSONPath_RootIsObject asserts that the
// jsonpath formatter operates on the wrapper object, so callers
// query {.programs[N]...}. The existing jsonpath tests on bpfman.go
// against Program (single object) cover the leaf-access behaviour;
// this test pins the wrapping contract specifically.
func TestRenderLoadedProgramsJSONPath_RootIsObject(t *testing.T) {
	t.Parallel()

	programs := []bpfman.Program{
		programWithID(11, "tp_c"),
		programWithID(13, "tp_a"),
	}

	var buf bytes.Buffer
	require.NoError(t, RenderLoadedPrograms(&buf, LoadedProgramsView{Programs: programs}, &OutputFlags{Output: OutputValue{Value: "jsonpath={.programs[1].record.program_id}"}}))
	require.Equal(t, "13\n", buf.String())

	buf.Reset()
	require.NoError(t, RenderLoadedPrograms(&buf, LoadedProgramsView{Programs: programs}, &OutputFlags{Output: OutputValue{Value: "jsonpath={range .programs[*]}{.record.program_id} {end}"}}))
	require.Equal(t, "11 13 \n", buf.String())
}
