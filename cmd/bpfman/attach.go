package main

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/frobware/go-bpfman"
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
func runAttach(cli *CLI, ctx context.Context, flags *OutputFlags, fn func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error)) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	result, err := RunWithLockValueAndScope(ctx, cli, func(ctx context.Context, scope lock.WriterScope) (attachResult, error) {
		return fn(ctx, mgr, scope)
	})
	if err != nil {
		return err
	}

	output, err := FormatLinkResult(result.Link, flags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

// AttachXDPCmd attaches an XDP program to a network interface.
type AttachXDPCmd struct {
	OutputFlags
	Example   ExampleFlag `name:"example" help:"Show working examples and exit."`
	Iface     string      `short:"i" name:"iface" required:"" help:"Network interface."`
	Priority  int         `short:"p" name:"priority" help:"Priority in chain (lower runs first, 0 = default)."`
	ProceedOn []string    `name:"proceed-on" sep:"," help:"XDP actions to proceed on (comma-separated or repeated). Values: aborted, drop, pass, tx, redirect, dispatcher_return." default:"pass,dispatcher_return"`
	Netns     string      `short:"n" name:"netns" help:"Network namespace path."`
	Metadata  []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachXDPCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		iface, err := net.InterfaceByName(c.Iface)
		if err != nil {
			return attachResult{}, fmt.Errorf("failed to find interface %q: %w", c.Iface, err)
		}

		proceedOn, err := bpfman.ParseXDPActions(c.ProceedOn)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid proceed-on value: %w", err)
		}

		spec, err := bpfman.NewXDPAttachSpec(c.ProgramID.Value, c.Iface, iface.Index)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid XDP spec: %w", err)
		}
		spec = spec.WithPriority(c.Priority).WithProceedOn(proceedOn)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachTCCmd attaches a TC program to a network interface.
type AttachTCCmd struct {
	OutputFlags
	Example   ExampleFlag `name:"example" help:"Show working examples and exit."`
	Iface     string      `short:"i" name:"iface" required:"" help:"Network interface."`
	Direction string      `short:"d" name:"direction" required:"" help:"Direction (ingress or egress)."`
	Priority  int         `short:"p" name:"priority" help:"Priority in chain (lower runs first, 0 = default)."`
	ProceedOn []string    `name:"proceed-on" sep:"," help:"TC actions to proceed on (comma-separated or repeated). Values: unspec, ok, reclassify, shot, pipe, stolen, queued, repeat, redirect, trap, dispatcher_return." default:"ok,pipe,dispatcher_return"`
	Netns     string      `short:"n" name:"netns" help:"Network namespace path."`
	Metadata  []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachTCCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		if c.Priority < 0 || c.Priority > 1000 {
			return attachResult{}, fmt.Errorf("--priority must be 0-1000, got %d", c.Priority)
		}

		direction, err := bpfman.ParseTCDirection(c.Direction)
		if err != nil {
			return attachResult{}, err
		}

		iface, err := net.InterfaceByName(c.Iface)
		if err != nil {
			return attachResult{}, fmt.Errorf("failed to find interface %q: %w", c.Iface, err)
		}

		proceedOn, err := bpfman.ParseTCActions(c.ProceedOn)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid proceed-on value: %w", err)
		}

		spec, err := bpfman.NewTCAttachSpec(c.ProgramID.Value, c.Iface, iface.Index, direction)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid TC spec: %w", err)
		}
		spec = spec.WithPriority(c.Priority).WithProceedOn(proceedOn)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachTCXCmd attaches a TCX program to a network interface.
type AttachTCXCmd struct {
	OutputFlags
	Example   ExampleFlag `name:"example" help:"Show working examples and exit."`
	Iface     string      `short:"i" name:"iface" required:"" help:"Network interface."`
	Direction string      `short:"d" name:"direction" required:"" help:"Direction (ingress or egress)."`
	Priority  int         `short:"p" name:"priority" help:"Priority in chain (lower runs first, 0 = default)."`
	Netns     string      `short:"n" name:"netns" help:"Network namespace path."`
	Metadata  []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachTCXCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		if c.Priority < 0 || c.Priority > 1000 {
			return attachResult{}, fmt.Errorf("--priority must be 0-1000, got %d", c.Priority)
		}

		direction, err := bpfman.ParseTCDirection(c.Direction)
		if err != nil {
			return attachResult{}, err
		}

		iface, err := net.InterfaceByName(c.Iface)
		if err != nil {
			return attachResult{}, fmt.Errorf("failed to find interface %q: %w", c.Iface, err)
		}

		spec, err := bpfman.NewTCXAttachSpec(c.ProgramID.Value, c.Iface, iface.Index, direction)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid TCX spec: %w", err)
		}
		spec = spec.WithPriority(c.Priority)
		if c.Netns != "" {
			spec = spec.WithNetns(c.Netns)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachTracepointCmd attaches a program to a tracepoint.
type AttachTracepointCmd struct {
	OutputFlags
	Example    ExampleFlag `name:"example" help:"Show working examples and exit."`
	Tracepoint string      `short:"t" name:"tracepoint" required:"" help:"Tracepoint (group/name format, e.g., sched/sched_switch)."`
	Metadata   []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID  ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachTracepointCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		parts := strings.SplitN(c.Tracepoint, "/", 2)
		if len(parts) != 2 {
			return attachResult{}, fmt.Errorf("tracepoint must be in 'group/name' format, got %q", c.Tracepoint)
		}
		group, name := parts[0], parts[1]

		spec, err := bpfman.NewTracepointAttachSpec(c.ProgramID.Value, group, name)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid tracepoint spec: %w", err)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachKprobeCmd attaches a program to a kernel probe.
type AttachKprobeCmd struct {
	OutputFlags
	Example   ExampleFlag `name:"example" help:"Show working examples and exit."`
	FnName    string      `short:"f" name:"fn-name" required:"" help:"Kernel function name to attach to."`
	Offset    uint64      `name:"offset" help:"Offset within the function." default:"0"`
	Metadata  []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachKprobeCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewKprobeAttachSpec(c.ProgramID.Value, c.FnName)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid kprobe spec: %w", err)
		}
		if c.Offset != 0 {
			spec = spec.WithOffset(c.Offset)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachUprobeCmd attaches a program to a user-space probe.
type AttachUprobeCmd struct {
	OutputFlags
	Example      ExampleFlag `name:"example" help:"Show working examples and exit."`
	Target       string      `name:"target" required:"" help:"Path to target binary or library."`
	FnName       string      `short:"f" name:"fn-name" help:"Function name to attach to."`
	Offset       uint64      `name:"offset" help:"Offset within the function." default:"0"`
	ContainerPid int32       `name:"container-pid" help:"Container PID for namespace-aware uprobe attachment."`
	Metadata     []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID    ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachUprobeCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
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

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachFentryCmd attaches a program to a function entry tracing point.
type AttachFentryCmd struct {
	OutputFlags
	Example   ExampleFlag `name:"example" help:"Show working examples and exit."`
	Metadata  []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachFentryCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewFentryAttachSpec(c.ProgramID.Value)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid fentry spec: %w", err)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}

// AttachFexitCmd attaches a program to a function exit tracing point.
type AttachFexitCmd struct {
	OutputFlags
	Example   ExampleFlag `name:"example" help:"Show working examples and exit."`
	Metadata  []KeyValue  `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
	ProgramID ProgramID   `arg:"" name:"program-id" help:"Program ID to attach."`
}

func (c *AttachFexitCmd) Run(cli *CLI, ctx context.Context) error {
	return runAttach(cli, ctx, &c.OutputFlags, func(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
		spec, err := bpfman.NewFexitAttachSpec(c.ProgramID.Value)
		if err != nil {
			return attachResult{}, fmt.Errorf("invalid fexit spec: %w", err)
		}

		link, err := mgr.Attach(ctx, scope, spec)
		if err != nil {
			return attachResult{}, err
		}
		return attachResult{Link: link}, nil
	})
}
