package ebpf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
)

// attachXDPWithRetry retries link.AttachXDP on EBUSY. Removing a
// pinned XDP link drops the last kernel reference, but the kernel
// releases the XDP hook asynchronously. A brief retry avoids
// spurious failures when re-attaching to the same interface
// immediately after detach.
func attachXDPWithRetry(opts link.XDPOptions) (link.Link, error) {
	const (
		maxAttempts = 5
		baseDelay   = 50 * time.Millisecond
	)
	var lnk link.Link
	var err error
	for i := range maxAttempts {
		lnk, err = link.AttachXDP(opts)
		if err == nil || !errors.Is(err, syscall.EBUSY) {
			return lnk, err
		}
		time.Sleep(baseDelay << i)
	}
	return nil, err
}

// AttachXDP attaches a pinned XDP program to a network interface.
func (k *kernelAdapter) AttachXDP(ctx context.Context, progPinPath bpfman.ProgPinPath, ifindex int, linkPinPath bpfman.LinkPath) (bpfman.AttachOutput, error) {
	linkPin := linkPinPath.String()
	prog, err := ebpf.LoadPinnedProgram(progPinPath.String(), nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	lnk, err := attachXDPWithRetry(link.XDPOptions{
		Program:   prog,
		Interface: ifindex,
	})
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return bpfman.AttachOutput{}, fmt.Errorf("attach XDP to ifindex %d: interface already has an XDP program attached: %w", ifindex, err)
		}
		return bpfman.AttachOutput{}, fmt.Errorf("attach XDP to ifindex %d: %w", ifindex, err)
	}

	success := false
	cleanup := func() {
		if !success {
			lnk.Close()
			if linkPin != "" {
				if err := os.Remove(linkPin); err != nil && !os.IsNotExist(err) {
					k.logger.Warn("failed to remove pinned link during cleanup",
						"path", linkPin, "error", err)
				}
			}
		}
	}
	defer cleanup()

	// Pin the link if a path is provided
	if linkPin != "" {
		if err := pinWithRetry(linkPin, lnk.Pin); err != nil {
			return bpfman.AttachOutput{}, fmt.Errorf("pin link to %s: %w", linkPin, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	// Close the fd now that the link is pinned. The pin keeps the
	// kernel link alive; leaking the fd would prevent DetachLink
	// (which only removes the pin) from fully releasing the link.
	lnk.Close()
	success = true

	return bpfman.AttachOutput{
		LinkID:     kernel.LinkID(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    linkPinPath,
	}, nil
}

// AttachXDPDispatcher loads and attaches an XDP dispatcher to an interface.
// The dispatcher allows multiple XDP programs to be chained together.
// Uses .rodata-based config baked in at load time (full rebuild approach).
func (k *kernelAdapter) AttachXDPDispatcher(ctx context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*platform.XDPDispatcherResult, error) {
	// Configure the dispatcher .rodata for xdp-tools compatibility.
	const xdpDispatcherRetval = 31
	cfg, err := dispatcher.NewXDPConfig(spec.NumProgs)
	if err != nil {
		return nil, fmt.Errorf("create XDP dispatcher config: %w", err)
	}
	for i := range dispatcher.MaxPrograms {
		cfg.ChainCallActions[i] = spec.ProceedOn | (1 << xdpDispatcherRetval)
	}

	collSpec, err := dispatcher.LoadXDPDispatcher(cfg)
	if err != nil {
		return nil, fmt.Errorf("load XDP dispatcher spec: %w", err)
	}

	coll, err := ebpf.NewCollection(collSpec)
	if err != nil {
		return nil, fmt.Errorf("create XDP dispatcher collection: %w", err)
	}
	defer coll.Close()

	dispatcherProg := coll.Programs["xdp_dispatcher"]
	if dispatcherProg == nil {
		return nil, fmt.Errorf("xdp_dispatcher program not found in collection")
	}

	if spec.Target.NetNS != "" {
		k.logger.Debug("entering network namespace for XDP dispatcher attachment",
			"netns", spec.Target.NetNS, "ifindex", spec.Target.IfIndex)
	}

	var result *platform.XDPDispatcherResult
	err = netns.Run(spec.Target.NetNS, func() error {
		k.logger.Debug("attaching XDP dispatcher to interface",
			"ifindex", spec.Target.IfIndex,
			"netns", spec.Target.NetNS,
			"prog_pin_path", spec.ProgPinPath)
		lnk, err := attachXDPWithRetry(link.XDPOptions{
			Program:   dispatcherProg,
			Interface: spec.Target.IfIndex,
		})
		if err != nil {
			k.logger.Debug("XDP dispatcher attach failed",
				"ifindex", spec.Target.IfIndex,
				"error", err,
				"is_ebusy", errors.Is(err, syscall.EBUSY))
			if errors.Is(err, syscall.EBUSY) {
				return fmt.Errorf("attach XDP dispatcher to ifindex %d: interface already has an XDP program attached: %w", spec.Target.IfIndex, err)
			}
			return fmt.Errorf("attach XDP dispatcher to ifindex %d: %w", spec.Target.IfIndex, err)
		}

		result = &platform.XDPDispatcherResult{}

		progInfo, err := dispatcherProg.Info()
		if err != nil {
			lnk.Close()
			return fmt.Errorf("get dispatcher program info: %w", err)
		}
		progID, ok := progInfo.ID()
		if !ok {
			lnk.Close()
			return fmt.Errorf("failed to get dispatcher program ID from kernel")
		}
		result.DispatcherID = kernel.ProgramID(progID)

		linkInfo, err := lnk.Info()
		if err != nil {
			lnk.Close()
			return fmt.Errorf("get dispatcher link info: %w", err)
		}
		result.LinkID = kernel.LinkID(linkInfo.ID)

		// Pin dispatcher program to the revision-specific path.
		if err := pinWithRetry(spec.ProgPinPath, dispatcherProg.Pin); err != nil {
			lnk.Close()
			return fmt.Errorf("pin dispatcher program to %s: %w", spec.ProgPinPath, err)
		}
		result.DispatcherPin = spec.ProgPinPath

		// Pin link to the stable path (outside revision directory).
		if err := pinWithRetry(spec.LinkPinPath, lnk.Pin); err != nil {
			if rmErr := k.RemovePin(ctx, spec.ProgPinPath); rmErr != nil {
				k.logger.Warn("failed to remove program pin during cleanup",
					"path", spec.ProgPinPath, "error", rmErr)
			}
			lnk.Close()
			return fmt.Errorf("pin dispatcher link to %s: %w", spec.LinkPinPath, err)
		}
		result.LinkPin = spec.LinkPinPath

		// Close the fd now that the link is pinned.
		lnk.Close()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// UpdateXDPDispatcherLink atomically updates an existing XDP
// dispatcher's BPF link to point to a new dispatcher program.
// This is used during rebuild to swap from old to new dispatcher.
func (k *kernelAdapter) UpdateXDPDispatcherLink(ctx context.Context, linkPinPath bpfman.LinkPath, newProgPinPath bpfman.ProgPinPath) error {
	lnk, err := link.LoadPinnedLink(linkPinPath.String(), nil)
	if err != nil {
		return fmt.Errorf("load pinned link %s: %w", linkPinPath, err)
	}
	defer lnk.Close()

	newProg, err := ebpf.LoadPinnedProgram(newProgPinPath.String(), nil)
	if err != nil {
		return fmt.Errorf("load pinned program %s: %w", newProgPinPath, err)
	}
	defer newProg.Close()

	if err := lnk.Update(newProg); err != nil {
		return fmt.Errorf("update XDP link to new dispatcher: %w", err)
	}

	k.logger.Debug("updated XDP dispatcher link",
		"link_pin", linkPinPath,
		"new_prog_pin", newProgPinPath)
	return nil
}

// LoadAndPinXDPDispatcher loads an XDP dispatcher program with .rodata
// config and pins it at progPinPath without creating an XDP link.
// Used during rebuild to prepare a new dispatcher before atomically
// swapping the link.
func (k *kernelAdapter) LoadAndPinXDPDispatcher(ctx context.Context, cfg dispatcher.XDPConfig, progPinPath bpfman.ProgPinPath) (kernel.ProgramID, error) {
	collSpec, err := dispatcher.LoadXDPDispatcher(cfg)
	if err != nil {
		return 0, fmt.Errorf("load XDP dispatcher spec: %w", err)
	}

	coll, err := ebpf.NewCollection(collSpec)
	if err != nil {
		return 0, fmt.Errorf("create XDP dispatcher collection: %w", err)
	}
	defer coll.Close()

	dispatcherProg := coll.Programs["xdp_dispatcher"]
	if dispatcherProg == nil {
		return 0, fmt.Errorf("xdp_dispatcher program not found in collection")
	}

	progInfo, err := dispatcherProg.Info()
	if err != nil {
		return 0, fmt.Errorf("get dispatcher program info: %w", err)
	}
	progID, ok := progInfo.ID()
	if !ok {
		return 0, fmt.Errorf("failed to get dispatcher program ID from kernel")
	}

	if err := pinWithRetry(progPinPath, dispatcherProg.Pin); err != nil {
		return 0, fmt.Errorf("pin dispatcher program to %s: %w", progPinPath, err)
	}

	k.logger.Debug("loaded and pinned XDP dispatcher",
		"program_id", progID,
		"prog_pin_path", progPinPath,
		"num_progs", cfg.NumProgsEnabled)
	return kernel.ProgramID(progID), nil
}

// CreateXDPLink creates an XDP link from a pinned dispatcher program
// to a network interface, optionally in a specific network namespace.
func (k *kernelAdapter) CreateXDPLink(ctx context.Context, progPinPath bpfman.ProgPinPath, ifindex int, linkPinPath bpfman.LinkPath, netnsPath string) (*platform.XDPDispatcherResult, error) {
	linkPin := linkPinPath.String()
	prog, err := ebpf.LoadPinnedProgram(progPinPath.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	progInfo, err := prog.Info()
	if err != nil {
		return nil, fmt.Errorf("get program info: %w", err)
	}
	progID, ok := progInfo.ID()
	if !ok {
		return nil, fmt.Errorf("failed to get program ID from kernel")
	}

	if netnsPath != "" {
		k.logger.Debug("entering network namespace for XDP link creation",
			"netns", netnsPath, "ifindex", ifindex)
	}

	var result *platform.XDPDispatcherResult
	err = netns.Run(netnsPath, func() error {
		k.logger.Debug("creating XDP link",
			"ifindex", ifindex,
			"prog_pin_path", progPinPath,
			"link_pin_path", linkPinPath,
			"netns", netnsPath)
		lnk, err := attachXDPWithRetry(link.XDPOptions{
			Program:   prog,
			Interface: ifindex,
		})
		if err != nil {
			k.logger.Debug("XDP link creation failed",
				"ifindex", ifindex,
				"error", err,
				"is_ebusy", errors.Is(err, syscall.EBUSY))
			if errors.Is(err, syscall.EBUSY) {
				return fmt.Errorf("attach XDP to ifindex %d: interface already has an XDP program attached: %w", ifindex, err)
			}
			return fmt.Errorf("attach XDP to ifindex %d: %w", ifindex, err)
		}

		linkInfo, err := lnk.Info()
		if err != nil {
			lnk.Close()
			return fmt.Errorf("get link info: %w", err)
		}

		if err := pinWithRetry(linkPin, lnk.Pin); err != nil {
			lnk.Close()
			return fmt.Errorf("pin link to %s: %w", linkPin, err)
		}

		lnk.Close()
		result = &platform.XDPDispatcherResult{
			DispatcherID:  kernel.ProgramID(progID),
			LinkID:        kernel.LinkID(linkInfo.ID),
			DispatcherPin: progPinPath,
			LinkPin:       linkPinPath,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// AttachXDPExtension loads a pinned extension program and attaches it
// to a dispatcher slot via freplace. The extension was already loaded
// as BPF_PROG_TYPE_EXT during the initial Load, so no ELF re-read or
// map replacement is needed.
func (k *kernelAdapter) AttachXDPExtension(ctx context.Context, spec dispatcher.XDPExtensionAttachSpec) (bpfman.AttachOutput, error) {
	if err := spec.Validate(); err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("invalid spec: %w", err)
	}
	linkPin := spec.LinkPinPath.String()

	// Load the pinned dispatcher to use as attach target.
	dispatcherProg, err := ebpf.LoadPinnedProgram(spec.DispatcherPinPath.String(), nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned dispatcher %s: %w", spec.DispatcherPinPath, err)
	}
	defer dispatcherProg.Close()

	// Load the pinned extension program.
	extensionProg, err := ebpf.LoadPinnedProgram(spec.ProgPinPath.String(), nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned extension %s: %w", spec.ProgPinPath, err)
	}
	defer extensionProg.Close()

	// Attach the extension using freplace link.
	slotName, err := dispatcher.SlotName(spec.Position)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("slot name for position %d: %w", spec.Position, err)
	}
	lnk, err := link.AttachFreplace(dispatcherProg, slotName, extensionProg)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach freplace to %s: %w", slotName, err)
	}

	success := false
	cleanup := func() {
		if !success {
			lnk.Close()
			if linkPin != "" {
				if err := os.Remove(linkPin); err != nil && !os.IsNotExist(err) {
					k.logger.Warn("failed to remove pinned extension link during cleanup",
						"path", linkPin, "error", err)
				}
			}
		}
	}
	defer cleanup()

	// Pin the link if path provided.
	if linkPin != "" {
		if err := pinWithRetry(linkPin, lnk.Pin); err != nil {
			return bpfman.AttachOutput{}, fmt.Errorf("pin extension link to %s: %w", linkPin, err)
		}
	}

	// Get link info.
	linkInfo, err := lnk.Info()
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	// Close the fd now that the link is pinned. The pin keeps the
	// kernel link alive; leaking the fd would prevent DetachLink
	// (which only removes the pin) from fully releasing the
	// freplace trampoline.
	lnk.Close()
	success = true

	return bpfman.AttachOutput{
		LinkID:     kernel.LinkID(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    spec.LinkPinPath,
	}, nil
}
