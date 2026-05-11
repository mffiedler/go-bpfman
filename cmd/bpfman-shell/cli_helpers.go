// CLI helpers duplicated from cmd/bpfman: small operational helpers
// that the REPL Command ADT shares with bpfman's Kong subcommands.
// The duplication keeps the two binaries independent at the package
// boundary; if these grow further or drift, lift them into a shared
// internal package.
package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
)

// loadFileResult captures the result of a load file operation.
type loadFileResult struct {
	Programs []bpfman.Program
}

// collectDeleteIDs resolves the set of program IDs to delete. When
// all is true, every managed program ID is returned via ListPrograms.
// Otherwise the explicit IDs are extracted.
func collectDeleteIDs(ctx context.Context, mgr *manager.Manager, all bool, explicit []bpfmancli.ProgramID) ([]kernel.ProgramID, error) {
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
func executeDeletePrograms(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, ids []kernel.ProgramID, recursive bool) error {
	type result struct {
		id  kernel.ProgramID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := bpfmancli.RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
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
