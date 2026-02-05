package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// LinkSchemaDoc returns the curated schema documentation for links.
// This is CLI documentation that evolves with user-facing behaviour.
func LinkSchemaDoc() SchemaDoc {
	return SchemaDoc{
		Kind:        "Link",
		Version:     "",
		Description: "A BPF link object returned by 'bpfman list links -o json'.",
		Fields: []FieldInfo{
			{Name: "id", Type: "<number>", Description: "Link ID"},
			{Name: "program_id", Type: "<number>", Description: "Associated program ID"},
			{Name: "kind", Type: "<string>", Description: "Link type (xdp, tc, kprobe, etc.)"},
			{Name: "pin_path", Type: "<string>", Description: "Pinned path in bpffs"},
			{Name: "details", Type: "<LinkDetails>", Description: "Type-specific attachment details", Children: []FieldInfo{
				{Name: "(varies by kind)", Type: "", Description: "See below for type-specific fields"},
			}},
			{Name: "created_at", Type: "<time>", Description: "Creation timestamp"},
		},
	}
}

// ExplainLinksCmd explains link fields and columns.
type ExplainLinksCmd struct {
	Columns bool `help:"Show available columns for custom-columns output."`
}

// Run executes the explain links command.
func (c *ExplainLinksCmd) Run(cli *CLI) error {
	var output string
	if c.Columns {
		output = FormatLinkColumnExplanation()
	} else {
		output = FormatLinkSchema()
	}
	return cli.PrintOut(output)
}

// FormatLinkSchema renders the field hierarchy using tabwriter.
func FormatLinkSchema() string {
	schema := LinkSchemaDoc()
	var b strings.Builder

	fmt.Fprintf(&b, "KIND:       %s\n", schema.Kind)
	b.WriteString("\n")
	fmt.Fprintf(&b, "DESCRIPTION:\n    %s\n", schema.Description)
	b.WriteString("\n")
	b.WriteString("FIELDS:\n")

	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	formatFieldTree(w, schema.Fields, 1)
	w.Flush()

	b.WriteString("\nLINK DETAILS BY KIND:\n")
	b.WriteString("    xdp:         interface, ifindex, priority, position, proceed_on, netns\n")
	b.WriteString("    tc:          interface, ifindex, direction, priority, position, proceed_on, netns\n")
	b.WriteString("    tcx:         interface, ifindex, direction, priority, netns\n")
	b.WriteString("    tracepoint:  group, name\n")
	b.WriteString("    kprobe:      fn_name, offset, retprobe\n")
	b.WriteString("    uprobe:      target, fn_name, offset, pid, retprobe, container_pid\n")
	b.WriteString("    fentry:      fn_name\n")
	b.WriteString("    fexit:       fn_name\n")

	b.WriteString("\nSee also:\n")
	b.WriteString("  bpfman explain links --columns\n")
	b.WriteString("  bpfman list links -o custom-columns=...\n")

	return b.String()
}

// FormatLinkColumnExplanation renders the columns table using tabwriter.
func FormatLinkColumnExplanation() string {
	var b strings.Builder

	b.WriteString("Available columns for 'bpfman list links -o custom-columns=...':\n\n")
	b.WriteString("These are the stable, documented columns. You can also reference any\n")
	b.WriteString("JSON field via JSONPath (see 'bpfman explain links' for the schema).\n\n")

	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COLUMN\tJSONPATH\tDESCRIPTION")

	for _, col := range LinkColumnRegistry() {
		jsonpath := col.JSONPath
		if col.Computed {
			jsonpath = "(special)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", col.Name, jsonpath, col.Description)
	}
	w.Flush()

	b.WriteString("\nExample:\n")
	b.WriteString("  bpfman list links -o custom-columns=ID:.id,KIND:.kind,PROG:.program_id\n")

	return b.String()
}
