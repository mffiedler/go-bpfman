package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/manager"
)

// GCCmd garbage collects stale database entries.
type GCCmd struct {
	Rules []string `arg:"" optional:"" help:"GC rule(s) to run. Omit to run all rules."`
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
		for _, r := range manager.GCRules() {
			gcRuleNames[r.Name] = true
		}
		for _, name := range c.Rules {
			if !gcRuleNames[name] {
				return fmt.Errorf("unknown GC rule: %s\n\nAvailable GC rules:\n%s",
					name, formatGCRuleNames())
			}
		}
	}

	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	// Mutation under lock
	result, err := RunWithLockValue(ctx, cli, func(ctx context.Context) (manager.GCResult, error) {
		gcResult, gcErr := runtime.Manager.GCWithRules(ctx, c.Rules)
		if gcErr != nil {
			return gcResult, fmt.Errorf("gc failed: %w", gcErr)
		}
		return gcResult, nil
	})
	if err != nil {
		if result.Outcome.Status != "" {
			return displayOutcomeError(cli, err, result.Outcome, &OutputFlags{Output: OutputValue{Value: "table"}})
		}
		return err
	}

	// Output outside lock
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
	for _, r := range manager.GCRules() {
		out += "  " + r.Name + "\n"
	}
	return out
}
