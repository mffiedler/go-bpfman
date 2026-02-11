package main

import (
	"strings"
	"testing"
)

func TestProgramColumnRegistry_Completeness(t *testing.T) {
	registry := ProgramColumnRegistry()

	// Check that expected columns exist
	expectedColumns := []string{
		"PROGRAM_ID", "TYPE", "NAME", "SOURCE", "MAP_IDS",
		"TAG", "BTF_ID", "PIN_PATH", "LOADED_AT", "JIT_SIZE",
		"MEMLOCK", "LINK_IDS", "ATTACH",
	}

	columnNames := make(map[string]bool)
	for _, col := range registry {
		columnNames[col.Name] = true
	}

	for _, expected := range expectedColumns {
		if !columnNames[expected] {
			t.Errorf("ProgramColumnRegistry() missing expected column %q", expected)
		}
	}
}

func TestProgramColumnRegistry_NoDrift_Names(t *testing.T) {
	// Verify DefaultColumns and WideColumns only reference columns in registry
	registry := ProgramColumnRegistry()
	registryNames := make(map[string]bool)
	for _, col := range registry {
		registryNames[col.Name] = true
	}

	for _, name := range defaultColumnNames {
		if !registryNames[name] {
			t.Errorf("defaultColumnNames contains %q which is not in registry", name)
		}
	}

	for _, name := range wideColumnNames {
		if !registryNames[name] {
			t.Errorf("wideColumnNames contains %q which is not in registry", name)
		}
	}
}

func TestProgramColumnRegistry_NoDrift_Paths(t *testing.T) {
	registry := ProgramColumnRegistry()
	jsonPaths := make(map[string]bool)

	for _, col := range registry {
		// Computed == false implies JSONPath != "" and starts with .
		if !col.Computed {
			if col.JSONPath == "" {
				t.Errorf("Column %q: Computed=false but JSONPath is empty", col.Name)
			}
			if !strings.HasPrefix(col.JSONPath, ".") {
				t.Errorf("Column %q: JSONPath %q does not start with '.'", col.Name, col.JSONPath)
			}
			// Check for duplicate JSONPaths
			if jsonPaths[col.JSONPath] {
				t.Errorf("Column %q: duplicate JSONPath %q", col.Name, col.JSONPath)
			}
			jsonPaths[col.JSONPath] = true
		}

		// Computed == true implies JSONPath == ""
		if col.Computed {
			if col.JSONPath != "" {
				t.Errorf("Column %q: Computed=true but JSONPath is %q (should be empty)", col.Name, col.JSONPath)
			}
		}
	}
}

func TestFormatProgramSchema_Contains(t *testing.T) {
	output := FormatProgramSchema()

	mustContain := []string{
		"KIND:",
		"FIELDS:",
		"See also",
	}

	for _, s := range mustContain {
		if !strings.Contains(output, s) {
			t.Errorf("FormatProgramSchema() missing %q", s)
		}
	}
}

func TestFormatProgramColumnExplanation_Contains(t *testing.T) {
	output := FormatProgramColumnExplanation()

	mustContain := []string{
		"COLUMN",
		"JSONPATH",
		"(special)",
	}

	for _, s := range mustContain {
		if !strings.Contains(output, s) {
			t.Errorf("FormatProgramColumnExplanation() missing %q", s)
		}
	}
}

func TestMustSelectProgramColumns_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("MustSelectProgramColumns([]string{\"BOGUS\"}) did not panic")
		}
	}()
	MustSelectProgramColumns([]string{"BOGUS"})
}

func TestProgramSchemaDoc_Structure(t *testing.T) {
	schema := ProgramSchemaDoc()

	if schema.Kind != "Program" {
		t.Errorf("ProgramSchemaDoc().Kind = %q, want %q", schema.Kind, "Program")
	}
	// Version is intentionally empty - we don't version the CLI schema
	if schema.Description == "" {
		t.Error("ProgramSchemaDoc().Description is empty")
	}
	if len(schema.Fields) == 0 {
		t.Error("ProgramSchemaDoc().Fields is empty")
	}

	// Check top-level fields exist
	fieldNames := make(map[string]bool)
	for _, f := range schema.Fields {
		fieldNames[f.Name] = true
	}
	if !fieldNames["spec"] {
		t.Error("ProgramSchemaDoc() missing 'spec' field")
	}
	if !fieldNames["status"] {
		t.Error("ProgramSchemaDoc() missing 'status' field")
	}
}

// Link explain tests

func TestLinkColumnRegistry_Completeness(t *testing.T) {
	registry := LinkColumnRegistry()

	// Check that expected columns exist
	expectedColumns := []string{
		"LINK_ID", "PROGRAM_ID", "KIND", "PIN_PATH", "CREATED_AT", "ATTACH",
	}

	columnNames := make(map[string]bool)
	for _, col := range registry {
		columnNames[col.Name] = true
	}

	for _, expected := range expectedColumns {
		if !columnNames[expected] {
			t.Errorf("LinkColumnRegistry() missing expected column %q", expected)
		}
	}
}

func TestLinkColumnRegistry_NoDrift_Names(t *testing.T) {
	// Verify DefaultLinkColumns and WideLinkColumns only reference columns in registry
	registry := LinkColumnRegistry()
	registryNames := make(map[string]bool)
	for _, col := range registry {
		registryNames[col.Name] = true
	}

	for _, name := range defaultLinkColumnNames {
		if !registryNames[name] {
			t.Errorf("defaultLinkColumnNames contains %q which is not in registry", name)
		}
	}

	for _, name := range wideLinkColumnNames {
		if !registryNames[name] {
			t.Errorf("wideLinkColumnNames contains %q which is not in registry", name)
		}
	}
}

func TestLinkColumnRegistry_NoDrift_Paths(t *testing.T) {
	registry := LinkColumnRegistry()
	jsonPaths := make(map[string]bool)

	for _, col := range registry {
		// Computed == false implies JSONPath != "" and starts with .
		if !col.Computed {
			if col.JSONPath == "" {
				t.Errorf("Link column %q: Computed=false but JSONPath is empty", col.Name)
			}
			if !strings.HasPrefix(col.JSONPath, ".") {
				t.Errorf("Link column %q: JSONPath %q does not start with '.'", col.Name, col.JSONPath)
			}
			// Check for duplicate JSONPaths
			if jsonPaths[col.JSONPath] {
				t.Errorf("Link column %q: duplicate JSONPath %q", col.Name, col.JSONPath)
			}
			jsonPaths[col.JSONPath] = true
		}

		// Computed == true implies JSONPath == ""
		if col.Computed {
			if col.JSONPath != "" {
				t.Errorf("Link column %q: Computed=true but JSONPath is %q (should be empty)", col.Name, col.JSONPath)
			}
		}
	}
}

func TestFormatLinkSchema_Contains(t *testing.T) {
	output := FormatLinkSchema()

	mustContain := []string{
		"KIND:",
		"FIELDS:",
		"LINK DETAILS BY KIND:",
		"See also",
	}

	for _, s := range mustContain {
		if !strings.Contains(output, s) {
			t.Errorf("FormatLinkSchema() missing %q", s)
		}
	}
}

func TestFormatLinkColumnExplanation_Contains(t *testing.T) {
	output := FormatLinkColumnExplanation()

	mustContain := []string{
		"COLUMN",
		"JSONPATH",
		"(special)",
	}

	for _, s := range mustContain {
		if !strings.Contains(output, s) {
			t.Errorf("FormatLinkColumnExplanation() missing %q", s)
		}
	}
}

func TestMustSelectLinkColumns_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("MustSelectLinkColumns([]string{\"BOGUS\"}) did not panic")
		}
	}()
	MustSelectLinkColumns([]string{"BOGUS"})
}

func TestLinkSchemaDoc_Structure(t *testing.T) {
	schema := LinkSchemaDoc()

	if schema.Kind != "Link" {
		t.Errorf("LinkSchemaDoc().Kind = %q, want %q", schema.Kind, "Link")
	}
	// Version is intentionally empty - we don't version the CLI schema
	if schema.Description == "" {
		t.Error("LinkSchemaDoc().Description is empty")
	}
	if len(schema.Fields) == 0 {
		t.Error("LinkSchemaDoc().Fields is empty")
	}

	// Check top-level fields exist
	fieldNames := make(map[string]bool)
	for _, f := range schema.Fields {
		fieldNames[f.Name] = true
	}
	if !fieldNames["id"] {
		t.Error("LinkSchemaDoc() missing 'id' field")
	}
	if !fieldNames["kind"] {
		t.Error("LinkSchemaDoc() missing 'kind' field")
	}
	if !fieldNames["details"] {
		t.Error("LinkSchemaDoc() missing 'details' field")
	}
}

func TestDefaultLinkColumns(t *testing.T) {
	cols := DefaultLinkColumns()
	if len(cols.Columns) != 4 {
		t.Errorf("DefaultLinkColumns() has %d columns, want 4", len(cols.Columns))
	}

	expected := []string{"LINK_ID", "KIND", "PROGRAM_ID", "PIN_PATH"}
	for i, col := range cols.Columns {
		if col.Name != expected[i] {
			t.Errorf("DefaultLinkColumns()[%d].Name = %q, want %q", i, col.Name, expected[i])
		}
	}
}

func TestWideLinkColumns(t *testing.T) {
	cols := WideLinkColumns()
	if len(cols.Columns) != 6 {
		t.Errorf("WideLinkColumns() has %d columns, want 6", len(cols.Columns))
	}

	expected := []string{"LINK_ID", "KIND", "PROGRAM_ID", "ATTACH", "PIN_PATH", "CREATED_AT"}
	for i, col := range cols.Columns {
		if col.Name != expected[i] {
			t.Errorf("WideLinkColumns()[%d].Name = %q, want %q", i, col.Name, expected[i])
		}
	}
}
