// CLI helpers duplicated from cmd/bpfman: small operational helpers
// that the REPL Command ADT shares with bpfman's Kong subcommands. The
// duplication keeps the two binaries independent at the package
// boundary; if these grow further or drift, lift them into a shared
// internal package.
package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/coherency"
)

// loadFileResult captures the result of a load file operation.
type loadFileResult struct {
	Programs []bpfman.Program
}

// collectDeleteIDs resolves the set of program IDs to delete. When
// all is true, every managed program ID is returned via ListPrograms.
// Otherwise the explicit IDs are extracted.
func collectDeleteIDs(ctx context.Context, mgr *manager.Manager, all bool, explicit []ProgramID) ([]kernel.ProgramID, error) {
	if all {
		result, err := mgr.ListPrograms(ctx)
		if err != nil {
			return nil, fmt.Errorf("list programs: %w", err)
		}
		ids := make([]kernel.ProgramID, len(result.Programs))
		for i, prog := range result.Programs {
			ids[i] = prog.Record.ProgramID
		}
		return ids, nil
	}
	ids := make([]kernel.ProgramID, len(explicit))
	for i, pid := range explicit {
		ids[i] = pid.Value
	}
	return ids, nil
}

// executeDeletePrograms is the shared implementation for deleting
// programs with cascading cleanup. Locking is handled internally.
func executeDeletePrograms(ctx context.Context, cli *CLI, mgr *manager.Manager, ids []kernel.ProgramID, recursive bool) error {
	type result struct {
		id  kernel.ProgramID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		for _, id := range ids {
			err := deleteProgram(ctx, writeLock, mgr, id, recursive)
			results = append(results, result{id: id, err: err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("program %d: %v\n", r.id, r.err)
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d program(s) failed to delete", failCount, len(results))
	}

	return nil
}

// deleteLink detaches the link, then deletes the program if it has no
// remaining links.
func deleteLink(ctx context.Context, writeLock lock.WriterScope, mgr *manager.Manager, linkID kernel.LinkID, recursive bool) error {
	link, err := mgr.GetLink(ctx, linkID)
	if err != nil {
		return fmt.Errorf("get link: %w", err)
	}

	programID := link.ProgramID

	if err := mgr.Detach(ctx, writeLock, linkID); err != nil {
		return fmt.Errorf("detach: %w", err)
	}

	links, err := mgr.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links for program %d: %w", programID, err)
	}

	if len(links) == 0 {
		if err := deleteProgram(ctx, writeLock, mgr, programID, recursive); err != nil {
			return fmt.Errorf("delete orphaned program %d: %w", programID, err)
		}
	}

	return nil
}

// deleteProgram detaches all links for the program, then unloads it.
// With recursive, also deletes dependent programs (those sharing maps
// via map_owner_id) before unloading the target.
func deleteProgram(ctx context.Context, writeLock lock.WriterScope, mgr *manager.Manager, programID kernel.ProgramID, recursive bool) error {
	if recursive {
		if err := deleteDependents(ctx, writeLock, mgr, programID); err != nil {
			return err
		}
	}

	links, err := mgr.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}

	for _, link := range links {
		if err := mgr.Detach(ctx, writeLock, link.ID); err != nil {
			return fmt.Errorf("detach link %d: %w", link.ID, err)
		}
	}

	if err := mgr.Unload(ctx, writeLock, programID); err != nil {
		return fmt.Errorf("unload: %w", err)
	}

	return nil
}

// deleteDependents finds programs that share maps with the target
// (map_owner_id = programID) and deletes them first.
func deleteDependents(ctx context.Context, writeLock lock.WriterScope, mgr *manager.Manager, ownerID kernel.ProgramID) error {
	result, err := mgr.ListPrograms(ctx)
	if err != nil {
		return fmt.Errorf("list programs: %w", err)
	}

	for _, prog := range result.Programs {
		if prog.Record.Handles.MapOwnerID != nil && *prog.Record.Handles.MapOwnerID == ownerID {
			if err := deleteProgram(ctx, writeLock, mgr, prog.Record.ProgramID, true); err != nil {
				return fmt.Errorf("delete dependent program %d: %w", prog.Record.ProgramID, err)
			}
		}
	}

	return nil
}

// repairFooter selects the closing prompt that points the user at the
// next step after a read-only audit.
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
