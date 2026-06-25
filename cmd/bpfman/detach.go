package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman/cliformat"
	"github.com/frobware/go-bpfman/cmd/internal/args"
	"github.com/frobware/go-bpfman/cmd/internal/runtime"
	"github.com/frobware/go-bpfman/lock"
)

// DetachCmd detaches links.
type DetachCmd struct {
	cliformat.OutputFlags
	LinkIDs       []args.LinkID `arg:"" name:"link-id" help:"Link IDs to detach." required:""`
	IgnoreMissing bool          `name:"ignore-missing" help:"Treat 'link not found' as success rather than an error; useful for idempotent cleanup (e.g. defer)."`
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
