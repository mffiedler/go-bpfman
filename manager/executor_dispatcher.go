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

// createXDPDispatcherHelper creates a new XDP dispatcher for the
// given interface. It performs kernel I/O (attach dispatcher), then
// persists state to the store. On store save failure, it rolls back
// the kernel artefacts directly (no self-recursion through the
// executor).
func createXDPDispatcherHelper(
	ctx context.Context,
	store platform.Store,
	kernel platform.KernelOperations,
	bpffs fs.BPFFS,
	logger *slog.Logger,
	nsid uint64,
	ifindex uint32,
	netnsPath string,
) (dispatcher.State, error) {
	revision := uint32(1)
	linkPinPath := bpffs.DispatcherLinkPath(dispatcher.DispatcherTypeXDP, nsid, ifindex)
	progPinPath := bpffs.DispatcherProgPath(dispatcher.DispatcherTypeXDP, nsid, ifindex, revision)

	logger.InfoContext(ctx, "creating XDP dispatcher",
		"nsid", nsid,
		"ifindex", ifindex,
		"netns", netnsPath,
		"revision", revision,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPinPath)

	spec := dispatcher.XDPDispatcherAttachSpec{
		Target: bpfman.AttachTarget{
			IfIndex: int(ifindex),
			NetNS:   netnsPath,
		},
		ProgPinPath: progPinPath,
		LinkPinPath: linkPinPath,
		NumProgs:    dispatcher.MaxPrograms,
		ProceedOn:   xdpProceedOnPass,
	}
	if err := spec.Validate(); err != nil {
		return dispatcher.State{}, fmt.Errorf("invalid XDP dispatcher spec: %w", err)
	}
	result, err := kernel.AttachXDPDispatcher(ctx, spec)
	if err != nil {
		return dispatcher.State{}, err
	}

	state := computeXDPDispatcherState(dispatcher.DispatcherTypeXDP, nsid, ifindex, revision, result)

	if err := store.SaveDispatcher(ctx, state); err != nil {
		logger.ErrorContext(ctx, "persist failed, rolling back XDP dispatcher",
			"ifindex", ifindex, "error", err)
		if rbErr := kernel.DetachLink(ctx, linkPinPath); rbErr != nil {
			logger.ErrorContext(ctx, "rollback: detach dispatcher link failed",
				"path", linkPinPath, "error", rbErr)
		}
		if rbErr := kernel.RemovePin(ctx, progPinPath); rbErr != nil {
			logger.ErrorContext(ctx, "rollback: remove prog pin failed",
				"path", progPinPath, "error", rbErr)
		}
		return dispatcher.State{}, fmt.Errorf("save dispatcher: %w", err)
	}

	logger.InfoContext(ctx, "created XDP dispatcher",
		"nsid", nsid,
		"ifindex", ifindex,
		"dispatcher_id", result.DispatcherID,
		"link_id", result.LinkID,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPinPath)

	return state, nil
}

// createTCDispatcherHelper creates a new TC dispatcher for the given
// interface and direction. Same pattern as XDP, but rollback uses
// DetachTCFilter instead of DetachLink.
func createTCDispatcherHelper(
	ctx context.Context,
	store platform.Store,
	kernel platform.KernelOperations,
	bpffs fs.BPFFS,
	logger *slog.Logger,
	nsid uint64,
	ifindex uint32,
	ifname string,
	direction bpfman.TCDirection,
	dispType dispatcher.DispatcherType,
	netnsPath string,
) (dispatcher.State, error) {
	revision := uint32(1)
	progPinPath := bpffs.DispatcherProgPath(dispType, nsid, ifindex, revision)

	logger.InfoContext(ctx, "creating TC dispatcher",
		"direction", direction,
		"nsid", nsid,
		"ifindex", ifindex,
		"ifname", ifname,
		"netns", netnsPath,
		"revision", revision,
		"prog_pin_path", progPinPath)

	spec := dispatcher.TCDispatcherAttachSpec{
		Target: bpfman.AttachTarget{
			IfIndex: int(ifindex),
			NetNS:   netnsPath,
		},
		IfName:      ifname,
		ProgPinPath: progPinPath,
		Direction:   direction,
		NumProgs:    dispatcher.MaxPrograms,
		ProceedOn:   uint32(DefaultTCProceedOn),
	}
	if err := spec.Validate(); err != nil {
		return dispatcher.State{}, fmt.Errorf("invalid TC dispatcher spec: %w", err)
	}
	result, err := kernel.AttachTCDispatcher(ctx, spec)
	if err != nil {
		return dispatcher.State{}, err
	}

	state := computeTCDispatcherState(dispType, nsid, ifindex, revision, result)

	if err := store.SaveDispatcher(ctx, state); err != nil {
		logger.ErrorContext(ctx, "persist failed, rolling back TC dispatcher",
			"ifname", ifname, "error", err)
		parent := dispatcher.TCParentHandle(dispType)
		if rbErr := kernel.DetachTCFilter(ctx, int(ifindex), ifname, parent, result.Priority, result.Handle); rbErr != nil {
			logger.ErrorContext(ctx, "rollback: detach TC filter failed",
				"ifname", ifname, "error", rbErr)
		}
		if rbErr := kernel.RemovePin(ctx, progPinPath); rbErr != nil {
			logger.ErrorContext(ctx, "rollback: remove prog pin failed",
				"path", progPinPath, "error", rbErr)
		}
		return dispatcher.State{}, fmt.Errorf("save TC dispatcher: %w", err)
	}

	logger.InfoContext(ctx, "created TC dispatcher",
		"direction", direction,
		"nsid", nsid,
		"ifindex", ifindex,
		"ifname", ifname,
		"dispatcher_id", result.DispatcherID,
		"handle", fmt.Sprintf("%x", result.Handle),
		"priority", result.Priority,
		"prog_pin_path", progPinPath)

	return state, nil
}

// extensionOps captures the two operations that vary between XDP and
// TC extension attach: the kernel attach call and the dispatcher
// recreation call. Everything else (slot counting, pin path
// construction, stale-dispatcher recovery) is identical.
type extensionOps struct {
	label    string
	dispType dispatcher.DispatcherType
	attach   func(ctx context.Context, dispatcherPinPath, objectPath, programName string,
		position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error)
	recreate func(ctx context.Context, nsid uint64, ifindex uint32) (dispatcher.State, error)
}

// attachExtensionWithRetry attaches a user program as an extension to
// a dispatcher slot. If the first attempt fails with os.ErrNotExist
// (stale dispatcher pin after bpffs remount), the dispatcher is
// deleted, recreated, and the attach retried once.
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
	position, err := store.CountDispatcherLinks(ctx, ds.ProgramID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links: %w", err)
	}
	linkPinPath := bpffs.ExtensionLinkPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath := bpffs.DispatcherProgPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err := ops.attach(ctx, progPinPath, objectPath, programName, position, linkPinPath, mapPinDir)
	if err == nil {
		return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
	}

	// Stale dispatcher recovery: pin missing after bpffs remount.
	if !errors.Is(err, os.ErrNotExist) {
		return extensionResult{}, fmt.Errorf("attach %s extension slot %d: %w", ops.label, position, err)
	}

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

	position, err = store.CountDispatcherLinks(ctx, ds.ProgramID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links after recreate: %w", err)
	}
	linkPinPath = bpffs.ExtensionLinkPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath = bpffs.DispatcherProgPath(ops.dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err = ops.attach(ctx, progPinPath, objectPath, programName, position, linkPinPath, mapPinDir)
	if err != nil {
		return extensionResult{}, fmt.Errorf("attach %s extension slot %d (after recreate): %w", ops.label, position, err)
	}
	return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
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
