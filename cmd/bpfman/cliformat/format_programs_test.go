package cliformat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bpfman/bpfman"
)

// programListCell renders one entry and returns the value under the named
// column, so a test asserts a cell by meaning. It relies on the fixture
// leaving no empty cells (an empty cell collapses under the gap split).
func programListCell(t *testing.T, entry bpfman.ProgramListEntry, column string) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, RenderProgramList(&buf, ProgramListView{Result: bpfman.ProgramListResult{Programs: []bpfman.ProgramListEntry{entry}}}, OutputFormatText))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2, "header plus one row")

	headers := tableGap.Split(strings.TrimRight(lines[0], " "), -1)
	cells := tableGap.Split(strings.TrimRight(lines[1], " "), -1)
	require.Len(t, cells, len(headers), "row column count matches header")

	for i, h := range headers {
		if h == column {
			return cells[i]
		}
	}
	t.Fatalf("no column %q in header %q", column, lines[0])
	return ""
}

// The default program-list table carries exactly these columns, with the
// multi-word headers spelled with spaces.
func TestRenderProgramList_Columns(t *testing.T) {
	t.Parallel()

	entry := bpfman.ProgramListEntry{ProgramID: 42, Application: "demo", Type: "xdp", FunctionName: "xdp_stats"}
	var buf bytes.Buffer
	require.NoError(t, RenderProgramList(&buf, ProgramListView{Result: bpfman.ProgramListResult{Programs: []bpfman.ProgramListEntry{entry}}}, OutputFormatText))

	header := tableGap.Split(strings.TrimRight(strings.SplitN(buf.String(), "\n", 2)[0], " "), -1)
	assert.Equal(t, []string{"PROGRAM ID", "APPLICATION", "TYPE", "FUNCTION NAME", "#LINKS"}, header)
}

// The link column reports the arity -- a bare count -- not the IDs.
func TestRenderProgramList_LinkColumnIsACount(t *testing.T) {
	t.Parallel()

	entry := bpfman.ProgramListEntry{ProgramID: 42, Application: "demo", Type: "xdp", FunctionName: "xdp_stats", Links: []bpfman.LinkID{100, 101}}
	if got := programListCell(t, entry, "#LINKS"); got != "2" {
		t.Errorf("# LINKS = %q, want %q (the count, not the IDs)", got, "2")
	}
}

// A program with no links reports zero, not a blank cell, so arity reads
// unambiguously.
func TestRenderProgramList_NoLinksReportsZero(t *testing.T) {
	t.Parallel()

	entry := bpfman.ProgramListEntry{ProgramID: 7, Application: "app", Type: "tc", FunctionName: "fn"}
	if got := programListCell(t, entry, "#LINKS"); got != "0" {
		t.Errorf("# LINKS with no links = %q, want %q", got, "0")
	}
}
