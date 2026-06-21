package main

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/internal/cliformat"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
)

// AttachCmd groups per-type attach subcommands.
type AttachCmd struct {
	XDP        AttachXDPCmd        `cmd:"" help:"Attach an XDP program to a network interface."`
	TC         AttachTCCmd         `cmd:"" help:"Attach a TC program to a network interface."`
	TCX        AttachTCXCmd        `cmd:"" help:"Attach a TCX program to a network interface."`
	Tracepoint AttachTracepointCmd `cmd:"" help:"Attach a program to a tracepoint."`
	Kprobe     AttachKprobeCmd     `cmd:"" help:"Attach a program to a kernel probe."`
	Uprobe     AttachUprobeCmd     `cmd:"" help:"Attach a program to a user-space probe."`
	Fentry     AttachFentryCmd     `cmd:"" help:"Attach a program to a function entry tracing point."`
	Fexit      AttachFexitCmd      `cmd:"" help:"Attach a program to a function exit tracing point."`
}

// attachResult holds the result of an attach operation for output outside the lock.
type attachResult struct {
	Link bpfman.Link
}

// runAttach is the common attach pattern: create manager, run under lock, format output.
func runAttach(cli *bpfmancli.CLI, ctx context.Context, flags *cliformat.OutputFlags, fn func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error)) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	if _, err := flags.Format(); err != nil {
		return err
	}

	result, err := bpfmancli.RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (attachResult, error) {
		return fn(ctx, mgr, writeLock)
	})
	if err != nil {
		return err
	}

	return cliformat.RenderLinkAttach(cli.Out, cliformat.LinkAttachView{Link: result.Link}, flags)
}

// AttachXDPCmd attaches an XDP program to a network interface.
type AttachXDPCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example   ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
	Iface     string              `arg:"" name:"iface" help:"Network interface."`
	Priority  int                 `short:"p" name:"priority" required:"" help:"Priority in chain (lower runs first; non-negative). Slot exhaustion (more than 10 attachments) is reported by the dispatcher, not by this flag."`
	ProceedOn []bpfman.XDPAction  `name:"proceed-on" sep:"," help:"XDP actions to proceed on (comma-separated or repeated). Values: aborted, drop, pass, tx, redirect, dispatcher_return." default:"pass,dispatcher_return"`
	Netns     string              `short:"n" name:"netns" help:"Network namespace path."`
}

func (c *AttachXDPCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewXDPAttachSpec(c.ProgramID.Value, c.Iface, c.Priority)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid XDP spec: %w", err)
		}
		spec = spec.WithProceedOnActions(c.ProceedOn)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachTCCmd attaches a TC program to a network interface.
type AttachTCCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example   ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
	Iface     string              `arg:"" name:"iface" help:"Network interface."`
	Direction bpfman.TCDirection  `arg:"" name:"direction" help:"Direction (ingress or egress)."`
	Priority  int                 `short:"p" name:"priority" required:"" help:"Priority in chain (lower runs first; non-negative). Slot exhaustion (more than 10 attachments) is reported by the dispatcher, not by this flag."`
	ProceedOn []bpfman.TCAction   `name:"proceed-on" sep:"," help:"TC actions to proceed on (comma-separated or repeated). Values: unspec, ok, reclassify, shot, pipe, stolen, queued, repeat, redirect, trap, dispatcher_return." default:"pipe,dispatcher_return"`
	Netns     string              `short:"n" name:"netns" help:"Network namespace path."`
}

func (c *AttachTCCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewTCAttachSpec(c.ProgramID.Value, c.Iface, c.Direction, c.Priority)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid TC spec: %w", err)
		}
		spec = spec.WithProceedOnActions(c.ProceedOn)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachTCXCmd attaches a TCX program to a network interface.
type AttachTCXCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example   ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
	Iface     string              `arg:"" name:"iface" help:"Network interface."`
	Direction bpfman.TCDirection  `arg:"" name:"direction" help:"Direction (ingress or egress)."`
	Priority  int                 `short:"p" name:"priority" required:"" help:"Priority in chain (lower runs first; non-negative). TCX uses native kernel ordering, not a dispatcher."`
	Netns     string              `short:"n" name:"netns" help:"Network namespace path."`
}

func (c *AttachTCXCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewTCXAttachSpec(c.ProgramID.Value, c.Iface, c.Direction, c.Priority)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid TCX spec: %w", err)
		}
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachTracepointCmd attaches a program to a tracepoint.
type AttachTracepointCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example    ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID  bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
	Tracepoint bpfman.Tracepoint   `arg:"" name:"tracepoint" help:"Tracepoint in group/name form (e.g. sched/sched_switch)."`
}

func (c *AttachTracepointCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewTracepointAttachSpec(c.ProgramID.Value, c.Tracepoint)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid tracepoint spec: %w", err)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachKprobeCmd attaches a program to a kernel probe.
type AttachKprobeCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example   ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
	FnName    string              `arg:"" name:"fn-name" help:"Kernel function name to attach to."`
	Offset    uint64              `name:"offset" help:"Offset within the function." default:"0"`
}

func (c *AttachKprobeCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewKprobeAttachSpec(c.ProgramID.Value, c.FnName)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid kprobe spec: %w", err)
		}
		if c.Offset != 0 {
			spec = spec.WithOffset(c.Offset)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachUprobeCmd attaches a program to a user-space probe.
type AttachUprobeCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example      ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID    bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
	Target       string              `arg:"" name:"target" help:"Absolute path to the target binary or library, or a bare library name (e.g. libc) resolved like the dynamic linker."`
	FnName       string              `short:"f" name:"fn-name" help:"Function name to attach to."`
	Offset       uint64              `name:"offset" help:"Offset within the function." default:"0"`
	Pid          int32               `name:"pid" help:"Only trace this process ID (0 traces all processes)."`
	ContainerPid int32               `name:"container-pid" help:"Container PID for namespace-aware uprobe attachment."`
}

func (c *AttachUprobeCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewUprobeAttachSpec(c.ProgramID.Value, c.Target, c.Pid, c.ContainerPid)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid uprobe spec: %w", err)
		}
		if c.FnName != "" {
			spec = spec.WithFnName(c.FnName)
		}
		if c.Offset != 0 {
			spec = spec.WithOffset(c.Offset)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachFentryCmd attaches a program to a function entry tracing point.
type AttachFentryCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example   ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachFentryCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewFentryAttachSpec(c.ProgramID.Value)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid fentry spec: %w", err)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachFexitCmd attaches a program to a function exit tracing point.
type AttachFexitCmd struct {
	cliformat.OutputFlags
	AttachMetadataFlags
	Example   ExampleFlag         `name:"example" help:"Show working examples and exit."`
	ProgramID bpfmancli.ProgramID `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachFexitCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewFexitAttachSpec(c.ProgramID.Value)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid fexit spec: %w", err)
		}

		spec = spec.WithMetadata(bpfmancli.MetadataMap(c.Metadata))

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}
