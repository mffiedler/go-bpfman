package cli

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
)

// DeleteCmd deletes BPF resources with cascading cleanup.
// Unlike unload/detach, delete works on both programs and links and
// automatically handles dependencies:
//   - delete link/123: detaches the link, then unloads the program if orphaned
//   - delete program/456: detaches all links, then unloads the program
type DeleteCmd struct {
	OutputFlags
	Resources []ResourceRef `arg:"" name:"resource" help:"Resources to delete (e.g., link/123, program/456)." required:""`
}

// Run executes the delete command with cascading cleanup.
func (c *DeleteCmd) Run(cli *CLI, ctx context.Context) error {
	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

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
				err = c.deleteLink(ctx, runtime, ref.ID)
			case ResourceKindProgram:
				err = c.deleteProgram(ctx, runtime, ref.ID)
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

// deleteLink detaches the link, then unloads the program if it has no remaining links.
func (c *DeleteCmd) deleteLink(ctx context.Context, runtime *CLIRuntime, linkID uint32) error {
	// Get link to find its program
	link, err := runtime.Manager.GetLink(ctx, bpfman.LinkID(linkID))
	if err != nil {
		return fmt.Errorf("get link: %w", err)
	}

	programID := link.ProgramID

	// Detach the link
	if err := runtime.Manager.Detach(ctx, bpfman.LinkID(linkID)); err != nil {
		return fmt.Errorf("detach: %w", err)
	}

	// Check if program now has no links
	links, err := runtime.Manager.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links for program %d: %w", programID, err)
	}

	if len(links) == 0 {
		// Program is orphaned, unload it
		if err := runtime.Manager.Unload(ctx, programID); err != nil {
			return fmt.Errorf("unload orphaned program %d: %w", programID, err)
		}
	}

	return nil
}

// deleteProgram detaches all links for the program, then unloads it.
func (c *DeleteCmd) deleteProgram(ctx context.Context, runtime *CLIRuntime, programID uint32) error {
	// Get all links for this program
	links, err := runtime.Manager.ListLinksByProgram(ctx, programID)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}

	// Detach all links
	for _, link := range links {
		if err := runtime.Manager.Detach(ctx, link.ID); err != nil {
			return fmt.Errorf("detach link %d: %w", link.ID, err)
		}
	}

	// Unload the program
	if err := runtime.Manager.Unload(ctx, programID); err != nil {
		return fmt.Errorf("unload: %w", err)
	}

	return nil
}
