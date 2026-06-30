package cliformat

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/kernel"
)

// ColumnSpec defines a single column for tabular output.
type ColumnSpec struct {
	// Name is the column header shown in tabular output.
	Name string

	// Extract produces this column's cell value for a program row.
	Extract func(bpfman.Program) string

	// ExtractLink produces this column's cell value for a link row.
	ExtractLink func(bpfman.LinkRecord) string
}

// ColumnSet holds multiple column specifications.
type ColumnSet struct {
	// Columns are the column specifications, in display order.
	Columns []ColumnSpec
}

// ColumnInfo describes a column in the registry for documentation
// and selection purposes.
type ColumnInfo struct {
	// Name is the column's identifier and header text.
	Name string

	// Description is the human-readable explanation of the column, shown when listing the available columns.
	Description string

	// Extract produces this column's cell value for a program row.
	Extract func(bpfman.Program) string

	// ExtractLink produces this column's cell value for a link row.
	ExtractLink func(bpfman.LinkRecord) string
}

// ProgramColumnRegistry returns all available columns for programs.
// This is the authoritative source - DefaultColumns derives from it.
func ProgramColumnRegistry() []ColumnInfo {
	return []ColumnInfo{
		{Name: "PROGRAM_ID", Description: "Program ID",
			Extract: func(p bpfman.Program) string {
				return strconv.FormatUint(uint64(p.Record.ProgramID), 10)
			}},
		{Name: "TYPE", Description: "Program type (xdp, tc, etc.)",
			Extract: func(p bpfman.Program) string {
				return p.Record.Load.ProgramType().String()
			}},
		{Name: "NAME", Description: "User-defined name",
			Extract: func(p bpfman.Program) string {
				return nonEmpty(p.Record.Meta.Name)
			}},
		{Name: "SOURCE", Description: "BPF object path (source)",
			Extract: func(p bpfman.Program) string {
				return nonEmpty(p.Record.Load.ObjectPath())
			}},
		{Name: "MAP_IDS", Description: "Associated map IDs",
			Extract: func(p bpfman.Program) string {
				if p.Status.Kernel == nil || len(p.Status.Kernel.MapIDs) == 0 {
					return "<none>"
				}
				return formatMapIDs(p.Status.Kernel.MapIDs)
			}},
		{Name: "TAG", Description: "Program tag (hash)",
			Extract: func(p bpfman.Program) string {
				if p.Status.Kernel == nil {
					return "<none>"
				}
				return nonEmpty(p.Status.Kernel.Tag)
			}},
		{Name: "BTF_ID", Description: "BTF type ID",
			Extract: func(p bpfman.Program) string {
				if p.Status.Kernel == nil || p.Status.Kernel.BTFId == 0 {
					return "<none>"
				}
				return strconv.FormatUint(uint64(p.Status.Kernel.BTFId), 10)
			}},
		{Name: "PIN_PATH", Description: "Pinned path in bpffs",
			Extract: func(p bpfman.Program) string {
				return nonEmpty(p.Record.Handles.PinPath.String())
			}},
		{Name: "LOADED_AT", Description: "Load timestamp",
			Extract: func(p bpfman.Program) string {
				if p.Status.Kernel == nil || p.Status.Kernel.LoadedAt.IsZero() {
					return "<none>"
				}
				return p.Status.Kernel.LoadedAt.Format(time.RFC3339)
			}},
		{Name: "JIT_SIZE", Description: "JIT-compiled size in bytes",
			Extract: func(p bpfman.Program) string {
				if p.Status.Kernel == nil {
					return "<none>"
				}
				return strconv.FormatUint(uint64(p.Status.Kernel.JitedSize), 10)
			}},
		{Name: "MEMLOCK", Description: "Locked memory in bytes",
			Extract: func(p bpfman.Program) string {
				if p.Status.Kernel == nil || p.Status.Kernel.Memlock == 0 {
					return "<none>"
				}
				return strconv.FormatUint(p.Status.Kernel.Memlock, 10)
			}},
		{Name: "RUN_COUNT", Description: "Number of times program executed",
			Extract: func(p bpfman.Program) string {
				if p.Status.Stats == nil {
					return "<none>"
				}
				return strconv.FormatUint(p.Status.Stats.RunCount, 10)
			}},
		{Name: "RUNTIME", Description: "Total execution time",
			Extract: func(p bpfman.Program) string {
				if p.Status.Stats == nil {
					return "<none>"
				}
				return p.Status.Stats.Runtime.String()
			}},
		{Name: "LINK_IDS", Description: "Comma-separated link IDs",
			Extract: func(p bpfman.Program) string {
				if len(p.Status.Links) == 0 {
					return "<none>"
				}
				ids := make([]string, len(p.Status.Links))
				for i, link := range p.Status.Links {
					ids[i] = strconv.FormatUint(uint64(link.Record.ID), 10)
				}
				return strings.Join(ids, ",")
			}},
		{Name: "ATTACH", Description: "Attach point descriptions",
			Extract: func(p bpfman.Program) string {
				if len(p.Status.Links) == 0 {
					return "<none>"
				}
				attachments := make([]string, 0, len(p.Status.Links))
				for _, link := range p.Status.Links {
					attach := formatAttachDetails(link.Record.Details)
					if attach != "" {
						attachments = append(attachments, attach)
					}
				}
				if len(attachments) == 0 {
					return "<none>"
				}
				return strings.Join(attachments, "; ")
			}},
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
// Use this for compile-time-known column sets (the default).
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
			Name:    info.Name,
			Extract: info.Extract,
		})
	}
	return ColumnSet{Columns: columns}, nil
}

// Column names for the default output.
var defaultColumnNames = []string{"PROGRAM_ID", "TYPE", "NAME", "SOURCE"}

// ExtractValue extracts a value from a program using the column's extractor.
func (cs ColumnSpec) ExtractValue(prog bpfman.Program) string {
	return cs.Extract(prog)
}

// RenderTable writes a table of programs using the column set.
func (cs ColumnSet) RenderTable(out io.Writer, programs []bpfman.Program) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

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

	return w.Flush()
}

// DefaultColumns returns the standard table columns.
// Matches the current default: PROGRAM_ID, TYPE, NAME, SOURCE
func DefaultColumns() ColumnSet {
	return MustSelectProgramColumns(defaultColumnNames)
}

// LinkColumnRegistry returns all available columns for links.
// This is the authoritative source - DefaultLinkColumns derives from it.
func LinkColumnRegistry() []ColumnInfo {
	return []ColumnInfo{
		{Name: "LINK ID", Description: "Link ID",
			ExtractLink: func(l bpfman.LinkRecord) string {
				return strconv.FormatUint(uint64(l.ID), 10)
			}},
		{Name: "KERNEL LINK ID", Description: "Captured kernel link ID",
			ExtractLink: func(l bpfman.LinkRecord) string {
				if l.KernelLinkID == nil {
					return "<none>"
				}
				return strconv.FormatUint(uint64(*l.KernelLinkID), 10)
			}},
		{Name: "PROGRAM ID", Description: "Associated program ID",
			ExtractLink: func(l bpfman.LinkRecord) string {
				return strconv.FormatUint(uint64(l.ProgramID), 10)
			}},
		{Name: "KIND", Description: "Link type (xdp, tc, kprobe, etc.)",
			ExtractLink: func(l bpfman.LinkRecord) string {
				return l.Kind.String()
			}},
		{Name: "PIN PATH", Description: "Pinned path in bpffs",
			ExtractLink: func(l bpfman.LinkRecord) string {
				if l.PinPath == nil {
					return "<none>"
				}
				return l.PinPath.String()
			}},
		{Name: "CREATED AT", Description: "Creation timestamp",
			ExtractLink: func(l bpfman.LinkRecord) string {
				if l.CreatedAt.IsZero() {
					return "<none>"
				}
				return l.CreatedAt.Format(time.RFC3339)
			}},
		{Name: "ATTACH", Description: "Attach point details",
			ExtractLink: func(l bpfman.LinkRecord) string {
				attach := formatAttachDetails(l.Details)
				if attach == "" {
					return "<none>"
				}
				return attach
			}},
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
// Use this for compile-time-known column sets (the default).
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
			Name:        info.Name,
			ExtractLink: info.ExtractLink,
		})
	}
	return ColumnSet{Columns: columns}, nil
}

// Link column names for the default output.
var defaultLinkColumnNames = []string{"LINK ID", "KERNEL LINK ID", "KIND", "PROGRAM ID", "PIN PATH"}

// DefaultLinkColumns returns the standard table columns for links.
func DefaultLinkColumns() ColumnSet {
	return MustSelectLinkColumns(defaultLinkColumnNames)
}

// ExtractLinkValue extracts a value from a link using the column's extractor.
func (cs ColumnSpec) ExtractLinkValue(link bpfman.LinkRecord) string {
	return cs.ExtractLink(link)
}

// RenderLinkTable writes a table of links using the column set.
func (cs ColumnSet) RenderLinkTable(out io.Writer, links []bpfman.LinkRecord) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

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

	return w.Flush()
}

// nonEmpty returns s if non-empty, otherwise "<none>".
func nonEmpty(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

// formatMapIDs formats a slice of map IDs to match the JSON array
// shape used by the table output (e.g., "[1,2,3]").
func formatMapIDs(ids []kernel.MapID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id), 10)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
