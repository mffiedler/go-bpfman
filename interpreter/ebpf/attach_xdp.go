package ebpf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/netns"
)

// AttachXDP attaches a pinned XDP program to a network interface.
func (k *kernelAdapter) AttachXDP(ctx context.Context, progPinPath string, ifindex int, linkPinPath string) (bpfman.Link, error) {
	prog, err := ebpf.LoadPinnedProgram(progPinPath, nil)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("load pinned program %s: %w", progPinPath, err)
	}
	defer prog.Close()

	lnk, err := link.AttachXDP(link.XDPOptions{
		Program:   prog,
		Interface: ifindex,
	})
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("attach XDP to ifindex %d: %w", ifindex, err)
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
			Kind:      bpfman.LinkKindXDP,
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.XDPDetails{Ifindex: uint32(ifindex)},
			// ProgramID is set by the manager after this call
		},
		Status: bpfman.LinkStatus{
			Kernel:     kernelLink,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}, nil
}

// AttachXDPDispatcher loads and attaches an XDP dispatcher to an interface.
// The dispatcher allows multiple XDP programs to be chained together.
func (k *kernelAdapter) AttachXDPDispatcher(ctx context.Context, spec dispatcher.XDPDispatcherAttachSpec) (*interpreter.XDPDispatcherResult, error) {
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

	var result *interpreter.XDPDispatcherResult
	err = netns.Run(spec.Target.NetNS, func() error {
		lnk, err := link.AttachXDP(link.XDPOptions{
			Program:   dispatcherProg,
			Interface: spec.Target.IfIndex,
		})
		if err != nil {
			return fmt.Errorf("attach XDP dispatcher to ifindex %d: %w", spec.Target.IfIndex, err)
		}

		result = &interpreter.XDPDispatcherResult{}

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

		// Pin dispatcher program to the revision-specific path
		if spec.ProgPinPath != "" {
			if err := os.MkdirAll(filepath.Dir(spec.ProgPinPath), 0755); err != nil {
				lnk.Close()
				return fmt.Errorf("create dispatcher program directory: %w", err)
			}
			if err := dispatcherProg.Pin(spec.ProgPinPath); err != nil {
				lnk.Close()
				return fmt.Errorf("pin dispatcher program to %s: %w", spec.ProgPinPath, err)
			}
			result.DispatcherPin = spec.ProgPinPath
		}

		// Pin link to the stable path (outside revision directory)
		if spec.LinkPinPath != "" {
			if err := os.MkdirAll(filepath.Dir(spec.LinkPinPath), 0755); err != nil {
				if spec.ProgPinPath != "" {
					if rmErr := os.Remove(spec.ProgPinPath); rmErr != nil && !os.IsNotExist(rmErr) {
						k.logger.Warn("failed to remove program pin during cleanup",
							"path", spec.ProgPinPath, "error", rmErr)
					}
				}
				lnk.Close()
				return fmt.Errorf("create link pin directory: %w", err)
			}
			if err := lnk.Pin(spec.LinkPinPath); err != nil {
				if spec.ProgPinPath != "" {
					if rmErr := os.Remove(spec.ProgPinPath); rmErr != nil && !os.IsNotExist(rmErr) {
						k.logger.Warn("failed to remove program pin during cleanup",
							"path", spec.ProgPinPath, "error", rmErr)
					}
				}
				lnk.Close()
				return fmt.Errorf("pin dispatcher link to %s: %w", spec.LinkPinPath, err)
			}
			result.LinkPin = spec.LinkPinPath
		}

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
//
// The mapPinDir parameter specifies the directory containing the program's
// pinned maps. These maps are loaded and passed as MapReplacements so the
// extension program shares the same maps as the original loaded program.
func (k *kernelAdapter) AttachXDPExtension(ctx context.Context, dispatcherPinPath, objectPath, programName string, position int, linkPinPath, mapPinDir string) (bpfman.Link, error) {
	// Load the pinned dispatcher to use as attach target
	dispatcherProg, err := ebpf.LoadPinnedProgram(dispatcherPinPath, nil)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("load pinned dispatcher %s: %w", dispatcherPinPath, err)
	}
	defer dispatcherProg.Close()

	// Load the collection spec from the ELF file
	collSpec, err := ebpf.LoadCollectionSpec(objectPath)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("load collection spec from %s: %w", objectPath, err)
	}

	// Verify the program exists in the collection
	progSpec, ok := collSpec.Programs[programName]
	if !ok {
		return bpfman.Link{}, fmt.Errorf("program %q not found in %s", programName, objectPath)
	}

	// Modify the program spec to be Extension type targeting the dispatcher
	progSpec.Type = ebpf.Extension
	progSpec.AttachTarget = dispatcherProg
	progSpec.AttachTo = dispatcher.SlotName(position)

	// Load pinned maps from the original program's map directory.
	// This ensures the extension program uses the same maps that were
	// created during the initial Load and are exposed via CSI.
	// We iterate over collSpec.Maps to get the exact ELF map names,
	// which must match the MapReplacements keys.
	mapReplacements := make(map[string]*ebpf.Map)
	if mapPinDir != "" {
		for name := range collSpec.Maps {
			// Skip internal maps (same filtering as Load)
			if strings.HasPrefix(name, ".") {
				continue
			}
			mapPath := filepath.Join(mapPinDir, name)
			m, err := ebpf.LoadPinnedMap(mapPath, nil)
			if err != nil {
				return bpfman.Link{}, fmt.Errorf("load pinned map %s: %w", mapPath, err)
			}
			mapReplacements[name] = m
			k.logger.Debug("loaded pinned map for extension", "name", name, "path", mapPath)
		}
	}

	// Ensure we close loaded maps on error or when done
	closeMapReplacements := func() {
		for _, m := range mapReplacements {
			m.Close()
		}
	}

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
		closeMapReplacements()
		return bpfman.Link{}, fmt.Errorf("load extension collection: %w", err)
	}
	defer coll.Close()
	// Note: maps in mapReplacements are now owned by the collection or
	// were used as replacements. We don't close them here as the collection
	// manages their lifecycle.

	// Get the loaded extension program
	extensionProg := coll.Programs[programName]
	if extensionProg == nil {
		return bpfman.Link{}, fmt.Errorf("extension program %q not in loaded collection", programName)
	}

	// Attach the extension using freplace link
	lnk, err := link.AttachFreplace(dispatcherProg, progSpec.AttachTo, extensionProg)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("attach freplace to %s: %w", progSpec.AttachTo, err)
	}

	// Pin the link if path provided
	if linkPinPath != "" {
		if err := pinWithRetry(lnk, linkPinPath); err != nil {
			lnk.Close()
			return bpfman.Link{}, fmt.Errorf("pin extension link to %s: %w", linkPinPath, err)
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
			Kind:      bpfman.LinkKindXDP, // XDP extension
			PinPath:   bpffs.NewLinkPath(linkPinPath),
			CreatedAt: time.Now(),
			Details:   bpfman.XDPDetails{Position: int32(position)},
			// ProgramID is set by the manager after this call
		},
		Status: bpfman.LinkStatus{
			Kernel:     kernelLink,
			KernelSeen: true,
			PinPresent: linkPinPath != "",
		},
	}, nil
}
