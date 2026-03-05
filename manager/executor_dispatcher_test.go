package manager

import (
	"fmt"
	"testing"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/platform"
	"github.com/stretchr/testify/assert"
)

func TestFindFreeSlot_EmptyReturnsZero(t *testing.T) {
	assert.Equal(t, 0, findFreeSlot(nil))
}

func TestFindFreeSlot_FirstOccupied(t *testing.T) {
	slots := []platform.DispatcherSlot{{Position: 0}}
	assert.Equal(t, 1, findFreeSlot(slots))
}

func TestFindFreeSlot_Gap(t *testing.T) {
	slots := []platform.DispatcherSlot{
		{Position: 0},
		{Position: 2},
	}
	assert.Equal(t, 1, findFreeSlot(slots))
}

func TestFindFreeSlot_AllOccupied(t *testing.T) {
	var slots []platform.DispatcherSlot
	for i := range dispatcher.MaxPrograms {
		slots = append(slots, platform.DispatcherSlot{Position: i})
	}
	assert.Equal(t, -1, findFreeSlot(slots))
}

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

func TestComputeRuntimeConfig_NewSlotOnly(t *testing.T) {
	newSlot := &platform.DispatcherSlot{
		Position:    0,
		Priority:    100,
		ProgramName: "prog",
		ProceedOn:   0x4, // bit 2
	}
	cfg := computeRuntimeConfig(nil, newSlot, 0)
	assert.Equal(t, uint32(1), cfg.NumProgsEnabled)
	assert.Equal(t, uint32(0), cfg.RunOrder[0])
	assert.Equal(t, uint32(0x4), cfg.ChainCallActions[0])
}

func TestComputeRuntimeConfig_PriorityDeterminesRunOrder(t *testing.T) {
	existing := []platform.DispatcherSlot{
		{Position: 0, Priority: 1000, ProgramName: "high", ProceedOn: 0x1},
		{Position: 1, Priority: 0, ProgramName: "low", ProceedOn: 0x2},
	}
	newSlot := &platform.DispatcherSlot{
		Position:    2,
		Priority:    500,
		ProgramName: "mid",
		ProceedOn:   0x4,
	}
	cfg := computeRuntimeConfig(existing, newSlot, 0)

	assert.Equal(t, uint32(3), cfg.NumProgsEnabled)
	// Sorted by priority: low(0) -> mid(500) -> high(1000)
	// Positions:           1         2            0
	assert.Equal(t, uint32(1), cfg.RunOrder[0], "first should be slot 1 (priority 0)")
	assert.Equal(t, uint32(2), cfg.RunOrder[1], "second should be slot 2 (priority 500)")
	assert.Equal(t, uint32(0), cfg.RunOrder[2], "third should be slot 0 (priority 1000)")

	// chain_call_actions indexed by physical position
	assert.Equal(t, uint32(0x1), cfg.ChainCallActions[0], "slot 0 proceed_on")
	assert.Equal(t, uint32(0x2), cfg.ChainCallActions[1], "slot 1 proceed_on")
	assert.Equal(t, uint32(0x4), cfg.ChainCallActions[2], "slot 2 proceed_on")
}

func TestComputeRuntimeConfig_NilNewSlot(t *testing.T) {
	existing := []platform.DispatcherSlot{
		{Position: 3, Priority: 10, ProgramName: "a", ProceedOn: 0x8},
		{Position: 7, Priority: 20, ProgramName: "b", ProceedOn: 0x10},
	}
	cfg := computeRuntimeConfig(existing, nil, 0)

	assert.Equal(t, uint32(2), cfg.NumProgsEnabled)
	assert.Equal(t, uint32(3), cfg.RunOrder[0])
	assert.Equal(t, uint32(7), cfg.RunOrder[1])
	assert.Equal(t, uint32(0x8), cfg.ChainCallActions[3])
	assert.Equal(t, uint32(0x10), cfg.ChainCallActions[7])
}

func TestComputeRuntimeConfig_SamePriorityBreaksByName(t *testing.T) {
	existing := []platform.DispatcherSlot{
		{Position: 5, Priority: 100, ProgramName: "zebra", ProceedOn: 0x1},
		{Position: 2, Priority: 100, ProgramName: "apple", ProceedOn: 0x2},
	}
	cfg := computeRuntimeConfig(existing, nil, 0)

	assert.Equal(t, uint32(2), cfg.NumProgsEnabled)
	// apple before zebra at same priority
	assert.Equal(t, uint32(2), cfg.RunOrder[0], "apple at position 2 runs first")
	assert.Equal(t, uint32(5), cfg.RunOrder[1], "zebra at position 5 runs second")
}

func TestComputeRuntimeConfig_AllTenSlots(t *testing.T) {
	// Fill all 10 slots with descending priorities: slot 0 has
	// priority 900, slot 1 has 800, ..., slot 9 has 0. The
	// run_order should reverse the slot indices (lowest priority
	// first).
	var existing []platform.DispatcherSlot
	for i := range dispatcher.MaxPrograms {
		existing = append(existing, platform.DispatcherSlot{
			Position:    i,
			Priority:    (dispatcher.MaxPrograms - 1 - i) * 100,
			ProgramName: fmt.Sprintf("prog_%d", i),
			ProceedOn:   1 << uint(i),
		})
	}
	cfg := computeRuntimeConfig(existing, nil, 0)

	assert.Equal(t, uint32(dispatcher.MaxPrograms), cfg.NumProgsEnabled)

	// Slot 9 (priority 0) should run first, slot 0 (priority 900)
	// should run last.
	for i := range dispatcher.MaxPrograms {
		expectedSlot := uint32(dispatcher.MaxPrograms - 1 - i)
		assert.Equal(t, expectedSlot, cfg.RunOrder[i],
			"run_order[%d] should be slot %d", i, expectedSlot)
	}

	// chain_call_actions should be indexed by physical position.
	for i := range dispatcher.MaxPrograms {
		assert.Equal(t, uint32(1<<uint(i)), cfg.ChainCallActions[i],
			"chain_call_actions[%d]", i)
	}
}

func TestComputeRuntimeConfig_AllTenSlotsSamePriority(t *testing.T) {
	// Fill all 10 slots at the same priority with names that sort
	// in reverse of position order. The run_order should follow
	// alphabetical name order.
	names := []string{
		"juliet", "india", "hotel", "golf", "foxtrot",
		"echo", "delta", "charlie", "bravo", "alpha",
	}
	var existing []platform.DispatcherSlot
	for i := range dispatcher.MaxPrograms {
		existing = append(existing, platform.DispatcherSlot{
			Position:    i,
			Priority:    50,
			ProgramName: names[i],
			ProceedOn:   0x1,
		})
	}
	cfg := computeRuntimeConfig(existing, nil, 0)

	assert.Equal(t, uint32(dispatcher.MaxPrograms), cfg.NumProgsEnabled)

	// Alphabetical order: alpha(9), bravo(8), charlie(7), delta(6),
	// echo(5), foxtrot(4), golf(3), hotel(2), india(1), juliet(0)
	for i := range dispatcher.MaxPrograms {
		expectedSlot := uint32(dispatcher.MaxPrograms - 1 - i)
		assert.Equal(t, expectedSlot, cfg.RunOrder[i],
			"run_order[%d] should be slot %d (name %s)", i, expectedSlot, names[expectedSlot])
	}
}

func TestComputeRuntimeConfig_DoesNotMutateExisting(t *testing.T) {
	existing := []platform.DispatcherSlot{
		{Position: 0, Priority: 100, ProgramName: "high"},
		{Position: 1, Priority: 50, ProgramName: "low"},
	}
	// Save original order
	origFirst := existing[0]
	origSecond := existing[1]

	computeRuntimeConfig(existing, nil, 0)

	// existing slice should not be reordered
	assert.Equal(t, origFirst, existing[0])
	assert.Equal(t, origSecond, existing[1])
}

func TestComputeRuntimeConfig_TCChainCallShift(t *testing.T) {
	// TC BPF dispatchers check (1 << (ret + 1)) instead of
	// (1 << ret) to accommodate TC_ACT_UNSPEC = -1. When the
	// chainCallShift is 1, proceed-on bitmasks should be shifted
	// left by 1 in the resulting config.
	existing := []platform.DispatcherSlot{
		{Position: 0, Priority: 100, ProgramName: "a", ProceedOn: 0x9},  // bits 0,3 (TC_ACT_OK, TC_ACT_PIPE)
		{Position: 1, Priority: 200, ProgramName: "b", ProceedOn: 0x40000009}, // bits 0,3,30
	}
	cfg := computeRuntimeConfig(existing, nil, 1)

	assert.Equal(t, uint32(2), cfg.NumProgsEnabled)
	// Bitmasks should be shifted left by 1.
	assert.Equal(t, uint32(0x12), cfg.ChainCallActions[0], "0x9 << 1 = 0x12")
	assert.Equal(t, uint32(0x80000012), cfg.ChainCallActions[1], "0x40000009 << 1 = 0x80000012")
}

func TestComputeRuntimeConfig_XDPNoShift(t *testing.T) {
	// XDP dispatchers check (1 << ret) directly, so chainCallShift=0
	// leaves proceed-on bitmasks unchanged.
	existing := []platform.DispatcherSlot{
		{Position: 0, Priority: 100, ProgramName: "a", ProceedOn: 0x4}, // XDP_PASS
	}
	cfg := computeRuntimeConfig(existing, nil, 0)

	assert.Equal(t, uint32(0x4), cfg.ChainCallActions[0], "no shift applied")
}
