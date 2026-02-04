package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// SchemaDoc holds metadata for the root of a schema.
type SchemaDoc struct {
	Kind        string
	Version     string
	Description string
	Fields      []FieldInfo
}

// FieldInfo represents a field in the schema documentation tree.
type FieldInfo struct {
	Name        string
	Type        string
	Description string
	Children    []FieldInfo
}

// ProgramSchemaDoc returns the curated schema documentation for programs.
// This is CLI documentation that evolves with user-facing behaviour.
func ProgramSchemaDoc() SchemaDoc {
	return SchemaDoc{
		Kind:        "Program",
		Version:     "",
		Description: "A BPF program object returned by 'bpfman list programs -o json'.",
		Fields: []FieldInfo{
			{Name: "spec", Type: "<ProgramSpec>", Children: []FieldInfo{
				{Name: "kernel_id", Type: "<number>", Description: "Kernel-assigned program ID"},
				{Name: "load", Type: "<ProgramLoadSpec>", Children: []FieldInfo{
					{Name: "program_type", Type: "<string>", Description: "Program type (xdp, tc, etc.)"},
					{Name: "object_path", Type: "<string>", Description: "Path to BPF object file"},
					{Name: "attach_func", Type: "<string>", Description: "Attach function (fentry/fexit)"},
					{Name: "global_data", Type: "<map>", Description: "Global data overrides"},
					{Name: "gpl_compatible", Type: "<bool>", Description: "GPL-compatible licence"},
				}},
				{Name: "handles", Type: "<ProgramHandles>", Children: []FieldInfo{
					{Name: "pin_path", Type: "<string>", Description: "Pinned path in bpffs"},
					{Name: "map_pin_path", Type: "<string>", Description: "Map pin directory"},
					{Name: "map_owner_id", Type: "<number>", Description: "Map owner program ID"},
				}},
				{Name: "meta", Type: "<ProgramMeta>", Children: []FieldInfo{
					{Name: "name", Type: "<string>", Description: "User-defined name"},
					{Name: "owner", Type: "<string>", Description: "Owner identifier"},
					{Name: "description", Type: "<string>", Description: "Description"},
					{Name: "metadata", Type: "<map>", Description: "Arbitrary key-value metadata"},
				}},
				{Name: "created_at", Type: "<time>", Description: "Creation timestamp"},
				{Name: "updated_at", Type: "<time>", Description: "Last update timestamp"},
			}},
			{Name: "status", Type: "<ProgramStatus>", Children: []FieldInfo{
				{Name: "kernel", Type: "<kernel.Program>", Description: "Kernel-reported program info"},
				{Name: "pin_present", Type: "<bool>", Description: "Pin file exists"},
				{Name: "maps_present", Type: "<bool>", Description: "Maps directory exists"},
				{Name: "links", Type: "<[]Link>", Description: "Attached links"},
				{Name: "maps", Type: "<[]kernel.Map>", Description: "Associated maps"},
			}},
		},
	}
}

// ExplainProgramsCmd explains program fields and columns.
type ExplainProgramsCmd struct {
	Columns bool `help:"Show available columns for custom-columns output."`
}

// Run executes the explain programs command.
func (c *ExplainProgramsCmd) Run(cli *CLI) error {
	var output string
	if c.Columns {
		output = FormatProgramColumnExplanation()
	} else {
		output = FormatProgramSchema()
	}
	return cli.PrintOut(output)
}

// FormatProgramSchema renders the field hierarchy using tabwriter.
func FormatProgramSchema() string {
	schema := ProgramSchemaDoc()
	var b strings.Builder

	fmt.Fprintf(&b, "KIND:       %s\n", schema.Kind)
	b.WriteString("\n")
	fmt.Fprintf(&b, "DESCRIPTION:\n    %s\n", schema.Description)
	b.WriteString("\n")
	b.WriteString("FIELDS:\n")

	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	formatFieldTree(w, schema.Fields, 1)
	w.Flush()

	b.WriteString("\nSee also:\n")
	b.WriteString("  bpfman explain programs --columns\n")
	b.WriteString("  bpfman list programs -o custom-columns=...\n")

	return b.String()
}

// formatFieldTree recursively formats fields with indentation.
func formatFieldTree(w *tabwriter.Writer, fields []FieldInfo, depth int) {
	indent := strings.Repeat("    ", depth)
	for _, f := range fields {
		if f.Description != "" {
			fmt.Fprintf(w, "%s%s\t%s\t%s\n", indent, f.Name, f.Type, f.Description)
		} else {
			fmt.Fprintf(w, "%s%s\t%s\n", indent, f.Name, f.Type)
		}
		if len(f.Children) > 0 {
			formatFieldTree(w, f.Children, depth+1)
		}
	}
}

// FormatProgramColumnExplanation renders the columns table using tabwriter.
func FormatProgramColumnExplanation() string {
	var b strings.Builder

	b.WriteString("Available columns for 'bpfman list programs -o custom-columns=...':\n\n")
	b.WriteString("These are the stable, documented columns. You can also reference any\n")
	b.WriteString("JSON field via JSONPath (see 'bpfman explain programs' for the schema).\n\n")

	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COLUMN\tJSONPATH\tDESCRIPTION")

	for _, col := range ProgramColumnRegistry() {
		jsonpath := col.JSONPath
		if col.Computed {
			jsonpath = "(special)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", col.Name, jsonpath, col.Description)
	}
	w.Flush()

	b.WriteString("\nExample:\n")
	b.WriteString("  bpfman list programs -o custom-columns=NAME:.spec.meta.name,TAG:.status.kernel.tag\n")

	return b.String()
}
