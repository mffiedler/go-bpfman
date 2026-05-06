// executor_dispatcher.go contains the full-rebuild dispatcher logic.
//
// Every XDP/TC attach or detach that changes the set of extensions
// triggers a full dispatcher rebuild: a new dispatcher program is
// loaded with updated .rodata config, all extensions are re-attached
// to it, and the link (XDP) or filter (TC) is atomically swapped.
// This matches the upstream Rust bpfman approach.

package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
)

// rebuildSlot carries per-extension data for the rebuild.
type rebuildSlot struct {
	ProgPinPath string
	ProgramName string
	Priority    int // user-specified priority (may be 0 for unspecified)
	ProceedOn   uint32
	LinkID      kernel.LinkID    // existing synthetic link ID (zero for new)
	ProgramID   kernel.ProgramID // managed program's kernel ID
	Ifname      string           // interface name from detail record
}

// xdpRebuildOps returns the type-specific operations for XDP
// dispatcher rebuild.
type xdpRebuildOps struct {
	ifindex   uint32
	ifname    string
	netnsPath string
}

// tcRebuildOps returns the type-specific operations for TC
// dispatcher rebuild.
type tcRebuildOps struct {
	ifindex   uint32
	ifname    string
	direction bpfman.TCDirection
	dispType  dispatcher.DispatcherType
	netnsPath string
}

// rebuildXDPDispatcher performs a full XDP dispatcher rebuild.
// It handles both first-attach (no dispatcher exists) and
// subsequent-attach (dispatcher exists, rebuild all extensions).
// The managedProgramID identifies the user program being attached.
func (e *executor) rebuildXDPDispatcher(
	ctx context.Context,
	managedProgramID kernel.ProgramID,
	ops xdpRebuildOps,
	progPinPath string,
	programName string,
	priority int,
	proceedOn uint32,
) (extensionResult, error) {
	newSlot := rebuildSlot{
		ProgPinPath: progPinPath,
		ProgramName: programName,
		Priority:    priority,
		ProceedOn:   proceedOn,
		ProgramID:   managedProgramID,
		Ifname:      ops.ifname,
	}

	nsid, err := netns.GetNsid(ops.netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("get nsid: %w", err)
	}

	dispType := dispatcher.DispatcherTypeXDP
	key := dispatcher.Key{Type: dispType, Nsid: nsid, Ifindex: ops.ifindex}

	// Query existing dispatcher snapshot (may not exist).
	snap, err := e.store.GetDispatcherSnapshot(ctx, key)
	firstAttach := false
	if err != nil {
		if !isNotFound(err) {
			return extensionResult{}, fmt.Errorf("get dispatcher: %w", err)
		}
		firstAttach = true
	}

	// Build the full set of extensions (existing + new).
	allSlots := make([]rebuildSlot, 0, len(snap.Members)+1)
	for _, m := range snap.Members {
		allSlots = append(allSlots, rebuildSlot{
			ProgPinPath: m.ProgPinPath,
			ProgramName: m.ProgramName,
			Priority:    m.Priority,
			ProceedOn:   m.ProceedOn,
			LinkID:      m.LinkID,
			ProgramID:   m.ProgramID,
			Ifname:      m.Ifname,
		})
	}
	allSlots = append(allSlots, newSlot)

	if len(allSlots) > dispatcher.MaxPrograms {
		return extensionResult{}, fmt.Errorf("no free dispatcher slots (all %d occupied)", dispatcher.MaxPrograms)
	}

	// Sort by (priority, programName) to determine positions.
	sortRebuildSlots(allSlots)

	// Compute .rodata config.
	const xdpDispatcherRetval = 31
	cfg, err := dispatcher.NewXDPConfig(len(allSlots))
	if err != nil {
		return extensionResult{}, fmt.Errorf("create XDP dispatcher config: %w", err)
	}
	for i, slot := range allSlots {
		cfg.ChainCallActions[i] = slot.ProceedOn | (1 << xdpDispatcherRetval)
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	// Compute new revision.
	var revision uint32
	if firstAttach {
		revision = 1
	} else {
		revision = snap.Revision + 1
	}

	dispProgPinPath := e.bpffs.DispatcherProgPath(dispType, nsid, ops.ifindex, revision)

	e.logger.InfoContext(ctx, "rebuilding XDP dispatcher",
		"nsid", nsid,
		"ifindex", ops.ifindex,
		"revision", revision,
		"num_extensions", len(allSlots),
		"first_attach", firstAttach)

	// Load new dispatcher with .rodata config.
	dispatcherID, err := e.kernel.LoadAndPinXDPDispatcher(ctx, cfg, dispProgPinPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("load XDP dispatcher: %w", err)
	}

	// Track cleanup for rollback on failure.
	cleanupNewDispatcher := func() {
		if rbErr := e.kernel.RemovePin(ctx, dispProgPinPath); rbErr != nil {
			e.logger.ErrorContext(ctx, "rollback: remove new dispatcher pin failed",
				"path", dispProgPinPath, "error", rbErr)
		}
	}

	// Attach all extensions to the new dispatcher.
	type attachedExt struct {
		out      bpfman.AttachOutput
		position int
		pinPath  bpfman.LinkPath
	}
	attached := make([]attachedExt, 0, len(allSlots))

	cleanupExtensions := func() {
		for _, ext := range attached {
			if ext.pinPath != "" {
				if rbErr := e.kernel.DetachLink(ctx, ext.pinPath); rbErr != nil {
					e.logger.ErrorContext(ctx, "rollback: detach extension link failed",
						"path", ext.pinPath, "error", rbErr)
				}
			}
		}
	}

	// Find which slot is the new one (the one we're adding).
	newSlotPosition := -1
	for i, slot := range allSlots {
		linkPinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ops.ifindex, revision, i)

		out, err := e.kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
			DispatcherPinPath: dispProgPinPath,
			ProgPinPath:       slot.ProgPinPath,
			ProgramName:       slot.ProgramName,
			Position:          i,
			LinkPinPath:       linkPinPath,
		})
		if err != nil {
			cleanupExtensions()
			cleanupNewDispatcher()
			return extensionResult{}, fmt.Errorf("attach XDP extension %s at position %d: %w", slot.ProgramName, i, err)
		}

		attached = append(attached, attachedExt{out: out, position: i, pinPath: linkPinPath})

		if slot.LinkID == 0 {
			newSlotPosition = i
		}
	}

	// Diagnostic: read each just-pinned freplace link back via
	// BPF_LINK_GET_INFO_BY_FD. This forces a syscall round-trip per
	// link before the dispatcher swap, giving the kernel another
	// chance to publish trampoline state, and surfaces any slot
	// whose target_obj_id does not match the new dispatcher (which
	// would mean the freplace is bound to a stale program). Errors
	// are logged but do not abort the rebuild.
	for _, ext := range attached {
		info, infoErr := e.kernel.ExtensionLinkInfo(ctx, ext.pinPath)
		if infoErr != nil {
			e.logger.WarnContext(ctx, "verify: extension link info failed",
				"type", dispType.String(),
				"ifindex", ops.ifindex,
				"revision", revision,
				"position", ext.position,
				"path", ext.pinPath,
				"error", infoErr)
			continue
		}
		e.logger.InfoContext(ctx, "verify: extension link",
			"type", dispType.String(),
			"ifindex", ops.ifindex,
			"revision", revision,
			"position", ext.position,
			"link_id", uint64(info.LinkID),
			"target_prog_id", uint64(info.TargetProgID),
			"target_btf_id", info.TargetBtfID,
			"attach_type", info.AttachType,
			"matches_dispatcher", uint64(info.TargetProgID) == uint64(dispatcherID))
	}

	// Atomic swap: create link (first-attach) or update existing link.
	dispLinkPinPath := e.bpffs.DispatcherLinkPath(dispType, nsid, ops.ifindex)
	var linkID kernel.LinkID

	if firstAttach {
		result, err := e.kernel.CreateXDPLink(ctx, dispProgPinPath, int(ops.ifindex), dispLinkPinPath, ops.netnsPath)
		if err != nil {
			cleanupExtensions()
			cleanupNewDispatcher()
			return extensionResult{}, fmt.Errorf("create XDP link: %w", err)
		}
		linkID = result.LinkID
	} else {
		if err := e.kernel.UpdateXDPDispatcherLink(ctx, dispLinkPinPath, dispProgPinPath); err != nil {
			cleanupExtensions()
			cleanupNewDispatcher()
			return extensionResult{}, fmt.Errorf("update XDP dispatcher link: %w", err)
		}
		if snap.Runtime.LinkID != nil {
			linkID = *snap.Runtime.LinkID
		}
	}

	// Build snapshot with all members and persist atomically.
	newSnap := platform.DispatcherSnapshot{
		Key:      key,
		Revision: revision,
		Runtime: platform.DispatcherRuntime{
			ProgramID: dispatcherID,
			LinkID:    &linkID,
		},
	}
	for i, slot := range allSlots {
		extLinkID := attached[i].out.LinkID
		if slot.LinkID != 0 {
			extLinkID = slot.LinkID
		}
		newSnap.Members = append(newSnap.Members, platform.DispatcherMember{
			ProgramID:   slot.ProgramID,
			ProgramName: slot.ProgramName,
			ProgPinPath: slot.ProgPinPath,
			LinkID:      extLinkID,
			LinkPinPath: attached[i].pinPath,
			Position:    i,
			Priority:    slot.Priority,
			ProceedOn:   slot.ProceedOn,
			Ifname:      slot.Ifname,
		})
	}

	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		return tx.ReplaceDispatcherSnapshot(ctx, newSnap)
	}); err != nil {
		e.logger.ErrorContext(ctx, "persist failed, rolling back XDP dispatcher",
			"ifindex", ops.ifindex, "error", err)
		cleanupExtensions()
		if firstAttach {
			if rbErr := e.kernel.DetachLink(ctx, dispLinkPinPath); rbErr != nil {
				e.logger.ErrorContext(ctx, "rollback: detach dispatcher link failed",
					"path", dispLinkPinPath, "error", rbErr)
			}
		}
		cleanupNewDispatcher()
		return extensionResult{}, err
	}

	// Clean up old revision directory (if not first-attach).
	if !firstAttach {
		oldRevDir := e.bpffs.DispatcherRevisionDir(dispType, nsid, ops.ifindex, snap.Revision)
		if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old revision directory",
				"path", oldRevDir, "error", err)
		}
	}

	// Find the new extension's attach output.
	if newSlotPosition < 0 {
		newSlotPosition = len(attached) - 1
	}
	newExt := attached[newSlotPosition]

	e.logger.InfoContext(ctx, "rebuilt XDP dispatcher",
		"nsid", nsid,
		"ifindex", ops.ifindex,
		"revision", revision,
		"dispatcher_id", dispatcherID,
		"num_extensions", len(allSlots),
		"new_position", newSlotPosition)

	// Construct the bpfman.Link for the new extension.
	newExtLinkRecord := bpfman.NewPinnedLinkRecord(
		newExt.out.LinkID,
		managedProgramID,
		bpfman.XDPDetails{
			Interface:    newSlot.Ifname,
			Ifindex:      ops.ifindex,
			Priority:     int32(newSlot.Priority),
			Position:     int32(newSlotPosition),
			ProceedOn:    bitmaskToActions(newSlot.ProceedOn),
			Nsid:         nsid,
			DispatcherID: dispatcherID,
			Revision:     revision,
		},
		*bpfman.NewLinkPath(newExt.pinPath),
		time.Now(),
	)

	return extensionResult{
		link: bpfman.Link{
			Record: newExtLinkRecord,
			Status: bpfman.LinkStatus{
				Kernel:     newExt.out.KernelLink,
				KernelSeen: newExt.out.KernelLink != nil,
				PinPresent: newExt.pinPath != "" && !newExt.out.Synthetic,
			},
		},
		key:      key,
		revision: revision,
		position: newSlotPosition,
		pinPath:  newExt.pinPath,
	}, nil
}

// rebuildTCDispatcher performs a full TC dispatcher rebuild.
// Same semantics as rebuildXDPDispatcher but for TC dispatchers.
// The managedProgramID identifies the user program being attached.
func (e *executor) rebuildTCDispatcher(
	ctx context.Context,
	managedProgramID kernel.ProgramID,
	ops tcRebuildOps,
	progPinPath string,
	programName string,
	priority int,
	proceedOn uint32,
) (extensionResult, error) {
	newSlot := rebuildSlot{
		ProgPinPath: progPinPath,
		ProgramName: programName,
		Priority:    priority,
		ProceedOn:   proceedOn,
		ProgramID:   managedProgramID,
		Ifname:      ops.ifname,
	}

	nsid, err := netns.GetNsid(ops.netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("get nsid: %w", err)
	}

	dispType := ops.dispType
	key := dispatcher.Key{Type: dispType, Nsid: nsid, Ifindex: ops.ifindex}

	// Query existing dispatcher snapshot (may not exist).
	snap, err := e.store.GetDispatcherSnapshot(ctx, key)
	firstAttach := false
	if err != nil {
		if !isNotFound(err) {
			return extensionResult{}, fmt.Errorf("get dispatcher: %w", err)
		}
		firstAttach = true
	}

	// Build the full set of extensions (existing + new).
	allSlots := make([]rebuildSlot, 0, len(snap.Members)+1)
	for _, m := range snap.Members {
		allSlots = append(allSlots, rebuildSlot{
			ProgPinPath: m.ProgPinPath,
			ProgramName: m.ProgramName,
			Priority:    m.Priority,
			ProceedOn:   m.ProceedOn,
			LinkID:      m.LinkID,
			ProgramID:   m.ProgramID,
			Ifname:      m.Ifname,
		})
	}
	allSlots = append(allSlots, newSlot)

	if len(allSlots) > dispatcher.MaxPrograms {
		return extensionResult{}, fmt.Errorf("no free dispatcher slots (all %d occupied)", dispatcher.MaxPrograms)
	}

	// Sort by (priority, programName) to determine positions.
	sortRebuildSlots(allSlots)

	// Compute .rodata config.
	cfg, err := dispatcher.NewTCConfig(len(allSlots))
	if err != nil {
		return extensionResult{}, fmt.Errorf("create TC dispatcher config: %w", err)
	}
	for i, slot := range allSlots {
		cfg.ChainCallActions[i] = slot.ProceedOn << dispType.ChainCallShift()
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	// Compute new revision.
	var revision uint32
	if firstAttach {
		revision = 1
	} else {
		revision = snap.Revision + 1
	}

	dispProgPinPath := e.bpffs.DispatcherProgPath(dispType, nsid, ops.ifindex, revision)

	e.logger.InfoContext(ctx, "rebuilding TC dispatcher",
		"nsid", nsid,
		"ifindex", ops.ifindex,
		"ifname", ops.ifname,
		"direction", ops.direction,
		"revision", revision,
		"num_extensions", len(allSlots),
		"first_attach", firstAttach)

	// Load new dispatcher with .rodata config.
	dispatcherID, err := e.kernel.LoadAndPinTCDispatcher(ctx, cfg, dispProgPinPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("load TC dispatcher: %w", err)
	}

	cleanupNewDispatcher := func() {
		if rbErr := e.kernel.RemovePin(ctx, dispProgPinPath); rbErr != nil {
			e.logger.ErrorContext(ctx, "rollback: remove new TC dispatcher pin failed",
				"path", dispProgPinPath, "error", rbErr)
		}
	}

	// Attach all extensions to the new dispatcher.
	type attachedExt struct {
		out      bpfman.AttachOutput
		position int
		pinPath  bpfman.LinkPath
	}
	attached := make([]attachedExt, 0, len(allSlots))

	cleanupExtensions := func() {
		for _, ext := range attached {
			if ext.pinPath != "" {
				if rbErr := e.kernel.DetachLink(ctx, ext.pinPath); rbErr != nil {
					e.logger.ErrorContext(ctx, "rollback: detach TC extension link failed",
						"path", ext.pinPath, "error", rbErr)
				}
			}
		}
	}

	newSlotPosition := -1
	for i, slot := range allSlots {
		linkPinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ops.ifindex, revision, i)

		out, err := e.kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
			DispatcherPinPath: dispProgPinPath,
			ProgPinPath:       slot.ProgPinPath,
			ProgramName:       slot.ProgramName,
			Position:          i,
			LinkPinPath:       linkPinPath,
		})
		if err != nil {
			cleanupExtensions()
			cleanupNewDispatcher()
			return extensionResult{}, fmt.Errorf("attach TC extension %s at position %d: %w", slot.ProgramName, i, err)
		}

		attached = append(attached, attachedExt{out: out, position: i, pinPath: linkPinPath})

		if slot.LinkID == 0 {
			newSlotPosition = i
		}
	}

	// Diagnostic: read each just-pinned freplace link back via
	// BPF_LINK_GET_INFO_BY_FD. This forces a syscall round-trip per
	// link before the dispatcher swap, giving the kernel another
	// chance to publish trampoline state, and surfaces any slot
	// whose target_obj_id does not match the new dispatcher (which
	// would mean the freplace is bound to a stale program). Errors
	// are logged but do not abort the rebuild.
	for _, ext := range attached {
		info, infoErr := e.kernel.ExtensionLinkInfo(ctx, ext.pinPath)
		if infoErr != nil {
			e.logger.WarnContext(ctx, "verify: extension link info failed",
				"type", dispType.String(),
				"ifindex", ops.ifindex,
				"revision", revision,
				"position", ext.position,
				"path", ext.pinPath,
				"error", infoErr)
			continue
		}
		e.logger.InfoContext(ctx, "verify: extension link",
			"type", dispType.String(),
			"ifindex", ops.ifindex,
			"revision", revision,
			"position", ext.position,
			"link_id", uint64(info.LinkID),
			"target_prog_id", uint64(info.TargetProgID),
			"target_btf_id", info.TargetBtfID,
			"attach_type", info.AttachType,
			"matches_dispatcher", uint64(info.TargetProgID) == uint64(dispatcherID))
	}

	// Atomic swap: create filter (first-attach) or swap (add new, remove old).
	var oldHandle uint32
	var oldPriority uint16
	if !firstAttach && snap.Runtime.FilterPriority != nil {
		oldPriority = *snap.Runtime.FilterPriority
		parent := dispatcher.TCParentHandle(dispType)
		handle, err := e.kernel.FindTCFilterHandle(ctx, int(ops.ifindex), parent, oldPriority)
		if err != nil {
			e.logger.WarnContext(ctx, "failed to find old TC filter handle",
				"error", err)
		} else {
			oldHandle = handle
			e.logger.InfoContext(ctx, "TC filter swap: found old filter",
				"old_handle", fmt.Sprintf("%x", oldHandle),
				"old_priority", oldPriority)
		}
	}

	result, err := e.kernel.CreateTCFilter(ctx, dispProgPinPath, int(ops.ifindex), ops.ifname, ops.direction, ops.netnsPath)
	if err != nil {
		cleanupExtensions()
		cleanupNewDispatcher()
		return extensionResult{}, fmt.Errorf("create TC filter: %w", err)
	}

	e.logger.InfoContext(ctx, "TC filter swap: new filter created",
		"new_handle", fmt.Sprintf("%x", result.Handle),
		"new_priority", result.Priority,
		"new_dispatcher_id", result.DispatcherID,
		"old_handle", fmt.Sprintf("%x", oldHandle),
		"handles_match", result.Handle == oldHandle)

	// Remove old filter after new one is in place.
	if !firstAttach && oldHandle != 0 {
		parent := dispatcher.TCParentHandle(dispType)
		if err := e.kernel.DetachTCFilter(ctx, int(ops.ifindex), ops.ifname, parent, oldPriority, oldHandle); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old TC filter",
				"handle", fmt.Sprintf("%x", oldHandle), "error", err)
		} else {
			e.logger.InfoContext(ctx, "TC filter swap: removed old filter",
				"removed_handle", fmt.Sprintf("%x", oldHandle))
		}
	}

	// Build snapshot with all members and persist atomically.
	newSnap := platform.DispatcherSnapshot{
		Key:      key,
		Revision: revision,
		Runtime: platform.DispatcherRuntime{
			ProgramID:      dispatcherID,
			FilterPriority: &result.Priority,
		},
	}
	for i, slot := range allSlots {
		extLinkID := attached[i].out.LinkID
		if slot.LinkID != 0 {
			extLinkID = slot.LinkID
		}
		newSnap.Members = append(newSnap.Members, platform.DispatcherMember{
			ProgramID:   slot.ProgramID,
			ProgramName: slot.ProgramName,
			ProgPinPath: slot.ProgPinPath,
			LinkID:      extLinkID,
			LinkPinPath: attached[i].pinPath,
			Position:    i,
			Priority:    slot.Priority,
			ProceedOn:   slot.ProceedOn,
			Ifname:      slot.Ifname,
		})
	}

	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		return tx.ReplaceDispatcherSnapshot(ctx, newSnap)
	}); err != nil {
		e.logger.ErrorContext(ctx, "persist failed, rolling back TC dispatcher",
			"ifindex", ops.ifindex, "error", err)
		cleanupExtensions()
		cleanupNewDispatcher()
		return extensionResult{}, err
	}

	// Clean up old revision directory.
	if !firstAttach {
		oldRevDir := e.bpffs.DispatcherRevisionDir(dispType, nsid, ops.ifindex, snap.Revision)
		if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old TC revision directory",
				"path", oldRevDir, "error", err)
		}
	}

	if newSlotPosition < 0 {
		newSlotPosition = len(attached) - 1
	}
	newExt := attached[newSlotPosition]

	e.logger.InfoContext(ctx, "rebuilt TC dispatcher",
		"nsid", nsid,
		"ifindex", ops.ifindex,
		"ifname", ops.ifname,
		"direction", ops.direction,
		"revision", revision,
		"dispatcher_id", dispatcherID,
		"num_extensions", len(allSlots),
		"new_position", newSlotPosition)

	// Construct the bpfman.Link for the new extension.
	newExtLinkRecord := bpfman.NewPinnedLinkRecord(
		newExt.out.LinkID,
		managedProgramID,
		bpfman.TCDetails{
			Interface:    newSlot.Ifname,
			Ifindex:      ops.ifindex,
			Direction:    ops.direction,
			Priority:     int32(newSlot.Priority),
			Position:     int32(newSlotPosition),
			ProceedOn:    bitmaskToActions(newSlot.ProceedOn),
			Nsid:         nsid,
			DispatcherID: dispatcherID,
			Revision:     revision,
		},
		*bpfman.NewLinkPath(newExt.pinPath),
		time.Now(),
	)

	return extensionResult{
		link: bpfman.Link{
			Record: newExtLinkRecord,
			Status: bpfman.LinkStatus{
				Kernel:     newExt.out.KernelLink,
				KernelSeen: newExt.out.KernelLink != nil,
				PinPresent: newExt.pinPath != "" && !newExt.out.Synthetic,
			},
		},
		key:      key,
		revision: revision,
		position: newSlotPosition,
		pinPath:  newExt.pinPath,
	}, nil
}

// rebuildDispatcherForDetach rebuilds the dispatcher after an
// extension has been detached. If no extensions remain, the
// dispatcher is removed entirely.
func (e *executor) rebuildDispatcherForDetach(ctx context.Context, key dispatcher.Key, excludeLinkID kernel.LinkID) error {
	snap, err := e.store.GetDispatcherSnapshot(ctx, key)
	if err != nil {
		return fmt.Errorf("get dispatcher snapshot: %w", err)
	}

	// Filter out the excluded member.
	if excludeLinkID != 0 {
		filtered := snap.Members[:0]
		for _, m := range snap.Members {
			if m.LinkID != excludeLinkID {
				filtered = append(filtered, m)
			}
		}
		snap.Members = filtered
	}

	if len(snap.Members) == 0 {
		return e.removeEmptyDispatcher(ctx, snap)
	}

	// Extensions remain: rebuild with the remaining members.
	rebuildSlots := make([]rebuildSlot, len(snap.Members))
	for i, m := range snap.Members {
		rebuildSlots[i] = rebuildSlot{
			ProgPinPath: m.ProgPinPath,
			ProgramName: m.ProgramName,
			Priority:    m.Priority,
			ProceedOn:   m.ProceedOn,
			LinkID:      m.LinkID,
			ProgramID:   m.ProgramID,
			Ifname:      m.Ifname,
		}
	}
	sortRebuildSlots(rebuildSlots)

	// Compute new revision.
	revision := snap.Revision + 1
	progPinPath := e.bpffs.DispatcherProgPath(key.Type, key.Nsid, key.Ifindex, revision)

	e.logger.InfoContext(ctx, "rebuilding dispatcher for detach",
		"type", key.Type,
		"nsid", key.Nsid,
		"ifindex", key.Ifindex,
		"revision", revision,
		"remaining", len(rebuildSlots))

	if key.Type == dispatcher.DispatcherTypeXDP {
		return e.rebuildXDPForDetach(ctx, snap, rebuildSlots, revision, progPinPath)
	}
	return e.rebuildTCForDetach(ctx, snap, rebuildSlots, revision, progPinPath)
}

// rebuildXDPForDetach handles the XDP-specific rebuild after detach.
func (e *executor) rebuildXDPForDetach(
	ctx context.Context,
	snap platform.DispatcherSnapshot,
	slots []rebuildSlot,
	revision uint32,
	progPinPath string,
) error {
	key := snap.Key

	const xdpDispatcherRetval = 31
	cfg, err := dispatcher.NewXDPConfig(len(slots))
	if err != nil {
		return fmt.Errorf("create XDP dispatcher config for detach rebuild: %w", err)
	}
	for i, slot := range slots {
		cfg.ChainCallActions[i] = slot.ProceedOn | (1 << xdpDispatcherRetval)
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	dispatcherID, err := e.kernel.LoadAndPinXDPDispatcher(ctx, cfg, progPinPath)
	if err != nil {
		return fmt.Errorf("load XDP dispatcher for detach rebuild: %w", err)
	}

	// Attach remaining extensions.
	type attachedExt struct {
		out     bpfman.AttachOutput
		pinPath bpfman.LinkPath
	}
	attached := make([]attachedExt, 0, len(slots))
	for i, slot := range slots {
		linkPinPath := e.bpffs.ExtensionLinkPath(key.Type, key.Nsid, key.Ifindex, revision, i)
		out, err := e.kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
			DispatcherPinPath: progPinPath,
			ProgPinPath:       slot.ProgPinPath,
			ProgramName:       slot.ProgramName,
			Position:          i,
			LinkPinPath:       linkPinPath,
		})
		if err != nil {
			return fmt.Errorf("re-attach XDP extension %s at position %d: %w", slot.ProgramName, i, err)
		}
		attached = append(attached, attachedExt{out: out, pinPath: linkPinPath})
	}

	// Diagnostic: see rebuildXDPDispatcher for rationale.
	for i, ext := range attached {
		info, infoErr := e.kernel.ExtensionLinkInfo(ctx, ext.pinPath)
		if infoErr != nil {
			e.logger.WarnContext(ctx, "verify: extension link info failed (detach rebuild)",
				"type", key.Type.String(),
				"ifindex", key.Ifindex,
				"revision", revision,
				"position", i,
				"path", ext.pinPath,
				"error", infoErr)
			continue
		}
		e.logger.InfoContext(ctx, "verify: extension link (detach rebuild)",
			"type", key.Type.String(),
			"ifindex", key.Ifindex,
			"revision", revision,
			"position", i,
			"link_id", uint64(info.LinkID),
			"target_prog_id", uint64(info.TargetProgID),
			"target_btf_id", info.TargetBtfID,
			"attach_type", info.AttachType,
			"matches_dispatcher", uint64(info.TargetProgID) == uint64(dispatcherID))
	}

	// Swap link.
	linkPinPath := e.bpffs.DispatcherLinkPath(key.Type, key.Nsid, key.Ifindex)
	if err := e.kernel.UpdateXDPDispatcherLink(ctx, linkPinPath, progPinPath); err != nil {
		return fmt.Errorf("update XDP dispatcher link: %w", err)
	}

	// Build new snapshot and persist.
	newSnap := platform.DispatcherSnapshot{
		Key:      key,
		Revision: revision,
		Runtime: platform.DispatcherRuntime{
			ProgramID: dispatcherID,
			LinkID:    snap.Runtime.LinkID,
		},
	}
	for i, slot := range slots {
		newSnap.Members = append(newSnap.Members, platform.DispatcherMember{
			ProgramID:   slot.ProgramID,
			ProgramName: slot.ProgramName,
			ProgPinPath: slot.ProgPinPath,
			LinkID:      slot.LinkID,
			LinkPinPath: attached[i].pinPath,
			Position:    i,
			Priority:    slot.Priority,
			ProceedOn:   slot.ProceedOn,
			Ifname:      slot.Ifname,
		})
	}

	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		return tx.ReplaceDispatcherSnapshot(ctx, newSnap)
	}); err != nil {
		return err
	}

	// Clean up old revision.
	oldRevDir := e.bpffs.DispatcherRevisionDir(key.Type, key.Nsid, key.Ifindex, snap.Revision)
	if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
		e.logger.WarnContext(ctx, "failed to remove old revision directory",
			"path", oldRevDir, "error", err)
	}

	return nil
}

// rebuildTCForDetach handles the TC-specific rebuild after detach.
func (e *executor) rebuildTCForDetach(
	ctx context.Context,
	snap platform.DispatcherSnapshot,
	slots []rebuildSlot,
	revision uint32,
	progPinPath string,
) error {
	key := snap.Key
	dispType := key.Type

	cfg, err := dispatcher.NewTCConfig(len(slots))
	if err != nil {
		return fmt.Errorf("create TC dispatcher config for detach rebuild: %w", err)
	}
	for i, slot := range slots {
		cfg.ChainCallActions[i] = slot.ProceedOn << dispType.ChainCallShift()
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	dispatcherID, err := e.kernel.LoadAndPinTCDispatcher(ctx, cfg, progPinPath)
	if err != nil {
		return fmt.Errorf("load TC dispatcher for detach rebuild: %w", err)
	}

	// Attach remaining extensions.
	type attachedExt struct {
		out     bpfman.AttachOutput
		pinPath bpfman.LinkPath
	}
	attached := make([]attachedExt, 0, len(slots))
	for i, slot := range slots {
		linkPinPath := e.bpffs.ExtensionLinkPath(key.Type, key.Nsid, key.Ifindex, revision, i)
		out, err := e.kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
			DispatcherPinPath: progPinPath,
			ProgPinPath:       slot.ProgPinPath,
			ProgramName:       slot.ProgramName,
			Position:          i,
			LinkPinPath:       linkPinPath,
		})
		if err != nil {
			return fmt.Errorf("re-attach TC extension %s at position %d: %w", slot.ProgramName, i, err)
		}
		attached = append(attached, attachedExt{out: out, pinPath: linkPinPath})
	}

	// Diagnostic: see rebuildTCDispatcher for rationale.
	for i, ext := range attached {
		info, infoErr := e.kernel.ExtensionLinkInfo(ctx, ext.pinPath)
		if infoErr != nil {
			e.logger.WarnContext(ctx, "verify: extension link info failed (detach rebuild)",
				"type", key.Type.String(),
				"ifindex", key.Ifindex,
				"revision", revision,
				"position", i,
				"path", ext.pinPath,
				"error", infoErr)
			continue
		}
		e.logger.InfoContext(ctx, "verify: extension link (detach rebuild)",
			"type", key.Type.String(),
			"ifindex", key.Ifindex,
			"revision", revision,
			"position", i,
			"link_id", uint64(info.LinkID),
			"target_prog_id", uint64(info.TargetProgID),
			"target_btf_id", info.TargetBtfID,
			"attach_type", info.AttachType,
			"matches_dispatcher", uint64(info.TargetProgID) == uint64(dispatcherID))
	}

	// Record old handle before swap.
	var oldHandle uint32
	var oldPriority uint16
	if snap.Runtime.FilterPriority != nil {
		oldPriority = *snap.Runtime.FilterPriority
		parent := dispatcher.TCParentHandle(dispType)
		handle, err := e.kernel.FindTCFilterHandle(ctx, int(key.Ifindex), parent, oldPriority)
		if err != nil {
			e.logger.WarnContext(ctx, "failed to find old TC filter handle for detach rebuild",
				"error", err)
		} else {
			oldHandle = handle
		}
	}

	// Determine direction from dispatcher type.
	var direction bpfman.TCDirection
	if dispType == dispatcher.DispatcherTypeTCIngress {
		direction = bpfman.TCDirectionIngress
	} else {
		direction = bpfman.TCDirectionEgress
	}

	// Create new filter.
	result, err := e.kernel.CreateTCFilter(ctx, progPinPath, int(key.Ifindex), "", direction, "")
	if err != nil {
		return fmt.Errorf("create TC filter for detach rebuild: %w", err)
	}

	// Remove old filter.
	if oldHandle != 0 {
		parent := dispatcher.TCParentHandle(dispType)
		if err := e.kernel.DetachTCFilter(ctx, int(key.Ifindex), "", parent, oldPriority, oldHandle); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old TC filter after detach rebuild",
				"handle", fmt.Sprintf("%x", oldHandle), "error", err)
		}
	}

	// Build new snapshot and persist.
	newSnap := platform.DispatcherSnapshot{
		Key:      key,
		Revision: revision,
		Runtime: platform.DispatcherRuntime{
			ProgramID:      result.DispatcherID,
			FilterPriority: &result.Priority,
		},
	}
	for i, slot := range slots {
		newSnap.Members = append(newSnap.Members, platform.DispatcherMember{
			ProgramID:   slot.ProgramID,
			ProgramName: slot.ProgramName,
			ProgPinPath: slot.ProgPinPath,
			LinkID:      slot.LinkID,
			LinkPinPath: attached[i].pinPath,
			Position:    i,
			Priority:    slot.Priority,
			ProceedOn:   slot.ProceedOn,
			Ifname:      slot.Ifname,
		})
	}

	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		return tx.ReplaceDispatcherSnapshot(ctx, newSnap)
	}); err != nil {
		return err
	}

	// Clean up old revision.
	oldRevDir := e.bpffs.DispatcherRevisionDir(key.Type, key.Nsid, key.Ifindex, snap.Revision)
	if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
		e.logger.WarnContext(ctx, "failed to remove old TC revision directory",
			"path", oldRevDir, "error", err)
	}

	return nil
}

// removeEmptyDispatcher removes a dispatcher when no extensions remain.
func (e *executor) removeEmptyDispatcher(ctx context.Context, snap platform.DispatcherSnapshot) error {
	key := snap.Key
	e.logger.DebugContext(ctx, "removing empty dispatcher",
		"type", key.Type,
		"nsid", key.Nsid,
		"ifindex", key.Ifindex,
		"program_id", snap.Runtime.ProgramID,
		"link_id", snap.Runtime.LinkID)

	// Convert snapshot to dispatcher.State for
	// computeDispatcherCleanupActions (to be migrated later).
	state := dispatcher.State{
		Type:      key.Type,
		Nsid:      key.Nsid,
		Ifindex:   key.Ifindex,
		Revision:  snap.Revision,
		ProgramID: snap.Runtime.ProgramID,
	}
	if snap.Runtime.LinkID != nil {
		state.LinkID = *snap.Runtime.LinkID
	}
	if snap.Runtime.FilterPriority != nil {
		state.Priority = *snap.Runtime.FilterPriority
	}

	// For TC dispatchers, query the kernel for the filter handle.
	var tcHandle uint32
	if key.Type == dispatcher.DispatcherTypeTCIngress || key.Type == dispatcher.DispatcherTypeTCEgress {
		parent := dispatcher.TCParentHandle(key.Type)
		handle, err := e.kernel.FindTCFilterHandle(ctx, int(key.Ifindex), parent, state.Priority)
		if err != nil {
			e.logger.WarnContext(ctx, "failed to find TC filter handle", "error", err)
		} else {
			tcHandle = handle
		}
	}

	cleanupActions := computeDispatcherCleanupActions(e.bpffs, state, tcHandle)
	if err := e.ExecuteAll(ctx, cleanupActions); err != nil {
		return fmt.Errorf("execute dispatcher cleanup actions: %w", err)
	}
	return nil
}

// isNotFound returns true if the error wraps platform.ErrRecordNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, platform.ErrRecordNotFound)
}

// effectivePriority returns the priority used for dispatcher slot
// ordering and .rodata configuration. Zero (unspecified) defaults
// to DefaultPriority (50).
func effectivePriority(p int) int {
	if p == 0 {
		return int(dispatcher.DefaultPriority)
	}
	return p
}

// sortRebuildSlots sorts rebuild slots by
// (effectivePriority ASC, attached ASC, programName ASC).
//
// The attached tie-breaker matches Rust bpfman: new (unattached,
// LinkID==0) programs sort before existing (attached) ones at the
// same priority. This matters when the default proceed-on excludes
// TC_ACT_OK — only position 0 executes, so the newly-added program
// must land there.
func sortRebuildSlots(slots []rebuildSlot) {
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0; j-- {
			pi := effectivePriority(slots[j].Priority)
			pj := effectivePriority(slots[j-1].Priority)
			ai := slots[j].LinkID != 0   // attached = true
			aj := slots[j-1].LinkID != 0 // attached = true
			if pi < pj ||
				(pi == pj && !ai && aj) ||
				(pi == pj && ai == aj && slots[j].ProgramName < slots[j-1].ProgramName) {
				slots[j], slots[j-1] = slots[j-1], slots[j]
			} else {
				break
			}
		}
	}
}

// bitmaskToActions converts a proceed-on bitmask back to a slice of
// action codes. This is the inverse of the bitmask computation in
// attach_tc.go and attach_xdp.go.
func bitmaskToActions(mask uint32) []int32 {
	var actions []int32
	for i := 0; i < 32; i++ {
		if mask&(1<<uint(i)) != 0 {
			actions = append(actions, int32(i))
		}
	}
	return actions
}
