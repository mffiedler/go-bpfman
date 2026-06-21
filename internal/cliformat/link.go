package cliformat

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/frobware/go-bpfman"
)

// LinkAttachView is the output view for link attach commands.
//
// Table output renders the created link details. Structured output
// continues to expose the created link object for machine consumers.
type LinkAttachView struct {
	Link bpfman.Link
}

// LinkGetView is the output view for get-link commands. ProgramName is
// a presentation-only join resolved by the caller from Link.Record.ProgramID.
type LinkGetView struct {
	Link        bpfman.Link
	ProgramName string
}

// LinkListView is the output view for link list commands.
type LinkListView struct {
	Links []bpfman.LinkRecord
}

// RenderLinkAttach writes the result of a link attach command.
func RenderLinkAttach(w io.Writer, view LinkAttachView, format OutputFormat) error {
	switch format {
	case OutputFormatJSON:
		output, err := json.MarshalIndent(view.Link, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal link: %w", err)
		}
		return writeOutput(w, string(output)+"\n")
	case OutputFormatText:
		return writeOutput(w, formatLinkTable(LinkGetView{Link: view.Link}))
	default:
		return unsupportedOutputFormat(format)
	}
}

// RenderLinkGet writes the result of a get-link command.
func RenderLinkGet(w io.Writer, view LinkGetView, format OutputFormat) error {
	switch format {
	case OutputFormatJSON:
		output, err := json.MarshalIndent(view.Link, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal link: %w", err)
		}
		return writeOutput(w, string(output)+"\n")
	case OutputFormatText:
		return writeOutput(w, formatLinkTable(view))
	default:
		return unsupportedOutputFormat(format)
	}
}

// RenderLinkList writes the result of a link list command.
func RenderLinkList(w io.Writer, view LinkListView, format OutputFormat) error {
	switch format {
	case OutputFormatJSON:
		output, err := formatLinkListJSON(view)
		if err != nil {
			return err
		}
		return writeOutput(w, output)
	case OutputFormatText:
		return renderLinkListTable(w, view)
	default:
		return unsupportedOutputFormat(format)
	}
}

func formatLinkListJSON(view LinkListView) (string, error) {
	links := view.Links
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

func renderLinkListTable(w io.Writer, view LinkListView) error {
	return DefaultLinkColumns().RenderLinkTable(w, view.Links)
}

func formatLinkTable(view LinkGetView) string {
	link := view.Link
	var b strings.Builder

	// Primary identifier at column one (like Program ID for programs)
	fmt.Fprintf(&b, "Link ID: %d\n", link.Record.ID)

	// Collect Spec fields from LinkSpec, then sort alphabetically
	var specFields []string

	if view.ProgramName != "" {
		specFields = append(specFields, fmt.Sprintf("    BPF Function:\t%s", view.ProgramName))
	}
	if !link.Record.CreatedAt.IsZero() {
		specFields = append(specFields, fmt.Sprintf("    Created At:\t%s", link.Record.CreatedAt.Format(time.RFC3339)))
	}
	if link.Record.KernelLinkID != nil {
		specFields = append(specFields, fmt.Sprintf("    Kernel Link ID:\t%d", *link.Record.KernelLinkID))
	} else {
		specFields = append(specFields, "    Kernel Link ID:\tNone")
	}
	specFields = append(specFields, fmt.Sprintf("    Metadata:\t%s", formatMetadata(link.Record.Metadata)))
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
		specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", netnsOrNone(d.Netns)))
		specFields = append(specFields, fmt.Sprintf("    Position:\t%d", d.Position))
		specFields = append(specFields, fmt.Sprintf("    Priority:\t%d", d.Priority))
		specFields = append(specFields, fmt.Sprintf("    Proceed On:\t%s", bpfman.TCActionsToString(d.ProceedOn)))
	case bpfman.TCXDetails:
		specFields = append(specFields, fmt.Sprintf("    Direction:\t%s", d.Direction))
		specFields = append(specFields, fmt.Sprintf("    Interface:\t%s", d.Interface))
		specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", netnsOrNone(d.Netns)))
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
		specFields = append(specFields, fmt.Sprintf("    Network Namespace:\t%s", netnsOrNone(d.Netns)))
		specFields = append(specFields, fmt.Sprintf("    Position:\t%d", d.Position))
		specFields = append(specFields, fmt.Sprintf("    Priority:\t%d", d.Priority))
		specFields = append(specFields, fmt.Sprintf("    Proceed On:\t%s", formatXDPProceedOn(d.ProceedOn)))
	}

	slices.Sort(specFields)

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

	slices.Sort(statusFields)

	var aligned strings.Builder
	w := tabwriter.NewWriter(&aligned, 0, 0, 1, ' ', 0)
	for _, f := range specFields {
		fmt.Fprintln(w, f)
	}
	for _, f := range statusFields {
		fmt.Fprintln(w, f)
	}
	w.Flush()

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

// netnsOrNone renders a link's network namespace path, falling back to
// "None" for host (empty) attaches.
func netnsOrNone(netns string) string {
	if netns == "" {
		return "None"
	}
	return netns
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
