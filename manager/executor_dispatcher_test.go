package manager

import (
	"testing"

	"github.com/frobware/go-bpfman/platform"
	"github.com/stretchr/testify/assert"
)

func TestSortSlotsByPriority_OrdersByPriorityThenName(t *testing.T) {
	slots := []platform.DispatcherSlot{
		{Position: 0, Priority: 100, ProgramName: "beta"},
		{Position: 1, Priority: 50, ProgramName: "alpha"},
		{Position: 2, Priority: 100, ProgramName: "alpha"},
	}
	sortSlotsByPriority(slots)
	assert.Equal(t, "alpha", slots[0].ProgramName)
	assert.Equal(t, 50, slots[0].Priority)
	assert.Equal(t, "alpha", slots[1].ProgramName)
	assert.Equal(t, 100, slots[1].Priority)
	assert.Equal(t, "beta", slots[2].ProgramName)
	assert.Equal(t, 100, slots[2].Priority)
}

func TestSortSlotsByPriority_Empty(t *testing.T) {
	var slots []platform.DispatcherSlot
	sortSlotsByPriority(slots) // should not panic
}

func TestSortSlotsByPriority_Single(t *testing.T) {
	slots := []platform.DispatcherSlot{{Position: 3, Priority: 10, ProgramName: "prog"}}
	sortSlotsByPriority(slots)
	assert.Equal(t, 3, slots[0].Position)
}

func TestEffectivePriority(t *testing.T) {
	assert.Equal(t, 50, effectivePriority(0), "zero maps to DefaultPriority")
	assert.Equal(t, 1, effectivePriority(1))
	assert.Equal(t, 50, effectivePriority(50))
	assert.Equal(t, 99, effectivePriority(99))
}

func TestSortRebuildSlots_ZeroPrioritySortsAsDefault(t *testing.T) {
	slots := []rebuildSlot{
		{ProgramName: "low", Priority: 10},
		{ProgramName: "unset", Priority: 0}, // effective 50
		{ProgramName: "high", Priority: 100},
	}
	sortRebuildSlots(slots)
	assert.Equal(t, "low", slots[0].ProgramName)
	assert.Equal(t, 10, slots[0].Priority)
	assert.Equal(t, "unset", slots[1].ProgramName)
	assert.Equal(t, 0, slots[1].Priority, "raw value preserved")
	assert.Equal(t, "high", slots[2].ProgramName)
}

func TestSortRebuildSlots_ZeroPriorityTiebreaksWithExplicit50(t *testing.T) {
	slots := []rebuildSlot{
		{ProgramName: "explicit", Priority: 50},
		{ProgramName: "unset", Priority: 0},
	}
	sortRebuildSlots(slots)
	// Both have effective priority 50; name tiebreak: "explicit" < "unset".
	assert.Equal(t, "explicit", slots[0].ProgramName)
	assert.Equal(t, "unset", slots[1].ProgramName)
}

func TestSortRebuildSlots_AllZeroPriority(t *testing.T) {
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
