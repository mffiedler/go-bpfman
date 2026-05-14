package cliformat

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
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/platform"
)

// CLI output trailing-newline contract.
//
// Every formatter that returns a string for CLI emission MUST end
// its output with exactly one "\n", matching the Unix convention
// for text streams and what every comparable CLI does (kubectl,
// aws, gcloud, jq, ...). Two paths reach this contract differently:
//
//   - Marshaller-driven formatters (formatProgramJSON,
//     formatLoadedProgramsJSON, etc.) lean on the encoding/json
//     contract. encoding/json.Marshal and MarshalIndent never emit
//     a trailing newline (see Marshal / MarshalIndent godoc),
//     so `string(output) + "\n"` produces exactly one. No trim
//     needed; the producer-side guarantee is checked by
//     TestStdlibJSONMarshal_NoTrailingNewline so a future Go
//     upgrade that changes the stdlib behaviour is caught.
//
//   - executeJSONPath is template-driven: a user-supplied template
//     may or may not emit trailing newlines (`{range...}{"\n"}{end}`
//     ends in one, `{.id}` does not). There is no producer-side
//     contract to lean on, so the function normalises by trimming
//     trailing newlines before appending exactly one. The trim
//     here is required for the contract to hold; it is NOT a
//     workaround for double-newline output.
//
// Code that emits CLI strings should not reinvent either path.
// Marshaller paths use `string(jsonBytes) + "\n"`. JSONPath paths
// route through executeJSONPath. Anything else risks breaking the
// shape that consumers (examples/tracepoint.sh, integration tests,
// downstream scripts) rely on.

// executeJSONPath parses and executes a JSONPath expression against
// the given data and returns the rendered string with exactly one
// trailing newline.
//
// Input contract: the user-supplied template `expr` may emit any
// shape; this function does not constrain it.
//
// Output contract: the returned string ends with exactly one "\n",
// regardless of whether the template's last token emits a newline.
// The buffer is normalised with TrimRight(..., "\n") + "\n" to
// enforce this; do not "simplify" the trim away.
//
// The data is marshalled to JSON and back to ensure consistent
// field access. UseNumber is enabled so that large integers (e.g.
// synthetic link IDs) render as decimal rather than scientific
// notation.
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
	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return "", fmt.Errorf("failed to unmarshal: %w", err)
	}

	var buf bytes.Buffer
	if err := jp.Execute(&buf, generic); err != nil {
		return "", fmt.Errorf("jsonpath execution failed: %w", err)
	}

	// Output contract enforcement: see file-level comment block.
	// Trim then re-append so the result has exactly one trailing
	// "\n" regardless of the template's terminating shape.
	out := strings.TrimRight(buf.String(), "\n")
	return out + "\n", nil
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
	case OutputFormatWide:
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
		fmt.Fprintf(&b, "Program %d: %s%s\n", p.ID, p.Name, p.ProgramType)
	}

	hasPathSection := prog.Status.ProgPin != ""

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
				fmt.Fprintf(&b, "%s [%d] %s%s\n", prefix, m.ID, m.Name, m.MapType)
				detailPrefix := "│  │  │ "
				if i == len(prog.Status.Maps)-1 {
					detailPrefix = "│  │    "
				}
				fmt.Fprintf(&b, "%s        keys: %dB, values: %dB, max: %d\n",
					detailPrefix, m.KeySize, m.ValueSize, m.MaxEntries)
				if m.PinPath != "" {
					fmt.Fprintf(&b, "%s        pin: %s%s\n",
						detailPrefix, m.PinPath, presenceSuffix(m.Present))
				}
			}
		} else {
			b.WriteString("│  ├─ Maps: none\n")
		}

		// Links - connector depends on whether Paths section follows
		linksConnector := "└─"
		linksIndent := "│     "
		if hasPathSection {
			linksConnector = "├─"
			linksIndent = "│  │  "
		}

		if len(prog.Status.Links) > 0 {
			fmt.Fprintf(&b, "│  %s Links (%d)\n", linksConnector, len(prog.Status.Links))
			for i, l := range prog.Status.Links {
				isLast := i == len(prog.Status.Links)-1
				prefix := linksIndent + "├─"
				if isLast {
					prefix = linksIndent + "└─"
				}
				if l.Status.Kernel != nil {
					fmt.Fprintf(&b, "%s [%d] %s\n", prefix, l.Record.ID, l.Status.Kernel.LinkType)
				} else {
					fmt.Fprintf(&b, "%s [%d] %s\n", prefix, l.Record.ID, l.Record.Kind)
				}
				if l.Record.PinPath != nil {
					detailPrefix := linksIndent + "│ "
					if isLast {
						detailPrefix = linksIndent + "  "
					}
					fmt.Fprintf(&b, "%s        pin: %s%s\n",
						detailPrefix, l.Record.PinPath.String(), presenceSuffix(l.Status.PinPresent))
				}
			}
		} else {
			fmt.Fprintf(&b, "│  %s Links: none\n", linksConnector)
		}

		// Paths section (only when filesystem enrichment is present)
		if hasPathSection {
			b.WriteString("│  └─ Paths\n")
			fmt.Fprintf(&b, "│     ├─ prog:     %s\n", prog.Status.ProgPin)
			fmt.Fprintf(&b, "│     ├─ maps:     %s\n", prog.Status.MapDir)
			fmt.Fprintf(&b, "│     ├─ links:    %s\n", prog.Status.LinkDir)
			fmt.Fprintf(&b, "│     └─ bytecode: %s\n", prog.Status.Bytecode)
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

	// Header - program identifier
	fmt.Fprintf(&b, "Program ID: %d\n", p.ProgramID)

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
	specFields = append(specFields, fmt.Sprintf("    Map Pin Path:\t%s", p.Handles.MapsDir))
	if len(p.Meta.Metadata) > 0 {
		specFields = append(specFields, fmt.Sprintf("    Metadata:\t%s", formatMetadata(p.Meta.Metadata)))
	} else {
		specFields = append(specFields, "    Metadata:\tNone")
	}
	specFields = append(specFields, fmt.Sprintf("    Name:\t%s", p.Meta.Name))
	specFields = append(specFields, fmt.Sprintf("    Path:\t%s", p.Load.ObjectPath()))
	specFields = append(specFields, fmt.Sprintf("    Type:\t%s", p.Load.ProgramType()))

	// Status fields (sorted alphabetically).
	// All lines use tabs so the tabwriter aligns values at
	// a single column. Sub-section headers (Link N:, Map N:)
	// carry an empty value; their properties use deeper indent.
	if prog.Status.Kernel != nil {
		kp := prog.Status.Kernel
		if kp.BTFId != 0 {
			statusFields = append(statusFields, fmt.Sprintf("    BTF ID:\t%d", kp.BTFId))
		}
		if prog.Status.ProgPin != "" {
			statusFields = append(statusFields, fmt.Sprintf("    Bytecode:\t%s", prog.Status.Bytecode))
		}
		statusFields = append(statusFields, fmt.Sprintf("    Instructions:\t%d", kp.VerifiedInstructions))
		if prog.Status.ProgPin != "" {
			statusFields = append(statusFields, fmt.Sprintf("    Link Dir:\t%s", prog.Status.LinkDir))
		}
		if len(prog.Status.Links) > 0 {
			statusFields = append(statusFields, "    Links:\t ")
			for _, l := range prog.Status.Links {
				statusFields = append(statusFields, fmt.Sprintf("    Link %d:\t ", l.Record.ID))
				if l.Record.Details != nil {
					statusFields = append(statusFields, fmt.Sprintf("      Attach:\t%s", formatAttachDetails(l.Record.Details)))
				}
				statusFields = append(statusFields, fmt.Sprintf("      Kind:\t%s", l.Record.Kind))
				if l.Record.PinPath != nil {
					statusFields = append(statusFields, fmt.Sprintf("      Pin:\t%s%s", l.Record.PinPath.String(), presenceSuffix(l.Status.PinPresent)))
				}
			}
		} else {
			statusFields = append(statusFields, "    Links:\tNone")
		}
		if !kp.LoadedAt.IsZero() {
			statusFields = append(statusFields, fmt.Sprintf("    Loaded At:\t%s", kp.LoadedAt.Format(time.RFC3339)))
		}
		if prog.Status.ProgPin != "" {
			statusFields = append(statusFields, fmt.Sprintf("    Map Dir:\t%s", prog.Status.MapDir))
		}
		if len(prog.Status.Maps) > 0 {
			statusFields = append(statusFields, "    Maps:\t ")
			for _, m := range prog.Status.Maps {
				statusFields = append(statusFields, fmt.Sprintf("    Map %d:\t ", m.ID))
				statusFields = append(statusFields, fmt.Sprintf("      Key Size:\t%dB", m.KeySize))
				statusFields = append(statusFields, fmt.Sprintf("      Max Entries:\t%d", m.MaxEntries))
				statusFields = append(statusFields, fmt.Sprintf("      Name:\t%s", m.Name))
				if m.PinPath != "" {
					statusFields = append(statusFields, fmt.Sprintf("      Pin:\t%s%s", m.PinPath, presenceSuffix(m.Present)))
				}
				statusFields = append(statusFields, fmt.Sprintf("      Type:\t%s", m.MapType))
				statusFields = append(statusFields, fmt.Sprintf("      Value Size:\t%dB", m.ValueSize))
			}
		} else if len(kp.MapIDs) > 0 {
			statusFields = append(statusFields, fmt.Sprintf("    Maps:\t%v", kp.MapIDs))
		} else {
			statusFields = append(statusFields, "    Maps:\tNone")
		}
		if kp.Memlock != 0 {
			statusFields = append(statusFields, fmt.Sprintf("    Memory:\t%d bytes", kp.Memlock))
		}
		if prog.Status.ProgPin != "" {
			statusFields = append(statusFields, fmt.Sprintf("    Prog Pin:\t%s", prog.Status.ProgPin))
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
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}
	b.WriteString("  Status:\n")
	for _, line := range lines[specEnd:statusEnd] {
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}
	b.WriteString("  Stats:\n")
	for _, line := range lines[statusEnd:] {
		b.WriteString(strings.TrimRight(line, " "))
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
	case OutputFormatJSONPath:
		return formatLinkListJSONPath(links, flags.JSONPathExpr())
	default:
		return formatLinkListTable(links), nil
	}
}

func formatLinkListJSON(links []bpfman.LinkRecord) (string, error) {
	if links == nil {
		links = []bpfman.LinkRecord{}
	}
	result := bpfman.LinkListResult{Links: links}
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(output) + "\n", nil
}

func formatLinkListJSONPath(links []bpfman.LinkRecord, expr string) (string, error) {
	if links == nil {
		links = []bpfman.LinkRecord{}
	}
	result := bpfman.LinkListResult{Links: links}
	return executeJSONPath(result, expr)
}

func formatLinkListTable(links []bpfman.LinkRecord) string {
	return DefaultLinkColumns().FormatLinkTable(links)
}

func formatLinkListWide(links []bpfman.LinkRecord) string {
	return WideLinkColumns().FormatLinkTable(links)
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
	case OutputFormatWide:
		return "", fmt.Errorf("output format %q is only supported for list commands", flags.Output.Value)
	default:
		return formatLinkResultTable(link), nil
	}
}

func formatLinkResultTable(link bpfman.Link) string {
	var b strings.Builder

	// Primary identifier at column one (like Program ID for programs)
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
		specFields = append(specFields, fmt.Sprintf("    Proceed On:\t%s", bpfman.TCActionsToString(d.ProceedOn)))
	case bpfman.TCXDetails:
		specFields = append(specFields, fmt.Sprintf("    Direction:\t%s", d.Direction))
		specFields = append(specFields, fmt.Sprintf("    Interface:\t%s", d.Interface))
		if d.Netns != "" {
			specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", d.Netns))
		}
		specFields = append(specFields, fmt.Sprintf("    Position:\t%d", d.Position))
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
		return fmt.Sprintf("%s/%s (ifindex=%d, pos=%d)", d.Interface, d.Direction, d.Ifindex, d.Position)
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
	if programs == nil {
		programs = []bpfman.Program{}
	}
	output, err := json.MarshalIndent(bpfman.LoadResult{Programs: programs}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(output) + "\n", nil
}

func formatLoadedProgramsJSONPath(programs []bpfman.Program, expr string) (string, error) {
	if programs == nil {
		programs = []bpfman.Program{}
	}
	return executeJSONPath(bpfman.LoadResult{Programs: programs}, expr)
}

func formatLoadedProgramsTable(programs []bpfman.Program) string {
	// Sort programs by program ID for consistent, scannable output
	sorted := slices.Clone(programs)
	slices.SortFunc(sorted, func(a, b bpfman.Program) int {
		if a.Record.ProgramID < b.Record.ProgramID {
			return -1
		}
		if a.Record.ProgramID > b.Record.ProgramID {
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

		// Header - program identifier
		fmt.Fprintf(&b, "Program ID: %d\n", p.ProgramID)

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
		specFields = append(specFields, fmt.Sprintf("    Map Pin Path:\t%s", p.Handles.MapsDir))
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
			if prog.Status.ProgPin != "" {
				statusFields = append(statusFields, fmt.Sprintf("    Bytecode:\t%s", prog.Status.Bytecode))
			}
			statusFields = append(statusFields, fmt.Sprintf("    Instructions:\t%d", kp.VerifiedInstructions))
			if prog.Status.ProgPin != "" {
				statusFields = append(statusFields, fmt.Sprintf("    Link Dir:\t%s", prog.Status.LinkDir))
			}
			if len(prog.Status.Links) > 0 {
				statusFields = append(statusFields, "    Links:\t ")
				for _, l := range prog.Status.Links {
					statusFields = append(statusFields, fmt.Sprintf("    Link %d:\t ", l.Record.ID))
					if l.Record.Details != nil {
						statusFields = append(statusFields, fmt.Sprintf("      Attach:\t%s", formatAttachDetails(l.Record.Details)))
					}
					statusFields = append(statusFields, fmt.Sprintf("      Kind:\t%s", l.Record.Kind))
					if l.Record.PinPath != nil {
						statusFields = append(statusFields, fmt.Sprintf("      Pin:\t%s%s", l.Record.PinPath.String(), presenceSuffix(l.Status.PinPresent)))
					}
				}
			} else {
				statusFields = append(statusFields, "    Links:\tNone")
			}
			if !kp.LoadedAt.IsZero() {
				statusFields = append(statusFields, fmt.Sprintf("    Loaded At:\t%s", kp.LoadedAt.Format(time.RFC3339)))
			}
			if prog.Status.ProgPin != "" {
				statusFields = append(statusFields, fmt.Sprintf("    Map Dir:\t%s", prog.Status.MapDir))
			}
			if len(prog.Status.Maps) > 0 {
				statusFields = append(statusFields, "    Maps:\t ")
				for _, m := range prog.Status.Maps {
					statusFields = append(statusFields, fmt.Sprintf("    Map %d:\t ", m.ID))
					statusFields = append(statusFields, fmt.Sprintf("      Key Size:\t%dB", m.KeySize))
					statusFields = append(statusFields, fmt.Sprintf("      Max Entries:\t%d", m.MaxEntries))
					statusFields = append(statusFields, fmt.Sprintf("      Name:\t%s", m.Name))
					if m.PinPath != "" {
						statusFields = append(statusFields, fmt.Sprintf("      Pin:\t%s%s", m.PinPath, presenceSuffix(m.Present)))
					}
					statusFields = append(statusFields, fmt.Sprintf("      Type:\t%s", m.MapType))
					statusFields = append(statusFields, fmt.Sprintf("      Value Size:\t%dB", m.ValueSize))
				}
			} else if len(kp.MapIDs) > 0 {
				statusFields = append(statusFields, fmt.Sprintf("    Maps:\t%v", kp.MapIDs))
			} else {
				statusFields = append(statusFields, "    Maps:\tNone")
			}
			if kp.Memlock != 0 {
				statusFields = append(statusFields, fmt.Sprintf("    Memory:\t%d bytes", kp.Memlock))
			}
			if prog.Status.ProgPin != "" {
				statusFields = append(statusFields, fmt.Sprintf("    Prog Pin:\t%s", prog.Status.ProgPin))
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
			b.WriteString(strings.TrimRight(line, " "))
			b.WriteString("\n")
		}
		b.WriteString("  Status:\n")
		for _, line := range lines[specEnd:statusEnd] {
			b.WriteString(strings.TrimRight(line, " "))
			b.WriteString("\n")
		}
		b.WriteString("  Stats:\n")
		for _, line := range lines[statusEnd:] {
			b.WriteString(strings.TrimRight(line, " "))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// FormatProgramsComposite formats bpfman.ProgramListResult with full spec/status.
// This returns the canonical domain type with both Spec and Status, plus observation metadata.
func FormatProgramsComposite(result bpfman.ProgramListResult, flags *OutputFlags) (string, error) {
	if result.Programs == nil {
		result.Programs = []bpfman.Program{}
	}
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
	default:
		return formatProgramsCompositeTable(result), nil
	}
}

func formatProgramsCompositeTable(result bpfman.ProgramListResult) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "PROGRAM ID\tTYPE\tNAME\tSOURCE")

	for _, p := range result.Programs {
		id := p.Record.ProgramID

		// Get info from spec (always present as value type)
		name := p.Record.Meta.Name
		progType := p.Record.Load.ProgramType().String()
		source := p.Record.Load.ObjectPath()

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", id, progType, name, source)
	}

	w.Flush()
	return b.String()
}

// formatProgramsCompositeWide formats the program list with wide columns.
func formatProgramsCompositeWide(result bpfman.ProgramListResult) string {
	return WideColumns().FormatTable(result.Programs)
}

// FormatDispatcherList formats a list of dispatcher summaries.
func FormatDispatcherList(summaries []platform.DispatcherSummary, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		if summaries == nil {
			summaries = []platform.DispatcherSummary{}
		}
		output, err := json.MarshalIndent(summaries, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal: %w", err)
		}
		return string(output) + "\n", nil
	case OutputFormatJSONPath:
		if summaries == nil {
			summaries = []platform.DispatcherSummary{}
		}
		return executeJSONPath(summaries, flags.JSONPathExpr())
	case OutputFormatTable:
		return formatDispatcherListTable(summaries), nil
	default:
		return formatDispatcherListTable(summaries), nil
	}
}

func formatDispatcherListTable(summaries []platform.DispatcherSummary) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "TYPE\tNSID\tIFINDEX\tREVISION\tPROGRAM_ID\tLINK_ID\tPRIORITY\tMEMBERS")

	for _, s := range summaries {
		linkID := "-"
		if s.Runtime.LinkID != nil {
			linkID = fmt.Sprintf("%d", *s.Runtime.LinkID)
		}
		priority := "-"
		if s.Runtime.FilterPriority != nil {
			priority = fmt.Sprintf("%d", *s.Runtime.FilterPriority)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%d\n",
			s.Key.Type, s.Key.Nsid, s.Key.Ifindex,
			s.Revision, s.Runtime.ProgramID,
			linkID, priority, s.MemberCount)
	}

	w.Flush()
	return b.String()
}

// FormatDispatcherSnapshot formats a single dispatcher snapshot.
func FormatDispatcherSnapshot(snap platform.DispatcherSnapshot, flags *OutputFlags) (string, error) {
	format, err := flags.Format()
	if err != nil {
		return "", err
	}
	switch format {
	case OutputFormatJSON:
		output, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return "", fmt.Errorf("failed to marshal: %w", err)
		}
		return string(output) + "\n", nil
	case OutputFormatJSONPath:
		return executeJSONPath(snap, flags.JSONPathExpr())
	case OutputFormatTable:
		return formatDispatcherSnapshotTable(snap), nil
	default:
		return formatDispatcherSnapshotTable(snap), nil
	}
}

func formatDispatcherSnapshotTable(snap platform.DispatcherSnapshot) string {
	var b strings.Builder

	// Header section
	fmt.Fprintf(&b, "Dispatcher: %s nsid=%d ifindex=%d\n", snap.Key.Type, snap.Key.Nsid, snap.Key.Ifindex)
	fmt.Fprintf(&b, "  Revision:    %d\n", snap.Revision)
	fmt.Fprintf(&b, "  Program ID:  %d\n", snap.Runtime.ProgramID)
	if snap.Runtime.LinkID != nil {
		fmt.Fprintf(&b, "  Link ID:     %d\n", *snap.Runtime.LinkID)
	}
	if snap.Runtime.FilterPriority != nil {
		fmt.Fprintf(&b, "  Priority:    %d\n", *snap.Runtime.FilterPriority)
	}

	// Members table
	fmt.Fprintf(&b, "\nMembers (%d/%d):\n", len(snap.Members), dispatcher.MaxPrograms)

	if len(snap.Members) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}

	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  POS\tPRIORITY\tPROGRAM_ID\tNAME\tLINK_ID\tPROCEED_ON")
	for _, m := range snap.Members {
		proceedOn := formatProceedOnMask(m.ProceedOn, snap.Key.Type)
		fmt.Fprintf(w, "  %d\t%d\t%d\t%s\t%d\t%s\n",
			m.Position, m.Priority, m.ProgramID,
			m.ProgramName, m.LinkID, proceedOn)
	}
	w.Flush()

	return b.String()
}

// formatProceedOnMask decodes a proceed-on bitmask into named actions.
// For XDP, bit N maps directly to XDP action N. For TC, bit N maps to
// TC action with int32 code N. The chain-call shift is only applied at
// BPF map write time, so stored bitmasks use unshifted bit positions.
func formatProceedOnMask(mask uint32, dispType dispatcher.DispatcherType) string {
	if mask == 0 {
		return "none"
	}

	xdpNames := map[uint]string{
		0: "aborted", 1: "drop", 2: "pass", 3: "tx", 4: "redirect",
		31: "dispatcher_return",
	}

	isXDP := dispType == dispatcher.DispatcherTypeXDP

	var names []string
	for bit := uint(0); bit < 32; bit++ {
		if mask&(1<<bit) == 0 {
			continue
		}
		if isXDP {
			if name, ok := xdpNames[bit]; ok {
				names = append(names, name)
			} else {
				names = append(names, fmt.Sprintf("unknown(%d)", bit))
			}
		} else {
			names = append(names, bpfman.TCActionToString(int32(bit)))
		}
	}

	return strings.Join(names, ", ")
}
