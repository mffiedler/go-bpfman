package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/lock"
)

// DispatcherCmd groups dispatcher management subcommands.
type DispatcherCmd struct {
	List   ListDispatchersCmd  `cmd:"" default:"withargs" help:"List dispatchers."`
	Get    GetDispatcherCmd    `cmd:"" help:"Get dispatcher details."`
	Delete DeleteDispatcherCmd `cmd:"" hidden:"" help:"Delete a dispatcher."`
}

// ListDispatchersCmd lists all dispatchers.
type ListDispatchersCmd struct {
	cliformat.OutputFlags
	Type string `name:"type" help:"Filter by dispatcher type (xdp, tc-ingress, tc-egress)."`
}

// Run executes the list dispatchers command.
func (c *ListDispatchersCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	summaries, err := mgr.ListDispatcherSummaries(ctx)
	if err != nil {
		return err
	}

	// Client-side type filter
	if c.Type != "" {
		filterType, err := dispatcher.ParseDispatcherType(c.Type)
		if err != nil {
			return err
		}
		filtered := summaries[:0]
		for _, s := range summaries {
			if s.Key.Type == filterType {
				filtered = append(filtered, s)
			}
		}
		summaries = filtered
	}

	if len(summaries) == 0 && !c.OutputFlags.IsStructured() {
		return nil
	}

	output, err := cliformat.FormatDispatcherList(summaries, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// GetDispatcherCmd gets details of a dispatcher by its key.
type GetDispatcherCmd struct {
	cliformat.OutputFlags
	Type    string `arg:"" help:"Dispatcher type (xdp, tc-ingress, tc-egress)."`
	Nsid    uint64 `arg:"" help:"Network namespace ID."`
	Ifindex uint32 `arg:"" help:"Interface index."`
}

// Run executes the get dispatcher command.
func (c *GetDispatcherCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	dispType, err := dispatcher.ParseDispatcherType(c.Type)
	if err != nil {
		return err
	}

	key := dispatcher.Key{
		Type:    dispType,
		Nsid:    c.Nsid,
		Ifindex: c.Ifindex,
	}

	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	snap, err := mgr.GetDispatcherSnapshot(ctx, key)
	if err != nil {
		return err
	}

	output, err := cliformat.FormatDispatcherSnapshot(snap, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// DeleteDispatcherCmd deletes a dispatcher by its key.
type DeleteDispatcherCmd struct {
	Type    string `arg:"" help:"Dispatcher type (xdp, tc-ingress, tc-egress)."`
	Nsid    uint64 `arg:"" help:"Network namespace ID."`
	Ifindex uint32 `arg:"" help:"Interface index."`
}

// Run executes the delete dispatcher command.
func (c *DeleteDispatcherCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	dispType, err := dispatcher.ParseDispatcherType(c.Type)
	if err != nil {
		return err
	}

	key := dispatcher.Key{
		Type:    dispType,
		Nsid:    c.Nsid,
		Ifindex: c.Ifindex,
	}

	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	return bpfmancli.RunMutation(ctx, cli, mgr, func(ctx context.Context, writeLock lock.WriterScope) error {
		return mgr.DeleteDispatcherSnapshot(ctx, writeLock, key)
	})
}
