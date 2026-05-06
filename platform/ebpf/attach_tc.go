package ebpf

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
)

// tcDispatcherPriority is the default TC priority for the dispatcher
// filter, matching the upstream Rust bpfman value.
const tcDispatcherPriority = 50

// AttachTCDispatcher loads and attaches a TC dispatcher to an interface
// using legacy netlink TC (clsact qdisc + BPF tc filter). This matches
// the upstream Rust bpfman approach: the dispatcher program is attached
// as a cls_bpf filter on the clsact qdisc, visible to tc(8) tooling,
// and works on kernels older than 6.6.
// Uses .rodata-based config baked in at load time (full rebuild approach).
func (k *kernelAdapter) AttachTCDispatcher(ctx context.Context, spec dispatcher.TCDispatcherAttachSpec) (*platform.TCDispatcherResult, error) {
	cfg, err := dispatcher.NewTCConfig(spec.NumProgs)
	if err != nil {
		return nil, fmt.Errorf("create TC dispatcher config: %w", err)
	}
	for i := 0; i < dispatcher.MaxPrograms; i++ {
		cfg.ChainCallActions[i] = spec.ProceedOn
	}

	collSpec, err := dispatcher.LoadTCDispatcher(cfg)
	if err != nil {
		return nil, fmt.Errorf("load TC dispatcher spec: %w", err)
	}

	coll, err := ebpf.NewCollection(collSpec)
	if err != nil {
		return nil, fmt.Errorf("create TC dispatcher collection: %w", err)
	}
	defer coll.Close()

	dispatcherProg := coll.Programs["tc_dispatcher"]
	if dispatcherProg == nil {
		return nil, fmt.Errorf("tc_dispatcher program not found in collection")
	}

	// Determine the parent handle based on direction
	var parent uint32
	switch spec.Direction {
	case bpfman.TCDirectionIngress:
		parent = netlink.HANDLE_MIN_INGRESS
	case bpfman.TCDirectionEgress:
		parent = netlink.HANDLE_MIN_EGRESS
	default:
		return nil, fmt.Errorf("invalid TC direction %q: must be ingress or egress", spec.Direction)
	}

	if spec.Target.NetNS != "" {
		k.logger.Debug("entering network namespace for TC dispatcher attachment",
			"netns", spec.Target.NetNS,
			"ifname", spec.IfName,
			"ifindex", spec.Target.IfIndex,
			"direction", spec.Direction)
	}

	var result *platform.TCDispatcherResult
	err = netns.Run(spec.Target.NetNS, func() error {
		// Step 1: Ensure clsact qdisc exists (matching Rust bpfman behaviour).
		qdisc := &netlink.Clsact{
			QdiscAttrs: netlink.QdiscAttrs{
				LinkIndex: spec.Target.IfIndex,
				Handle:    netlink.MakeHandle(0xffff, 0),
				Parent:    netlink.HANDLE_INGRESS,
			},
		}
		if err := netlink.QdiscAdd(qdisc); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("add clsact qdisc to %s (ifindex %d): %w", spec.IfName, spec.Target.IfIndex, err)
			}
		}

		// Step 2: Add a BPF tc filter with the dispatcher program.
		filter := &netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: spec.Target.IfIndex,
				Parent:    parent,
				Priority:  tcDispatcherPriority,
				Protocol:  unix.ETH_P_ALL,
			},
			Fd:           dispatcherProg.FD(),
			Name:         "tc_dispatcher",
			DirectAction: true,
		}
		if err := netlink.FilterAdd(filter); err != nil {
			return fmt.Errorf("add TC BPF filter to %s (ifindex %d) %s: %w",
				spec.IfName, spec.Target.IfIndex, spec.Direction, err)
		}

		// Step 3: Read back the kernel-assigned handle.
		handle, err := readBackTCFilterHandle(spec.Target.IfIndex, parent, tcDispatcherPriority)
		if err != nil {
			k.logger.Warn("TC filter added but handle readback failed; filter may be orphaned",
				"ifname", spec.IfName,
				"ifindex", spec.Target.IfIndex,
				"direction", spec.Direction,
				"error", err)
			return fmt.Errorf("read back TC filter handle: %w", err)
		}

		// Defer cleanup of the tc filter. On success we skip deletion;
		// on any error the filter is removed.
		success := false
		defer func() {
			if success {
				return
			}
			delFilter := &netlink.BpfFilter{
				FilterAttrs: netlink.FilterAttrs{
					LinkIndex: spec.Target.IfIndex,
					Parent:    parent,
					Handle:    handle,
					Priority:  tcDispatcherPriority,
					Protocol:  unix.ETH_P_ALL,
				},
			}
			if delErr := netlink.FilterDel(delFilter); delErr != nil {
				k.logger.Warn("failed to clean up TC filter after error",
					"ifname", spec.IfName,
					"ifindex", spec.Target.IfIndex,
					"handle", fmt.Sprintf("%x", handle),
					"error", delErr)
			}
		}()

		result = &platform.TCDispatcherResult{
			Handle:   handle,
			Priority: tcDispatcherPriority,
		}

		progInfo, err := dispatcherProg.Info()
		if err != nil {
			return fmt.Errorf("get TC dispatcher program info: %w", err)
		}
		progID, ok := progInfo.ID()
		if !ok {
			return fmt.Errorf("failed to get TC dispatcher program ID from kernel")
		}
		result.DispatcherID = kernel.ProgramID(progID)

		if err := pinWithRetry(spec.ProgPinPath, dispatcherProg.Pin); err != nil {
			return fmt.Errorf("pin TC dispatcher program to %s: %w", spec.ProgPinPath, err)
		}
		result.DispatcherPin = spec.ProgPinPath

		success = true
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// FindTCFilterHandle looks up the kernel-assigned handle for a TC BPF
// filter by listing filters on the given parent and matching priority.
func (k *kernelAdapter) FindTCFilterHandle(ctx context.Context, ifindex int, parent uint32, priority uint16) (uint32, error) {
	return readBackTCFilterHandle(ifindex, parent, priority)
}

// readBackTCFilterHandle lists tc filters on the given parent/priority
// and returns the handle of the first BPF filter found. This is
// needed because vishvananda/netlink FilterAdd does not echo back the
// kernel-assigned handle the way aya does with NLM_F_ECHO.
func readBackTCFilterHandle(ifindex int, parent uint32, priority uint16) (uint32, error) {
	lo := &netlink.Dummy{}
	lo.Index = ifindex
	filters, err := netlink.FilterList(lo, parent)
	if err != nil {
		return 0, fmt.Errorf("list filters on ifindex %d parent %x: %w", ifindex, parent, err)
	}
	for _, f := range filters {
		bpf, ok := f.(*netlink.BpfFilter)
		if !ok {
			continue
		}
		if bpf.Priority == priority {
			return bpf.Handle, nil
		}
	}
	return 0, fmt.Errorf("no BPF filter found at priority %d on ifindex %d parent %x", priority, ifindex, parent)
}

// DetachTCFilter removes a legacy TC BPF filter via netlink.
func (k *kernelAdapter) DetachTCFilter(ctx context.Context, ifindex int, ifname string, parent uint32, priority uint16, handle uint32) error {
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: ifindex,
			Parent:    parent,
			Handle:    handle,
			Priority:  priority,
			Protocol:  unix.ETH_P_ALL,
		},
	}
	if err := netlink.FilterDel(filter); err != nil {
		return fmt.Errorf("delete TC filter (ifindex=%d parent=%x prio=%d handle=%x): %w",
			ifindex, parent, priority, handle, err)
	}
	k.logger.Debug("detached TC filter",
		"ifindex", ifindex,
		"ifname", ifname,
		"parent", fmt.Sprintf("%x", parent),
		"priority", priority,
		"handle", fmt.Sprintf("%x", handle))
	return nil
}

// LoadAndPinTCDispatcher loads a TC dispatcher program with .rodata config
// and pins it at progPinPath without creating a TC filter. Used during
// rebuild to prepare a new dispatcher before atomically swapping.
func (k *kernelAdapter) LoadAndPinTCDispatcher(ctx context.Context, cfg dispatcher.TCConfig, progPinPath string) (kernel.ProgramID, error) {
	collSpec, err := dispatcher.LoadTCDispatcher(cfg)
	if err != nil {
		return 0, fmt.Errorf("load TC dispatcher spec: %w", err)
	}

	coll, err := ebpf.NewCollection(collSpec)
	if err != nil {
		return 0, fmt.Errorf("create TC dispatcher collection: %w", err)
	}
	defer coll.Close()

	dispatcherProg := coll.Programs["tc_dispatcher"]
	if dispatcherProg == nil {
		return 0, fmt.Errorf("tc_dispatcher program not found in collection")
	}

	progInfo, err := dispatcherProg.Info()
	if err != nil {
		return 0, fmt.Errorf("get TC dispatcher program info: %w", err)
	}
	progID, ok := progInfo.ID()
	if !ok {
		return 0, fmt.Errorf("failed to get TC dispatcher program ID from kernel")
	}

	if err := pinWithRetry(progPinPath, dispatcherProg.Pin); err != nil {
		return 0, fmt.Errorf("pin TC dispatcher program to %s: %w", progPinPath, err)
	}

	k.logger.Debug("loaded and pinned TC dispatcher",
		"program_id", progID,
		"prog_pin_path", progPinPath,
		"num_progs", cfg.NumProgsEnabled)
	return kernel.ProgramID(progID), nil
}

// CreateTCFilter creates a TC filter from a pinned dispatcher program
// on a network interface, optionally in a specific network namespace.
// Creates the clsact qdisc if needed.
func (k *kernelAdapter) CreateTCFilter(ctx context.Context, progPinPath string, ifindex int, ifname string, direction bpfman.TCDirection, netnsPath string) (*platform.TCDispatcherResult, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return nil, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	var parent uint32
	switch direction {
	case bpfman.TCDirectionIngress:
		parent = netlink.HANDLE_MIN_INGRESS
	case bpfman.TCDirectionEgress:
		parent = netlink.HANDLE_MIN_EGRESS
	default:
		return nil, fmt.Errorf("invalid TC direction %q: must be ingress or egress", direction)
	}

	if netnsPath != "" {
		k.logger.Debug("entering network namespace for TC filter creation",
			"netns", netnsPath, "ifname", ifname, "ifindex", ifindex, "direction", direction)
	}

	var result *platform.TCDispatcherResult
	err = netns.Run(netnsPath, func() error {
		qdisc := &netlink.Clsact{
			QdiscAttrs: netlink.QdiscAttrs{
				LinkIndex: ifindex,
				Handle:    netlink.MakeHandle(0xffff, 0),
				Parent:    netlink.HANDLE_INGRESS,
			},
		}
		if err := netlink.QdiscAdd(qdisc); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("add clsact qdisc to %s (ifindex %d): %w", ifname, ifindex, err)
			}
		}

		filter := &netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: ifindex,
				Parent:    parent,
				Priority:  tcDispatcherPriority,
				Protocol:  unix.ETH_P_ALL,
			},
			Fd:           prog.FD(),
			Name:         "tc_dispatcher",
			DirectAction: true,
		}
		if err := netlink.FilterAdd(filter); err != nil {
			return fmt.Errorf("add TC BPF filter to %s (ifindex %d) %s: %w",
				ifname, ifindex, direction, err)
		}

		handle, err := readBackTCFilterHandle(ifindex, parent, tcDispatcherPriority)
		if err != nil {
			return fmt.Errorf("read back TC filter handle: %w", err)
		}

		progInfo, err := prog.Info()
		if err != nil {
			return fmt.Errorf("get TC dispatcher program info: %w", err)
		}
		progID, ok := progInfo.ID()
		if !ok {
			return fmt.Errorf("failed to get TC dispatcher program ID from kernel")
		}

		result = &platform.TCDispatcherResult{
			DispatcherID:  kernel.ProgramID(progID),
			DispatcherPin: progPinPath,
			Handle:        handle,
			Priority:      tcDispatcherPriority,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// AttachTCExtension loads a pinned extension program and attaches it
// to a TC dispatcher slot via freplace. The extension was already
// loaded as BPF_PROG_TYPE_EXT during the initial Load, so no ELF
// re-read or map replacement is needed.
func (k *kernelAdapter) AttachTCExtension(ctx context.Context, spec dispatcher.TCExtensionAttachSpec) (bpfman.AttachOutput, error) {
	if err := spec.Validate(); err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("invalid spec: %w", err)
	}
	linkPin := string(spec.LinkPinPath)

	// Load the pinned dispatcher to use as attach target.
	dispatcherProg, err := ebpf.LoadPinnedProgram(spec.DispatcherPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned TC dispatcher %s: %w", spec.DispatcherPinPath, err)
	}
	defer dispatcherProg.Close()

	// Load the pinned extension program.
	extensionProg, err := ebpf.LoadPinnedProgram(spec.ProgPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned TC extension %s: %w", spec.ProgPinPath, err)
	}
	defer extensionProg.Close()

	// Attach the extension using freplace link.
	slotName, err := dispatcher.SlotName(spec.Position)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("slot name for position %d: %w", spec.Position, err)
	}
	lnk, err := link.AttachFreplace(dispatcherProg, slotName, extensionProg)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach TC freplace to %s: %w", slotName, err)
	}

	success := false
	cleanup := func() {
		if !success {
			lnk.Close()
			if linkPin != "" {
				if err := os.Remove(linkPin); err != nil && !os.IsNotExist(err) {
					k.logger.Warn("failed to remove pinned TC extension link during cleanup",
						"path", linkPin, "error", err)
				}
			}
		}
	}
	defer cleanup()

	// Pin the link if path provided.
	if linkPin != "" {
		if err := pinWithRetry(spec.LinkPinPath, lnk.Pin); err != nil {
			return bpfman.AttachOutput{}, fmt.Errorf("pin TC extension link to %s: %w", linkPin, err)
		}
	}

	// Get link info.
	linkInfo, err := lnk.Info()
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("get TC link info: %w", err)
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
		PinPath:    linkPin,
	}, nil
}

// AttachTCX attaches a loaded program directly to an interface using TCX link.
// Unlike TC which uses dispatchers, TCX uses native kernel multi-program support.
// The order parameter specifies where to insert the program in the TCX chain.
func (k *kernelAdapter) AttachTCX(ctx context.Context, ifindex int, direction, programPinPath string, linkPinPath bpfman.LinkPath, netnsPath string, order bpfman.TCXAttachOrder) (bpfman.AttachOutput, error) {
	linkPin := string(linkPinPath)
	// Load the pinned program
	prog, err := ebpf.LoadPinnedProgram(programPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", programPinPath, err)
	}
	defer prog.Close()

	// Determine attach type based on direction
	var attachType ebpf.AttachType
	switch direction {
	case "ingress":
		attachType = ebpf.AttachTCXIngress
	case "egress":
		attachType = ebpf.AttachTCXEgress
	default:
		return bpfman.AttachOutput{}, fmt.Errorf("invalid TCX direction %q: must be ingress or egress", direction)
	}

	// Convert TCXAttachOrder to cilium/ebpf link.Anchor
	var anchor link.Anchor
	switch {
	case order.First:
		anchor = link.Head()
	case order.Last:
		anchor = link.Tail()
	case order.BeforeProgID != 0:
		anchor = link.BeforeProgramByID(ebpf.ProgramID(order.BeforeProgID))
	case order.AfterProgID != 0:
		anchor = link.AfterProgramByID(ebpf.ProgramID(order.AfterProgID))
	default:
		// Default to head for safety - ensures new programs run before existing ones
		anchor = link.Head()
	}

	// Attach and pin in target namespace (if specified)
	if netnsPath != "" {
		k.logger.Debug("entering network namespace for TCX attachment", "netns", netnsPath, "ifindex", ifindex, "direction", direction)
	}

	var result bpfman.AttachOutput
	err = netns.Run(netnsPath, func() error {
		// Attach using TCX link with ordering anchor
		lnk, err := link.AttachTCX(link.TCXOptions{
			Interface: ifindex,
			Program:   prog,
			Attach:    attachType,
			Anchor:    anchor,
		})
		if err != nil {
			return fmt.Errorf("attach TCX to ifindex %d %s: %w", ifindex, direction, err)
		}

		success := false
		cleanup := func() {
			if !success {
				lnk.Close()
				if linkPin != "" {
					if err := os.Remove(linkPin); err != nil && !os.IsNotExist(err) {
						k.logger.Warn("failed to remove pinned TCX link during cleanup",
							"path", linkPin, "error", err)
					}
				}
			}
		}
		defer cleanup()

		// Pin the link if path provided
		if linkPin != "" {
			if err := pinWithRetry(linkPinPath, lnk.Pin); err != nil {
				return fmt.Errorf("pin TCX link to %s: %w", linkPin, err)
			}
		}

		// Get link info
		linkInfo, err := lnk.Info()
		if err != nil {
			return fmt.Errorf("get TCX link info: %w", err)
		}

		// Close the fd now that the link is pinned. The pin
		// keeps the kernel link alive; leaking the fd would
		// prevent DetachLink (which only removes the pin) from
		// fully releasing the link.
		lnk.Close()
		success = true
		result = bpfman.AttachOutput{
			LinkID:     kernel.LinkID(linkInfo.ID),
			KernelLink: ToKernelLink(linkInfo),
			PinPath:    linkPin,
		}
		return nil
	})
	if err != nil {
		return bpfman.AttachOutput{}, err
	}

	return result, nil
}
