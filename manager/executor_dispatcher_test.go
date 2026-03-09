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
