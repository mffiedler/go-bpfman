package cliformat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
)

// TestRenderProgramListTable_Columns asserts the default
// program-list table carries the Program ID, Application, Type,
// Function Name and Links columns from the precomputed entry fields,
// and that the Links cell shows a count with its IDs.
func TestRenderProgramListTable_Columns(t *testing.T) {
	t.Parallel()

	result := bpfman.ProgramListResult{
		Programs: []bpfman.ProgramListEntry{
			{
				ProgramID:    42,
				Managed:      true,
				Application:  "demo",
				Type:         "xdp",
				FunctionName: "xdp_stats",
				Links:        []bpfman.LinkID{100, 101},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderProgramList(&buf, ProgramListView{Result: result}, &OutputFlags{Output: OutputValue{Value: "table"}}))
	out := buf.String()

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

// TestProgramLinksColumn_CountAndTruncation proves the Links cell is
// empty with no links, lists a count with IDs, and truncates beyond
// numListLinks.
func TestProgramLinksColumn_CountAndTruncation(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", programLinksColumn(nil))
	assert.Equal(t, "(2) 1, 2", programLinksColumn([]bpfman.LinkID{1, 2}))
	assert.Equal(t, "(5) 1, 2, 3, ...", programLinksColumn([]bpfman.LinkID{1, 2, 3, 4, 5}))
}
