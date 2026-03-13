package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/coherency"
)

// GCCmd garbage collects stale database entries.
type GCCmd struct {
	DryRun bool     `help:"Show what would be cleaned up without executing."`
	Prune  bool     `help:"Also remove live orphans (programs pinned in bpffs but not tracked in DB)."`
	Rules  []string `arg:"" optional:"" help:"GC rule(s) to run. Omit to run all rules."`
}

// Help returns extended help for the gc command.
func (c GCCmd) Help() string {
	return `
In Kubernetes/OpenShift, the daemon container sets
BPFMAN_MODE=bpfman-rpc which restricts the CLI to serve-only mode.
Unset it to run gc:

  oc exec $(oc get pod -n bpfman -l name=bpfman-daemon -o name) -n bpfman -c bpfman -- env -u BPFMAN_MODE /bpfman gc

Run specific GC rules:

  oc exec $(oc get pod -n bpfman -l name=bpfman-daemon -o name) -n bpfman -c bpfman -- env -u BPFMAN_MODE /bpfman gc orphan-program-artefacts

Available GC rules: stale-dispatcher, orphan-program-artefacts,
orphan-dispatcher-artefacts

Use 'bpfman doctor explain <rule>' for rule details.
`
}

// Run executes the gc command: mutation under lock, output outside.
func (c *GCCmd) Run(cli *CLI, ctx context.Context) error {
	// Validate rule names if provided.
	if len(c.Rules) > 0 {
		gcRuleNames := make(map[string]bool)
		for _, r := range coherency.GCRules() {
			gcRuleNames[r.Name] = true
		}
		for _, name := range c.Rules {
			if !gcRuleNames[name] {
				return fmt.Errorf("unknown GC rule: %s\n\nAvailable GC rules:\n%s",
					name, formatGCRuleNames())
			}
		}
	}

	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	gcOpts := manager.GCOptions{
		Rules: c.Rules,
		Prune: c.Prune,
	}

	if c.DryRun {
		return c.runDryRun(cli, ctx, mgr, gcOpts)
	}
	return c.runExecute(cli, ctx, mgr, gcOpts)
}

// runDryRun computes the GC plan under lock and displays what would
// be executed without performing any mutations.
func (c *GCCmd) runDryRun(cli *CLI, ctx context.Context, mgr *manager.Manager, opts manager.GCOptions) error {
	plan, err := RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (manager.GCPlan, error) {
		p, pErr := mgr.ComputeGC(ctx, writeLock, opts)
		if pErr != nil {
			return p, fmt.Errorf("gc compute failed: %w", pErr)
		}
		return p, nil
	})
	if err != nil {
		return err
	}

	if len(plan.StoreActions) == 0 && len(plan.Violations) == 0 {
		if plan.LiveOrphans > 0 {
			return cli.PrintOutf("Nothing to clean up. %d live orphan(s) skipped (run 'bpfman doctor' for details).\n",
				plan.LiveOrphans)
		}
		return cli.PrintOut("Nothing to clean up.\n")
	}

	var b strings.Builder

	if len(plan.StoreActions) > 0 {
		fmt.Fprintln(&b, "Store-level GC:")
		for _, a := range plan.StoreActions {
			fmt.Fprintf(&b, "  %s\n", action.Describe(a))
		}
	}

	if len(plan.Violations) > 0 {
		if b.Len() > 0 {
			fmt.Fprintln(&b)
		}
		fmt.Fprintln(&b, "Coherency GC:")
		for _, v := range plan.Violations {
			fmt.Fprintf(&b, "  [%s] %s\n", v.RuleName, v.Description)
			for _, a := range v.Op.Actions {
				fmt.Fprintf(&b, "    %s\n", action.Describe(a))
			}
		}
	}

	fmt.Fprintf(&b, "\n%d store action(s), %d coherency operation(s) would be executed.",
		len(plan.StoreActions), len(plan.Violations))
	if plan.LiveOrphans > 0 {
		fmt.Fprintf(&b, " %d live orphan(s) skipped.", plan.LiveOrphans)
	}
	fmt.Fprintln(&b)

	return cli.PrintOut(b.String())
}

// runExecute performs the full GC under lock and reports results.
func (c *GCCmd) runExecute(cli *CLI, ctx context.Context, mgr *manager.Manager, opts manager.GCOptions) error {
	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (manager.GCResult, error) {
		gcResult, gcErr := mgr.GCWithOptions(ctx, writeLock, opts)
		if gcErr != nil {
			return gcResult, fmt.Errorf("gc failed: %w", gcErr)
		}
		return gcResult, nil
	})
	if err != nil {
		return err
	}

	// Output outside lock.
	cleaned := result.ProgramsRemoved + result.DispatchersRemoved + result.LinksRemoved + result.OrphanPinsRemoved

	if cleaned == 0 && result.LiveOrphans == 0 {
		return cli.PrintOut("Nothing to clean up.\n")
	}

	if cleaned == 0 && result.LiveOrphans > 0 {
		return cli.PrintOutf("Nothing to clean up. %d live orphan(s) skipped (run 'bpfman doctor' for details).\n",
			result.LiveOrphans)
	}

	if result.LiveOrphans > 0 {
		return cli.PrintOutf("GC complete: %d programs, %d dispatchers, %d links, %d orphan pins removed. %d live orphan(s) skipped.\n",
			result.ProgramsRemoved, result.DispatchersRemoved, result.LinksRemoved, result.OrphanPinsRemoved, result.LiveOrphans)
	}

	return cli.PrintOutf("GC complete: %d programs, %d dispatchers, %d links, %d orphan pins removed.\n",
		result.ProgramsRemoved, result.DispatchersRemoved, result.LinksRemoved, result.OrphanPinsRemoved)
}

func formatGCRuleNames() string {
	var out string
	for _, r := range coherency.GCRules() {
		out += "  " + r.Name + "\n"
	}
	return out
}
