package cliformat

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/bpfman/bpfman"
)

// RenderShowLinks writes a table of link details.
func RenderShowLinks(out io.Writer, prog bpfman.Program) error {
	if len(prog.Status.Links) == 0 {
		return writeOutput(out, "No links.\n")
	}

	headers := []string{"ID", "KIND", "ATTACH", "PIN", "PRESENT"}
	rows := make([][]string, len(prog.Status.Links))
	for i, l := range prog.Status.Links {
		attach := ""
		if l.Record.Details != nil {
			attach = formatAttachDetails(l.Record.Details)
		}
		pin := "(none)"
		if l.Record.PinPath != nil {
			pin = l.Record.PinPath.String()
		}
		rows[i] = []string{
			fmt.Sprintf("%d", l.Record.ID),
			l.Record.Kind.String(),
			attach,
			pin,
			presenceYN(l.Status.PinPresent),
		}
	}
	return writeOutput(out, renderTable("", headers, rows))
}

// RenderShowMaps writes a table of map details.
func RenderShowMaps(out io.Writer, prog bpfman.Program) error {
	if len(prog.Status.Maps) == 0 {
		return writeOutput(out, "No maps.\n")
	}

	headers := []string{"ID", "NAME", "TYPE", "KEYS", "VALUES", "MAX", "PIN", "PRESENT"}
	rows := make([][]string, len(prog.Status.Maps))
	for i, m := range prog.Status.Maps {
		rows[i] = []string{
			fmt.Sprintf("%d", m.ID),
			mapDisplayName(m),
			m.MapType.String(),
			fmt.Sprintf("%dB", m.KeySize),
			fmt.Sprintf("%dB", m.ValueSize),
			fmt.Sprintf("%d", m.MaxEntries),
			m.PinPath.String(),
			presenceYN(m.Present),
		}
	}
	return writeOutput(out, renderTable("", headers, rows))
}

// RenderShowPaths writes a two-column path inventory.
func RenderShowPaths(out io.Writer, prog bpfman.Program) error {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	// Program pin
	fmt.Fprintln(w, prog.Status.ProgPin)

	// Map directory
	if len(prog.Status.Maps) > 0 {
		fmt.Fprintf(w, "%s\t(%d pin)\n", prog.Status.MapDir, len(prog.Status.Maps))
	} else {
		fmt.Fprintln(w, prog.Status.MapDir)
	}

	// Individual map pins
	for _, m := range prog.Status.Maps {
		fmt.Fprintf(w, "%s\t%s\n", m.PinPath, presenceStatus(m.Present))
	}

	// Individual link pins
	for _, l := range prog.Status.Links {
		if l.Record.PinPath != nil {
			fmt.Fprintf(w, "%s\t%s\n", l.Record.PinPath.String(), presenceStatus(l.Status.PinPresent))
		}
	}

	// Bytecode file
	fmt.Fprintln(w, prog.Status.Bytecode)

	w.Flush()
	return writeOutput(out, b.String())
}

// RenderShowJSON writes the full Program as indented JSON.
func RenderShowJSON(out io.Writer, prog bpfman.Program) error {
	output, err := json.MarshalIndent(prog, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal program: %w", err)
	}
	return writeOutput(out, string(output)+"\n")
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
		return filepath.Base(m.PinPath.String())
	}
	return m.Name
}
