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

	result, err := bpfmancli.RunWithLockValue(ctx, cli, func(ctx context.Context, writeLock lock.WriterScope) (attachResult, error) {
		return fn(ctx, mgr, writeLock)
	})
	if err != nil {
		return err
	}

	output, err := cliformat.FormatLinkResult(result.Link, flags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// AttachXDPCmd attaches an XDP program to a network interface.
type AttachXDPCmd struct {
	cliformat.OutputFlags
	Example   ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Iface     string               `short:"i" name:"iface" required:"" help:"Network interface."`
	Priority  int                  `short:"p" name:"priority" help:"Priority in chain (lower runs first; non-negative; 0 = default). Slot exhaustion (more than 10 attachments) is reported by the dispatcher, not by this flag."`
	ProceedOn []bpfman.XDPAction   `name:"proceed-on" sep:"," help:"XDP actions to proceed on (comma-separated or repeated). Values: aborted, drop, pass, tx, redirect, dispatcher_return." default:"pass,dispatcher_return"`
	Netns     string               `short:"n" name:"netns" help:"Network namespace path."`
	Metadata  []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachXDPCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewXDPAttachSpec(c.ProgramID.Value, c.Iface)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid XDP spec: %w", err)
		}
		spec = spec.WithPriority(c.Priority).WithProceedOnActions(c.ProceedOn)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

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
	Example   ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Iface     string               `short:"i" name:"iface" required:"" help:"Network interface."`
	Direction bpfman.TCDirection   `short:"d" name:"direction" required:"" help:"Direction (ingress or egress)."`
	Priority  int                  `short:"p" name:"priority" help:"Priority in chain (lower runs first; non-negative; 0 = default). Slot exhaustion (more than 10 attachments) is reported by the dispatcher, not by this flag."`
	ProceedOn []bpfman.TCAction    `name:"proceed-on" sep:"," help:"TC actions to proceed on (comma-separated or repeated). Values: unspec, ok, reclassify, shot, pipe, stolen, queued, repeat, redirect, trap, dispatcher_return." default:"pipe,dispatcher_return"`
	Netns     string               `short:"n" name:"netns" help:"Network namespace path."`
	Metadata  []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachTCCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		if c.Priority < 0 {
			return attachResult{}, fmt.Errorf("--priority must be non-negative, got %d", c.Priority)
		}

		spec, err := bpfman.NewTCAttachSpec(c.ProgramID.Value, c.Iface, c.Direction)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid TC spec: %w", err)
		}
		spec = spec.WithPriority(c.Priority).WithProceedOnActions(c.ProceedOn)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

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
	Example   ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Iface     string               `short:"i" name:"iface" required:"" help:"Network interface."`
	Direction bpfman.TCDirection   `short:"d" name:"direction" required:"" help:"Direction (ingress or egress)."`
	Priority  int                  `short:"p" name:"priority" help:"Priority in chain (lower runs first; non-negative; 0 = default). Slot exhaustion (more than 10 attachments) is reported by the dispatcher, not by this flag."`
	Netns     string               `short:"n" name:"netns" help:"Network namespace path."`
	Metadata  []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachTCXCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		if c.Priority < 0 {
			return attachResult{}, fmt.Errorf("--priority must be non-negative, got %d", c.Priority)
		}

		spec, err := bpfman.NewTCXAttachSpec(c.ProgramID.Value, c.Iface, c.Direction)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid TCX spec: %w", err)
		}
		spec = spec.WithPriority(c.Priority)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

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
	Example    ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Metadata   []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID  bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
	Tracepoint bpfman.Tracepoint    `arg:"" name:"tracepoint" help:"Tracepoint in group/name form (e.g. sched/sched_switch)."`
}

func (c *AttachTracepointCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewTracepointAttachSpec(c.ProgramID.Value, c.Tracepoint)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid tracepoint spec: %w", err)
		}

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
	Example   ExampleFlag          `name:"example" help:"Show working examples and exit."`
	FnName    string               `short:"f" name:"fn-name" required:"" help:"Kernel function name to attach to."`
	Offset    uint64               `name:"offset" help:"Offset within the function." default:"0"`
	Metadata  []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
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
	Example      ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Target       string               `name:"target" required:"" help:"Path to target binary or library."`
	FnName       string               `short:"f" name:"fn-name" help:"Function name to attach to."`
	Offset       uint64               `name:"offset" help:"Offset within the function." default:"0"`
	ContainerPid int32                `name:"container-pid" help:"Container PID for namespace-aware uprobe attachment."`
	Metadata     []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID    bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachUprobeCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewUprobeAttachSpec(c.ProgramID.Value, c.Target)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid uprobe spec: %w", err)
		}
		if c.FnName != "" {
			spec = spec.WithFnName(c.FnName)
		}
		if c.Offset != 0 {
			spec = spec.WithOffset(c.Offset)
		}
		if c.ContainerPid > 0 {
			spec = spec.WithContainerPid(c.ContainerPid)
		}

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
	Example   ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Metadata  []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachFentryCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewFentryAttachSpec(c.ProgramID.Value)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid fentry spec: %w", err)
		}

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
	Example   ExampleFlag          `name:"example" help:"Show working examples and exit."`
	Metadata  []bpfmancli.KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID bpfmancli.ProgramID  `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachFexitCmd) Run(cli *bpfmancli.CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, writeLock lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewFexitAttachSpec(c.ProgramID.Value)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid fexit spec: %w", err)
		}

		link, err := mgr.Attach(ctx, writeLock, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}
