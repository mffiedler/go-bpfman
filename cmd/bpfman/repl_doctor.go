// Doctor commands for the REPL: "doctor checkup" runs every
// coherency rule and renders the findings as a categorised
// report; "doctor explain [rule]" lists the rule catalogue or
// describes a single rule.  The category-heading helper lives
// alongside the rule machinery in doctor.go; the rendering and
// dispatch live here.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/coherency"
)

func replDoctorCheckup(ctx context.Context, cli *CLI, mgr *manager.Manager) error {
	report, err := mgr.Doctor(ctx)
	if err != nil {
		return fmt.Errorf("doctor failed: %w", err)
	}

	if len(report.Findings) == 0 {
		return cli.PrintOut("All checks passed. Database, kernel, and filesystem are coherent.\n")
	}

	ruleCounts := make(map[string]int)
	for _, f := range report.Findings {
		ruleCounts[f.RuleName]++
	}

	var out strings.Builder
	var errorCount, warningCount int
	lastCategory := ""
	lastRule := ""

	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)

	for _, f := range report.Findings {
		category := categoryHeading(f.Category)
		if category != lastCategory {
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
			w.Flush()
			fmt.Fprintf(&out, "  [%s] (%d)\n", f.RuleName, ruleCounts[f.RuleName])
			lastRule = f.RuleName
			w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		}
		fmt.Fprintf(w, "    %s\t%s\n", f.Severity, f.Description)
		switch f.Severity {
		case coherency.SeverityError:
			errorCount++
		case coherency.SeverityWarning:
			warningCount++
		}
	}
	w.Flush()

	fmt.Fprintf(&out, "\nSummary: %d error(s), %d warning(s)\n", errorCount, warningCount)
	return cli.PrintOut(out.String())
}

func replDoctorExplain(cli *CLI, args []string) error {
	if len(args) == 0 {
		// List all rules.
		var out strings.Builder
		out.WriteString("Available coherency rules:\n\n")
		names := coherency.RuleNames()
		sort.Strings(names)
		for _, name := range names {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString("\n")
		}
		out.WriteString("\nUse 'doctor explain <rule>' for details.\n")
		return cli.PrintOut(out.String())
	}

	ruleName := args[0]
	rule := coherency.FindRule(ruleName)
	if rule == nil {
		return fmt.Errorf("unknown rule: %s\n\nUse 'doctor explain' to list all rules", ruleName)
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
