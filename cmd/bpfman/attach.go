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

// AttachCmd attaches a loaded program to a hook.
type AttachCmd struct {
	OutputFlags

	ProgramID ProgramID `arg:"" help:"Program ID to attach."`
	Type      string    `arg:"" enum:"xdp,tracepoint,kprobe,tc,tcx,uprobe,fentry,fexit" help:"Attach type."`

	// Tracepoint flags
	Tracepoint string `short:"t" name:"tracepoint" help:"Tracepoint to attach to (group/name format, e.g., sched/sched_switch)."`

	// XDP/TC/TCX flags
	Iface    string `short:"i" name:"iface" help:"Network interface to attach to."`
	Priority int    `short:"p" name:"priority" help:"Priority in chain (1-1000, lower runs first)."`

	// TC/TCX direction flag
	Direction string `short:"d" name:"direction" help:"Direction for TC/TCX (ingress or egress)."`

	// TC proceed-on flag
	ProceedOn []string `name:"proceed-on" sep:"," help:"TC actions to proceed on (comma-separated or repeated). Values: unspec, ok, reclassify, shot, pipe, stolen, queued, repeat, redirect, trap, dispatcher_return." default:"ok,pipe,dispatcher_return"`

	// Network namespace flag
	Netns string `short:"n" name:"netns" help:"Network namespace path (e.g., /var/run/netns/myns)."`

	// Kprobe/uprobe flags
	// Note: retprobe is NOT a CLI flag - it's derived from the program type
	// (kretprobe/uretprobe vs kprobe/uprobe) which is determined at load time.
	FnName       string `short:"f" name:"fn-name" help:"Function name to attach to."`
	Offset       uint64 `name:"offset" help:"Offset within the function." default:"0"`
	Target       string `name:"target" help:"Path to target binary or library (required for uprobe)."`
	ContainerPid int32  `name:"container-pid" help:"Container PID for namespace-aware uprobe attachment."`

	// Common flags
	Metadata []KeyValue `short:"m" name:"metadata" help:"KEY=VALUE metadata (can be repeated)."`
}

// attachResult holds the result of an attach operation for output outside the lock.
type attachResult struct {
	Link bpfman.Link
}

// Run executes the attach command: mutation under lock, output outside.
func (c *AttachCmd) Run(cli *CLI, ctx context.Context) error {
	mgr, cleanup, err := cli.NewManager(ctx)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	defer cleanup()

	// Execute mutation under lock
	result, err := c.execute(ctx, cli, mgr)
	if err != nil {
		return err
	}

	// Output outside lock
	return c.output(cli, result)
}

// execute performs the attach operation under the global writer lock.
func (c *AttachCmd) execute(ctx context.Context, cli *CLI, mgr *manager.Manager) (attachResult, error) {
	return RunWithLockValueAndScope(ctx, cli, func(ctx context.Context, scope lock.WriterScope) (attachResult, error) {
		switch c.Type {
		case "tracepoint":
			return c.attachTracepoint(ctx, mgr, scope)
		case "xdp":
			return c.attachXDP(ctx, mgr, scope)
		case "tc":
			return c.attachTC(ctx, mgr, scope)
		case "tcx":
			return c.attachTCX(ctx, mgr, scope)
		case "kprobe":
			return c.attachKprobe(ctx, mgr, scope)
		case "uprobe":
			return c.attachUprobe(ctx, mgr, scope)
		case "fentry":
			return c.attachFentry(ctx, mgr, scope)
		case "fexit":
			return c.attachFexit(ctx, mgr, scope)
		default:
			return attachResult{}, fmt.Errorf("unknown attach type: %s", c.Type)
		}
	})
}

// output formats the link result outside the lock.
func (c *AttachCmd) output(cli *CLI, result attachResult) error {
	output, err := FormatLinkResult(result.Link, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

func (c *AttachCmd) attachTracepoint(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	if c.Tracepoint == "" {
		return attachResult{}, fmt.Errorf("--tracepoint is required for tracepoint attachment")
	}

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
}

func (c *AttachCmd) attachXDP(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	if c.Iface == "" {
		return attachResult{}, fmt.Errorf("--iface is required for XDP attachment")
	}

	iface, err := net.InterfaceByName(c.Iface)
	if err != nil {
		return attachResult{}, fmt.Errorf("failed to find interface %q: %w", c.Iface, err)
	}

	spec, err := bpfman.NewXDPAttachSpec(c.ProgramID.Value, c.Iface, iface.Index)
	if err != nil {
		return attachResult{}, fmt.Errorf("invalid XDP spec: %w", err)
	}
	if c.Netns != "" {
		spec = spec.WithNetns(c.Netns)
	}

	link, err := mgr.Attach(ctx, scope, spec)
	if err != nil {
		return attachResult{}, err
	}
	return attachResult{Link: link}, nil
}

func (c *AttachCmd) attachTC(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	if c.Iface == "" {
		return attachResult{}, fmt.Errorf("--iface is required for TC attachment")
	}
	if c.Direction == "" {
		return attachResult{}, fmt.Errorf("--direction is required for TC attachment")
	}
	if c.Priority < 1 || c.Priority > 1000 {
		return attachResult{}, fmt.Errorf("--priority is required for TC attachment (must be 1-1000)")
	}

	direction, err := bpfman.ParseTCDirection(c.Direction)
	if err != nil {
		return attachResult{}, err
	}

	iface, err := net.InterfaceByName(c.Iface)
	if err != nil {
		return attachResult{}, fmt.Errorf("failed to find interface %q: %w", c.Iface, err)
	}

	// Parse proceed-on values
	proceedOn, err := ParseTCActions(c.ProceedOn)
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
}

func (c *AttachCmd) attachTCX(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	if c.Iface == "" {
		return attachResult{}, fmt.Errorf("--iface is required for TCX attachment")
	}
	if c.Direction == "" {
		return attachResult{}, fmt.Errorf("--direction is required for TCX attachment")
	}
	if c.Priority < 1 || c.Priority > 1000 {
		return attachResult{}, fmt.Errorf("--priority is required for TCX attachment (must be 1-1000)")
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
}

func (c *AttachCmd) attachKprobe(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	if c.FnName == "" {
		return attachResult{}, fmt.Errorf("--fn-name is required for kprobe attachment")
	}

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
}

func (c *AttachCmd) attachUprobe(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	if c.Target == "" {
		return attachResult{}, fmt.Errorf("--target is required for uprobe attachment")
	}

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
}

func (c *AttachCmd) attachFentry(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	spec, err := bpfman.NewFentryAttachSpec(c.ProgramID.Value)
	if err != nil {
		return attachResult{}, fmt.Errorf("invalid fentry spec: %w", err)
	}

	link, err := mgr.Attach(ctx, scope, spec)
	if err != nil {
		return attachResult{}, err
	}
	return attachResult{Link: link}, nil
}

func (c *AttachCmd) attachFexit(ctx context.Context, mgr *manager.Manager, scope lock.WriterScope) (attachResult, error) {
	spec, err := bpfman.NewFexitAttachSpec(c.ProgramID.Value)
	if err != nil {
		return attachResult{}, fmt.Errorf("invalid fexit spec: %w", err)
	}

	link, err := mgr.Attach(ctx, scope, spec)
	if err != nil {
		return attachResult{}, err
	}
	return attachResult{Link: link}, nil
}
