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

	ids, err := mgr.ResolveDeleteProgramIDs(ctx, c.All, programIDs(c.ProgramIDs))
	if err != nil {
		return err
	}
	return executeDeletePrograms(ctx, cli, mgr, ids, c.Recursive)
}

func programIDs(explicit []bpfmancli.ProgramID) []kernel.ProgramID {
	ids := make([]kernel.ProgramID, len(explicit))
	for i, pid := range explicit {
		ids[i] = pid.Value
	}
	return ids
}

// executeDeletePrograms is the shared implementation for deleting
// programs with cascading cleanup. Both the CLI command and
// bpfman-shell call this function. Locking is handled internally.
func executeDeletePrograms(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, ids []kernel.ProgramID, recursive bool) error {
	type result struct {
		id  kernel.ProgramID
		err error
	}
	results := make([]result, 0, len(ids))

	lockErr := bpfmancli.RunWithLock(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) error {
		deleteResults := mgr.DeletePrograms(ctx, writeLock, ids, manager.DeleteProgramsOpts{Recursive: recursive})
		for _, r := range deleteResults {
			results = append(results, result{id: r.ProgramID, err: r.Err})
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
		linkIDs := make([]kernel.LinkID, len(c.LinkIDs))
		for i, lid := range c.LinkIDs {
			linkIDs[i] = lid.Value
		}
		deleteResults := mgr.DeleteLinks(ctx, writeLock, linkIDs, manager.DeleteLinksOpts{Recursive: c.Recursive})
		for _, r := range deleteResults {
			results = append(results, result{id: r.LinkID, err: r.Err})
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
