package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/cmd/bpfman/cliformat"
	"github.com/bpfman/bpfman/cmd/internal/args"
	"github.com/bpfman/bpfman/cmd/internal/runtime"
	"github.com/bpfman/bpfman/lock"
)

// DetachCmd detaches links.
type DetachCmd struct {
	// OutputFlags carries the -o/--output flag selecting text or
	// JSON rendering.
	cliformat.OutputFlags

	// LinkIDs are the IDs of the links to detach; at least one is
	// required.
	LinkIDs []args.LinkID `arg:"" name:"link-id" help:"Link IDs to detach." required:""`

	// IgnoreMissing treats a "link not found" error as success,
	// making a repeated detach (e.g. from a defer) idempotent.
	IgnoreMissing bool `name:"ignore-missing" help:"Treat 'link not found' as success rather than an error; useful for idempotent cleanup (e.g. defer)."`
}

// Run executes the detach command: mutation under lock, output outside.
func (c *DetachCmd) Run(cli *runtime.CLI, ctx context.Context) error {
	mgr, cleanup, err := newManager(ctx, cli)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	ids := make([]bpfman.LinkID, len(c.LinkIDs))
	for i, lid := range c.LinkIDs {
		ids[i] = lid.Value
	}
	return runtime.RunBatchMutation(ctx, cli, ids, "link", "detach",
		func(ctx context.Context, writeLock lock.WriterScope, id bpfman.LinkID) error {
			err := mgr.Detach(ctx, writeLock, id)
			if err != nil && c.IgnoreMissing {
				var notFound bpfman.ErrLinkNotFound
				if errors.As(err, &notFound) {
					return nil
				}
			}

			return err
		})
}
