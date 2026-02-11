package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

func TestParseCustomColumns(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    int // number of columns
		wantErr bool
	}{
		{
			name:    "empty spec",
			spec:    "",
			wantErr: true,
		},
		{
			name:    "single column",
			spec:    "ID:.record.program_id",
			want:    1,
			wantErr: false,
		},
		{
			name:    "multiple columns",
			spec:    "ID:.record.program_id,NAME:.record.meta.name,TYPE:.record.load.program_type",
			want:    3,
			wantErr: false,
		},
		{
			name:    "with spaces",
			spec:    "ID : .record.program_id , NAME : .record.meta.name",
			want:    2,
			wantErr: false,
		},
		{
			name:    "missing colon",
			spec:    "ID.record.program_id",
			wantErr: true,
		},
		{
			name:    "empty column name",
			spec:    ":.record.program_id",
			wantErr: true,
		},
		{
			name:    "empty jsonpath",
			spec:    "ID:",
			wantErr: true,
		},
		{
			name:    "only commas",
			spec:    ",,",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCustomColumns(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCustomColumns() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got.Columns) != tt.want {
				t.Errorf("ParseCustomColumns() got %d columns, want %d", len(got.Columns), tt.want)
			}
		})
	}
}

func TestParseCustomColumnsFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int // number of columns
		wantErr bool
	}{
		{
			name:    "valid two-line file",
			content: "ID NAME TYPE\n.record.program_id .record.meta.name .record.load.program_type",
			want:    3,
			wantErr: false,
		},
		{
			name:    "empty file",
			content: "",
			wantErr: true,
		},
		{
			name:    "single line only",
			content: "ID NAME TYPE",
			wantErr: true,
		},
		{
			name:    "mismatched counts",
			content: "ID NAME\n.record.program_id",
			wantErr: true,
		},
		{
			name:    "empty header line",
			content: "\n.record.program_id",
			wantErr: true,
		},
		{
			name:    "with tabs",
			content: "ID\tNAME\tTYPE\n.record.program_id\t.record.meta.name\t.record.load.program_type",
			want:    3,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tmpFile := filepath.Join(tmpDir, "columns.txt")
			if err := os.WriteFile(tmpFile, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to create temp file: %v", err)
			}

			got, err := ParseCustomColumnsFile(tmpFile)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCustomColumnsFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got.Columns) != tt.want {
				t.Errorf("ParseCustomColumnsFile() got %d columns, want %d", len(got.Columns), tt.want)
			}
		})
	}
}

func TestParseCustomColumnsFile_FileNotFound(t *testing.T) {
	_, err := ParseCustomColumnsFile("/nonexistent/path/columns.txt")
	if err == nil {
		t.Error("ParseCustomColumnsFile() expected error for nonexistent file")
	}
}

func TestColumnSet_Validate(t *testing.T) {
	tests := []struct {
		name    string
		columns ColumnSet
		wantErr bool
	}{
		{
			name: "valid jsonpaths",
			columns: ColumnSet{
				Columns: []ColumnSpec{
					{Name: "ID", JSONPath: ".record.program_id"},
					{Name: "NAME", JSONPath: ".record.meta.name"},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid jsonpath",
			columns: ColumnSet{
				Columns: []ColumnSpec{
					{Name: "BAD", JSONPath: ".record.[invalid"},
				},
			},
			wantErr: true,
		},
		{
			name: "empty columns",
			columns: ColumnSet{
				Columns: []ColumnSpec{},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.columns.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("ColumnSet.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

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

	tests := []struct {
		name string
		col  ColumnSpec
		want string
	}{
		{
			name: "program_id",
			col:  ColumnSpec{Name: "ID", JSONPath: ".record.program_id"},
			want: "42",
		},
		{
			name: "program type",
			col:  ColumnSpec{Name: "TYPE", JSONPath: ".record.load.program_type"},
			want: "xdp",
		},
		{
			name: "name",
			col:  ColumnSpec{Name: "NAME", JSONPath: ".record.meta.name"},
			want: "test_prog",
		},
		{
			name: "tag",
			col:  ColumnSpec{Name: "TAG", JSONPath: ".status.kernel.tag"},
			want: "abc123",
		},
		{
			name: "map_ids",
			col:  ColumnSpec{Name: "MAP_IDS", JSONPath: ".status.kernel.map_ids"},
			want: "[1,2,3]",
		},
		{
			name: "missing field",
			col:  ColumnSpec{Name: "MISSING", JSONPath: ".record.nonexistent"},
			want: "<none>",
		},
		{
			name: "special LINK_IDS",
			col:  ColumnSpec{Name: "LINK_IDS", JSONPath: ""},
			want: "100",
		},
		{
			name: "special ATTACH",
			col:  ColumnSpec{Name: "ATTACH", JSONPath: ""},
			want: "eth0 (ifindex=0, pos=1)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.col.ExtractValue(prog)
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

	columns := ColumnSet{
		Columns: []ColumnSpec{
			{Name: "ID", JSONPath: ".record.program_id"},
			{Name: "NAME", JSONPath: ".record.meta.name"},
		},
	}

	output := columns.FormatTable(programs)

	// Verify header exists
	if !strings.Contains(output, "ID") || !strings.Contains(output, "NAME") {
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

	col := ColumnSpec{Name: "LINK_IDS", JSONPath: ""}
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

	col := ColumnSpec{Name: "ATTACH", JSONPath: ""}
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

	col := ColumnSpec{Name: "LINK_IDS", JSONPath: ""}
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

	col := ColumnSpec{Name: "ATTACH", JSONPath: ""}
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

func TestIntegration_FormatProgramsCompositeCustomColumns(t *testing.T) {
	result := bpfman.ProgramListResult{
		ObservedAt: time.Now(),
		Programs: []bpfman.Program{
			{
				Record: bpfman.ProgramRecord{
					ProgramID: 42,
					Meta: bpfman.ProgramMeta{
						Name: "test",
					},
				},
			},
		},
	}

	flags := &OutputFlags{Output: OutputValue{Value: "custom-columns=ID:.record.program_id,NAME:.record.meta.name"}}
	output, err := FormatProgramsComposite(result, flags)
	if err != nil {
		t.Fatalf("FormatProgramsComposite() error = %v", err)
	}

	if !strings.Contains(output, "ID") || !strings.Contains(output, "NAME") {
		t.Errorf("Custom columns output missing headers: %s", output)
	}
	if !strings.Contains(output, "42") || !strings.Contains(output, "test") {
		t.Errorf("Custom columns output missing data: %s", output)
	}
}

func TestIntegration_FormatProgramsCompositeCustomColumnsFile(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "columns.txt")
	content := "ID NAME\n.record.program_id .record.meta.name"
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	result := bpfman.ProgramListResult{
		ObservedAt: time.Now(),
		Programs: []bpfman.Program{
			{
				Record: bpfman.ProgramRecord{
					ProgramID: 42,
					Meta: bpfman.ProgramMeta{
						Name: "test",
					},
				},
			},
		},
	}

	flags := &OutputFlags{Output: OutputValue{Value: "custom-columns-file=" + tmpFile}}
	output, err := FormatProgramsComposite(result, flags)
	if err != nil {
		t.Fatalf("FormatProgramsComposite() error = %v", err)
	}

	if !strings.Contains(output, "ID") || !strings.Contains(output, "NAME") {
		t.Errorf("Custom columns file output missing headers: %s", output)
	}
	if !strings.Contains(output, "42") || !strings.Contains(output, "test") {
		t.Errorf("Custom columns file output missing data: %s", output)
	}
}

func TestIntegration_InvalidCustomColumnsSpec(t *testing.T) {
	result := bpfman.ProgramListResult{
		Programs: []bpfman.Program{},
	}

	flags := &OutputFlags{Output: OutputValue{Value: "custom-columns=INVALID"}}
	_, err := FormatProgramsComposite(result, flags)
	if err == nil {
		t.Error("FormatProgramsComposite() expected error for invalid spec")
	}
}

func TestIntegration_InvalidCustomColumnsFile(t *testing.T) {
	result := bpfman.ProgramListResult{
		Programs: []bpfman.Program{},
	}

	flags := &OutputFlags{Output: OutputValue{Value: "custom-columns-file=/nonexistent/file.txt"}}
	_, err := FormatProgramsComposite(result, flags)
	if err == nil {
		t.Error("FormatProgramsComposite() expected error for nonexistent file")
	}
}
