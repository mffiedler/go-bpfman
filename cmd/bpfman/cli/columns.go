package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"k8s.io/client-go/util/jsonpath"

	"github.com/frobware/go-bpfman"
)

// ColumnSpec defines a single column for custom-columns output.
type ColumnSpec struct {
	Name     string // Column header
	JSONPath string // JSONPath expression (e.g., ".spec.kernel_id")
}

// ColumnSet holds multiple column specifications.
type ColumnSet struct {
	Columns []ColumnSpec
}

// ParseCustomColumns parses a custom-columns spec string.
// Format: NAME:.jsonpath,NAME2:.jsonpath2
// Example: ID:.spec.kernel_id,NAME:.spec.meta.name
func ParseCustomColumns(spec string) (ColumnSet, error) {
	if spec == "" {
		return ColumnSet{}, fmt.Errorf("custom-columns spec cannot be empty")
	}

	parts := strings.Split(spec, ",")
	columns := make([]ColumnSpec, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		idx := strings.Index(part, ":")
		if idx <= 0 {
			return ColumnSet{}, fmt.Errorf("invalid column spec %q: expected NAME:.jsonpath format", part)
		}

		name := strings.TrimSpace(part[:idx])
		path := strings.TrimSpace(part[idx+1:])

		if name == "" {
			return ColumnSet{}, fmt.Errorf("invalid column spec %q: column name cannot be empty", part)
		}
		if path == "" {
			return ColumnSet{}, fmt.Errorf("invalid column spec %q: jsonpath cannot be empty", part)
		}

		columns = append(columns, ColumnSpec{
			Name:     name,
			JSONPath: path,
		})
	}

	if len(columns) == 0 {
		return ColumnSet{}, fmt.Errorf("custom-columns spec must contain at least one column")
	}

	return ColumnSet{Columns: columns}, nil
}

// ParseCustomColumnsFile parses a custom-columns file.
// Format: Two lines, whitespace-separated
// Line 1: Column headers (e.g., "ID NAME MAPS")
// Line 2: JSONPath expressions (e.g., ".spec.kernel_id .spec.meta.name .status.kernel.map_ids")
func ParseCustomColumnsFile(path string) (ColumnSet, error) {
	file, err := os.Open(path)
	if err != nil {
		return ColumnSet{}, fmt.Errorf("cannot open custom-columns file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// Read header line
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return ColumnSet{}, fmt.Errorf("error reading custom-columns file: %w", err)
		}
		return ColumnSet{}, fmt.Errorf("custom-columns file is empty")
	}
	headerLine := scanner.Text()

	// Read jsonpath line
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return ColumnSet{}, fmt.Errorf("error reading custom-columns file: %w", err)
		}
		return ColumnSet{}, fmt.Errorf("custom-columns file must have two lines (headers and jsonpaths)")
	}
	pathLine := scanner.Text()

	if err := scanner.Err(); err != nil {
		return ColumnSet{}, fmt.Errorf("error reading custom-columns file: %w", err)
	}

	headers := strings.Fields(headerLine)
	paths := strings.Fields(pathLine)

	if len(headers) == 0 {
		return ColumnSet{}, fmt.Errorf("custom-columns file has no column headers")
	}
	if len(headers) != len(paths) {
		return ColumnSet{}, fmt.Errorf("custom-columns file: header count (%d) does not match jsonpath count (%d)", len(headers), len(paths))
	}

	columns := make([]ColumnSpec, len(headers))
	for i := range headers {
		columns[i] = ColumnSpec{
			Name:     headers[i],
			JSONPath: paths[i],
		}
	}

	return ColumnSet{Columns: columns}, nil
}

// Validate checks that all JSONPath expressions in the column set are valid.
func (cs ColumnSet) Validate() error {
	for _, col := range cs.Columns {
		jp := jsonpath.New(col.Name)
		// Wrap in {}{} for k8s jsonpath syntax
		expr := "{" + col.JSONPath + "}"
		if err := jp.Parse(expr); err != nil {
			return fmt.Errorf("invalid jsonpath %q for column %q: %w", col.JSONPath, col.Name, err)
		}
	}
	return nil
}

// ExtractValue extracts a value from a program using the JSONPath expression.
// Returns "<none>" if the value is missing or empty.
func (cs ColumnSpec) ExtractValue(prog bpfman.Program) string {
	// Handle special columns that need custom logic
	switch strings.ToUpper(cs.Name) {
	case "LINK_IDS":
		return extractLinkIDs(prog)
	case "ATTACH":
		return extractAttach(prog)
	}

	// Standard JSONPath extraction
	return extractJSONPath(prog, cs.JSONPath)
}

// extractJSONPath extracts a value using JSONPath from a program.
func extractJSONPath(prog bpfman.Program, expr string) string {
	jp := jsonpath.New("extract")
	// Wrap in {} for k8s jsonpath syntax
	if err := jp.Parse("{" + expr + "}"); err != nil {
		return "<error>"
	}

	// Marshal program to JSON then back to generic interface
	jsonBytes, err := json.Marshal(prog)
	if err != nil {
		return "<error>"
	}

	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return "<error>"
	}

	var buf bytes.Buffer
	if err := jp.Execute(&buf, generic); err != nil {
		return "<none>"
	}

	result := strings.TrimSpace(buf.String())
	if result == "" || result == "<no value>" || result == "null" || result == "[]" {
		return "<none>"
	}

	return result
}

// extractLinkIDs extracts link IDs from a program.
// This is a special column that requires custom logic.
func extractLinkIDs(prog bpfman.Program) string {
	if len(prog.Status.Links) == 0 {
		return "<none>"
	}

	ids := make([]string, len(prog.Status.Links))
	for i, link := range prog.Status.Links {
		ids[i] = strconv.FormatUint(uint64(link.Spec.ID), 10)
	}
	return strings.Join(ids, ",")
}

// extractAttach extracts attach information from a program's links.
// This is a special column that requires custom logic.
func extractAttach(prog bpfman.Program) string {
	if len(prog.Status.Links) == 0 {
		return "<none>"
	}

	attachments := make([]string, 0, len(prog.Status.Links))
	for _, link := range prog.Status.Links {
		attach := formatAttachDetails(link.Spec.Details)
		if attach != "" {
			attachments = append(attachments, attach)
		}
	}

	if len(attachments) == 0 {
		return "<none>"
	}
	return strings.Join(attachments, "; ")
}

// FormatTable renders a table of programs using the column set.
func (cs ColumnSet) FormatTable(programs []bpfman.Program) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	// Write header
	headers := make([]string, len(cs.Columns))
	for i, col := range cs.Columns {
		headers[i] = col.Name
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))

	// Write rows
	for _, prog := range programs {
		values := make([]string, len(cs.Columns))
		for i, col := range cs.Columns {
			values[i] = col.ExtractValue(prog)
		}
		fmt.Fprintln(w, strings.Join(values, "\t"))
	}

	w.Flush()
	return b.String()
}

// DefaultColumns returns the standard table columns.
// Matches the current default: KERNEL ID, TYPE, NAME, SOURCE
func DefaultColumns() ColumnSet {
	return ColumnSet{
		Columns: []ColumnSpec{
			{Name: "KERNEL ID", JSONPath: ".spec.kernel_id"},
			{Name: "TYPE", JSONPath: ".spec.load.program_type"},
			{Name: "NAME", JSONPath: ".spec.meta.name"},
			{Name: "SOURCE", JSONPath: ".spec.load.object_path"},
		},
	}
}

// WideColumns returns the wide table columns.
// Includes additional detail: MAP_IDS, LINK_IDS, ATTACH, TAG
func WideColumns() ColumnSet {
	return ColumnSet{
		Columns: []ColumnSpec{
			{Name: "KERNEL ID", JSONPath: ".spec.kernel_id"},
			{Name: "TYPE", JSONPath: ".spec.load.program_type"},
			{Name: "NAME", JSONPath: ".spec.meta.name"},
			{Name: "MAP_IDS", JSONPath: ".status.kernel.map_ids"},
			{Name: "LINK_IDS", JSONPath: ""}, // Special handling
			{Name: "ATTACH", JSONPath: ""},   // Special handling
			{Name: "TAG", JSONPath: ".status.kernel.tag"},
			{Name: "SOURCE", JSONPath: ".spec.load.object_path"},
		},
	}
}
