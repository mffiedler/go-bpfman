package bpfman_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
)

func TestProgramTypeConsistency(t *testing.T) {
	// Verify AllProgramTypes and ProgramTypeNames are consistent
	allTypes := bpfman.AllProgramTypes()
	allNames := bpfman.ProgramTypeNames()

	require.Equal(t, len(allTypes), len(allNames), "AllProgramTypes and ProgramTypeNames should have same length")

	for i, pt := range allTypes {
		assert.Equal(t, pt.String(), allNames[i], "ProgramTypeNames[%d] should match AllProgramTypes[%d].String()", i, i)
	}

	// Verify ParseProgramType accepts all names from ProgramTypeNames
	for _, name := range allNames {
		pt, ok := bpfman.ParseProgramType(name)
		assert.True(t, ok, "ParseProgramType should accept %q", name)
		assert.Equal(t, name, pt.String(), "round-trip should preserve name")
	}

	// Verify AllProgramTypes doesn't include Unspecified
	for _, pt := range allTypes {
		assert.NotEqual(t, bpfman.ProgramTypeUnspecified, pt, "AllProgramTypes should not include Unspecified")
	}
}
