package main

import (
	"strings"
	"testing"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

func TestColumnSpec_ExtractValue(t *testing.T) {
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

func TestColumnSet_FormatTable(t *testing.T) {
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

	output := columns.FormatTable(programs)

	// Verify header exists
	if !strings.Contains(output, "PROGRAM_ID") || !strings.Contains(output, "NAME") {
		t.Errorf("FormatTable() output missing headers: %s", output)
	}

	// Verify data rows
	if !strings.Contains(output, "42") || !strings.Contains(output, "prog1") {
		t.Errorf("FormatTable() output missing first row data: %s", output)
	}
	if !strings.Contains(output, "43") || !strings.Contains(output, "prog2") {
		t.Errorf("FormatTable() output missing second row data: %s", output)
	}
}

func TestDefaultColumns(t *testing.T) {
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

func TestWideColumns(t *testing.T) {
	cols := WideColumns()
	if len(cols.Columns) != 8 {
		t.Errorf("WideColumns() has %d columns, want 8", len(cols.Columns))
	}

	expected := []string{"PROGRAM_ID", "TYPE", "NAME", "MAP_IDS", "LINK_IDS", "ATTACH", "TAG", "SOURCE"}
	for i, col := range cols.Columns {
		if col.Name != expected[i] {
			t.Errorf("WideColumns()[%d].Name = %q, want %q", i, col.Name, expected[i])
		}
	}
}

func TestExtractLinkIDs_NoLinks(t *testing.T) {
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

func TestIntegration_FormatProgramsCompositeWide(t *testing.T) {
	result := bpfman.ProgramListResult{
		ObservedAt: time.Now(),
		Programs: []bpfman.Program{
			{
				Record: bpfman.ProgramRecord{
					ProgramID: 42,
					Load:      bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeXDP, "/path/to/prog.o"),
					Meta: bpfman.ProgramMeta{
						Name: "xdp_pass",
					},
				},
				Status: bpfman.ProgramStatus{
					Kernel: &kernel.Program{
						ID:     42,
						Tag:    "abc123",
						MapIDs: []kernel.MapID{1, 2},
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
			},
		},
	}

	flags := &OutputFlags{Output: OutputValue{Value: "wide"}}
	output, err := FormatProgramsComposite(result, flags)
	if err != nil {
		t.Fatalf("FormatProgramsComposite() error = %v", err)
	}

	// Check all wide columns appear in header
	expectedHeaders := []string{"PROGRAM_ID", "TYPE", "NAME", "MAP_IDS", "LINK_IDS", "ATTACH", "TAG", "SOURCE"}
	for _, h := range expectedHeaders {
		if !strings.Contains(output, h) {
			t.Errorf("Wide output missing header %q: %s", h, output)
		}
	}

	// Check data values
	if !strings.Contains(output, "42") {
		t.Errorf("Wide output missing program_id: %s", output)
	}
	if !strings.Contains(output, "xdp_pass") {
		t.Errorf("Wide output missing name: %s", output)
	}
	if !strings.Contains(output, "abc123") {
		t.Errorf("Wide output missing tag: %s", output)
	}
}
