package cliformat

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/dispatcher"
	"github.com/bpfman/bpfman/platform"
)

func writeOutput(w io.Writer, output string) error {
	n, err := io.WriteString(w, output)
	if err != nil {
		return err
	}
	if n != len(output) {
		return io.ErrShortWrite
	}
	return nil
}

// CLI output trailing-newline contract.
//
// Every formatter that returns a string for CLI emission MUST end
// its output with exactly one "\n", matching the Unix convention
// for text streams and what every comparable CLI does (kubectl,
// aws, gcloud, jq, ...). Marshaller-driven formatters
// (formatProgramJSON, formatLoadedProgramsJSON, etc.) lean on the
// encoding/json contract. encoding/json.Marshal and MarshalIndent
// never emit a trailing newline (see Marshal / MarshalIndent godoc),
// so `string(output) + "\n"` produces exactly one. No trim needed;
// the producer-side guarantee is checked by
// TestStdlibJSONMarshal_NoTrailingNewline so a future Go upgrade that
// changes the stdlib behaviour is caught.
//
// Code that emits CLI strings should not reinvent either path.
// Marshaller paths use `string(jsonBytes) + "\n"`. Anything else risks
// breaking the shape that consumers, integration tests, and downstream
// scripts rely on.

func unsupportedOutputFormat(format OutputFormat) error {
	return fmt.Errorf("unsupported output format %q", format)
}

// renderOutput dispatches CLI output by format. The JSON branch marshals
// jsonValue indented, with the single trailing newline the CLI contract
// requires; the text branch runs textFn. Per-resource shaping -- envelope
// wrappers, nil-to-empty coercion, presentation joins -- stays in the
// caller; renderOutput owns only the format switch, the JSON encoding and
// trailing newline, and the unsupported-format error.
func renderOutput(w io.Writer, format OutputFormat, jsonValue any, textFn func(io.Writer) error) error {
	switch format {
	case OutputFormatJSON:
		output, err := json.MarshalIndent(jsonValue, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal %T: %w", jsonValue, err)
		}
		return writeOutput(w, string(output)+"\n")
	case OutputFormatText:
		return textFn(w)
	default:
		return unsupportedOutputFormat(format)
	}
}

// RenderProgram writes a program get result in the specified output format.
func RenderProgram(w io.Writer, prog bpfman.Program, format OutputFormat) error {
	return renderOutput(w, format, prog, func(w io.Writer) error {
		return writeOutput(w, formatProgramTable(prog))
	})
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
	for _, k := range slices.Sorted(maps.Keys(data)) {
		parts = append(parts, fmt.Sprintf("%s=%x", k, data[k]))
	}
	return strings.Join(parts, ", ")
}

// formatMetadata formats metadata map for display.
func formatMetadata(meta map[string]string) string {
	if len(meta) == 0 {
		return "None"
	}
	parts := make([]string, 0, len(meta))
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		parts = append(parts, fmt.Sprintf("%s=%s", k, meta[k]))
	}
	return strings.Join(parts, ", ")
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

// LoadedProgramsView is the output view for commands that load programs.
type LoadedProgramsView struct {
	// Programs are the programs loaded by the command, one row per program.
	Programs []bpfman.Program
}

// RenderLoadedPrograms writes the result of a load command.
func RenderLoadedPrograms(w io.Writer, view LoadedProgramsView, format OutputFormat) error {
	programs := view.Programs
	if programs == nil {
		programs = []bpfman.Program{}
	}
	return renderOutput(w, format, bpfman.LoadResult{Programs: programs}, func(w io.Writer) error {
		return writeOutput(w, formatLoadedProgramsTable(view))
	})
}

func formatLoadedProgramsTable(view LoadedProgramsView) string {
	// Sort programs by program ID for consistent, scannable output
	programs := view.Programs
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

// ProgramListView is the output view for program list commands.
type ProgramListView struct {
	// Result is the program list to render, including both bpfman-managed and kernel-only programs.
	Result bpfman.ProgramListResult
}

// RenderProgramList writes a program list result.
func RenderProgramList(w io.Writer, view ProgramListView, format OutputFormat) error {
	result := view.Result
	if result.Programs == nil {
		result.Programs = []bpfman.ProgramListEntry{}
	}
	return renderOutput(w, format, result, func(w io.Writer) error {
		return writeOutput(w, formatProgramsCompositeTable(result))
	})
}

// numListLinks bounds how many link IDs the LINKS column lists before
// truncating with ", ...".
const numListLinks = 3

// formatProgramsCompositeTable renders the default program-list table.
// The columns -- Program ID, Application, Type, Function Name, Links --
// let the listing answer "which application?" and "is it attached?"
// without a second command. The per-entry fields are precomputed by
// the manager, so kernel-only rows render with their kernel type and
// name and an empty application and links cell.
func formatProgramsCompositeTable(result bpfman.ProgramListResult) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "PROGRAM ID\tAPPLICATION\tTYPE\tFUNCTION NAME\tLINKS")

	for _, e := range result.Programs {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			e.ProgramID,
			e.Application,
			e.Type,
			e.FunctionName,
			programLinksColumn(e.Links),
		)
	}

	w.Flush()
	return b.String()
}

// programLinksColumn renders a program's links as a count followed by
// up to numListLinks IDs, with ", ..." when more exist and an empty
// cell when there are none.
func programLinksColumn(links []bpfman.LinkID) string {
	count := len(links)
	if count == 0 {
		return ""
	}
	shown := min(count, numListLinks)
	ids := make([]string, shown)
	for i := range shown {
		ids[i] = fmt.Sprintf("%d", links[i])
	}
	list := strings.Join(ids, ", ")
	if count > numListLinks {
		list += ", ..."
	}
	return fmt.Sprintf("(%d) %s", count, list)
}

// DispatcherListView is the output view for dispatcher list commands.
type DispatcherListView struct {
	// Summaries are the dispatcher summaries to render, one row per dispatcher.
	Summaries []platform.DispatcherSummary
}

// RenderDispatcherList writes a dispatcher list result.
func RenderDispatcherList(w io.Writer, view DispatcherListView, format OutputFormat) error {
	summaries := view.Summaries
	if summaries == nil {
		summaries = []platform.DispatcherSummary{}
	}
	return renderOutput(w, format, platform.DispatcherListResult{Dispatchers: summaries}, func(w io.Writer) error {
		return writeOutput(w, formatDispatcherListTable(view))
	})
}

func formatDispatcherListTable(view DispatcherListView) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "TYPE\tNSID\tIFINDEX\tREVISION\tPROGRAM_ID\tKERNEL_LINK_ID\tPRIORITY\tHANDLE\tMEMBERS\tNETNS")

	for _, s := range view.Summaries {
		linkID := "-"
		if s.Runtime.KernelLinkID != nil {
			linkID = fmt.Sprintf("%d", *s.Runtime.KernelLinkID)
		}
		priority := "-"
		if s.Runtime.FilterPriority != nil {
			priority = fmt.Sprintf("%d", *s.Runtime.FilterPriority)
		}
		handle := "-"
		if s.Runtime.FilterHandle != nil {
			handle = fmt.Sprintf("%#x", *s.Runtime.FilterHandle)
		}
		netns := s.Runtime.NetnsPath
		if netns == "" {
			netns = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%d\t%s\n",
			s.Key.Type, s.Key.Nsid, s.Key.Ifindex,
			s.Revision, s.Runtime.ProgramID,
			linkID, priority, handle, s.MemberCount, netns)
	}

	w.Flush()
	return b.String()
}

// RenderDispatcherSnapshot writes a single dispatcher snapshot.
func RenderDispatcherSnapshot(w io.Writer, snap platform.DispatcherSnapshot, format OutputFormat) error {
	return renderOutput(w, format, snap, func(w io.Writer) error {
		return writeOutput(w, formatDispatcherSnapshotTable(snap))
	})
}

func formatDispatcherSnapshotTable(snap platform.DispatcherSnapshot) string {
	var b strings.Builder

	// Header section
	fmt.Fprintf(&b, "Dispatcher: %s nsid=%d ifindex=%d\n", snap.Key.Type, snap.Key.Nsid, snap.Key.Ifindex)
	fmt.Fprintf(&b, "  Revision:    %d\n", snap.Revision)
	fmt.Fprintf(&b, "  Program ID:  %d\n", snap.Runtime.ProgramID)
	if snap.Runtime.KernelLinkID != nil {
		fmt.Fprintf(&b, "  Kernel Link ID: %d\n", *snap.Runtime.KernelLinkID)
	}
	if snap.Runtime.FilterPriority != nil {
		fmt.Fprintf(&b, "  Priority:    %d\n", *snap.Runtime.FilterPriority)
	}
	if snap.Runtime.FilterHandle != nil {
		fmt.Fprintf(&b, "  Filter Handle: %#x\n", *snap.Runtime.FilterHandle)
	}

	// Members table
	fmt.Fprintf(&b, "\nMembers (%d/%d):\n", len(snap.Members), dispatcher.MaxPrograms)

	if len(snap.Members) == 0 {
		b.WriteString("  (none)\n")
		return b.String()
	}

	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  POS\tPRIORITY\tPROGRAM_ID\tNAME\tLINK_ID\tKERNEL_LINK_ID\tPROCEED_ON")
	for _, m := range snap.Members {
		proceedOn := formatProceedOnMask(m.ProceedOn, snap.Key.Type)
		kernelLinkID := "<none>"
		if m.KernelLinkID != nil {
			kernelLinkID = fmt.Sprintf("%d", *m.KernelLinkID)
		}
		fmt.Fprintf(w, "  %d\t%d\t%d\t%s\t%d\t%s\t%s\n",
			m.Position, m.Priority, m.ProgramID,
			m.ProgramName, m.LinkID, kernelLinkID, proceedOn)
	}
	w.Flush()

	return b.String()
}

// formatProceedOnMask decodes a dispatcher ABI proceed-on bitmask into
// named actions.
func formatProceedOnMask(mask uint32, dispType dispatcher.DispatcherType) string {
	if mask == 0 {
		return "none"
	}

	actions, err := dispatcher.ProceedOnActions(dispType, mask)
	if err != nil {
		return fmt.Sprintf("invalid(%v)", err)
	}
	if dispType == dispatcher.DispatcherTypeXDP {
		return formatXDPProceedOn(actions)
	}
	return bpfman.TCActionsToString(actions)
}
