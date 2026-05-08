package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
)

// ProgramDeleteCmd deletes BPF programs with cascading cleanup.
// For each program: detaches all links, then unloads the program.
// With --recursive, also removes programs that depend on the target
// through map ownership (map_owner_id). With --all, every managed
// program is deleted.
type ProgramDeleteCmd struct {
	cliformat.OutputFlags
	Recursive  bool                  `short:"r" name:"recursive" help:"Also delete programs that share maps with the target (map_owner_id dependents)."`
	All        bool                  `name:"all" help:"Delete all managed programs."`
	ProgramIDs []bpfmancli.ProgramID `arg:"" name:"program-id" optional:"" help:"Program IDs to delete."`
}

// Validate ensures exactly one of --all or explicit program IDs is
// provided.
func (c *ProgramDeleteCmd) Validate() error {
	if c.All && len(c.ProgramIDs) > 0 {
		return fmt.Errorf("--all and explicit program IDs are mutually exclusive")
	}
	if !c.All && len(c.ProgramIDs) == 0 {
		return fmt.Errorf("provide at least one program ID or use --all")
	}
	return nil
}

// Run executes the program delete command with cascading cleanup.
func (c *ProgramDeleteCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	ids, err := collectDeleteIDs(ctx, mgr, c.All, c.ProgramIDs)
	if err != nil {
		return err
	}
	return executeDeletePrograms(ctx, cli, mgr, ids, c.Recursive)
}

// collectDeleteIDs resolves the set of program IDs to delete. When
// all is true, every managed program ID is returned via
// ListPrograms. Otherwise the explicit IDs are extracted.
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
// programs with cascading cleanup. Both the CLI command and the REPL
// call this function. Locking is handled internally.
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

// LinkDeleteCmd deletes BPF links with cascading cleanup.
// For each link: detaches the link, then unloads the program if it
// has no remaining links. With --recursive, also removes programs
// that depend on the orphaned program through map ownership.
type LinkDeleteCmd struct {
	cliformat.OutputFlags
	Recursive bool               `short:"r" name:"recursive" help:"Also delete programs that share maps with orphaned programs (map_owner_id dependents)."`
	LinkIDs   []bpfmancli.LinkID `arg:"" name:"link-id" help:"Link IDs to delete." required:""`
}

// Run executes the link delete command with cascading cleanup.
func (c *LinkDeleteCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	type result struct {
		id  kernel.LinkID
		err error
	}
	results := make([]result, 0, len(c.LinkIDs))

	lockErr := bpfmancli.RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		for _, lid := range c.LinkIDs {
			err := deleteLink(ctx, writeLock, mgr, lid.Value, c.Recursive)
			results = append(results, result{id: lid.Value, err: err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("link %d: %v\n", r.id, r.err)
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d link(s) failed to delete", failCount, len(results))
	}

	return nil
}

// deleteLink detaches the link, then deletes the program if it has no remaining links.
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
