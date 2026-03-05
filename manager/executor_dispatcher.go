// executor_dispatcher.go contains the composite dispatcher actions.
// Unlike the thin single-call wrappers in executor.go, each operation
// here is a multi-step transaction with its own rollback: create a
// kernel object, persist to the store, and undo the kernel operation
// if persistence fails. Extension attach adds stale-dispatcher
// recovery on top (detect missing pin, recreate, retry once).

package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/platform"
)

// dispatcherCreateOps captures the type-specific operations for
// dispatcher creation. The shared skeleton in createDispatcher handles
// revision initialisation, path computation, logging, store
// persistence, and rollback on persist failure.
type dispatcherCreateOps struct {
	label    string
	dispType dispatcher.DispatcherType
	nsid     uint64
	ifindex  uint32

	// kernelCreate builds the type-specific spec, validates it,
	// calls the kernel attach method, and computes the resulting
	// dispatcher state. On success it returns a rollback closure
	// and extra log attributes for the success message.
	kernelCreate func(ctx context.Context, progPinPath string) (dispatcherKernelResult, error)

	// creatingAttrs holds extra slog attributes appended to the
	// "creating dispatcher" log message.
	creatingAttrs []any
}

// dispatcherKernelResult bundles the output of a type-specific kernel
// attach: the computed state, a rollback closure for persist failure,
// and extra log attributes for the success message.
type dispatcherKernelResult struct {
	state        dispatcher.State
	rollback     func(ctx context.Context)
	createdAttrs []any
}

// createDispatcher is the shared skeleton for creating a new
// dispatcher. It computes the initial revision and program pin path,
// delegates the kernel attach to ops.kernelCreate, persists the
// resulting state to the store, and rolls back on persist failure.
func (e *executor) createDispatcher(
	ctx context.Context,
	ops dispatcherCreateOps,
) (dispatcher.State, error) {
	progPinPath := e.bpffs.DispatcherProgPath(ops.dispType, ops.nsid, ops.ifindex, 1)

	attrs := []any{
		"nsid", ops.nsid,
		"ifindex", ops.ifindex,
		"revision", uint32(1),
		"prog_pin_path", progPinPath,
	}
	attrs = append(attrs, ops.creatingAttrs...)
	e.logger.InfoContext(ctx, "creating "+ops.label+" dispatcher", attrs...)

	result, err := ops.kernelCreate(ctx, progPinPath)
	if err != nil {
		return dispatcher.State{}, err
	}

	if err := e.store.SaveDispatcher(ctx, result.state); err != nil {
		e.logger.ErrorContext(ctx, "persist failed, rolling back "+ops.label+" dispatcher",
			"ifindex", ops.ifindex, "error", err)
		result.rollback(ctx)
		return dispatcher.State{}, fmt.Errorf("save %s dispatcher: %w", ops.label, err)
	}

	attrs = []any{
		"nsid", ops.nsid,
		"ifindex", ops.ifindex,
		"dispatcher_id", result.state.ProgramID,
		"prog_pin_path", progPinPath,
	}
	attrs = append(attrs, result.createdAttrs...)
	e.logger.InfoContext(ctx, "created "+ops.label+" dispatcher", attrs...)

	return result.state, nil
}

// xdpDispatcherCreateOps returns the type-specific operations for
// creating an XDP dispatcher.
func (e *executor) xdpDispatcherCreateOps(
	nsid uint64,
	ifindex uint32,
	netnsPath string,
) dispatcherCreateOps {
	linkPinPath := e.bpffs.DispatcherLinkPath(dispatcher.DispatcherTypeXDP, nsid, ifindex)
	configMapPinPath := e.bpffs.DispatcherConfigMapPath(dispatcher.DispatcherTypeXDP, nsid, ifindex)
	activeMapPinPath := e.bpffs.DispatcherActiveMapPath(dispatcher.DispatcherTypeXDP, nsid, ifindex)
	return dispatcherCreateOps{
		label:    "XDP",
		dispType: dispatcher.DispatcherTypeXDP,
		nsid:     nsid,
		ifindex:  ifindex,
		creatingAttrs: []any{
			"netns", netnsPath,
			"link_pin_path", linkPinPath,
		},
		kernelCreate: func(ctx context.Context, progPinPath string) (dispatcherKernelResult, error) {
			spec := dispatcher.XDPDispatcherAttachSpec{
				Target: bpfman.AttachTarget{
					IfIndex: int(ifindex),
					NetNS:   netnsPath,
				},
				ProgPinPath:      progPinPath,
				LinkPinPath:      linkPinPath,
				ConfigMapPinPath: configMapPinPath,
				ActiveMapPinPath: activeMapPinPath,
				NumProgs:         dispatcher.MaxPrograms,
				ProceedOn:        xdpProceedOnPass,
			}
			if err := spec.Validate(); err != nil {
				return dispatcherKernelResult{}, fmt.Errorf("invalid XDP dispatcher spec: %w", err)
			}
			result, err := e.kernel.AttachXDPDispatcher(ctx, spec)
			if err != nil {
				return dispatcherKernelResult{}, err
			}
			state := computeXDPDispatcherState(dispatcher.DispatcherTypeXDP, nsid, ifindex, 1, result)
			return dispatcherKernelResult{
				state: state,
				rollback: func(ctx context.Context) {
					if rbErr := e.kernel.DetachLink(ctx, linkPinPath); rbErr != nil {
						e.logger.ErrorContext(ctx, "rollback: detach dispatcher link failed",
							"path", linkPinPath, "error", rbErr)
					}
					if rbErr := e.kernel.RemovePin(ctx, progPinPath); rbErr != nil {
						e.logger.ErrorContext(ctx, "rollback: remove prog pin failed",
							"path", progPinPath, "error", rbErr)
					}
				},
				createdAttrs: []any{
					"link_id", result.LinkID,
					"link_pin_path", linkPinPath,
				},
			}, nil
		},
	}
}

// tcDispatcherCreateOps returns the type-specific operations for
// creating a TC dispatcher.
func (e *executor) tcDispatcherCreateOps(
	nsid uint64,
	ifindex uint32,
	ifname string,
	direction bpfman.TCDirection,
	dispType dispatcher.DispatcherType,
	netnsPath string,
) dispatcherCreateOps {
	configMapPinPath := e.bpffs.DispatcherConfigMapPath(dispType, nsid, ifindex)
	activeMapPinPath := e.bpffs.DispatcherActiveMapPath(dispType, nsid, ifindex)
	return dispatcherCreateOps{
		label:    "TC",
		dispType: dispType,
		nsid:     nsid,
		ifindex:  ifindex,
		creatingAttrs: []any{
			"direction", direction,
			"ifname", ifname,
			"netns", netnsPath,
		},
		kernelCreate: func(ctx context.Context, progPinPath string) (dispatcherKernelResult, error) {
			spec := dispatcher.TCDispatcherAttachSpec{
				Target: bpfman.AttachTarget{
					IfIndex: int(ifindex),
					NetNS:   netnsPath,
				},
				IfName:           ifname,
				ProgPinPath:      progPinPath,
				ConfigMapPinPath: configMapPinPath,
				ActiveMapPinPath: activeMapPinPath,
				Direction:        direction,
				NumProgs:         dispatcher.MaxPrograms,
				ProceedOn:        uint32(DefaultTCProceedOn),
			}
			if err := spec.Validate(); err != nil {
				return dispatcherKernelResult{}, fmt.Errorf("invalid TC dispatcher spec: %w", err)
			}
			result, err := e.kernel.AttachTCDispatcher(ctx, spec)
			if err != nil {
				return dispatcherKernelResult{}, err
			}
			state := computeTCDispatcherState(dispType, nsid, ifindex, 1, result)
			return dispatcherKernelResult{
				state: state,
				rollback: func(ctx context.Context) {
					parent := dispatcher.TCParentHandle(dispType)
					if rbErr := e.kernel.DetachTCFilter(ctx, int(ifindex), ifname, parent, result.Priority, result.Handle); rbErr != nil {
						e.logger.ErrorContext(ctx, "rollback: detach TC filter failed",
							"ifname", ifname, "error", rbErr)
					}
					if rbErr := e.kernel.RemovePin(ctx, progPinPath); rbErr != nil {
						e.logger.ErrorContext(ctx, "rollback: remove prog pin failed",
							"path", progPinPath, "error", rbErr)
					}
				},
				createdAttrs: []any{
					"direction", direction,
					"ifname", ifname,
					"handle", fmt.Sprintf("%x", result.Handle),
					"priority", result.Priority,
				},
			}, nil
		},
	}
}

// computeXDPDispatcherState is a pure function that builds a
// dispatcher.State from kernel attach results.
func computeXDPDispatcherState(
	dispType dispatcher.DispatcherType,
	nsid uint64,
	ifindex, revision uint32,
	result *platform.XDPDispatcherResult,
) dispatcher.State {
	return dispatcher.State{
		Type:      dispType,
		Nsid:      nsid,
		Ifindex:   ifindex,
		Revision:  revision,
		ProgramID: result.DispatcherID,
		LinkID:    result.LinkID,
	}
}

// computeTCDispatcherState is a pure function that builds a
// dispatcher.State from TC kernel attach results.
func computeTCDispatcherState(
	dispType dispatcher.DispatcherType,
	nsid uint64,
	ifindex, revision uint32,
	result *platform.TCDispatcherResult,
) dispatcher.State {
	return dispatcher.State{
		Type:      dispType,
		Nsid:      nsid,
		Ifindex:   ifindex,
		Revision:  revision,
		ProgramID: result.DispatcherID,
		Priority:  result.Priority,
	}
}

// extensionOps captures the type-specific operations for extension
// attach. The shared skeleton handles slot allocation, pin path
// construction, stale-dispatcher recovery, and runtime config update.
type extensionOps struct {
	label    string
	dispType dispatcher.DispatcherType
	attach   func(ctx context.Context, dispatcherPinPath, objectPath, programName string,
		position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error)
	recreate     func(ctx context.Context, nsid uint64, ifindex uint32) (dispatcher.State, error)
	updateConfig func(ctx context.Context, configMapPin, activeMapPin string, config dispatcher.RuntimeConfig) error
	priority     int
	proceedOn    uint32
}

// attachExtensionWithRetry attaches a user program as an extension to
// a dispatcher slot. It finds the next free physical slot, attaches
// the extension, then computes the priority-based run_order and
// updates the dispatcher's runtime config via double-buffer flip.
//
// If the first attempt fails with os.ErrNotExist (stale dispatcher
// pin after bpffs remount), the dispatcher is deleted, recreated,
// and the attach retried once.
func attachExtensionWithRetry(
	ctx context.Context,
	store platform.Store,
	bpffs fs.BPFFS,
	logger *slog.Logger,
	ops extensionOps,
	ds dispatcher.State,
	objectPath string,
	programName string,
	mapPinDir string,
) (extensionResult, error) {
	result, err := tryAttachExtension(ctx, store, bpffs, logger, ops, ds, objectPath, programName, mapPinDir)
	if err == nil {
		return result, nil
	}

	// Stale dispatcher recovery: pin missing after bpffs remount.
	if !errors.Is(err, os.ErrNotExist) {
		return extensionResult{}, err
	}

	progPinPath := bpffs.DispatcherProgPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision)
	logger.WarnContext(ctx, "dispatcher pin missing, recreating",
		"prog_pin_path", progPinPath,
		"dispatcher_id", ds.ProgramID,
		"error", err)

	if delErr := store.DeleteDispatcher(ctx, ops.dispType, ds.Nsid, ds.Ifindex); delErr != nil {
		return extensionResult{}, fmt.Errorf("delete stale %s dispatcher: %w", ops.label, delErr)
	}
	ds, err = ops.recreate(ctx, ds.Nsid, ds.Ifindex)
	if err != nil {
		return extensionResult{}, fmt.Errorf("recreate %s dispatcher: %w", ops.label, err)
	}

	return tryAttachExtension(ctx, store, bpffs, logger, ops, ds, objectPath, programName, mapPinDir)
}

// tryAttachExtension performs a single attempt at extension attach:
// find free slot, attach, update runtime config.
func tryAttachExtension(
	ctx context.Context,
	store platform.Store,
	bpffs fs.BPFFS,
	_ *slog.Logger,
	ops extensionOps,
	ds dispatcher.State,
	objectPath string,
	programName string,
	mapPinDir string,
) (extensionResult, error) {
	// Find the next free physical slot by querying occupied slots.
	slots, err := store.ListDispatcherSlots(ctx, ds.ProgramID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("list dispatcher slots: %w", err)
	}
	position := findFreeSlot(slots)
	if position < 0 {
		return extensionResult{}, fmt.Errorf("no free dispatcher slots (all %d occupied)", dispatcher.MaxPrograms)
	}

	linkPinPath := bpffs.ExtensionLinkPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath := bpffs.DispatcherProgPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err := ops.attach(ctx, progPinPath, objectPath, programName, position, linkPinPath, mapPinDir)
	if err != nil {
		return extensionResult{}, fmt.Errorf("attach %s extension slot %d: %w", ops.label, position, err)
	}

	// After successful attach, compute the new runtime config
	// including the just-attached slot and update the BPF maps.
	if ops.updateConfig != nil {
		newSlot := platform.DispatcherSlot{
			Position:    position,
			Priority:    ops.priority,
			ProgramName: programName,
			ProceedOn:   ops.proceedOn,
		}
		config := computeRuntimeConfig(slots, &newSlot)
		configMapPin := bpffs.DispatcherConfigMapPath(ops.dispType, ds.Nsid, ds.Ifindex)
		activeMapPin := bpffs.DispatcherActiveMapPath(ops.dispType, ds.Nsid, ds.Ifindex)
		if err := ops.updateConfig(ctx, configMapPin, activeMapPin, config); err != nil {
			return extensionResult{}, fmt.Errorf("update dispatcher config after attach (slot %d): %w", position, err)
		}
	}

	return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
}

// findFreeSlot returns the lowest unoccupied slot index [0, MaxPrograms).
// Returns -1 if all slots are occupied.
func findFreeSlot(occupied []platform.DispatcherSlot) int {
	used := make(map[int]bool, len(occupied))
	for _, s := range occupied {
		used[s.Position] = true
	}
	for i := range dispatcher.MaxPrograms {
		if !used[i] {
			return i
		}
	}
	return -1
}

// computeRuntimeConfig builds a RuntimeConfig from the currently
// occupied slots plus an optional new slot. Slots are sorted by
// (priority, program_name) to determine run_order.
func computeRuntimeConfig(existing []platform.DispatcherSlot, newSlot *platform.DispatcherSlot) dispatcher.RuntimeConfig {
	// Combine existing slots with the new one (if any).
	all := make([]platform.DispatcherSlot, len(existing))
	copy(all, existing)
	if newSlot != nil {
		all = append(all, *newSlot)
	}

	// Sort by (priority, program_name) to match Rust bpfman ordering.
	sortSlotsByPriority(all)

	var cfg dispatcher.RuntimeConfig
	cfg.NumProgsEnabled = uint32(len(all))
	for i, slot := range all {
		cfg.RunOrder[i] = uint32(slot.Position)
		cfg.ChainCallActions[slot.Position] = slot.ProceedOn
	}
	return cfg
}

// sortSlotsByPriority sorts slots by (priority ASC, program_name ASC).
func sortSlotsByPriority(slots []platform.DispatcherSlot) {
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0; j-- {
			if slots[j].Priority < slots[j-1].Priority ||
				(slots[j].Priority == slots[j-1].Priority &&
					slots[j].ProgramName < slots[j-1].ProgramName) {
				slots[j], slots[j-1] = slots[j-1], slots[j]
			} else {
				break
			}
		}
	}
}
