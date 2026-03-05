package manager

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTcProceedOnBitmask_Empty(t *testing.T) {
	// Empty action list falls back to the default.
	got := tcProceedOnBitmask(nil)
	assert.Equal(t, uint32(DefaultTCProceedOn), got)

	got = tcProceedOnBitmask([]int32{})
	assert.Equal(t, uint32(DefaultTCProceedOn), got)
}

func TestTcProceedOnBitmask_SingleAction(t *testing.T) {
	// TC_ACT_OK = 0 -> bit 0
	got := tcProceedOnBitmask([]int32{0})
	assert.Equal(t, uint32(1<<0), got)

	// TC_ACT_PIPE = 3 -> bit 3
	got = tcProceedOnBitmask([]int32{3})
	assert.Equal(t, uint32(1<<3), got)
}

func TestTcProceedOnBitmask_MultipleActions(t *testing.T) {
	// TC_ACT_OK=0, TC_ACT_PIPE=3 -> bits 0 and 3
	got := tcProceedOnBitmask([]int32{0, 3})
	assert.Equal(t, uint32((1<<0)|(1<<3)), got)
}

func TestTcProceedOnBitmask_IgnoresOutOfRange(t *testing.T) {
	// Negative and >= 32 values are silently ignored.
	got := tcProceedOnBitmask([]int32{-1, 0, 32, 3})
	assert.Equal(t, uint32((1<<0)|(1<<3)), got)
}

func TestTcProceedOnBitmask_MatchesDefaultConstant(t *testing.T) {
	// Actions that produce the same bitmask as DefaultTCProceedOn.
	got := tcProceedOnBitmask([]int32{0, 3, 30})
	assert.Equal(t, uint32(DefaultTCProceedOn), got)
}
