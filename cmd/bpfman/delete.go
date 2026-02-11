package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
)

// DeleteCmd deletes BPF resources with cascading cleanup.
// Unlike unload/detach which are single-level primitives, delete
// cascades through links:
//   - delete link/123: detaches the link, unloads the program if orphaned
//   - delete program/456: detaches all links, then unloads the program
//
// With --recursive, also removes programs that depend on the target
// through map ownership (map_owner_id). Without it, delete fails if
// other programs share the target's maps.
type DeleteCmd struct {
	OutputFlags
	Recursive bool          `short:"r" name:"recursive" help:"Also delete programs that share maps with the target (map_owner_id dependents)."`
	Resources []ResourceRef `arg:"" name:"resource" help:"Resources to delete (e.g., link/123, program/456)." required:""`
}

// Run executes the delete command with cascading cleanup.
func (c *DeleteCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	// Collect results to print after releasing lock
	type result struct {
		ref ResourceRef
		err error
	}
	results := make([]result, 0, len(c.Resources))

	// Mutation under lock - process all resources
	lockErr := RunWithLock(ctx, cli, func(ctx context.Context) error {
		for _, ref := range c.Resources {
			var err error
			switch ref.Kind {
			case ResourceKindLink:
				err = c.deleteLink(ctx, mgr, kernel.LinkID(ref.ID))
			case ResourceKindProgram:
				err = c.deleteProgram(ctx, mgr, kernel.ProgramID(ref.ID))
			}
			results = append(results, result{ref: ref, err: err})
		}
		return nil
	})
	if lockErr != nil {
		return lockErr
	}

	// Print results outside lock
	var failCount int
	for _, r := range results {
		if r.err != nil {
			_ = cli.PrintErrf("%s: %v\n", r.ref, r.err)
			failCount++
		}
	}

	if failCount > 0 {
		return fmt.Errorf("%d of %d resource(s) failed to delete", failCount, len(results))
	}

	return nil
}

// deleteLink detaches the link, then deletes the program if it has no remaining links.
func (c *DeleteCmd) deleteLink(ctx context.Context, mgr *manager.Manager, linkID kernel.LinkID) error {
	// Get link to find its program
	link, err := mgr.GetLink(ctx, linkID)
	if err != nil {
		return fmt.Errorf("get link: %w", err)
	}

	programID := link.ProgramID

	// Detach the link
	if err := mgr.Detach(ctx, linkID); err != nil {
		return fmt.Errorf("detach: %w", err)
	}

	// Check if program now has no links
	links, err := mgr.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links for program %d: %w", programID, err)
	}

	if len(links) == 0 {
		if err := c.deleteProgram(ctx, mgr, programID); err != nil {
			return fmt.Errorf("delete orphaned program %d: %w", programID, err)
		}
	}

	return nil
}

// deleteProgram detaches all links for the program, then unloads it.
// With --recursive, also deletes dependent programs (those sharing
// maps via map_owner_id) before unloading the target.
func (c *DeleteCmd) deleteProgram(ctx context.Context, mgr *manager.Manager, programID kernel.ProgramID) error {
	// With full-graph, find and delete map dependents first
	if c.Recursive {
		if err := c.deleteDependents(ctx, mgr, programID); err != nil {
			return err
		}
	}

	// Detach all links for this program
	links, err := mgr.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}

	for _, link := range links {
		if err := mgr.Detach(ctx, link.ID); err != nil {
			return fmt.Errorf("detach link %d: %w", link.ID, err)
		}
	}

	// Unload the program
	if err := mgr.Unload(ctx, programID); err != nil {
		return fmt.Errorf("unload: %w", err)
	}

	return nil
}

// deleteDependents finds programs that share maps with the target
// (map_owner_id = programID) and deletes them first.
func (c *DeleteCmd) deleteDependents(ctx context.Context, mgr *manager.Manager, ownerID kernel.ProgramID) error {
	result, err := mgr.ListPrograms(ctx)
	if err != nil {
		return fmt.Errorf("list programs: %w", err)
	}

	for _, prog := range result.Programs {
		if prog.Record.Handles.MapOwnerID != nil && *prog.Record.Handles.MapOwnerID == ownerID {
			if err := c.deleteProgram(ctx, mgr, prog.Record.ProgramID); err != nil {
				return fmt.Errorf("delete dependent program %d: %w", prog.Record.ProgramID, err)
			}
		}
	}

	return nil
}
