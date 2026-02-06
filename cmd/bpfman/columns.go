package main

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
	JSONPath string // JSONPath expression (e.g., ".record.kernel_id")
}

// ColumnSet holds multiple column specifications.
type ColumnSet struct {
	Columns []ColumnSpec
}

// ColumnInfo is for documentation/explain output.
// It extends ColumnSpec with description and computed flag.
type ColumnInfo struct {
	Name        string // Column name (e.g., "KERNEL_ID")
	JSONPath    string // JSONPath expression (empty if Computed)
	Description string // Human-readable description
	Computed    bool   // True for special columns that need custom extraction
}

// ProgramColumnRegistry returns all available columns for programs.
// This is the authoritative source - DefaultColumns/WideColumns derive from it.
func ProgramColumnRegistry() []ColumnInfo {
	return []ColumnInfo{
		{Name: "KERNEL_ID", JSONPath: ".record.kernel_id", Description: "Kernel-assigned program ID"},
		{Name: "TYPE", JSONPath: ".record.load.program_type", Description: "Program type (xdp, tc, etc.)"},
		{Name: "NAME", JSONPath: ".record.meta.name", Description: "User-defined name"},
		{Name: "SOURCE", JSONPath: ".record.load.object_path", Description: "BPF object path (source)"},
		{Name: "MAP_IDS", JSONPath: ".status.kernel.map_ids", Description: "Associated map IDs"},
		{Name: "TAG", JSONPath: ".status.kernel.tag", Description: "Program tag (hash)"},
		{Name: "BTF_ID", JSONPath: ".status.kernel.btf_id", Description: "BTF type ID"},
		{Name: "PIN_PATH", JSONPath: ".record.handles.pin_path", Description: "Pinned path in bpffs"},
		{Name: "LOADED_AT", JSONPath: ".status.kernel.loaded_at", Description: "Load timestamp"},
		{Name: "JIT_SIZE", JSONPath: ".status.kernel.jited_size", Description: "JIT-compiled size in bytes"},
		{Name: "MEMLOCK", JSONPath: ".status.kernel.memlock", Description: "Locked memory in bytes"},
		{Name: "LINK_IDS", Computed: true, Description: "Comma-separated link IDs"},
		{Name: "ATTACH", Computed: true, Description: "Attach point descriptions"},
	}
}

// programColumnIndex builds a lookup map from column name to ColumnInfo.
func programColumnIndex() map[string]ColumnInfo {
	registry := ProgramColumnRegistry()
	index := make(map[string]ColumnInfo, len(registry))
	for _, col := range registry {
		index[col.Name] = col
	}
	return index
}

// MustSelectProgramColumns selects columns by name, panicking if any are unknown.
// Use this for compile-time-known column sets (default, wide).
func MustSelectProgramColumns(names []string) ColumnSet {
	cs, err := selectProgramColumns(names)
	if err != nil {
		panic(err) // Programmer error, not user error
	}
	return cs
}

// selectProgramColumns selects columns by name from the registry.
func selectProgramColumns(names []string) (ColumnSet, error) {
	index := programColumnIndex()
	columns := make([]ColumnSpec, 0, len(names))
	for _, name := range names {
		info, ok := index[name]
		if !ok {
			return ColumnSet{}, fmt.Errorf("unknown column %q", name)
		}
		columns = append(columns, ColumnSpec{
			Name:     info.Name,
			JSONPath: info.JSONPath,
		})
	}
	return ColumnSet{Columns: columns}, nil
}

// Column name constants for default and wide output.
var (
	defaultColumnNames = []string{"KERNEL_ID", "TYPE", "NAME", "SOURCE"}
	wideColumnNames    = []string{"KERNEL_ID", "TYPE", "NAME", "MAP_IDS", "LINK_IDS", "ATTACH", "TAG", "SOURCE"}
)

// ParseCustomColumns parses a custom-columns spec string.
// Format: NAME:.jsonpath,NAME2:.jsonpath2
// Example: ID:.record.kernel_id,NAME:.record.meta.name
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
// Line 2: JSONPath expressions (e.g., ".record.kernel_id .record.meta.name .status.kernel.map_ids")
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
		ids[i] = strconv.FormatUint(uint64(link.Record.ID), 10)
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
		attach := formatAttachDetails(link.Record.Details)
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
// Matches the current default: KERNEL_ID, TYPE, NAME, SOURCE
func DefaultColumns() ColumnSet {
	return MustSelectProgramColumns(defaultColumnNames)
}

// WideColumns returns the wide table columns.
// Includes additional detail: MAP_IDS, LINK_IDS, ATTACH, TAG
func WideColumns() ColumnSet {
	return MustSelectProgramColumns(wideColumnNames)
}

// LinkColumnRegistry returns all available columns for links.
// This is the authoritative source - DefaultLinkColumns/WideLinkColumns derive from it.
func LinkColumnRegistry() []ColumnInfo {
	return []ColumnInfo{
		{Name: "LINK_ID", JSONPath: ".id", Description: "Link ID"},
		{Name: "PROGRAM_ID", JSONPath: ".program_id", Description: "Associated program ID"},
		{Name: "KIND", JSONPath: ".kind", Description: "Link type (xdp, tc, kprobe, etc.)"},
		{Name: "PIN_PATH", JSONPath: ".pin_path", Description: "Pinned path in bpffs"},
		{Name: "CREATED_AT", JSONPath: ".created_at", Description: "Creation timestamp"},
		{Name: "ATTACH", Computed: true, Description: "Attach point details"},
	}
}

// linkColumnIndex builds a lookup map from column name to ColumnInfo.
func linkColumnIndex() map[string]ColumnInfo {
	registry := LinkColumnRegistry()
	index := make(map[string]ColumnInfo, len(registry))
	for _, col := range registry {
		index[col.Name] = col
	}
	return index
}

// MustSelectLinkColumns selects columns by name, panicking if any are unknown.
// Use this for compile-time-known column sets (default, wide).
func MustSelectLinkColumns(names []string) ColumnSet {
	cs, err := selectLinkColumns(names)
	if err != nil {
		panic(err) // Programmer error, not user error
	}
	return cs
}

// selectLinkColumns selects columns by name from the registry.
func selectLinkColumns(names []string) (ColumnSet, error) {
	index := linkColumnIndex()
	columns := make([]ColumnSpec, 0, len(names))
	for _, name := range names {
		info, ok := index[name]
		if !ok {
			return ColumnSet{}, fmt.Errorf("unknown link column %q", name)
		}
		columns = append(columns, ColumnSpec{
			Name:     info.Name,
			JSONPath: info.JSONPath,
		})
	}
	return ColumnSet{Columns: columns}, nil
}

// Link column name constants for default and wide output.
var (
	defaultLinkColumnNames = []string{"LINK_ID", "KIND", "PROGRAM_ID", "PIN_PATH"}
	wideLinkColumnNames    = []string{"LINK_ID", "KIND", "PROGRAM_ID", "ATTACH", "PIN_PATH", "CREATED_AT"}
)

// DefaultLinkColumns returns the standard table columns for links.
func DefaultLinkColumns() ColumnSet {
	return MustSelectLinkColumns(defaultLinkColumnNames)
}

// WideLinkColumns returns the wide table columns for links.
func WideLinkColumns() ColumnSet {
	return MustSelectLinkColumns(wideLinkColumnNames)
}

// ExtractLinkValue extracts a value from a link using the JSONPath expression.
// Returns "<none>" if the value is missing or empty.
func (cs ColumnSpec) ExtractLinkValue(link bpfman.LinkRecord) string {
	// Handle special columns that need custom logic
	switch strings.ToUpper(cs.Name) {
	case "ATTACH":
		return extractLinkAttach(link)
	}

	// Standard JSONPath extraction
	return extractLinkJSONPath(link, cs.JSONPath)
}

// extractLinkJSONPath extracts a value using JSONPath from a link.
func extractLinkJSONPath(link bpfman.LinkRecord, expr string) string {
	jp := jsonpath.New("extract")
	// Wrap in {} for k8s jsonpath syntax
	if err := jp.Parse("{" + expr + "}"); err != nil {
		return "<error>"
	}

	// Marshal link to JSON then back to generic interface
	jsonBytes, err := json.Marshal(link)
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
	if result == "" || result == "<no value>" || result == "null" {
		return "<none>"
	}

	return result
}

// extractLinkAttach extracts attach details from a link.
// This is a special column that requires custom logic.
func extractLinkAttach(link bpfman.LinkRecord) string {
	attach := formatAttachDetails(link.Details)
	if attach == "" {
		return "<none>"
	}
	return attach
}

// FormatLinkTable renders a table of links using the column set.
func (cs ColumnSet) FormatLinkTable(links []bpfman.LinkRecord) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	// Write header
	headers := make([]string, len(cs.Columns))
	for i, col := range cs.Columns {
		headers[i] = col.Name
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))

	// Write rows
	for _, link := range links {
		values := make([]string, len(cs.Columns))
		for i, col := range cs.Columns {
			values[i] = col.ExtractLinkValue(link)
		}
		fmt.Fprintln(w, strings.Join(values, "\t"))
	}

	w.Flush()
	return b.String()
}
