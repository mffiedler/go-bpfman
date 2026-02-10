package ebpf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/netns"
	"github.com/frobware/go-bpfman/platform"
)

// AttachXDP attaches a pinned XDP program to a network interface.
func (k *kernelAdapter) AttachXDP(ctx context.Context, progPinPath string, ifindex int, linkPinPath string) (bpfman.AttachOutput, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	lnk, err := link.AttachXDP(link.XDPOptions{
		Program:   prog,
		Interface: ifindex,
	})
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach XDP to ifindex %d: %w", ifindex, err)
	}

	success := false
	cleanup := func() {
		if !success {
			lnk.Close()
			if linkPinPath != "" {
				if err := os.Remove(linkPinPath); err != nil && !os.IsNotExist(err) {
					k.logger.Warn("failed to remove pinned link during cleanup",
						"path", linkPinPath, "error", err)
				}
			}
		}
	}
	defer cleanup()

	// Pin the link if a path is provided
	if linkPinPath != "" {
		if err := pinWithRetry(lnk, linkPinPath); err != nil {
			return bpfman.AttachOutput{}, fmt.Errorf("pin link to %s: %w", linkPinPath, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	success = true

	return bpfman.AttachOutput{
		LinkID:     uint32(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    linkPinPath,
	}, nil
}

// AttachXDPDispatcher loads and attaches an XDP dispatcher to an interface.
// The dispatcher allows multiple XDP programs to be chained together.
func (k *kernelAdapter) AttachXDPDispatcher(ctx context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*platform.XDPDispatcherResult, error) {
	// Configure the dispatcher
	// XDP_DISPATCHER_RETVAL (31) is returned by empty slots - we must include
	// this bit so the dispatcher continues past empty slots to the final XDP_PASS.
	const xdpDispatcherRetval = 31
	cfg := dispatcher.NewXDPConfig(spec.NumProgs)
	for i := 0; i < dispatcher.MaxPrograms; i++ {
		cfg.ChainCallActions[i] = spec.ProceedOn | (1 << xdpDispatcherRetval)
	}

	// Load the dispatcher collection spec with config injected
	collSpec, err := dispatcher.LoadXDPDispatcher(cfg)
	if err != nil {
		return nil, fmt.Errorf("load XDP dispatcher spec: %w", err)
	}

	// Create collection from spec
	coll, err := ebpf.NewCollection(collSpec)
	if err != nil {
		return nil, fmt.Errorf("create XDP dispatcher collection: %w", err)
	}
	defer coll.Close()

	// Get the dispatcher program
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
		lnk, err := link.AttachXDP(link.XDPOptions{
			Program:   dispatcherProg,
			Interface: spec.Target.IfIndex,
		})
		if err != nil {
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
		result.DispatcherID = uint32(progID)

		linkInfo, err := lnk.Info()
		if err != nil {
			lnk.Close()
			return fmt.Errorf("get dispatcher link info: %w", err)
		}
		result.LinkID = uint32(linkInfo.ID)

		// Pin dispatcher program to the revision-specific path.
		if err := pinWithRetry(dispatcherProg, spec.ProgPinPath); err != nil {
			lnk.Close()
			return fmt.Errorf("pin dispatcher program to %s: %w", spec.ProgPinPath, err)
		}
		result.DispatcherPin = spec.ProgPinPath

		// Pin link to the stable path (outside revision directory).
		if err := pinWithRetry(lnk, spec.LinkPinPath); err != nil {
			if rmErr := k.RemovePin(ctx, spec.ProgPinPath); rmErr != nil {
				k.logger.Warn("failed to remove program pin during cleanup",
					"path", spec.ProgPinPath, "error", rmErr)
			}
			lnk.Close()
			return fmt.Errorf("pin dispatcher link to %s: %w", spec.LinkPinPath, err)
		}
		result.LinkPin = spec.LinkPinPath

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// AttachXDPExtension loads a program from ELF as Extension type and attaches
// it to a dispatcher slot.
//
// This is different from simple XDP attachment - the program must be loaded
// specifically as BPF_PROG_TYPE_EXT with the dispatcher as the attach target.
// The same ELF bytecode used for direct XDP attachment is reloaded with
// different type settings.
func (k *kernelAdapter) AttachXDPExtension(ctx context.Context, spec dispatcher.XDPExtensionAttachSpec) (bpfman.AttachOutput, error) {
	if err := spec.Validate(); err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("invalid spec: %w", err)
	}

	// Load the pinned dispatcher to use as attach target
	dispatcherProg, err := ebpf.LoadPinnedProgram(spec.DispatcherPinPath, nil)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load pinned dispatcher %s: %w", spec.DispatcherPinPath, err)
	}
	defer dispatcherProg.Close()

	// Load the collection spec from the ELF file
	collSpec, err := ebpf.LoadCollectionSpec(spec.ObjectPath)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load collection spec from %s: %w", spec.ObjectPath, err)
	}

	// Verify the program exists in the collection
	progSpec, ok := collSpec.Programs[spec.ProgramName]
	if !ok {
		return bpfman.AttachOutput{}, fmt.Errorf("program %q not found in %s", spec.ProgramName, spec.ObjectPath)
	}

	// Modify the program spec to be Extension type targeting the dispatcher
	progSpec.Type = ebpf.Extension
	progSpec.AttachTarget = dispatcherProg
	progSpec.AttachTo = dispatcher.SlotName(spec.Position)

	// Load pinned maps from the original program's map directory.
	// This ensures the extension program uses the same maps that were
	// created during the initial Load and are exposed via CSI.
	// We iterate over collSpec.Maps to get the exact ELF map names,
	// which must match the MapReplacements keys.
	mapReplacements := make(map[string]*ebpf.Map)
	if spec.MapPinDir != "" {
		for name := range collSpec.Maps {
			// Skip internal maps (same filtering as Load)
			if strings.HasPrefix(name, ".") {
				continue
			}
			mapPath := filepath.Join(spec.MapPinDir, name)
			m, err := ebpf.LoadPinnedMap(mapPath, nil)
			if err != nil {
				// Close any maps we've already loaded before returning
				for _, loaded := range mapReplacements {
					loaded.Close()
				}
				return bpfman.AttachOutput{}, fmt.Errorf("load pinned map %s: %w", mapPath, err)
			}
			mapReplacements[name] = m
			k.logger.Debug("loaded pinned map for extension", "name", name, "path", mapPath)
		}
	}

	// Always close map FDs when done. Closing a pinned map's FD is safe -
	// it just releases our handle, not the underlying kernel object.
	defer func() {
		for _, m := range mapReplacements {
			m.Close()
		}
	}()

	// Clear map pinning flags - maps will come from MapReplacements
	for _, mapSpec := range collSpec.Maps {
		mapSpec.Pinning = ebpf.PinNone
	}

	// Load the collection with map replacements from the original program.
	// This ensures the extension uses the same maps that were pinned during Load.
	coll, err := ebpf.NewCollectionWithOptions(collSpec, ebpf.CollectionOptions{
		MapReplacements: mapReplacements,
	})
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("load extension collection: %w", err)
	}
	defer coll.Close()

	// Get the loaded extension program
	extensionProg := coll.Programs[spec.ProgramName]
	if extensionProg == nil {
		return bpfman.AttachOutput{}, fmt.Errorf("extension program %q not in loaded collection", spec.ProgramName)
	}

	// Attach the extension using freplace link
	lnk, err := link.AttachFreplace(dispatcherProg, progSpec.AttachTo, extensionProg)
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("attach freplace to %s: %w", progSpec.AttachTo, err)
	}

	success := false
	cleanup := func() {
		if !success {
			lnk.Close()
			if spec.LinkPinPath != "" {
				if err := os.Remove(spec.LinkPinPath); err != nil && !os.IsNotExist(err) {
					k.logger.Warn("failed to remove pinned extension link during cleanup",
						"path", spec.LinkPinPath, "error", err)
				}
			}
		}
	}
	defer cleanup()

	// Pin the link if path provided
	if spec.LinkPinPath != "" {
		if err := pinWithRetry(lnk, spec.LinkPinPath); err != nil {
			return bpfman.AttachOutput{}, fmt.Errorf("pin extension link to %s: %w", spec.LinkPinPath, err)
		}
	}

	// Get link info
	linkInfo, err := lnk.Info()
	if err != nil {
		return bpfman.AttachOutput{}, fmt.Errorf("get link info: %w", err)
	}

	success = true

	return bpfman.AttachOutput{
		LinkID:     uint32(linkInfo.ID),
		KernelLink: ToKernelLink(linkInfo),
		PinPath:    spec.LinkPinPath,
	}, nil
}
