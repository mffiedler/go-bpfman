// Audit commands for the REPL: "audit" runs every coherency rule
// and renders findings plus the cleanup plan; "audit explain
// [rule]" lists the rule catalogue or describes a single rule. The
// REPL never executes --repair; use '! bpfman audit --repair' to
// shell out for the destructive path.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/coherency"
)

func replAuditCheckup(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager) error {
	plan, err := bpfmancli.RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (manager.GCPlan, error) {
		p, pErr := mgr.ComputeGC(ctx, writeLock, manager.GCOptions{})
		if pErr != nil {
			return p, fmt.Errorf("audit failed: %w", pErr)
		}
		return p, nil
	})
	if err != nil {
		return err
	}
	return cli.PrintOut(renderAuditPlan(plan, repairFooterREPL))
}

func replAuditExplain(cli *bpfmancli.CLI, args []string) error {
	if len(args) == 0 {
		var out strings.Builder
		out.WriteString("Available coherency rules:\n\n")
		names := coherency.RuleNames()
		sort.Strings(names)
		for _, name := range names {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString("\n")
		}
		out.WriteString("\nUse 'audit explain <rule>' for details.\n")
		return cli.PrintOut(out.String())
	}

	ruleName := args[0]
	rule := coherency.FindRule(ruleName)
	if rule == nil {
		return fmt.Errorf("unknown rule: %s\n\nUse 'audit explain' to list all rules", ruleName)
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
