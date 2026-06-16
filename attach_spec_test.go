package bpfman

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Priority for XDP and TC is parsed at construction: an omitted (0)
// priority normalises to DefaultAttachPriority, a positive value is
// kept verbatim, and a negative value is rejected. The stored value
// is therefore the effective value and the library never re-checks it.

func TestXDPAttachSpecPriorityParsing(t *testing.T) {
	t.Parallel()

	omitted, err := NewXDPAttachSpec(1, "eth0", 0)
	require.NoError(t, err)
	assert.Equal(t, DefaultAttachPriority, omitted.Priority())

	explicit, err := NewXDPAttachSpec(1, "eth0", 25)
	require.NoError(t, err)
	assert.Equal(t, 25, explicit.Priority())

	_, err = NewXDPAttachSpec(1, "eth0", -1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAttachSpec)
	assert.Contains(t, err.Error(), "priority must be non-negative")
}

func TestTCAttachSpecPriorityParsing(t *testing.T) {
	t.Parallel()

	omitted, err := NewTCAttachSpec(1, "eth0", TCDirectionIngress, 0)
	require.NoError(t, err)
	assert.Equal(t, DefaultAttachPriority, omitted.Priority())

	explicit, err := NewTCAttachSpec(1, "eth0", TCDirectionIngress, 25)
	require.NoError(t, err)
	assert.Equal(t, 25, explicit.Priority())

	_, err = NewTCAttachSpec(1, "eth0", TCDirectionIngress, -1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAttachSpec)
	assert.Contains(t, err.Error(), "priority must be non-negative")
}

func TestXDPAttachSpecRejectsMissingFields(t *testing.T) {
	t.Parallel()

	_, err := NewXDPAttachSpec(0, "eth0", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "programID is required")

	_, err = NewXDPAttachSpec(1, "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ifname is required")
}

func TestXDPAttachSpecProceedOnCodesValidateActions(t *testing.T) {
	t.Parallel()

	spec, err := NewXDPAttachSpec(1, "eth0", 0)
	require.NoError(t, err)

	got, err := spec.WithProceedOnCodes([]int32{2, 31})
	require.NoError(t, err)
	assert.Equal(t, []int32{2, 31}, got.ProceedOn())

	_, err = spec.WithProceedOnCodes([]int32{5})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown XDP action code 5")
}

func TestTCAttachSpecProceedOnCodesValidateActions(t *testing.T) {
	t.Parallel()

	spec, err := NewTCAttachSpec(1, "eth0", TCDirectionIngress, 0)
	require.NoError(t, err)

	got, err := spec.WithProceedOnCodes([]int32{-1, 30})
	require.NoError(t, err)
	assert.Equal(t, []int32{-1, 30}, got.ProceedOn())

	_, err = spec.WithProceedOnCodes([]int32{9})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown TC action code 9")
}

// TCX priority is stored verbatim, matching Rust: TCX has no
// dispatcher default, so an omitted (0) priority stays 0, a positive
// value is kept as given, and only a negative value is rejected.
func TestTCXAttachSpecPriorityParsing(t *testing.T) {
	t.Parallel()

	omitted, err := NewTCXAttachSpec(1, "eth0", TCDirectionIngress, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, omitted.Priority())

	explicit, err := NewTCXAttachSpec(1, "eth0", TCDirectionIngress, 25)
	require.NoError(t, err)
	assert.Equal(t, 25, explicit.Priority())

	_, err = NewTCXAttachSpec(1, "eth0", TCDirectionIngress, -1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAttachSpec)
	assert.Contains(t, err.Error(), "priority must be non-negative")
}

// uprobe pid and containerPid are parsed at construction: 0 means
// unset, a positive value is kept, and a negative value is rejected.
func TestUprobeAttachSpecPidParsing(t *testing.T) {
	t.Parallel()

	unset, err := NewUprobeAttachSpec(1, "/bin/example", 0, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 0, unset.Pid())
	assert.EqualValues(t, 0, unset.ContainerPid())

	set, err := NewUprobeAttachSpec(1, "/bin/example", 42, 7)
	require.NoError(t, err)
	assert.EqualValues(t, 42, set.Pid())
	assert.EqualValues(t, 7, set.ContainerPid())

	_, err = NewUprobeAttachSpec(1, "/bin/example", -1, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAttachSpec)
	assert.Contains(t, err.Error(), "pid must be non-negative")

	_, err = NewUprobeAttachSpec(1, "/bin/example", 0, -1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAttachSpec)
	assert.Contains(t, err.Error(), "container pid must be non-negative")
}
