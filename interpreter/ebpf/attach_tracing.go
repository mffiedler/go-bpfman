package ebpf

import (
	"context"
	"fmt"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
)

// AttachTracepoint attaches a pinned program to a tracepoint.
func (k *kernelAdapter) AttachTracepoint(ctx context.Context, progPinPath, group, name, linkPinPath string) (bpfman.Link, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	lnk, err := link.Tracepoint(group, name, prog, nil)
	if err != nil {
		// Provide a user-friendly error if the tracepoint doesn't exist
		if isTracepointNotFoundError(err) {
			if validationErr := validateTracepoint(group, name); validationErr != nil {
				return bpfman.Link{}, validationErr
			}
		}
		// Fall back to the original error for other failures
		return bpfman.Link{}, fmt.Errorf("attach to tracepoint %s/%s: %w", group, name, err)
	}

	// Pin the link if a path is provided
	if linkPinPath != "" {
		if err := pinWithRetry(lnk, linkPinPath); err != nil {
			lnk.Close()
			return bpfman.Link{}, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return bpfman.Link{}, fmt.Errorf("get link info: %w", err)
	}

	kernelLinkID := uint32(linkInfo.ID)
	kernelLink := ToKernelLink(linkInfo)
	return bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(kernelLinkID),
			Kind:      bpfman.LinkKindTracepoint,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.TracepointDetails{Group: group, Name: name},
			// ProgramID is set by the manager after this call
		},
		Status: bpfman.LinkStatus{
			Kernel:     kernelLink,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}, nil
}

// AttachKprobe attaches a pinned program to a kernel function.
// If retprobe is true, attaches as a kretprobe instead of kprobe.
func (k *kernelAdapter) AttachKprobe(ctx context.Context, progPinPath, fnName string, offset uint64, retprobe bool, linkPinPath string) (bpfman.Link, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
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
		return bpfman.Link{}, fmt.Errorf("attach kprobe to %s: %w", fnName, err)
	}

	// Pin the link if a path is provided
	if linkPinPath != "" {
		if err := pinWithRetry(lnk, linkPinPath); err != nil {
			lnk.Close()
			return bpfman.Link{}, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return bpfman.Link{}, fmt.Errorf("get link info: %w", err)
	}

	// Determine link kind based on retprobe flag
	linkKind := bpfman.LinkKindKprobe
	if retprobe {
		linkKind = bpfman.LinkKindKretprobe
	}

	kernelLinkID := uint32(linkInfo.ID)
	kernelLink := ToKernelLink(linkInfo)
	return bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(kernelLinkID),
			Kind:      linkKind,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.KprobeDetails{FnName: fnName, Offset: offset, Retprobe: retprobe},
			// ProgramID is set by the manager after this call
		},
		Status: bpfman.LinkStatus{
			Kernel:     kernelLink,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}, nil
}

// AttachFentry attaches a pinned fentry program to a kernel function.
// The target function was specified at load time and is stored in the program.
func (k *kernelAdapter) AttachFentry(ctx context.Context, progPinPath, fnName, linkPinPath string) (bpfman.Link, error) {
	return k.attachTracing(ctx, progPinPath, fnName, linkPinPath, bpfman.LinkKindFentry)
}

// AttachFexit attaches a pinned fexit program to a kernel function.
// The target function was specified at load time and is stored in the program.
func (k *kernelAdapter) AttachFexit(ctx context.Context, progPinPath, fnName, linkPinPath string) (bpfman.Link, error) {
	return k.attachTracing(ctx, progPinPath, fnName, linkPinPath, bpfman.LinkKindFexit)
}

// attachTracing is the shared implementation for fentry and fexit attachment.
func (k *kernelAdapter) attachTracing(ctx context.Context, progPinPath, fnName, linkPinPath string, linkKind bpfman.LinkKind) (bpfman.Link, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	// Attach using link.AttachTracing - the program already has the target
	// function and attach type set from load time (via ELF section name).
	lnk, err := link.AttachTracing(link.TracingOptions{
		Program: prog,
	})
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("attach tracing to %s: %w", fnName, err)
	}

	// Pin the link if a path is provided
	if linkPinPath != "" {
		if err := pinWithRetry(lnk, linkPinPath); err != nil {
			lnk.Close()
			return bpfman.Link{}, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		lnk.Close()
		return bpfman.Link{}, fmt.Errorf("get link info: %w", err)
	}

	// Build details based on link kind
	var details bpfman.LinkDetails
	if linkKind == bpfman.LinkKindFentry {
		details = bpfman.FentryDetails{FnName: fnName}
	} else {
		details = bpfman.FexitDetails{FnName: fnName}
	}

	kernelLinkID := uint32(linkInfo.ID)
	kernelLink := ToKernelLink(linkInfo)
	return bpfman.Link{
		Spec: bpfman.LinkSpec{
			ID:        bpfman.LinkID(kernelLinkID),
			Kind:      linkKind,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   details,
			// ProgramID is set by the manager after this call
		},
		Status: bpfman.LinkStatus{
			Kernel:     kernelLink,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}, nil
}
