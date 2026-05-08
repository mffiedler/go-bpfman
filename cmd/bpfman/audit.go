package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/coherency"
)

// AuditCmd checks coherency of database, kernel, and filesystem
// state. The default invocation is read-only; --repair must be
// passed explicitly to mutate state.
type AuditCmd struct {
	Checkup AuditCheckupCmd `cmd:"" default:"withargs" help:"Run coherency checks (default)."`
	Explain AuditExplainCmd `cmd:"" help:"Explain a coherency rule."`
}

// Help returns extended help for the audit command.
func (c AuditCmd) Help() string {
	return `
audit is read-only by default and prints findings plus the cleanup
plan that would be executed under --repair. Pass --repair to
actually mutate state; combine with --prune to also remove live
orphans (programs pinned in bpffs that the kernel still holds but
bpfman no longer tracks).

In Kubernetes/OpenShift, the daemon container sets
BPFMAN_MODE=bpfman-rpc which restricts the CLI to serve-only mode.
Unset it to run audit:

  oc exec $(oc get pod -n bpfman -l name=bpfman-daemon -o name) -n bpfman -c bpfman -- env -u BPFMAN_MODE /bpfman audit

For a specific node (replace $NODE with the node name):

  oc exec $(oc get pod -n bpfman -l name=bpfman-daemon --field-selector spec.nodeName=$NODE -o name) -n bpfman -c bpfman -- env -u BPFMAN_MODE /bpfman audit

Use 'bpfman audit explain' to list all coherency rules.
`
}

// AuditCheckupCmd runs the coherency checks and, with --repair,
// executes the resulting cleanup plan.
type AuditCheckupCmd struct {
	Repair bool     `help:"Execute the cleanup plan. Without this flag, audit is read-only."`
	Prune  bool     `help:"Also remove live orphans (programs pinned in bpffs but not tracked in DB). Requires --repair to mutate."`
	Rules  []string `arg:"" optional:"" help:"Limit audit to the named rules. Omit to evaluate all rules."`
}

// Run dispatches to the read-only or --repair path. Rule-name
// validation runs first regardless of mode so typos surface before
// any state-gathering work.
func (c *AuditCheckupCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	if len(c.Rules) > 0 {
		known := make(map[string]bool)
		for _, r := range coherency.Rules() {
			known[r.Name] = true
		}
		known[coherency.PruneRuleName] = true
		for _, name := range c.Rules {
			if !known[name] {
				return fmt.Errorf("unknown rule: %s\n\nUse 'bpfman audit explain' to list all rules", name)
			}
		}
	}

	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	if c.Repair {
		return c.runRepair(cli, ctx, mgr)
	}
	return c.runReadOnly(cli, ctx, mgr)
}

// runReadOnly produces the audit report plus the cleanup plan that
// would be executed under --repair, without taking any action.
func (c *AuditCheckupCmd) runReadOnly(cli *bpfmancli.CLI, ctx context.Context, mgr *manager.Manager) error {
	plan, err := bpfmancli.RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (manager.GCPlan, error) {
		p, pErr := mgr.ComputeGC(ctx, writeLock, manager.GCOptions{Rules: c.Rules, Prune: c.Prune})
		if pErr != nil {
			return p, fmt.Errorf("audit compute failed: %w", pErr)
		}
		return p, nil
	})
	if err != nil {
		return err
	}
	return cli.PrintOut(renderAuditPlan(plan, repairFooterCLI))
}

// runRepair acquires the writer lock and executes the planned
// cleanup, reporting the result counts.
func (c *AuditCheckupCmd) runRepair(cli *bpfmancli.CLI, ctx context.Context, mgr *manager.Manager) error {
	result, err := bpfmancli.RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (manager.GCResult, error) {
		gcResult, gcErr := mgr.GCWithOptions(ctx, writeLock, manager.GCOptions{Rules: c.Rules, Prune: c.Prune})
		if gcErr != nil {
			return gcResult, fmt.Errorf("audit --repair failed: %w", gcErr)
		}
		return gcResult, nil
	})
	if err != nil {
		return err
	}

	cleaned := result.ProgramsRemoved + result.DispatchersRemoved + result.LinksRemoved + result.OrphanPinsRemoved

	if cleaned == 0 && result.LiveOrphans == 0 {
		return cli.PrintOut("Nothing to clean up.\n")
	}
	if cleaned == 0 && result.LiveOrphans > 0 {
		return cli.PrintOutf("Nothing to clean up. %d live orphan(s) skipped (pass --prune to also remove these).\n",
			result.LiveOrphans)
	}
	if result.LiveOrphans > 0 {
		return cli.PrintOutf("Repair complete: %d programs, %d dispatchers, %d links, %d orphan pins removed. %d live orphan(s) skipped.\n",
			result.ProgramsRemoved, result.DispatchersRemoved, result.LinksRemoved, result.OrphanPinsRemoved, result.LiveOrphans)
	}
	return cli.PrintOutf("Repair complete: %d programs, %d dispatchers, %d links, %d orphan pins removed.\n",
		result.ProgramsRemoved, result.DispatchersRemoved, result.LinksRemoved, result.OrphanPinsRemoved)
}

// repairFooter selects the closing prompt that points the user at
// the next step after a read-only audit.
type repairFooter int

const (
	// repairFooterCLI suggests re-running with --repair.
	repairFooterCLI repairFooter = iota
	// repairFooterREPL suggests shelling out from the REPL.
	repairFooterREPL
)

// renderAuditPlan formats a GCPlan as the read-only audit output:
// findings grouped by category and rule, plus the cleanup plan that
// --repair would execute. The footer text varies by context.
func renderAuditPlan(plan manager.GCPlan, footer repairFooter) string {
	if len(plan.Violations) == 0 && len(plan.StoreActions) == 0 && plan.LiveOrphans == 0 {
		return "All checks passed. Database, kernel, and filesystem are coherent.\n"
	}

	var out strings.Builder
	var errorCount, warningCount, repairableCount int

	if len(plan.Violations) > 0 {
		ruleCounts := make(map[string]int)
		for _, v := range plan.Violations {
			ruleCounts[v.RuleName]++
		}
		lastCategory := ""
		lastRule := ""
		w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)

		for _, v := range plan.Violations {
			category := categoryHeading(v.Category)
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
			if v.RuleName != lastRule {
				w.Flush()
				fmt.Fprintf(&out, "  [%s] (%d)\n", v.RuleName, ruleCounts[v.RuleName])
				lastRule = v.RuleName
				w = tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
			}
			fmt.Fprintf(w, "    %s\t%s\n", v.Severity, v.Description)
			if v.Intent != nil {
				fmt.Fprintf(w, "    \t  -> %s\n", v.Intent.Describe())
				repairableCount++
			}
			switch v.Severity {
			case coherency.SeverityError:
				errorCount++
			case coherency.SeverityWarning:
				warningCount++
			}
		}
		w.Flush()
	}

	if len(plan.StoreActions) > 0 {
		out.WriteString("\nStore-level cleanup:\n")
		for _, a := range plan.StoreActions {
			fmt.Fprintf(&out, "  %s\n", action.Describe(a))
		}
	}

	fmt.Fprintf(&out, "\nSummary: %d error(s), %d warning(s)", errorCount, warningCount)
	if plan.LiveOrphans > 0 {
		fmt.Fprintf(&out, "; %d live orphan(s) skipped", plan.LiveOrphans)
	}
	out.WriteString("\n")

	if repairableCount > 0 || len(plan.StoreActions) > 0 {
		switch footer {
		case repairFooterCLI:
			out.WriteString("\nRe-run with --repair to execute the cleanup plan.\n")
		case repairFooterREPL:
			out.WriteString("\nRun '! bpfman audit --repair' to execute the cleanup plan.\n")
		}
	}

	return out.String()
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

// AuditExplainCmd explains a coherency rule.
type AuditExplainCmd struct {
	Rule string `arg:"" optional:"" help:"Rule name to explain. Omit to list all rules."`
}

// Run executes the audit explain command.
func (c *AuditExplainCmd) Run(cli *bpfmancli.CLI) error {
	if c.Rule == "" {
		return c.listRules(cli)
	}
	return c.explainRule(cli, c.Rule)
}

func (c *AuditExplainCmd) listRules(cli *bpfmancli.CLI) error {
	var out strings.Builder
	out.WriteString("Available coherency rules:\n\n")

	names := coherency.RuleNames()
	sort.Strings(names)

	for _, name := range names {
		out.WriteString("  ")
		out.WriteString(name)
		out.WriteString("\n")
	}

	out.WriteString("\nUse 'bpfman audit explain <rule>' for details.\n")
	return cli.PrintOut(out.String())
}

func (c *AuditExplainCmd) explainRule(cli *bpfmancli.CLI, name string) error {
	rule := coherency.FindRule(name)
	if rule == nil {
		return fmt.Errorf("unknown rule: %s\n\nUse 'bpfman audit explain' to list all rules", name)
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
