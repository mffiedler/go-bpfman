package bpfmanbuiltin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/runtime"
	"github.com/frobware/go-bpfman/kernel"
)

// TestDecodeBpfmanResult_ProgramListDTO proves the external dispatch
// backend decodes `program list` into the ProgramEntryListResult DTO,
// preserving the top-level summary fields and -- crucially -- a
// kernel-only entry's null record. Decoding into the old []Program
// shape would silently drop those, so this locks external/library
// parity for the list command.
func TestDecodeBpfmanResult_ProgramListDTO(t *testing.T) {
	t.Parallel()

	const managedID = kernel.ProgramID(11)
	const strayID = kernel.ProgramID(900)

	result := bpfman.ProgramEntryListResult{
		Programs: []bpfman.ProgramListEntry{
			{
				ProgramID:    managedID,
				Managed:      true,
				Application:  "demo",
				Type:         "xdp",
				FunctionName: "xdp_stats",
				Links:        []bpfman.LinkID{100},
				Record: &bpfman.ProgramRecord{
					ProgramID: managedID,
					Load:      bpfman.TestLoadSpec(bpfman.ProgramTypeXDP),
				},
				Kernel: &kernel.Program{ID: managedID, Name: "xdp_stats"},
			},
			{
				ProgramID:    strayID,
				Managed:      false,
				Type:         "tracepoint",
				FunctionName: "stray",
				Links:        []bpfman.LinkID{},
				Record:       nil,
				Kernel:       &kernel.Program{ID: strayID, Name: "stray"},
			},
		},
	}
	stdout, err := json.Marshal(result)
	require.NoError(t, err)

	v, err := decodeBpfmanResult([]runtime.Arg{word("program"), word("list")}, stdout)
	require.NoError(t, err)

	decoded, ok := v.Origin().(bpfman.ProgramEntryListResult)
	require.True(t, ok, "program list must decode into ProgramEntryListResult, not the old []Program shape")
	require.Len(t, decoded.Programs, 2)

	managed := decoded.Programs[0]
	assert.True(t, managed.Managed)
	require.NotNil(t, managed.Record)
	assert.Equal(t, managedID, managed.Record.ProgramID)
	assert.NotNil(t, managed.Kernel)

	stray := decoded.Programs[1]
	assert.False(t, stray.Managed)
	assert.Nil(t, stray.Record, "kernel-only entry keeps a null record through external decode")
	require.NotNil(t, stray.Kernel)
	assert.Equal(t, strayID, stray.Kernel.ID)
	assert.Equal(t, "tracepoint", stray.Type)
}
