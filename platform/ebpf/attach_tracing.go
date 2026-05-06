package ebpf

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// AttachTracepoint attaches a pinned program to a tracepoint.
func (k *kernelAdapter) AttachTracepoint(ctx context.Context, progPinPath, group, name string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	linkPin := string(linkPinPath)
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	lnk, err := link.Tracepoint(group, name, prog, nil)
	if err != nil {
		// Preserve the domain-level not-found error when tracefs
		// enumeration was unavailable and manager preflight skipped.
		if isTracepointNotFoundError(err) {
			return bpfman.AttachOutput{}, bpfman.ErrTracepointNotFound{Group: group, Name: name}
		}
		return bpfman.AttachOutput{}, fmt.Errorf("attach to tracepoint %s/%s: %w", group, name, err)
	}

	// Pin the link if a path is provided
	if linkPin != "" {
		if err := pinWithRetry(linkPin, lnk.Pin); err != nil {
			lnk.Close()
			return bpfman.AttachOutput{}, fmt.Errorf("pin link to %s: %w", linkPin, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	// Hand the live link to the kernelAdapter so DetachLink can
	// Close it after unpinning. For probe-style attachments,
	// pin-removal alone does not run perf_event_free_bpf_prog;
	// the program stays attached to the perf_event until the
	// link object is explicitly Closed.
	if linkPin != "" {
		k.trackLink(linkPin, lnk)
	} else {
		lnk.Close()
	}

	return bpfman.AttachOutput{
		LinkID:     kernel.LinkID(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    linkPinPath,
	}, nil
}

func isTracepointNotFoundError(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// AttachKprobe attaches a pinned program to a kernel function.
// If retprobe is true, attaches as a kretprobe instead of kprobe.
func (k *kernelAdapter) AttachKprobe(ctx context.Context, progPinPath, fnName string, offset uint64, retprobe bool, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	linkPin := string(linkPinPath)
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	// Build kprobe options
	opts := &link.KprobeOptions{
		Offset: offset,
	}

	// Attach as kprobe or kretprobe
	var lnk link.Link
	if retprobe {
		lnk, err = link.Kretprobe(fnName, prog, opts)
	} else {
		lnk, err = link.Kprobe(fnName, prog, opts)
	}
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach kprobe to %s: %w", fnName, err)
	}

	// Pin the link if a path is provided
	if linkPin != "" {
		if err := pinWithRetry(linkPin, lnk.Pin); err != nil {
			lnk.Close()
			return bpfman.AttachOutput{}, fmt.Errorf("pin link to %s: %w", linkPin, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	// Hand the live link to the kernelAdapter so DetachLink can
	// Close it after unpinning. For probe-style attachments,
	// pin-removal alone does not run perf_event_free_bpf_prog;
	// the program stays attached to the perf_event until the
	// link object is explicitly Closed.
	if linkPin != "" {
		k.trackLink(linkPin, lnk)
	} else {
		lnk.Close()
	}

	return bpfman.AttachOutput{
		LinkID:     kernel.LinkID(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    linkPinPath,
	}, nil
}

// AttachFentry attaches a pinned fentry program to a kernel function.
// The target function was specified at load time and is stored in the program.
func (k *kernelAdapter) AttachFentry(ctx context.Context, progPinPath, fnName string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	return k.attachTracing(ctx, progPinPath, fnName, linkPinPath)
}

// AttachFexit attaches a pinned fexit program to a kernel function.
// The target function was specified at load time and is stored in the program.
func (k *kernelAdapter) AttachFexit(ctx context.Context, progPinPath, fnName string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	return k.attachTracing(ctx, progPinPath, fnName, linkPinPath)
}

// attachTracing is the shared implementation for fentry and fexit attachment.
func (k *kernelAdapter) attachTracing(ctx context.Context, progPinPath, fnName string, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	linkPin := string(linkPinPath)
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	// Attach using link.AttachTracing - the program already has the target
	// function and attach type set from load time (via ELF section name).
	lnk, err := link.AttachTracing(link.TracingOptions{
		Program: prog,
	})
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach tracing to %s: %w", fnName, err)
	}

	// Pin the link if a path is provided
	if linkPinPath != "" {
		if err := pinWithRetry(linkPinPath, lnk.Pin); err != nil {
			lnk.Close()
			return bpfman.AttachOutput{}, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	// Hand the live link to the kernelAdapter so DetachLink can
	// Close it after unpinning. For probe-style attachments,
	// pin-removal alone does not run perf_event_free_bpf_prog;
	// the program stays attached to the perf_event until the
	// link object is explicitly Closed.
	if linkPin != "" {
		k.trackLink(linkPin, lnk)
	} else {
		lnk.Close()
	}

	return bpfman.AttachOutput{
		LinkID:     kernel.LinkID(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    linkPinPath,
	}, nil
}
