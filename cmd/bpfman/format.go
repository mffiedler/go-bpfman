package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/util/jsonpath"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/outcome"
)

// executeJSONPath parses and executes a JSONPath expression against the given data.
// The data is marshalled to JSON and back to ensure consistent field access.
func executeJSONPath(data any, expr string) (string, error) {
	jp := jsonpath.New("output")
	if err := jp.Parse(expr); err != nil {
		return "", fmt.Errorf("invalid jsonpath expression %q: %w", expr, err)
	}

	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("failed to marshal: %w", err)
	}

	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return "", fmt.Errorf("failed to unmarshal: %w", err)
	}

	var buf bytes.Buffer
	if err := jp.Execute(&buf, generic); err != nil {
		return "", fmt.Errorf("jsonpath execution failed: %w", err)
	}

	return buf.String() + "\n", nil
}

// FormatProgram formats a bpfman.Program according to the specified output flags.
func FormatProgram(prog bpfman.Program, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		return formatProgramJSON(prog)
	case OutputFormatTree:
		return formatProgramTree(prog), nil
	case OutputFormatTable:
		return formatProgramTable(prog), nil
	case OutputFormatJSONPath:
		return formatProgramJSONPath(prog, flags.JSONPathExpr())
	case OutputFormatWide, OutputFormatCustomColumns, OutputFormatCustomColumnsFile:
		return "", fmt.Errorf("output format %q is only supported for list commands", flags.Output.Value)
	default:
		return formatProgramTable(prog), nil
	}
}

func formatProgramJSON(prog bpfman.Program) (string, error) {
	output, err := json.MarshalIndent(prog, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(output) + "\n", nil
}

func formatProgramJSONPath(prog bpfman.Program, expr string) (string, error) {
	return executeJSONPath(prog, expr)
}

func formatProgramTree(prog bpfman.Program) string {
	var b strings.Builder

	// Header
	if prog.Status.Kernel != nil {
		p := prog.Status.Kernel
		fmt.Fprintf(&b, "Program %d: %s (%s)\n", p.ID, p.Name, p.ProgramType)
	}

	// Status (runtime state)
	b.WriteString("├─ Status\n")
	if prog.Status.Kernel != nil {
		p := prog.Status.Kernel
		fmt.Fprintf(&b, "│  ├─ tag:        %s\n", p.Tag)
		if !p.LoadedAt.IsZero() {
			fmt.Fprintf(&b, "│  ├─ loaded_at:  %s\n", p.LoadedAt.Format(time.RFC3339))
		}
		if p.BTFId != 0 {
			fmt.Fprintf(&b, "│  ├─ btf_id:     %d\n", p.BTFId)
		}
		if p.JitedSize != 0 {
			fmt.Fprintf(&b, "│  ├─ jited:      %d bytes\n", p.JitedSize)
		}
		if p.XlatedSize != 0 {
			fmt.Fprintf(&b, "│  ├─ xlated:     %d bytes\n", p.XlatedSize)
		}

		// Maps
		if len(prog.Status.Maps) > 0 {
			fmt.Fprintf(&b, "│  ├─ Maps (%d)\n", len(prog.Status.Maps))
			for i, m := range prog.Status.Maps {
				prefix := "│  │  ├─"
				if i == len(prog.Status.Maps)-1 {
					prefix = "│  │  └─"
				}
				fmt.Fprintf(&b, "%s [%d] %s (%s)\n", prefix, m.ID, m.Name, m.MapType)
				detailPrefix := "│  │  │ "
				if i == len(prog.Status.Maps)-1 {
					detailPrefix = "│  │    "
				}
				fmt.Fprintf(&b, "%s        keys: %dB, values: %dB, max: %d\n",
					detailPrefix, m.KeySize, m.ValueSize, m.MaxEntries)
			}
		} else {
			b.WriteString("│  ├─ Maps: none\n")
		}

		// Links
		if len(prog.Status.Links) > 0 {
			fmt.Fprintf(&b, "│  └─ Links (%d)\n", len(prog.Status.Links))
			for i, l := range prog.Status.Links {
				prefix := "│     ├─"
				if i == len(prog.Status.Links)-1 {
					prefix = "│     └─"
				}
				if l.Status.Kernel != nil {
					fmt.Fprintf(&b, "%s [%d] %s\n", prefix, l.Record.ID, l.Status.Kernel.LinkType)
				} else {
					fmt.Fprintf(&b, "%s [%d] %s\n", prefix, l.Record.ID, l.Record.Kind)
				}
			}
		} else {
			b.WriteString("│  └─ Links: none\n")
		}
	}

	// Spec (configured state)
	b.WriteString("│\n")
	b.WriteString("└─ Spec\n")
	p := &prog.Record
	if !p.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "   ├─ created:    %s\n", p.CreatedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "   ├─ source:     %s\n", p.Load.ObjectPath())
	fmt.Fprintf(&b, "   └─ pin_path:   %s\n", p.Handles.PinPath)

	return b.String()
}

func formatProgramTable(prog bpfman.Program) string {
	var b strings.Builder
	p := &prog.Record

	// Header - kernel-assigned identifier
	fmt.Fprintf(&b, "Kernel ID: %d\n", p.KernelID)

	// Collect Spec, Status, and Stats fields, then align them together
	var specFields, statusFields, statsFields []string

	// Spec fields (sorted alphabetically)
	if len(p.Load.GlobalData()) > 0 {
		specFields = append(specFields, fmt.Sprintf("    Global:\t%s", formatGlobalData(p.Load.GlobalData())))
	} else {
		specFields = append(specFields, "    Global:\tNone")
	}
	specFields = append(specFields, fmt.Sprintf("    GPL Compatible:\t%t", p.GPLCompatible))
	if p.License != "" {
		specFields = append(specFields, fmt.Sprintf("    License:\t%s", p.License))
	} else {
		specFields = append(specFields, "    License:\tNone")
	}
	if p.Handles.MapOwnerID != nil {
		specFields = append(specFields, fmt.Sprintf("    Map Owner ID:\t%d", *p.Handles.MapOwnerID))
	} else {
		specFields = append(specFields, "    Map Owner ID:\tNone")
	}
	specFields = append(specFields, fmt.Sprintf("    Map Pin Path:\t%s", p.Handles.MapPinPath))
	if len(p.Meta.Metadata) > 0 {
		specFields = append(specFields, fmt.Sprintf("    Metadata:\t%s", formatMetadata(p.Meta.Metadata)))
	} else {
		specFields = append(specFields, "    Metadata:\tNone")
	}
	specFields = append(specFields, fmt.Sprintf("    Name:\t%s", p.Meta.Name))
	specFields = append(specFields, fmt.Sprintf("    Path:\t%s", p.Load.ObjectPath()))
	specFields = append(specFields, fmt.Sprintf("    Type:\t%s", p.Load.ProgramType()))

	// Status fields (sorted alphabetically)
	if prog.Status.Kernel != nil {
		kp := prog.Status.Kernel
		if kp.BTFId != 0 {
			statusFields = append(statusFields, fmt.Sprintf("    BTF ID:\t%d", kp.BTFId))
		}
		statusFields = append(statusFields, fmt.Sprintf("    Instructions:\t%d", kp.VerifiedInstructions))
		if len(prog.Status.Links) > 0 {
			for i, l := range prog.Status.Links {
				var linkStr string
				if l.Record.Details != nil {
					attachInfo := formatAttachDetails(l.Record.Details)
					linkStr = fmt.Sprintf("%d (%s)", l.Record.ID, attachInfo)
				} else {
					linkStr = fmt.Sprintf("%d", l.Record.ID)
				}
				if i == 0 {
					statusFields = append(statusFields, fmt.Sprintf("    Links:\t%s", linkStr))
				} else {
					statusFields = append(statusFields, fmt.Sprintf("    \t%s", linkStr))
				}
			}
		} else {
			statusFields = append(statusFields, "    Links:\tNone")
		}
		if !kp.LoadedAt.IsZero() {
			statusFields = append(statusFields, fmt.Sprintf("    Loaded At:\t%s", kp.LoadedAt.Format(time.RFC3339)))
		}
		if len(kp.MapIDs) > 0 {
			statusFields = append(statusFields, fmt.Sprintf("    Map IDs:\t%v", kp.MapIDs))
		} else {
			statusFields = append(statusFields, "    Map IDs:\tNone")
		}
		if kp.Memlock != 0 {
			statusFields = append(statusFields, fmt.Sprintf("    Memory:\t%d bytes", kp.Memlock))
		}
		statusFields = append(statusFields, fmt.Sprintf("    Size JITted:\t%d bytes", kp.JitedSize))
		statusFields = append(statusFields, fmt.Sprintf("    Size Translated:\t%d bytes", kp.XlatedSize))
		statusFields = append(statusFields, fmt.Sprintf("    Tag:\t%s", kp.Tag))
	} else {
		statusFields = append(statusFields, "    (no kernel info available)")
	}

	// Stats fields (sorted alphabetically)
	if prog.Status.Stats != nil {
		if prog.Status.Stats.RecursionMisses > 0 {
			statsFields = append(statsFields, fmt.Sprintf("    Recursion Misses:\t%d", prog.Status.Stats.RecursionMisses))
		}
		statsFields = append(statsFields, fmt.Sprintf("    Run Count:\t%d", prog.Status.Stats.RunCount))
		statsFields = append(statsFields, fmt.Sprintf("    Runtime:\t%s", prog.Status.Stats.Runtime))
	} else {
		statsFields = append(statsFields, "    (not enabled, see sysctl kernel.bpf_stats_enabled)")
	}

	// Run all fields through single tabwriter to get unified alignment
	var aligned strings.Builder
	w := tabwriter.NewWriter(&aligned, 0, 0, 1, ' ', 0)
	for _, f := range specFields {
		fmt.Fprintln(w, f)
	}
	for _, f := range statusFields {
		fmt.Fprintln(w, f)
	}
	for _, f := range statsFields {
		fmt.Fprintln(w, f)
	}
	w.Flush()

	// Split aligned output and reassemble with headers
	lines := strings.Split(strings.TrimSuffix(aligned.String(), "\n"), "\n")
	specEnd := len(specFields)
	statusEnd := specEnd + len(statusFields)

	b.WriteString("  Spec:\n")
	for _, line := range lines[:specEnd] {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  Status:\n")
	for _, line := range lines[specEnd:statusEnd] {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  Stats:\n")
	for _, line := range lines[statusEnd:] {
		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

// formatGlobalData formats global data map for display.
func formatGlobalData(data map[string][]byte) string {
	if len(data) == 0 {
		return "None"
	}
	parts := make([]string, 0, len(data))
	for k, v := range data {
		parts = append(parts, fmt.Sprintf("%s=%x", k, v))
	}
	return strings.Join(parts, ", ")
}

// formatMetadata formats metadata map for display.
func formatMetadata(meta map[string]string) string {
	if len(meta) == 0 {
		return "None"
	}
	parts := make([]string, 0, len(meta))
	for k, v := range meta {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ", ")
}

// FormatProgramList formats a list of bpfman.Program according to the specified output flags.
func FormatProgramList(programs []bpfman.Program, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		return formatProgramListJSON(programs)
	case OutputFormatTable:
		return formatProgramListTable(programs), nil
	case OutputFormatJSONPath:
		return formatProgramListJSONPath(programs, flags.JSONPathExpr())
	default:
		return formatProgramListTable(programs), nil
	}
}

func formatProgramListJSON(programs []bpfman.Program) (string, error) {
	output, err := json.MarshalIndent(programs, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(output) + "\n", nil
}

func formatProgramListJSONPath(programs []bpfman.Program, expr string) (string, error) {
	return executeJSONPath(programs, expr)
}

func formatProgramListTable(programs []bpfman.Program) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "KERNEL ID\tTYPE\tNAME\tSOURCE")

	for _, p := range programs {
		id := p.Record.KernelID
		name := p.Record.Meta.Name
		progType := p.Record.Load.ProgramType()
		source := p.Record.Load.ObjectPath()

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", id, progType, name, source)
	}

	w.Flush()
	return b.String()
}

// FormatLinkList formats a list of LinkRecord according to the specified output flags.
func FormatLinkList(links []bpfman.LinkRecord, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		return formatLinkListJSON(links)
	case OutputFormatTable:
		return formatLinkListTable(links), nil
	case OutputFormatWide:
		return formatLinkListWide(links), nil
	case OutputFormatCustomColumns:
		return formatLinkListCustomColumns(links, flags.CustomColumnsSpec())
	case OutputFormatCustomColumnsFile:
		return formatLinkListCustomColumnsFile(links, flags.CustomColumnsFile())
	case OutputFormatJSONPath:
		return formatLinkListJSONPath(links, flags.JSONPathExpr())
	default:
		return formatLinkListTable(links), nil
	}
}

func formatLinkListJSON(links []bpfman.LinkRecord) (string, error) {
	result := bpfman.LinkListResult{Links: links}
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(output) + "\n", nil
}

func formatLinkListJSONPath(links []bpfman.LinkRecord, expr string) (string, error) {
	result := bpfman.LinkListResult{Links: links}
	return executeJSONPath(result, expr)
}

func formatLinkListTable(links []bpfman.LinkRecord) string {
	return DefaultLinkColumns().FormatLinkTable(links)
}

func formatLinkListWide(links []bpfman.LinkRecord) string {
	return WideLinkColumns().FormatLinkTable(links)
}

func formatLinkListCustomColumns(links []bpfman.LinkRecord, spec string) (string, error) {
	columns, err := ParseCustomColumns(spec)
	if err != nil {
		return "", err
	}
	if err := columns.Validate(); err != nil {
		return "", err
	}
	return columns.FormatLinkTable(links), nil
}

func formatLinkListCustomColumnsFile(links []bpfman.LinkRecord, path string) (string, error) {
	columns, err := ParseCustomColumnsFile(path)
	if err != nil {
		return "", err
	}
	if err := columns.Validate(); err != nil {
		return "", err
	}
	return columns.FormatLinkTable(links), nil
}

// FormatLinkResult formats a link result (from attach command) according to
// the specified output flags.
func FormatLinkResult(link bpfman.Link, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		output, err := json.MarshalIndent(link, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal link: %w", err)
		}
		return string(output) + "\n", nil
	case OutputFormatTable:
		return formatLinkResultTable(link), nil
	case OutputFormatJSONPath:
		return executeJSONPath(link, flags.JSONPathExpr())
	case OutputFormatWide, OutputFormatCustomColumns, OutputFormatCustomColumnsFile:
		return "", fmt.Errorf("output format %q is only supported for list commands", flags.Output.Value)
	default:
		return formatLinkResultTable(link), nil
	}
}

func formatLinkResultTable(link bpfman.Link) string {
	var b strings.Builder

	// Primary identifier at column one (like Kernel ID for programs)
	fmt.Fprintf(&b, "Link ID: %d\n", link.Record.ID)

	// Collect Spec fields from LinkSpec, then sort alphabetically
	var specFields []string

	// LinkSpec fields
	if !link.Record.CreatedAt.IsZero() {
		specFields = append(specFields, fmt.Sprintf("    Created At:\t%s", link.Record.CreatedAt.Format(time.RFC3339)))
	}
	specFields = append(specFields, "    Metadata:\tNone")
	if link.Record.PinPath != nil {
		specFields = append(specFields, fmt.Sprintf("    Pin Path:\t%s", link.Record.PinPath.String()))
	} else {
		specFields = append(specFields, "    Pin Path:\tNone")
	}
	specFields = append(specFields, fmt.Sprintf("    Program ID:\t%d", link.Record.ProgramID))
	specFields = append(specFields, fmt.Sprintf("    Type:\t%s", link.Record.Kind))

	// Type-specific fields from LinkDetails
	switch d := link.Record.Details.(type) {
	case bpfman.FentryDetails:
		specFields = append(specFields, fmt.Sprintf("    Target Function:\t%s", d.FnName))
	case bpfman.FexitDetails:
		specFields = append(specFields, fmt.Sprintf("    Target Function:\t%s", d.FnName))
	case bpfman.KprobeDetails:
		if d.Retprobe {
			specFields = append(specFields, "    Attach Type:\tkretprobe")
		} else {
			specFields = append(specFields, "    Attach Type:\tkprobe")
		}
		specFields = append(specFields, fmt.Sprintf("    Target Function:\t%s", d.FnName))
		if d.Offset != 0 {
			specFields = append(specFields, fmt.Sprintf("    Target Offset:\t%d", d.Offset))
		}
	case bpfman.TCDetails:
		specFields = append(specFields, fmt.Sprintf("    Direction:\t%s", d.Direction))
		specFields = append(specFields, fmt.Sprintf("    Interface:\t%s", d.Interface))
		if d.Netns != "" {
			specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", d.Netns))
		}
		specFields = append(specFields, fmt.Sprintf("    Position:\t%d", d.Position))
		specFields = append(specFields, fmt.Sprintf("    Priority:\t%d", d.Priority))
		specFields = append(specFields, fmt.Sprintf("    Proceed On:\t%s", TCActionsToString(d.ProceedOn)))
	case bpfman.TCXDetails:
		specFields = append(specFields, fmt.Sprintf("    Direction:\t%s", d.Direction))
		specFields = append(specFields, fmt.Sprintf("    Interface:\t%s", d.Interface))
		if d.Netns != "" {
			specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", d.Netns))
		}
		specFields = append(specFields, fmt.Sprintf("    Priority:\t%d", d.Priority))
	case bpfman.TracepointDetails:
		specFields = append(specFields, fmt.Sprintf("    Tracepoint:\t%s/%s", d.Group, d.Name))
	case bpfman.UprobeDetails:
		if d.Retprobe {
			specFields = append(specFields, "    Attach Type:\turetprobe")
		} else {
			specFields = append(specFields, "    Attach Type:\tuprobe")
		}
		if d.PID != 0 {
			specFields = append(specFields, fmt.Sprintf("    PID:\t%d", d.PID))
		}
		specFields = append(specFields, fmt.Sprintf("    Target:\t%s", d.Target))
		specFields = append(specFields, fmt.Sprintf("    Target Function:\t%s", d.FnName))
		if d.Offset != 0 {
			specFields = append(specFields, fmt.Sprintf("    Target Offset:\t%d", d.Offset))
		}
	case bpfman.XDPDetails:
		specFields = append(specFields, fmt.Sprintf("    Interface:\t%s", d.Interface))
		if d.Netns != "" {
			specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", d.Netns))
		}
		specFields = append(specFields, fmt.Sprintf("    Position:\t%d", d.Position))
		specFields = append(specFields, fmt.Sprintf("    Priority:\t%d", d.Priority))
		specFields = append(specFields, fmt.Sprintf("    Proceed On:\t%s", formatXDPProceedOn(d.ProceedOn)))
	}

	// Sort Spec fields alphabetically
	slices.Sort(specFields)

	// Collect Status fields from LinkStatus
	var statusFields []string
	if link.Status.KernelSeen {
		statusFields = append(statusFields, "    Kernel Seen:\ttrue")
	} else {
		statusFields = append(statusFields, "    Kernel Seen:\tfalse")
	}
	if link.Status.PinPresent {
		statusFields = append(statusFields, "    Pin Present:\ttrue")
	} else {
		statusFields = append(statusFields, "    Pin Present:\tfalse")
	}

	// Sort Status fields alphabetically
	slices.Sort(statusFields)

	// Run all fields through single tabwriter for unified alignment
	var aligned strings.Builder
	w := tabwriter.NewWriter(&aligned, 0, 0, 1, ' ', 0)
	for _, f := range specFields {
		fmt.Fprintln(w, f)
	}
	for _, f := range statusFields {
		fmt.Fprintln(w, f)
	}
	w.Flush()

	// Split aligned output and reassemble with headers
	lines := strings.Split(strings.TrimSuffix(aligned.String(), "\n"), "\n")
	b.WriteString("  Spec:\n")
	for _, line := range lines[:len(specFields)] {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("  Status:\n")
	for _, line := range lines[len(specFields):] {
		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

// formatXDPProceedOn converts XDP proceed-on values to a human-readable string.
func formatXDPProceedOn(actions []int32) string {
	if len(actions) == 0 {
		return "None"
	}
	// XDP actions: 0=aborted, 1=drop, 2=pass, 3=tx, 4=redirect, 31=dispatcher_return
	xdpNames := map[int32]string{
		0:  "aborted",
		1:  "drop",
		2:  "pass",
		3:  "tx",
		4:  "redirect",
		31: "dispatcher_return",
	}
	names := make([]string, len(actions))
	for i, a := range actions {
		if name, ok := xdpNames[a]; ok {
			names[i] = name
		} else {
			names[i] = fmt.Sprintf("unknown(%d)", a)
		}
	}
	return strings.Join(names, ", ")
}

// formatAttachDetails formats type-specific link details for display.
func formatAttachDetails(details bpfman.LinkDetails) string {
	if details == nil {
		return ""
	}
	switch d := details.(type) {
	case bpfman.TracepointDetails:
		return d.Group + "/" + d.Name
	case bpfman.KprobeDetails:
		if d.Retprobe {
			return "kretprobe:" + d.FnName
		}
		return d.FnName
	case bpfman.UprobeDetails:
		if d.Retprobe {
			return fmt.Sprintf("uretprobe:%s:%s", d.Target, d.FnName)
		}
		return fmt.Sprintf("%s:%s", d.Target, d.FnName)
	case bpfman.FentryDetails:
		return d.FnName
	case bpfman.FexitDetails:
		return d.FnName
	case bpfman.XDPDetails:
		return fmt.Sprintf("%s (ifindex=%d, pos=%d)", d.Interface, d.Ifindex, d.Position)
	case bpfman.TCDetails:
		return fmt.Sprintf("%s/%s (ifindex=%d, pos=%d)", d.Interface, d.Direction, d.Ifindex, d.Position)
	case bpfman.TCXDetails:
		return fmt.Sprintf("%s/%s (ifindex=%d)", d.Interface, d.Direction, d.Ifindex)
	default:
		return ""
	}
}

// FormatLoadedPrograms formats a list of loaded bpfman.Program according to the specified output flags.
// This is used for the load command output.
func FormatLoadedPrograms(programs []bpfman.Program, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		return formatLoadedProgramsJSON(programs)
	case OutputFormatTable:
		return formatLoadedProgramsTable(programs), nil
	case OutputFormatJSONPath:
		return formatLoadedProgramsJSONPath(programs, flags.JSONPathExpr())
	default:
		return formatLoadedProgramsTable(programs), nil
	}
}

func formatLoadedProgramsJSON(programs []bpfman.Program) (string, error) {
	output, err := json.MarshalIndent(programs, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(output) + "\n", nil
}

func formatLoadedProgramsJSONPath(programs []bpfman.Program, expr string) (string, error) {
	return executeJSONPath(programs, expr)
}

func formatLoadedProgramsTable(programs []bpfman.Program) string {
	// Sort programs by kernel ID for consistent, scannable output
	sorted := slices.Clone(programs)
	slices.SortFunc(sorted, func(a, b bpfman.Program) int {
		if a.Record.KernelID < b.Record.KernelID {
			return -1
		}
		if a.Record.KernelID > b.Record.KernelID {
			return 1
		}
		return 0
	})

	var b strings.Builder

	for i, prog := range sorted {
		if i > 0 {
			b.WriteString("\n")
		}

		p := &prog.Record

		// Header - kernel-assigned identifier
		fmt.Fprintf(&b, "Kernel ID: %d\n", p.KernelID)

		// Collect Spec, Status, and Stats fields, then align them together
		var specFields, statusFields, statsFields []string

		// Spec fields (sorted alphabetically)
		if len(p.Load.GlobalData()) > 0 {
			specFields = append(specFields, fmt.Sprintf("    Global:\t%s", formatGlobalData(p.Load.GlobalData())))
		} else {
			specFields = append(specFields, "    Global:\tNone")
		}
		specFields = append(specFields, fmt.Sprintf("    GPL Compatible:\t%t", p.GPLCompatible))
		if p.License != "" {
			specFields = append(specFields, fmt.Sprintf("    License:\t%s", p.License))
		} else {
			specFields = append(specFields, "    License:\tNone")
		}
		if p.Handles.MapOwnerID != nil {
			specFields = append(specFields, fmt.Sprintf("    Map Owner ID:\t%d", *p.Handles.MapOwnerID))
		} else {
			specFields = append(specFields, "    Map Owner ID:\tNone")
		}
		specFields = append(specFields, fmt.Sprintf("    Map Pin Path:\t%s", p.Handles.MapPinPath))
		if len(p.Meta.Metadata) > 0 {
			specFields = append(specFields, fmt.Sprintf("    Metadata:\t%s", formatMetadata(p.Meta.Metadata)))
		} else {
			specFields = append(specFields, "    Metadata:\tNone")
		}
		specFields = append(specFields, fmt.Sprintf("    Name:\t%s", p.Meta.Name))
		specFields = append(specFields, fmt.Sprintf("    Path:\t%s", p.Load.ObjectPath()))
		specFields = append(specFields, fmt.Sprintf("    Type:\t%s", p.Load.ProgramType()))

		// Status fields (sorted alphabetically)
		if prog.Status.Kernel != nil {
			kp := prog.Status.Kernel
			if kp.BTFId != 0 {
				statusFields = append(statusFields, fmt.Sprintf("    BTF ID:\t%d", kp.BTFId))
			}
			statusFields = append(statusFields, fmt.Sprintf("    Instructions:\t%d", kp.VerifiedInstructions))
			if len(prog.Status.Links) > 0 {
				for j, l := range prog.Status.Links {
					var linkStr string
					if l.Record.Details != nil {
						attachInfo := formatAttachDetails(l.Record.Details)
						linkStr = fmt.Sprintf("%d (%s)", l.Record.ID, attachInfo)
					} else {
						linkStr = fmt.Sprintf("%d", l.Record.ID)
					}
					if j == 0 {
						statusFields = append(statusFields, fmt.Sprintf("    Links:\t%s", linkStr))
					} else {
						statusFields = append(statusFields, fmt.Sprintf("    \t%s", linkStr))
					}
				}
			} else {
				statusFields = append(statusFields, "    Links:\tNone")
			}
			if !kp.LoadedAt.IsZero() {
				statusFields = append(statusFields, fmt.Sprintf("    Loaded At:\t%s", kp.LoadedAt.Format(time.RFC3339)))
			}
			if len(kp.MapIDs) > 0 {
				statusFields = append(statusFields, fmt.Sprintf("    Map IDs:\t%v", kp.MapIDs))
			} else {
				statusFields = append(statusFields, "    Map IDs:\tNone")
			}
			if kp.Memlock != 0 {
				statusFields = append(statusFields, fmt.Sprintf("    Memory:\t%d bytes", kp.Memlock))
			}
			statusFields = append(statusFields, fmt.Sprintf("    Size JITted:\t%d bytes", kp.JitedSize))
			statusFields = append(statusFields, fmt.Sprintf("    Size Translated:\t%d bytes", kp.XlatedSize))
			statusFields = append(statusFields, fmt.Sprintf("    Tag:\t%s", kp.Tag))
		} else {
			statusFields = append(statusFields, "    (no kernel info available)")
		}

		// Stats fields (sorted alphabetically)
		if prog.Status.Stats != nil {
			if prog.Status.Stats.RecursionMisses > 0 {
				statsFields = append(statsFields, fmt.Sprintf("    Recursion Misses:\t%d", prog.Status.Stats.RecursionMisses))
			}
			statsFields = append(statsFields, fmt.Sprintf("    Run Count:\t%d", prog.Status.Stats.RunCount))
			statsFields = append(statsFields, fmt.Sprintf("    Runtime:\t%s", prog.Status.Stats.Runtime))
		} else {
			statsFields = append(statsFields, "    (not enabled, see sysctl kernel.bpf_stats_enabled)")
		}

		// Run all fields through single tabwriter to get unified alignment
		var aligned strings.Builder
		w := tabwriter.NewWriter(&aligned, 0, 0, 1, ' ', 0)
		for _, f := range specFields {
			fmt.Fprintln(w, f)
		}
		for _, f := range statusFields {
			fmt.Fprintln(w, f)
		}
		for _, f := range statsFields {
			fmt.Fprintln(w, f)
		}
		w.Flush()

		// Split aligned output and reassemble with headers
		lines := strings.Split(strings.TrimSuffix(aligned.String(), "\n"), "\n")
		specEnd := len(specFields)
		statusEnd := specEnd + len(statusFields)

		b.WriteString("  Spec:\n")
		for _, line := range lines[:specEnd] {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("  Status:\n")
		for _, line := range lines[specEnd:statusEnd] {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("  Stats:\n")
		for _, line := range lines[statusEnd:] {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	return b.String()
}

// FormatProgramsComposite formats bpfman.ProgramListResult with full spec/status.
// This returns the canonical domain type with both Spec and Status, plus observation metadata.
func FormatProgramsComposite(result bpfman.ProgramListResult, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		output, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal: %w", err)
		}
		return string(output) + "\n", nil
	case OutputFormatJSONPath:
		return executeJSONPath(result, flags.JSONPathExpr())
	case OutputFormatTable:
		return formatProgramsCompositeTable(result), nil
	case OutputFormatWide:
		return formatProgramsCompositeWide(result), nil
	case OutputFormatCustomColumns:
		return formatProgramsCompositeCustomColumns(result, flags.CustomColumnsSpec())
	case OutputFormatCustomColumnsFile:
		return formatProgramsCompositeCustomColumnsFile(result, flags.CustomColumnsFile())
	default:
		return formatProgramsCompositeTable(result), nil
	}
}

func formatProgramsCompositeTable(result bpfman.ProgramListResult) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "KERNEL ID\tTYPE\tNAME\tSOURCE")

	for _, p := range result.Programs {
		id := p.Record.KernelID

		// Get info from spec (always present as value type)
		name := p.Record.Meta.Name
		progType := p.Record.Load.ProgramType().String()
		source := p.Record.Load.ObjectPath()

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", id, progType, name, source)
	}

	w.Flush()
	return b.String()
}

// FormatOutcome formats a OperationOutcome according to the specified output flags.
// This is used to display structured error information on failure paths.
func FormatOutcome(o outcome.OperationOutcome, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		return formatOutcomeJSON(o)
	case OutputFormatJSONPath:
		return formatOutcomeJSONPath(o, flags.JSONPathExpr())
	default:
		return formatOutcomeTable(o), nil
	}
}

func formatOutcomeJSON(o outcome.OperationOutcome) (string, error) {
	output, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal outcome: %w", err)
	}
	return string(output) + "\n", nil
}

func formatOutcomeJSONPath(o outcome.OperationOutcome, expr string) (string, error) {
	return executeJSONPath(o, expr)
}

func formatOutcomeTable(o outcome.OperationOutcome) string {
	var b strings.Builder

	// Primary identifier at column one (kubectl-describe style)
	fmt.Fprintf(&b, "Operation: %s\n", strings.ToUpper(string(o.Status)))

	// Build summary fields for tabwriter alignment (sorted alphabetically)
	var summaryFields []string

	// Cleanup
	if len(o.ManualCleanupCommands) > 0 {
		for i, cmd := range o.ManualCleanupCommands {
			if i == 0 {
				summaryFields = append(summaryFields, fmt.Sprintf("    Cleanup:\t%s", strings.Join(cmd, " ")))
			} else {
				summaryFields = append(summaryFields, fmt.Sprintf("    \t%s", strings.Join(cmd, " ")))
			}
		}
	}

	// Error
	if o.PrimaryError != "" {
		summaryFields = append(summaryFields, fmt.Sprintf("    Error:\t%s", o.PrimaryError))
	}

	// Op ID
	if o.OpID != 0 {
		summaryFields = append(summaryFields, fmt.Sprintf("    Op ID:\t%d", o.OpID))
	}

	// Orphaned
	if len(o.Residual) > 0 {
		for i, a := range o.Residual {
			if i == 0 {
				summaryFields = append(summaryFields, fmt.Sprintf("    Orphaned:\t%s", a.String()))
			} else {
				summaryFields = append(summaryFields, fmt.Sprintf("    \t%s", a.String()))
			}
		}
	} else {
		summaryFields = append(summaryFields, "    Orphaned:\tNone")
	}

	// Rollback Errors
	if len(o.RollbackErrors) > 0 {
		for i, re := range o.RollbackErrors {
			if i == 0 {
				summaryFields = append(summaryFields, fmt.Sprintf("    Rollback Errors:\tStep %d: %s", re.Step, re.Err))
			} else {
				summaryFields = append(summaryFields, fmt.Sprintf("    \tStep %d: %s", re.Step, re.Err))
			}
		}
	}

	// System State (only show when not clean)
	switch o.SystemState {
	case "inconsistent":
		summaryFields = append(summaryFields, "    System State:\tINCONSISTENT")
	case "unknown":
		summaryFields = append(summaryFields, "    System State:\tUNKNOWN")
	}

	// Run summary fields through tabwriter
	var aligned strings.Builder
	w := tabwriter.NewWriter(&aligned, 0, 0, 1, ' ', 0)
	for _, f := range summaryFields {
		fmt.Fprintln(w, f)
	}
	w.Flush()

	// Write summary section
	b.WriteString("  Summary:\n")
	b.WriteString(aligned.String())

	// Events section
	// Skip if there's only one failed entry with the same error as PrimaryError
	// (avoids redundant display for simple single-step failures)
	showEvents := len(o.Timeline) > 0
	if showEvents && len(o.Timeline) == 1 && o.Timeline[0].Error == o.PrimaryError {
		showEvents = false
	}
	if showEvents {
		b.WriteString("  Events:\n")
		for _, entry := range o.Timeline {
			// Each timeline entry as a sub-block (fields sorted alphabetically)
			var entryFields []string
			if entry.Error != "" {
				entryFields = append(entryFields, fmt.Sprintf("      Error:\t%s", entry.Error))
			}
			entryFields = append(entryFields, fmt.Sprintf("      Kind:\t%s", entry.Kind))
			// Only show Phase for rollback steps (primary is implied)
			if entry.Phase == outcome.PhaseRollback {
				entryFields = append(entryFields, fmt.Sprintf("      Phase:\t%s", entry.Phase))
			}
			entryFields = append(entryFields, fmt.Sprintf("      Step:\t%d", entry.Seq))
			entryFields = append(entryFields, fmt.Sprintf("      Status:\t%s", entry.Status))
			entryFields = append(entryFields, fmt.Sprintf("      Target:\t%s", entry.Target))
			// Add any details (sorted within formatTimelineDetailsDescribe)
			if detailStr := formatTimelineDetailsDescribe(entry.Details); detailStr != "" {
				entryFields = append(entryFields, detailStr)
			}

			// Write entry header with timestamp (provides natural ordering)
			fmt.Fprintf(&b, "    %s\n", entry.Timestamp.Format(time.RFC3339Nano))

			// Align entry fields
			var entryAligned strings.Builder
			ew := tabwriter.NewWriter(&entryAligned, 0, 0, 1, ' ', 0)
			for _, f := range entryFields {
				fmt.Fprintln(ew, f)
			}
			ew.Flush()
			b.WriteString(entryAligned.String())
		}
	}

	return b.String()
}

// formatTimelineDetailsDescribe formats timeline entry details for kubectl-describe style.
// Fields within each detail type are sorted alphabetically.
func formatTimelineDetailsDescribe(details any) string {
	if details == nil {
		return ""
	}
	var fields []string
	switch d := details.(type) {
	case outcome.ProgramDetails:
		if d.KernelID != 0 {
			fields = append(fields, fmt.Sprintf("      Kernel ID:\t%d", d.KernelID))
		}
		if d.MapsDirPath != "" {
			fields = append(fields, fmt.Sprintf("      Maps Dir:\t%s", d.MapsDirPath))
		}
		if d.PinPath != "" {
			fields = append(fields, fmt.Sprintf("      Pin Path:\t%s", d.PinPath))
		}
	case outcome.LinkDetails:
		if d.LinkID != 0 {
			fields = append(fields, fmt.Sprintf("      Link ID:\t%d", d.LinkID))
		}
		if d.PinPath != "" {
			fields = append(fields, fmt.Sprintf("      Pin Path:\t%s", d.PinPath))
		}
	case outcome.ImageDetails:
		if d.Digest != "" {
			fields = append(fields, fmt.Sprintf("      Digest:\t%s", d.Digest))
		}
		if d.URL != "" {
			fields = append(fields, fmt.Sprintf("      Image URL:\t%s", d.URL))
		}
		if d.ObjectPath != "" {
			fields = append(fields, fmt.Sprintf("      Object Path:\t%s", d.ObjectPath))
		}
	case outcome.DispatcherDetails:
		if d.DispatcherID != 0 {
			fields = append(fields, fmt.Sprintf("      Dispatcher ID:\t%d", d.DispatcherID))
		}
	case outcome.GCPhaseDetails:
		fields = append(fields, fmt.Sprintf("      Removed:\t%d", d.Removed))
	case outcome.OrphanDetails:
		fields = append(fields, fmt.Sprintf("      Category:\t%s", d.Category))
	}
	return strings.Join(fields, "\n")
}

// formatProgramsCompositeWide formats the program list with wide columns.
func formatProgramsCompositeWide(result bpfman.ProgramListResult) string {
	return WideColumns().FormatTable(result.Programs)
}

// formatProgramsCompositeCustomColumns formats the program list with custom columns.
func formatProgramsCompositeCustomColumns(result bpfman.ProgramListResult, spec string) (string, error) {
	columns, err := ParseCustomColumns(spec)
	if err != nil {
		return "", err
	}
	if err := columns.Validate(); err != nil {
		return "", err
	}
	return columns.FormatTable(result.Programs), nil
}

// formatProgramsCompositeCustomColumnsFile formats the program list with columns from a file.
func formatProgramsCompositeCustomColumnsFile(result bpfman.ProgramListResult, path string) (string, error) {
	columns, err := ParseCustomColumnsFile(path)
	if err != nil {
		return "", err
	}
	if err := columns.Validate(); err != nil {
		return "", err
	}
	return columns.FormatTable(result.Programs), nil
}
