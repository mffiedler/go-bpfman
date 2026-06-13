package cliformat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// TestFormatProgramsCompositeTable_Columns asserts the default
// program-list table carries the Program ID, Application, Type,
// Function Name and Links columns, and that the Links cell shows a
// count with its IDs.
func TestFormatProgramsCompositeTable_Columns(t *testing.T) {
	t.Parallel()

	result := bpfman.ProgramListResult{
		Programs: []bpfman.Program{
			{
				Record: bpfman.ProgramRecord{
					ProgramID: 42,
					Load:      bpfman.TestLoadSpec(bpfman.ProgramTypeXDP),
					Meta: bpfman.ProgramMeta{
						Name:     "xdp_stats",
						Metadata: map[string]string{applicationMetadataKey: "demo"},
					},
				},
				Status: bpfman.ProgramStatus{
					Links: []bpfman.Link{
						{Record: bpfman.LinkRecord{ID: 100, Kind: bpfman.LinkKindXDP}},
						{Record: bpfman.LinkRecord{ID: 101, Kind: bpfman.LinkKindXDP}},
					},
				},
			},
		},
	}

	out, err := FormatProgramsComposite(result, &OutputFlags{Output: OutputValue{Value: "table"}})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2, "header plus one row")

	header := strings.Fields(lines[0])
	assert.Equal(t, []string{"PROGRAM", "ID", "APPLICATION", "TYPE", "FUNCTION", "NAME", "LINKS"}, header)

	assert.Contains(t, lines[1], "42")
	assert.Contains(t, lines[1], "demo")
	assert.Contains(t, lines[1], "xdp")
	assert.Contains(t, lines[1], "xdp_stats")
	assert.Contains(t, lines[1], "(2) 100, 101")
}

// TestProgramFunctionName_FallsBackToKernelName proves the Function
// Name column prefers the stored ELF name and falls back to the
// kernel name only when no managed name is present.
func TestProgramFunctionName_FallsBackToKernelName(t *testing.T) {
	t.Parallel()

	managed := bpfman.Program{
		Record: bpfman.ProgramRecord{Meta: bpfman.ProgramMeta{Name: "full_elf_name"}},
		Status: bpfman.ProgramStatus{Kernel: &kernel.Program{Name: "trunc_kernel"}},
	}
	assert.Equal(t, "full_elf_name", programFunctionName(managed))

	kernelOnly := bpfman.Program{
		Status: bpfman.ProgramStatus{Kernel: &kernel.Program{Name: "trunc_kernel"}},
	}
	assert.Equal(t, "trunc_kernel", programFunctionName(kernelOnly))
}

// TestProgramLinksColumn_CountAndTruncation proves the Links cell is
// empty with no links, lists a count with IDs, and truncates beyond
// numListLinks.
func TestProgramLinksColumn_CountAndTruncation(t *testing.T) {
	t.Parallel()

	none := bpfman.Program{}
	assert.Equal(t, "", programLinksColumn(none))

	link := func(id bpfman.LinkID) bpfman.Link {
		return bpfman.Link{Record: bpfman.LinkRecord{ID: id}}
	}

	two := bpfman.Program{Status: bpfman.ProgramStatus{Links: []bpfman.Link{link(1), link(2)}}}
	assert.Equal(t, "(2) 1, 2", programLinksColumn(two))

	five := bpfman.Program{Status: bpfman.ProgramStatus{Links: []bpfman.Link{
		link(1), link(2), link(3), link(4), link(5),
	}}}
	assert.Equal(t, "(5) 1, 2, 3, ...", programLinksColumn(five))
}
