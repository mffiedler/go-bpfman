package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEffectivePriority(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 50, effectivePriority(0), "zero maps to DefaultPriority")
	assert.Equal(t, 1, effectivePriority(1))
	assert.Equal(t, 50, effectivePriority(50))
	assert.Equal(t, 99, effectivePriority(99))
}

func TestSortRebuildSlots_LegacyZeroPrioritySortsAsDefault(t *testing.T) {
	t.Parallel()

	slots := []rebuildSlot{
		{ProgramName: "low", Priority: 10},
		{ProgramName: "legacy", Priority: 0},
		{ProgramName: "high", Priority: 100},
	}
	sortRebuildSlots(slots)
	assert.Equal(t, "low", slots[0].ProgramName)
	assert.Equal(t, 10, slots[0].Priority)
	assert.Equal(t, "legacy", slots[1].ProgramName)
	assert.Equal(t, "high", slots[2].ProgramName)
}

func TestSortRebuildSlots_LegacyZeroPriorityTiebreaksWithExplicit50(t *testing.T) {
	t.Parallel()

	slots := []rebuildSlot{
		{ProgramName: "explicit", Priority: 50},
		{ProgramName: "legacy", Priority: 0},
	}
	sortRebuildSlots(slots)
	// Both have effective priority 50; name tiebreak: "explicit" < "legacy".
	assert.Equal(t, "explicit", slots[0].ProgramName)
	assert.Equal(t, "legacy", slots[1].ProgramName)
}

func TestSortRebuildSlots_AllLegacyZeroPriority(t *testing.T) {
	t.Parallel()

	slots := []rebuildSlot{
		{ProgramName: "charlie", Priority: 0},
		{ProgramName: "alpha", Priority: 0},
		{ProgramName: "bravo", Priority: 0},
	}
	sortRebuildSlots(slots)
	assert.Equal(t, "alpha", slots[0].ProgramName)
	assert.Equal(t, "bravo", slots[1].ProgramName)
	assert.Equal(t, "charlie", slots[2].ProgramName)
}
