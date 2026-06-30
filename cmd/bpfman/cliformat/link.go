package cliformat

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bpfman/bpfman"
)

// LinkAttachView is the output view for link attach commands.
//
// Table output renders the created link details. Structured output
// continues to expose the created link object for machine consumers.
type LinkAttachView struct {
	// Link is the attachment created by the command, rendered as table or JSON.
	Link bpfman.Link
}

// LinkGetView is the output view for get-link commands. ProgramName is
// a presentation-only join resolved by the caller from Link.Record.ProgramID.
type LinkGetView struct {
	// Link is the attachment being displayed.
	Link bpfman.Link

	// ProgramName is the presentation-only program name, resolved by the caller from Link.Record.ProgramID and shown only in table output.
	ProgramName string
}

// LinkListView is the output view for link list commands.
type LinkListView struct {
	// Links are the attachment records to display, one row per link.
	Links []bpfman.LinkRecord
}

// RenderLinkAttach writes the result of a link attach command.
func RenderLinkAttach(w io.Writer, view LinkAttachView, format OutputFormat) error {
	return renderOutput(w, format, view.Link, func(w io.Writer) error {
		return writeOutput(w, formatLinkTable(LinkGetView{Link: view.Link}))
	})
}

// RenderLinkGet writes the result of a get-link command.
func RenderLinkGet(w io.Writer, view LinkGetView, format OutputFormat) error {
	return renderOutput(w, format, view.Link, func(w io.Writer) error {
		return writeOutput(w, formatLinkTable(view))
	})
}

// RenderLinkList writes the result of a link list command.
func RenderLinkList(w io.Writer, view LinkListView, format OutputFormat) error {
	links := view.Links
	if links == nil {
		links = []bpfman.LinkRecord{}
	}
	return renderOutput(w, format, bpfman.LinkListResult{Links: links}, func(w io.Writer) error {
		return renderLinkListTable(w, view)
	})
}

func renderLinkListTable(w io.Writer, view LinkListView) error {
	return DefaultLinkColumns().RenderLinkTable(w, view.Links)
}

func formatLinkTable(view LinkGetView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Link ID: %d\n", view.Link.Record.ID)
	renderRows(&b, linkDetailRows(view), 1)
	return b.String()
}

// linkDetailRows builds the Spec and Status sections of a link get view.
// Spec fields are ordered by label, not by their rendered line, so a label
// that is a prefix of another (Target vs Target Function vs Target Offset)
// sorts by the name rather than by the punctuation that follows it.
func linkDetailRows(view LinkGetView) []row {
	link := view.Link

	var spec []row
	add := func(label, value string) { spec = append(spec, fieldRow(label, value)) }

	if view.ProgramName != "" {
		add("BPF Function", view.ProgramName)
	}
	if !link.Record.CreatedAt.IsZero() {
		add("Created At", link.Record.CreatedAt.Format(time.RFC3339))
	}
	if link.Record.KernelLinkID != nil {
		add("Kernel Link ID", fmt.Sprintf("%d", *link.Record.KernelLinkID))
	} else {
		add("Kernel Link ID", "None")
	}
	add("Metadata", formatMetadata(link.Record.Metadata))
	if link.Record.PinPath != nil {
		add("Pin Path", link.Record.PinPath.String())
	} else {
		add("Pin Path", "None")
	}
	add("Program ID", fmt.Sprintf("%d", link.Record.ProgramID))
	add("Type", string(link.Record.Kind))

	// Type-specific fields from LinkDetails.
	switch d := link.Record.Details.(type) {
	case bpfman.FentryDetails:
		add("Target Function", d.FnName)
	case bpfman.FexitDetails:
		add("Target Function", d.FnName)
	case bpfman.KprobeDetails:
		if d.Retprobe {
			add("Attach Type", "kretprobe")
		} else {
			add("Attach Type", "kprobe")
		}
		add("Target Function", d.FnName)
		if d.Offset != 0 {
			add("Target Offset", fmt.Sprintf("%d", d.Offset))
		}
	case bpfman.TCDetails:
		add("Direction", string(d.Direction))
		add("Interface", d.Interface)
		add("Network Namespace", netnsOrNone(d.Netns))
		add("Position", fmt.Sprintf("%d", d.Position))
		add("Priority", fmt.Sprintf("%d", d.Priority))
		add("Proceed On", bpfman.TCActionsToString(d.ProceedOn))
	case bpfman.TCXDetails:
		add("Direction", string(d.Direction))
		add("Interface", d.Interface)
		add("Network Namespace", netnsOrNone(d.Netns))
		add("Position", fmt.Sprintf("%d", d.Position))
		add("Priority", fmt.Sprintf("%d", d.Priority))
	case bpfman.TracepointDetails:
		add("Tracepoint", d.Group+"/"+d.Name)
	case bpfman.UprobeDetails:
		if d.Retprobe {
			add("Attach Type", "uretprobe")
		} else {
			add("Attach Type", "uprobe")
		}
		if d.PID != 0 {
			add("PID", fmt.Sprintf("%d", d.PID))
		}
		add("Target", d.Target)
		add("Target Function", d.FnName)
		if d.Offset != 0 {
			add("Target Offset", fmt.Sprintf("%d", d.Offset))
		}
	case bpfman.XDPDetails:
		add("Interface", d.Interface)
		add("Network Namespace", netnsOrNone(d.Netns))
		add("Position", fmt.Sprintf("%d", d.Position))
		add("Priority", fmt.Sprintf("%d", d.Priority))
		add("Proceed On", formatXDPProceedOn(d.ProceedOn))
	}

	sortRowsByLabel(spec)

	status := []row{
		fieldRow("Kernel Seen", fmt.Sprintf("%t", link.Status.KernelSeen)),
		fieldRow("Pin Present", fmt.Sprintf("%t", link.Status.PinPresent)),
	}
	sortRowsByLabel(status)

	return []row{
		sectionRow("Spec", spec...),
		sectionRow("Status", status...),
	}
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
