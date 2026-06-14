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

// ValidateAttachSpec leaves XDP and TC untouched: they are fully
// refined by their constructors, so the library has nothing to do.
func TestValidateAttachSpecPassesRefinedXDPTCThrough(t *testing.T) {
	t.Parallel()

	xdp, err := NewXDPAttachSpec(1, "eth0", 0)
	require.NoError(t, err)
	gotXDP, err := ValidateAttachSpec(xdp)
	require.NoError(t, err)
	assert.Equal(t, xdp, gotXDP)

	tc, err := NewTCAttachSpec(1, "eth0", TCDirectionIngress, 25)
	require.NoError(t, err)
	gotTC, err := ValidateAttachSpec(tc)
	require.NoError(t, err)
	assert.Equal(t, tc, gotTC)
}

// TCX priority handling has not yet moved to the constructor (parked
// pending the Rust-parity decision, since Rust's TCX has no default
// priority). It still defaults to 50, normalises 0 via WithPriority,
// and is negative-checked by ValidateAttachSpec.
func TestTCXAttachSpecPriorityParkedBehaviour(t *testing.T) {
	t.Parallel()

	tcx, err := NewTCXAttachSpec(1, "eth0", TCDirectionIngress)
	require.NoError(t, err)
	assert.Equal(t, DefaultAttachPriority, tcx.Priority())
	assert.Equal(t, DefaultAttachPriority, tcx.WithPriority(0).Priority())

	_, err = ValidateAttachSpec(tcx.WithPriority(-1))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAttachSpec)
	assert.Contains(t, err.Error(), "priority must be non-negative")
}
