package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// sortRebuildSlots orders dispatcher members by priority ascending,
// then program name at equal priority. Priorities reaching it are
// already normalised at spec construction (0 -> DefaultAttachPriority,
// negatives rejected), so these exercise the ordering with the
// concrete values it actually sees.

func TestSortRebuildSlots_ByPriority(t *testing.T) {
	t.Parallel()

	slots := []rebuildSlot{
		{ProgramName: "high", Priority: 100},
		{ProgramName: "low", Priority: 10},
		{ProgramName: "mid", Priority: 50},
	}
	sortRebuildSlots(slots)
	assert.Equal(t,
		[]string{"low", "mid", "high"},
		[]string{slots[0].ProgramName, slots[1].ProgramName, slots[2].ProgramName})
}

func TestSortRebuildSlots_NameTiebreakAtEqualPriority(t *testing.T) {
	t.Parallel()

	slots := []rebuildSlot{
		{ProgramName: "charlie", Priority: 50},
		{ProgramName: "alpha", Priority: 50},
		{ProgramName: "bravo", Priority: 50},
	}
	sortRebuildSlots(slots)
	assert.Equal(t,
		[]string{"alpha", "bravo", "charlie"},
		[]string{slots[0].ProgramName, slots[1].ProgramName, slots[2].ProgramName})
}
