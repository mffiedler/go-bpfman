package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/frobware/go-bpfman"
)

// formatShowLinks renders a tabwriter table of link details.
func formatShowLinks(prog bpfman.Program) string {
	if len(prog.Status.Links) == 0 {
		return "No links.\n"
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "ID\tKIND\tATTACH\tPIN\tPRESENT")

	for _, l := range prog.Status.Links {
		attach := ""
		if l.Record.Details != nil {
			attach = formatAttachDetails(l.Record.Details)
		}
		var pin string
		if l.Record.PinPath != nil {
			pin = l.Record.PinPath.String()
		} else {
			pin = "(none)"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			l.Record.ID, l.Record.Kind, attach, pin, presenceYN(l.Status.PinPresent))
	}

	w.Flush()
	return b.String()
}

// formatShowMaps renders a tabwriter table of map details.
func formatShowMaps(prog bpfman.Program) string {
	if len(prog.Status.Maps) == 0 {
		return "No maps.\n"
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "ID\tNAME\tTYPE\tKEYS\tVALUES\tMAX\tPIN\tPRESENT")

	for _, m := range prog.Status.Maps {
		fmt.Fprintf(w, "%d\t%s\t%s\t%dB\t%dB\t%d\t%s\t%s\n",
			m.ID, mapDisplayName(m), m.MapType,
			m.KeySize, m.ValueSize, m.MaxEntries,
			m.PinPath, presenceYN(m.Present))
	}

	w.Flush()
	return b.String()
}

// formatShowPaths renders a two-column path inventory.
func formatShowPaths(prog bpfman.Program) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	// Program pin
	fmt.Fprintf(w, "%s\t%s\n", prog.Status.ProgPin.Path, presenceStatus(prog.Status.ProgPin.Present))

	// Map directory
	if len(prog.Status.Maps) > 0 {
		fmt.Fprintf(w, "%s\t%s\n", prog.Status.MapDir.Path, presenceStatusCount(prog.Status.MapDir.Present, len(prog.Status.Maps), "pin"))
	} else {
		fmt.Fprintf(w, "%s\t%s\n", prog.Status.MapDir.Path, presenceStatus(prog.Status.MapDir.Present))
	}

	// Individual map pins
	for _, m := range prog.Status.Maps {
		fmt.Fprintf(w, "%s\t%s\n", m.PinPath, presenceStatus(m.Present))
	}

	// Link directory
	if len(prog.Status.Links) > 0 {
		fmt.Fprintf(w, "%s\t%s\n", prog.Status.LinkDir.Path, presenceStatusCount(prog.Status.LinkDir.Present, len(prog.Status.Links), "pin"))
	} else {
		fmt.Fprintf(w, "%s\t%s\n", prog.Status.LinkDir.Path, presenceStatus(prog.Status.LinkDir.Present))
	}

	// Individual link pins
	for _, l := range prog.Status.Links {
		if l.Record.PinPath != nil {
			fmt.Fprintf(w, "%s\t%s\n", l.Record.PinPath.String(), presenceStatus(l.Status.PinPresent))
		}
	}

	// Bytecode file
	fmt.Fprintf(w, "%s\t%s\n", prog.Status.Bytecode.Path, presenceStatus(prog.Status.Bytecode.Present))

	// Provenance file
	fmt.Fprintf(w, "%s\t%s\n", prog.Status.Provenance.Path, presenceStatus(prog.Status.Provenance.Present))

	w.Flush()
	return b.String()
}

// formatShowJSON serialises the full Program as indented JSON.
func formatShowJSON(prog bpfman.Program) (string, error) {
	output, err := json.MarshalIndent(prog, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal program: %w", err)
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
		return ""
	}
	return "missing"
}

// presenceSuffix returns a parenthesised annotation for inline use:
// empty when present, " (missing)" when absent.
func presenceSuffix(present bool) string {
	if present {
		return ""
	}
	return " (missing)"
}

// mapDisplayName returns the filesystem pin name when available,
// falling back to the kernel name. The kernel truncates map names to
// 15 characters; the pin path preserves the full ELF section name.
func mapDisplayName(m bpfman.MapStatus) string {
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
