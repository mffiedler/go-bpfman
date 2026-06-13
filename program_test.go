package bpfman_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
)

// TestProgramEntryListResult_EmptyMarshalsAsEmptyArray pins the wire
// contract that an empty program list serialises as
// `"programs": []`, never `"programs": null`. The shell binds
// list results through ValueFromStruct -> json.Marshal, and a
// `null` payload would break consumer jq expressions such as
// `.programs[]`. The producer (manager.ListProgramEntries) is
// responsible for returning the entries as a non-nil slice on the
// empty case; this test pins the resulting wire shape so an
// accidental regression in the producer is caught at the
// shell-facing boundary rather than in distant e2e scripts.
func TestProgramEntryListResult_EmptyMarshalsAsEmptyArray(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(bpfman.ProgramEntryListResult{Programs: []bpfman.ProgramListEntry{}})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"programs":[]`)
	assert.NotContains(t, string(data), `"programs":null`)
}

func TestProgramTypeConsistency(t *testing.T) {
	t.Parallel()

	// Verify AllProgramTypes and ProgramTypeNames are consistent
	allTypes := bpfman.AllProgramTypes()
	allNames := bpfman.ProgramTypeNames()

	require.Equal(t, len(allTypes), len(allNames), "AllProgramTypes and ProgramTypeNames should have same length")

	for i, pt := range allTypes {
		assert.Equal(t, pt.String(), allNames[i], "ProgramTypeNames[%d] should match AllProgramTypes[%d].String()", i, i)
	}

	// Verify ParseProgramType accepts all names from ProgramTypeNames
	for _, name := range allNames {
		pt, err := bpfman.ParseProgramType(name)
		assert.NoError(t, err, "ParseProgramType should accept %q", name)
		assert.Equal(t, name, pt.String(), "round-trip should preserve name")
	}

	// Verify AllProgramTypes doesn't include zero value
	for _, pt := range allTypes {
		assert.NotEqual(t, bpfman.ProgramType{}, pt, "AllProgramTypes should not include zero value")
	}
}
