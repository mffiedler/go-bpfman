package cli

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/outcome"
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
	Link          bpfman.Link
	FailedOutcome *outcome.ManagerOperationOutcome
}

// Run executes the attach command: mutation under lock, output outside.
func (c *AttachCmd) Run(cli *CLI, ctx context.Context) error {
	runtime, err := cli.NewCLIRuntime(ctx)
	if err != nil {
		return fmt.Errorf("create runtime: %w", err)
	}
	defer runtime.Close()

	// Execute mutation under lock
	result, err := c.execute(ctx, cli, runtime)
	if err != nil {
		// On failure, display the outcome if available
		if result.FailedOutcome != nil {
			outcomeStr, fmtErr := FormatOutcome(*result.FailedOutcome, &c.OutputFlags)
			if fmtErr == nil {
				format, _ := c.OutputFlags.Format()
				if format == OutputFormatJSON || format == OutputFormatJSONPath {
					_ = cli.PrintOut(outcomeStr)
					return ErrSilent
				}
				_ = cli.PrintErr(outcomeStr)
			}
		}
		return err
	}

	// Output outside lock
	return c.output(cli, ctx, runtime, result)
}

// execute performs the attach operation under the global writer lock.
func (c *AttachCmd) execute(ctx context.Context, cli *CLI, runtime *CLIRuntime) (attachResult, error) {
	// Uprobe needs scope for container attachment
	if c.Type == "uprobe" {
		return RunWithLockValueAndScope(ctx, cli, func(ctx context.Context, scope lock.WriterScope) (attachResult, error) {
			return c.attachUprobe(ctx, runtime, scope)
		})
	}

	// All other types don't need scope
	return RunWithLockValue(ctx, cli, func(ctx context.Context) (attachResult, error) {
		switch c.Type {
		case "tracepoint":
			return c.attachTracepoint(ctx, runtime)
		case "xdp":
			return c.attachXDP(ctx, runtime)
		case "tc":
			return c.attachTC(ctx, runtime)
		case "tcx":
			return c.attachTCX(ctx, runtime)
		case "kprobe":
			return c.attachKprobe(ctx, runtime)
		case "fentry":
			return c.attachFentry(ctx, runtime)
		case "fexit":
			return c.attachFexit(ctx, runtime)
		default:
			return attachResult{}, fmt.Errorf("unknown attach type: %s", c.Type)
		}
	})
}

// output fetches link details and formats output outside the lock.
func (c *AttachCmd) output(cli *CLI, ctx context.Context, runtime *CLIRuntime, result attachResult) error {
	record, err := runtime.Manager.GetLink(ctx, result.Link.Spec.ID)
	if err != nil {
		// Attachment succeeded but we can't fetch details for display.
		// This shouldn't normally happen - log it and show minimal output.
		return cli.PrintOutf("Attached link %d (warning: failed to fetch details: %v)\n", result.Link.Spec.ID, err)
	}

	// Fetch program info to get the BPF function name using the original program ID
	var bpfFunction string
	prog, err := runtime.Manager.Get(ctx, c.ProgramID.Value)
	if err == nil && prog.Status.Kernel != nil {
		bpfFunction = prog.Status.Kernel.Name
	}

	output, err := FormatLinkResult(bpfFunction, record, record.Details, &c.OutputFlags)
	if err != nil {
		return err
	}
	return cli.PrintOut(output)
}

func (c *AttachCmd) attachTracepoint(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
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

	result, err := runtime.Manager.AttachTracepoint(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachXDP(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
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

	result, err := runtime.Manager.AttachXDP(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachTC(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
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

	result, err := runtime.Manager.AttachTC(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachTCX(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
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

	result, err := runtime.Manager.AttachTCX(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachKprobe(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
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

	result, err := runtime.Manager.AttachKprobe(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachUprobe(ctx context.Context, runtime *CLIRuntime, scope lock.WriterScope) (attachResult, error) {
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

	result, err := runtime.Manager.AttachUprobe(ctx, scope, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachFentry(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
	spec, err := bpfman.NewFentryAttachSpec(c.ProgramID.Value)
	if err != nil {
		return attachResult{}, fmt.Errorf("invalid fentry spec: %w", err)
	}

	result, err := runtime.Manager.AttachFentry(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}

func (c *AttachCmd) attachFexit(ctx context.Context, runtime *CLIRuntime) (attachResult, error) {
	spec, err := bpfman.NewFexitAttachSpec(c.ProgramID.Value)
	if err != nil {
		return attachResult{}, fmt.Errorf("invalid fexit spec: %w", err)
	}

	result, err := runtime.Manager.AttachFexit(ctx, spec, bpfman.AttachOpts{})
	if err != nil {
		return attachResult{FailedOutcome: &result.Outcome}, err
	}
	return attachResult{Link: result.Link}, nil
}
