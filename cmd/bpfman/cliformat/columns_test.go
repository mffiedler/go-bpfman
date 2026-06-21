package cliformat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

func TestColumnSpec_ExtractValue(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{
			ProgramID: 42,
			Load:      bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeXDP, "/path/to/prog.o"),
			Meta: bpfman.ProgramMeta{
				Name: "test_prog",
			},
		},
		Status: bpfman.ProgramStatus{
			Kernel: &kernel.Program{
				ID:     42,
				Tag:    "abc123",
				MapIDs: []kernel.MapID{1, 2, 3},
			},
			Links: []bpfman.Link{
				{
					Record: bpfman.LinkRecord{
						ID:   100,
						Kind: bpfman.LinkKindXDP,
						Details: bpfman.XDPDetails{
							Interface: "eth0",
							Position:  1,
						},
					},
				},
			},
		},
	}

	// Use the registry to get extractors for known columns
	index := programColumnIndex()

	tests := []struct {
		name       string
		columnName string
		want       string
	}{
		{
			name:       "program_id",
			columnName: "PROGRAM_ID",
			want:       "42",
		},
		{
			name:       "program type",
			columnName: "TYPE",
			want:       "xdp",
		},
		{
			name:       "name",
			columnName: "NAME",
			want:       "test_prog",
		},
		{
			name:       "tag",
			columnName: "TAG",
			want:       "abc123",
		},
		{
			name:       "map_ids",
			columnName: "MAP_IDS",
			want:       "[1,2,3]",
		},
		{
			name:       "special LINK_IDS",
			columnName: "LINK_IDS",
			want:       "100",
		},
		{
			name:       "special ATTACH",
			columnName: "ATTACH",
			want:       "eth0 (ifindex=0, pos=1)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info, ok := index[tt.columnName]
			if !ok {
				t.Fatalf("column %q not found in registry", tt.columnName)
			}
			col := ColumnSpec{Name: info.Name, Extract: info.Extract}
			got := col.ExtractValue(prog)
			if got != tt.want {
				t.Errorf("ColumnSpec.ExtractValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestColumnSet_RenderTable(t *testing.T) {
	t.Parallel()

	programs := []bpfman.Program{
		{
			Record: bpfman.ProgramRecord{
				ProgramID: 42,
				Load:      bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeXDP, "/path/to/prog1.o"),
				Meta: bpfman.ProgramMeta{
					Name: "prog1",
				},
			},
		},
		{
			Record: bpfman.ProgramRecord{
				ProgramID: 43,
				Load:      bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeTC, "/path/to/prog2.o"),
				Meta: bpfman.ProgramMeta{
					Name: "prog2",
				},
			},
		},
	}

	// Use registry-backed column selection
	columns := MustSelectProgramColumns([]string{"PROGRAM_ID", "NAME"})

	var buf bytes.Buffer
	if err := columns.RenderTable(&buf, programs); err != nil {
		t.Fatalf("RenderTable() error = %v", err)
	}
	output := buf.String()

	// Verify header exists
	if !strings.Contains(output, "PROGRAM_ID") || !strings.Contains(output, "NAME") {
		t.Errorf("RenderTable() output missing headers: %s", output)
	}

	// Verify data rows
	if !strings.Contains(output, "42") || !strings.Contains(output, "prog1") {
		t.Errorf("RenderTable() output missing first row data: %s", output)
	}
	if !strings.Contains(output, "43") || !strings.Contains(output, "prog2") {
		t.Errorf("RenderTable() output missing second row data: %s", output)
	}
}

func TestDefaultColumns(t *testing.T) {
	t.Parallel()

	cols := DefaultColumns()
	if len(cols.Columns) != 4 {
		t.Errorf("DefaultColumns() has %d columns, want 4", len(cols.Columns))
	}

	expected := []string{"PROGRAM_ID", "TYPE", "NAME", "SOURCE"}
	for i, col := range cols.Columns {
		if col.Name != expected[i] {
			t.Errorf("DefaultColumns()[%d].Name = %q, want %q", i, col.Name, expected[i])
		}
	}
}

func TestDefaultLinkColumns_ExposeManagedAndKernelIDs(t *testing.T) {
	t.Parallel()

	cols := DefaultLinkColumns()
	expected := []string{"LINK ID", "KERNEL LINK ID", "KIND", "PROGRAM ID", "PIN PATH"}
	if len(cols.Columns) != len(expected) {
		t.Fatalf("DefaultLinkColumns() has %d columns, want %d", len(cols.Columns), len(expected))
	}
	for i, col := range cols.Columns {
		if col.Name != expected[i] {
			t.Errorf("DefaultLinkColumns()[%d].Name = %q, want %q", i, col.Name, expected[i])
		}
	}

	kernelLinkID := kernel.LinkID(17)
	link := bpfman.LinkRecord{
		ID:           2_123_456_789,
		ProgramID:    42,
		KernelLinkID: &kernelLinkID,
		Kind:         bpfman.LinkKindTracepoint,
	}
	var buf bytes.Buffer
	if err := cols.RenderLinkTable(&buf, []bpfman.LinkRecord{link}); err != nil {
		t.Fatalf("RenderLinkTable() error = %v", err)
	}
	output := buf.String()
	for _, want := range []string{"LINK ID", "KERNEL LINK ID", "2123456789", "17"} {
		if !strings.Contains(output, want) {
			t.Errorf("link table missing %q: %s", want, output)
		}
	}
}

func TestLinkKernelIDColumn_Nil(t *testing.T) {
	t.Parallel()

	index := linkColumnIndex()
	info := index["KERNEL LINK ID"]
	col := ColumnSpec{Name: info.Name, ExtractLink: info.ExtractLink}
	got := col.ExtractLinkValue(bpfman.LinkRecord{})
	if got != "<none>" {
		t.Errorf("KERNEL LINK ID for link with no captured kernel ID = %q, want %q", got, "<none>")
	}
}

func TestExtractLinkIDs_NoLinks(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Status: bpfman.ProgramStatus{
			Links: nil,
		},
	}

	index := programColumnIndex()
	info := index["LINK_IDS"]
	col := ColumnSpec{Name: info.Name, Extract: info.Extract}
	got := col.ExtractValue(prog)
	if got != "<none>" {
		t.Errorf("LINK_IDS for program with no links = %q, want %q", got, "<none>")
	}
}

func TestExtractAttach_NoLinks(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Status: bpfman.ProgramStatus{
			Links: nil,
		},
	}

	index := programColumnIndex()
	info := index["ATTACH"]
	col := ColumnSpec{Name: info.Name, Extract: info.Extract}
	got := col.ExtractValue(prog)
	if got != "<none>" {
		t.Errorf("ATTACH for program with no links = %q, want %q", got, "<none>")
	}
}

func TestExtractLinkIDs_MultipleLinks(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Status: bpfman.ProgramStatus{
			Links: []bpfman.Link{
				{Record: bpfman.LinkRecord{ID: 100}},
				{Record: bpfman.LinkRecord{ID: 101}},
				{Record: bpfman.LinkRecord{ID: 102}},
			},
		},
	}

	index := programColumnIndex()
	info := index["LINK_IDS"]
	col := ColumnSpec{Name: info.Name, Extract: info.Extract}
	got := col.ExtractValue(prog)
	if got != "100,101,102" {
		t.Errorf("LINK_IDS = %q, want %q", got, "100,101,102")
	}
}

func TestExtractAttach_TracepointDetails(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Status: bpfman.ProgramStatus{
			Links: []bpfman.Link{
				{
					Record: bpfman.LinkRecord{
						ID:   100,
						Kind: bpfman.LinkKindTracepoint,
						Details: bpfman.TracepointDetails{
							Group: "syscalls",
							Name:  "sys_enter_open",
						},
					},
				},
			},
		},
	}

	index := programColumnIndex()
	info := index["ATTACH"]
	col := ColumnSpec{Name: info.Name, Extract: info.Extract}
	got := col.ExtractValue(prog)
	if got != "syscalls/sys_enter_open" {
		t.Errorf("ATTACH = %q, want %q", got, "syscalls/sys_enter_open")
	}
}
