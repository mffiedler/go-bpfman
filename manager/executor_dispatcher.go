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
		parent := tcParentHandle(dispType)
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

// attachXDPExtensionWithRetry attaches a user program as an extension
// to an XDP dispatcher slot. If the first attempt fails with
// os.ErrNotExist (stale dispatcher pin after bpffs remount), the
// dispatcher is deleted, recreated, and the attach retried once.
func attachXDPExtensionWithRetry(
	ctx context.Context,
	store platform.Store,
	kernel platform.KernelOperations,
	bpffs fs.BPFFS,
	logger *slog.Logger,
	ds dispatcher.State,
	netnsPath string,
	objectPath string,
	programName string,
	mapPinDir string,
) (extensionResult, error) {
	dispType := dispatcher.DispatcherTypeXDP

	position, err := store.CountDispatcherLinks(ctx, ds.KernelID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links: %w", err)
	}
	linkPinPath := bpffs.ExtensionLinkPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath := bpffs.DispatcherProgPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err := kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
		DispatcherPinPath: progPinPath,
		ObjectPath:        objectPath,
		ProgramName:       programName,
		Position:          position,
		LinkPinPath:       linkPinPath,
		MapPinDir:         mapPinDir,
	})
	if err == nil {
		return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
	}

	// Stale dispatcher recovery: pin missing after bpffs remount.
	if !errors.Is(err, os.ErrNotExist) {
		return extensionResult{}, fmt.Errorf("attach XDP extension slot %d: %w", position, err)
	}

	logger.WarnContext(ctx, "dispatcher pin missing, recreating",
		"prog_pin_path", progPinPath,
		"dispatcher_id", ds.KernelID,
		"error", err)

	if delErr := store.DeleteDispatcher(ctx, string(dispType), ds.Nsid, ds.Ifindex); delErr != nil {
		return extensionResult{}, fmt.Errorf("delete stale XDP dispatcher: %w", delErr)
	}
	ds, err = createXDPDispatcherHelper(ctx, store, kernel, bpffs, logger, ds.Nsid, ds.Ifindex, netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("recreate XDP dispatcher: %w", err)
	}

	position, err = store.CountDispatcherLinks(ctx, ds.KernelID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links after recreate: %w", err)
	}
	linkPinPath = bpffs.ExtensionLinkPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath = bpffs.DispatcherProgPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err = kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
		DispatcherPinPath: progPinPath,
		ObjectPath:        objectPath,
		ProgramName:       programName,
		Position:          position,
		LinkPinPath:       linkPinPath,
		MapPinDir:         mapPinDir,
	})
	if err != nil {
		return extensionResult{}, fmt.Errorf("attach XDP extension slot %d (after recreate): %w", position, err)
	}
	return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
}

// attachTCExtensionWithRetry attaches a user program as an extension
// to a TC dispatcher slot. Same stale-dispatcher recovery pattern as
// XDP.
func attachTCExtensionWithRetry(
	ctx context.Context,
	store platform.Store,
	kernel platform.KernelOperations,
	bpffs fs.BPFFS,
	logger *slog.Logger,
	ds dispatcher.State,
	ifname string,
	direction bpfman.TCDirection,
	dispType dispatcher.DispatcherType,
	netnsPath string,
	objectPath string,
	programName string,
	mapPinDir string,
) (extensionResult, error) {
	position, err := store.CountDispatcherLinks(ctx, ds.KernelID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links: %w", err)
	}
	linkPinPath := bpffs.ExtensionLinkPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath := bpffs.DispatcherProgPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err := kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
		DispatcherPinPath: progPinPath,
		ObjectPath:        objectPath,
		ProgramName:       programName,
		Position:          position,
		LinkPinPath:       linkPinPath,
		MapPinDir:         mapPinDir,
	})
	if err == nil {
		return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
	}

	// Stale dispatcher recovery: pin missing after bpffs remount.
	if !errors.Is(err, os.ErrNotExist) {
		return extensionResult{}, fmt.Errorf("attach TC extension slot %d: %w", position, err)
	}

	logger.WarnContext(ctx, "dispatcher pin missing, recreating",
		"prog_pin_path", progPinPath,
		"dispatcher_id", ds.KernelID,
		"error", err)

	if delErr := store.DeleteDispatcher(ctx, string(dispType), ds.Nsid, ds.Ifindex); delErr != nil {
		return extensionResult{}, fmt.Errorf("delete stale TC dispatcher: %w", delErr)
	}
	ds, err = createTCDispatcherHelper(ctx, store, kernel, bpffs, logger, ds.Nsid, ds.Ifindex, ifname, direction, dispType, netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("recreate TC dispatcher: %w", err)
	}

	position, err = store.CountDispatcherLinks(ctx, ds.KernelID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links after recreate: %w", err)
	}
	linkPinPath = bpffs.ExtensionLinkPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision, position)
	progPinPath = bpffs.DispatcherProgPath(dispType, ds.Nsid, ds.Ifindex, ds.Revision)

	out, err = kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
		DispatcherPinPath: progPinPath,
		ObjectPath:        objectPath,
		ProgramName:       programName,
		Position:          position,
		LinkPinPath:       linkPinPath,
		MapPinDir:         mapPinDir,
	})
	if err != nil {
		return extensionResult{}, fmt.Errorf("attach TC extension slot %d (after recreate): %w", position, err)
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
		Type:     dispType,
		Nsid:     nsid,
		Ifindex:  ifindex,
		Revision: revision,
		KernelID: result.DispatcherID,
		LinkID:   result.LinkID,
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
		Type:     dispType,
		Nsid:     nsid,
		Ifindex:  ifindex,
		Revision: revision,
		KernelID: result.DispatcherID,
		Priority: result.Priority,
	}
}
