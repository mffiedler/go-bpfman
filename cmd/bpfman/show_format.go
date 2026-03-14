package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// formatShowSummary renders a compact overview of the program.
func formatShowSummary(d ProgramDetail) string {
	var b strings.Builder
	prog := d.Program

	// Header line
	name := prog.Record.Meta.Name
	progType := prog.Record.Load.ProgramType()
	fmt.Fprintf(&b, "Program %d: %s (%s)\n", prog.Record.ProgramID, name, progType)

	// Source and timestamps
	fmt.Fprintf(&b, "  Source:     %s\n", prog.Record.Load.ObjectPath())
	if prog.Status.Kernel != nil && !prog.Status.Kernel.LoadedAt.IsZero() {
		fmt.Fprintf(&b, "  Loaded:     %s\n", prog.Status.Kernel.LoadedAt.Format(time.RFC3339))
	}

	// Kernel details
	if kp := prog.Status.Kernel; kp != nil {
		if kp.Tag != "" {
			fmt.Fprintf(&b, "  Tag:        %s\n", kp.Tag)
		}
		var sizes []string
		if kp.JitedSize != 0 {
			sizes = append(sizes, fmt.Sprintf("JIT: %dB", kp.JitedSize))
		}
		if kp.XlatedSize != 0 {
			sizes = append(sizes, fmt.Sprintf("Xlated: %dB", kp.XlatedSize))
		}
		if kp.Memlock != 0 {
			sizes = append(sizes, fmt.Sprintf("Memlock: %dB", kp.Memlock))
		}
		if len(sizes) > 0 {
			fmt.Fprintf(&b, "  Size:       %s\n", strings.Join(sizes, "  "))
		}
	}

	// Presence
	storeYN := "yes"
	kernelYN := presenceYN(prog.Status.Kernel != nil)
	fsYN := presenceYN(d.ProgPin.Present)
	fmt.Fprintf(&b, "  Presence:   store=%s  kernel=%s  fs=%s\n", storeYN, kernelYN, fsYN)

	// Summary counts
	b.WriteString("\n")
	if len(d.Maps) > 0 {
		names := make([]string, len(d.Maps))
		for i, m := range d.Maps {
			names[i] = mapDisplayName(m)
		}
		fmt.Fprintf(&b, "  Maps (%d):   %s\n", len(d.Maps), strings.Join(names, ", "))
	} else {
		b.WriteString("  Maps:       none\n")
	}

	if len(d.Links) > 0 {
		var parts []string
		for _, l := range d.Links {
			s := fmt.Sprintf("%d", l.Record.ID)
			if l.Record.Details != nil {
				s += " (" + formatAttachDetails(l.Record.Details) + ")"
			}
			parts = append(parts, s)
		}
		fmt.Fprintf(&b, "  Links (%d):  %s\n", len(d.Links), strings.Join(parts, ", "))
	} else {
		b.WriteString("  Links:      none\n")
	}

	return b.String()
}

// formatShowLinks renders a tabwriter table of link details.
func formatShowLinks(d ProgramDetail) string {
	if len(d.Links) == 0 {
		return "No links.\n"
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "ID\tKIND\tATTACH\tPIN\tPRESENT")

	for _, l := range d.Links {
		attach := ""
		if l.Record.Details != nil {
			attach = formatAttachDetails(l.Record.Details)
		}
		pin := l.PinPath
		if pin == "" {
			pin = "(none)"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			l.Record.ID, l.Record.Kind, attach, pin, presenceYN(l.Present))
	}

	w.Flush()
	return b.String()
}

// formatShowMaps renders a tabwriter table of map details.
func formatShowMaps(d ProgramDetail) string {
	if len(d.Maps) == 0 {
		return "No maps.\n"
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "ID\tNAME\tTYPE\tKEYS\tVALUES\tMAX\tPIN\tPRESENT")

	for _, m := range d.Maps {
		fmt.Fprintf(w, "%d\t%s\t%s\t%dB\t%dB\t%d\t%s\t%s\n",
			m.ID, mapDisplayName(m), m.MapType,
			m.KeySize, m.ValueSize, m.MaxEntries,
			m.PinPath, presenceYN(m.Present))
	}

	w.Flush()
	return b.String()
}

// formatShowPaths renders a two-column path inventory.
func formatShowPaths(d ProgramDetail) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	// Program pin
	fmt.Fprintf(w, "%s\t%s\n", d.ProgPin.Path, presenceStatus(d.ProgPin.Present))

	// Map directory
	if len(d.Maps) > 0 {
		fmt.Fprintf(w, "%s\t%s\n", d.MapDir.Path, presenceStatusCount(d.MapDir.Present, len(d.Maps), "pin"))
	} else {
		fmt.Fprintf(w, "%s\t%s\n", d.MapDir.Path, presenceStatus(d.MapDir.Present))
	}

	// Individual map pins
	for _, m := range d.Maps {
		fmt.Fprintf(w, "%s\t%s\n", m.PinPath, presenceStatus(m.Present))
	}

	// Link directory
	if len(d.Links) > 0 {
		fmt.Fprintf(w, "%s\t%s\n", d.LinkDir.Path, presenceStatusCount(d.LinkDir.Present, len(d.Links), "pin"))
	} else {
		fmt.Fprintf(w, "%s\t%s\n", d.LinkDir.Path, presenceStatus(d.LinkDir.Present))
	}

	// Individual link pins
	for _, l := range d.Links {
		if l.PinPath != "" {
			fmt.Fprintf(w, "%s\t%s\n", l.PinPath, presenceStatus(l.Present))
		}
	}

	// Bytecode directory
	fmt.Fprintf(w, "%s\t%s\n", d.Bytecode.Path, presenceStatus(d.Bytecode.Present))

	w.Flush()
	return b.String()
}

// formatShowJSON serialises the full ProgramDetail as indented JSON.
func formatShowJSON(d ProgramDetail) (string, error) {
	output, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal program detail: %w", err)
	}
	return string(output) + "\n", nil
}

func presenceYN(present bool) string {
	if present {
		return "yes"
	}
	return "no"
}

func presenceStatus(present bool) string {
	if present {
		return "present"
	}
	return "absent"
}

// mapDisplayName returns the filesystem pin name when available,
// falling back to the kernel name. The kernel truncates map names to
// 15 characters; the pin path preserves the full ELF section name.
func mapDisplayName(m MapDetail) string {
	if m.PinPath != "" {
		return filepath.Base(m.PinPath)
	}
	return m.Name
}

func presenceStatusCount(present bool, count int, unit string) string {
	if !present {
		return "absent"
	}
	suffix := "s"
	if count == 1 {
		suffix = ""
	}
	return fmt.Sprintf("present (%d %s%s)", count, unit, suffix)
}
