package cliformat

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/kernel"
)

// tableGap matches the run of padding tabwriter inserts between columns
// (always two or more spaces), so a header like "LINK ID" stays one cell.
var tableGap = regexp.MustCompile(`\s{2,}`)

// linkListCell renders one link and returns the value under the named
// column, so a test asserts a cell by what it means rather than by a bare
// substring. It requires a header and exactly one data row, and that the
// fixture leaves no empty cells (an empty cell would collapse under the
// gap split and shift the columns).
func linkListCell(t *testing.T, link bpfman.LinkRecord, column string) string {
	t.Helper()

	var buf bytes.Buffer
	if err := RenderLinkList(&buf, LinkListView{Links: []bpfman.LinkRecord{link}}, OutputFormatText); err != nil {
		t.Fatalf("RenderLinkList() error = %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a header and one row, got %d lines:\n%s", len(lines), buf.String())
	}

	headers := tableGap.Split(strings.TrimRight(lines[0], " "), -1)
	cells := tableGap.Split(strings.TrimRight(lines[1], " "), -1)
	if len(headers) != len(cells) {
		t.Fatalf("header/row column mismatch (%d vs %d):\n%s", len(headers), len(cells), buf.String())
	}

	for i, h := range headers {
		if h == column {
			return cells[i]
		}
	}
	t.Fatalf("no column %q in header %q", column, lines[0])
	return ""
}

// The list surfaces the bpfman-managed link ID and the captured kernel
// link ID as separate columns, so a user can correlate the two; a
// regression that dropped or conflated either would land the wrong value
// under the column.
func TestRenderLinkList_ManagedAndKernelIDsInTheirColumns(t *testing.T) {
	t.Parallel()

	kernelLinkID := kernel.LinkID(17)
	link := bpfman.LinkRecord{
		ID:           8,
		ProgramID:    42,
		KernelLinkID: &kernelLinkID,
		Kind:         bpfman.LinkKindTracepoint,
	}

	if got := linkListCell(t, link, "LINK ID"); got != "8" {
		t.Errorf("LINK ID column = %q, want %q", got, "8")
	}
	if got := linkListCell(t, link, "KERNEL LINK ID"); got != "17" {
		t.Errorf("KERNEL LINK ID column = %q, want %q", got, "17")
	}
}

// A link bpfman never captured a kernel ID for shows the sentinel in the
// kernel-ID column, not a blank that reads as missing data nor a zero
// that reads as a real kernel ID.
func TestRenderLinkList_NilKernelIDShowsSentinel(t *testing.T) {
	t.Parallel()

	link := bpfman.LinkRecord{ID: 1, Kind: bpfman.LinkKindXDP}
	if got := linkListCell(t, link, "KERNEL LINK ID"); got != "<none>" {
		t.Errorf("KERNEL LINK ID column with no captured kernel ID = %q, want %q", got, "<none>")
	}
}
