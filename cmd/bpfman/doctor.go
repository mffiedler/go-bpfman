package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/frobware/go-bpfman/manager"
)

// DoctorCmd checks coherency of database, kernel, and filesystem state.
type DoctorCmd struct {
	Checkup DoctorCheckupCmd `cmd:"" default:"withargs" help:"Run coherency checks (default)."`
	Explain DoctorExplainCmd `cmd:"" help:"Explain a coherency rule."`
}

// Help returns extended help for the doctor command.
func (c DoctorCmd) Help() string {
	return `
In Kubernetes/OpenShift, the daemon container sets
BPFMAN_MODE=bpfman-rpc which restricts the CLI to serve-only mode.
Unset it to run doctor:

  oc exec $(oc get pod -n bpfman -l name=bpfman-daemon -o name) -n bpfman -c bpfman -- env -u BPFMAN_MODE /bpfman doctor

For a specific node (replace $NODE with the node name):

  oc exec $(oc get pod -n bpfman -l name=bpfman-daemon --field-selector spec.nodeName=$NODE -o name) -n bpfman -c bpfman -- env -u BPFMAN_MODE /bpfman doctor

Use 'bpfman doctor explain' to list all coherency rules.
`
}

// DoctorCheckupCmd runs the coherency checks.
type DoctorCheckupCmd struct{}

// Run executes the doctor check command.
func (c *DoctorCheckupCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	report, err := mgr.Doctor(ctx)
	if err != nil {
		return fmt.Errorf("doctor failed: %w", err)
	}

	if len(report.Findings) == 0 {
		return cli.PrintOut("All checks passed. Database, kernel, and filesystem are coherent.\n")
	}

	// Count findings per rule for display in headers.
	ruleCounts := make(map[string]int)
	for _, f := range report.Findings {
		ruleCounts[f.RuleName]++
	}

	// Build output in memory then write once.
	var out strings.Builder
	var errorCount, warningCount int
	lastCategory := ""
	lastRule := ""

	// Use tabwriter for aligned columns within each rule group.
	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)

	for _, f := range report.Findings {
		category := categoryHeading(f.Category)
		if category != lastCategory {
			// Flush previous findings before starting new category.
			w.Flush()
			if lastCategory != "" {
				out.WriteString("\n")
			}
			out.WriteString(category)
			out.WriteString("\n")
			lastCategory = category
			lastRule = ""
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		if f.RuleName != lastRule {
			// Flush previous rule's findings before showing new rule header.
			w.Flush()
			fmt.Fprintf(&out, "  [%s] (%d)\n", f.RuleName, ruleCounts[f.RuleName])
			lastRule = f.RuleName
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		fmt.Fprintf(w, "    %s\t%s\n", f.Severity, f.Description)
		switch f.Severity {
		case manager.SeverityError:
			errorCount++
		case manager.SeverityWarning:
			warningCount++
		}
	}
	w.Flush()

	fmt.Fprintf(&out, "\nSummary: %d error(s), %d warning(s)\n", errorCount, warningCount)

	return cli.PrintOut(out.String())
}

func categoryHeading(cat string) string {
	switch cat {
	case "enumeration":
		return "Checking enumeration quality..."
	case "db-vs-kernel":
		return "Checking database vs kernel..."
	case "db-vs-fs":
		return "Checking database vs filesystem..."
	case "fs-vs-db":
		return "Checking filesystem for orphans..."
	case "kernel-vs-db":
		return "Checking kernel vs database..."
	case "consistency":
		return "Checking derived state consistency..."
	case "gc-dispatcher":
		return "Stale dispatchers..."
	case "gc-orphan-pin":
		return "Orphan filesystem artefacts..."
	default:
		return cat
	}
}

// DoctorExplainCmd explains a coherency rule.
type DoctorExplainCmd struct {
	Rule string `arg:"" optional:"" help:"Rule name to explain. Omit to list all rules."`
}

// Run executes the doctor explain command.
func (c *DoctorExplainCmd) Run(cli *CLI) error {
	if c.Rule == "" {
		return c.listRules(cli)
	}
	return c.explainRule(cli, c.Rule)
}

func (c *DoctorExplainCmd) listRules(cli *CLI) error {
	var out strings.Builder
	out.WriteString("Available coherency rules:\n\n")

	names := manager.RuleNames()
	sort.Strings(names)

	for _, name := range names {
		out.WriteString("  ")
		out.WriteString(name)
		out.WriteString("\n")
	}

	out.WriteString("\nUse 'bpfman doctor explain <rule>' for details.\n")
	return cli.PrintOut(out.String())
}

func (c *DoctorExplainCmd) explainRule(cli *CLI, name string) error {
	rule := manager.FindRule(name)
	if rule == nil {
		return fmt.Errorf("unknown rule: %s\n\nUse 'bpfman doctor explain' to list all rules", name)
	}

	var out strings.Builder
	out.WriteString(rule.Name)
	out.WriteString("\n")
	out.WriteString(strings.Repeat("=", len(rule.Name)))
	out.WriteString("\n\n")

	if rule.Description != "" {
		out.WriteString(rule.Description)
	} else {
		out.WriteString("(No description available)")
	}
	out.WriteString("\n")

	return cli.PrintOut(out.String())
}
