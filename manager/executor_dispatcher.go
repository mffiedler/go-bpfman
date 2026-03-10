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
func (e *executor) rebuildXDPDispatcher(
	ctx context.Context,
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
		Ifname:      ops.ifname,
	}

	nsid, err := netns.GetNsid(ops.netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("get nsid: %w", err)
	}

	dispType := dispatcher.DispatcherTypeXDP

	// Query existing dispatcher state (may not exist).
	existingState, err := e.store.GetDispatcher(ctx, dispType, nsid, ops.ifindex)
	firstAttach := false
	if err != nil {
		if !isNotFound(err) {
			return extensionResult{}, fmt.Errorf("get dispatcher: %w", err)
		}
		firstAttach = true
	}

	// Query existing extension slots.
	var existingSlots []platform.DispatcherSlot
	if !firstAttach {
		existingSlots, err = e.store.ListDispatcherSlots(ctx, existingState.ProgramID)
		if err != nil {
			return extensionResult{}, fmt.Errorf("list dispatcher slots: %w", err)
		}
	}

	// Build the full set of extensions (existing + new).
	allSlots := make([]rebuildSlot, 0, len(existingSlots)+1)
	for _, s := range existingSlots {
		allSlots = append(allSlots, rebuildSlot{
			ProgPinPath: s.ProgPinPath,
			ProgramName: s.ProgramName,
			Priority:    s.Priority,
			ProceedOn:   s.ProceedOn,
			LinkID:      s.LinkID,
			ProgramID:   s.ProgramID,
			Ifname:      s.Ifname,
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
	cfg := dispatcher.NewXDPConfig(len(allSlots))
	for i, slot := range allSlots {
		cfg.ChainCallActions[i] = slot.ProceedOn | (1 << xdpDispatcherRetval)
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	// Compute new revision.
	var revision uint32
	if firstAttach {
		revision = 1
	} else {
		revision, err = e.store.IncrementRevision(ctx, dispType, nsid, ops.ifindex)
		if err != nil {
			return extensionResult{}, fmt.Errorf("increment revision: %w", err)
		}
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
		pinPath  string
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

	// Atomic swap: create link (first-attach) or update existing link.
	linkPinPath := e.bpffs.DispatcherLinkPath(dispType, nsid, ops.ifindex)
	var linkID kernel.LinkID

	if firstAttach {
		result, err := e.kernel.CreateXDPLink(ctx, dispProgPinPath, int(ops.ifindex), linkPinPath, ops.netnsPath)
		if err != nil {
			cleanupExtensions()
			cleanupNewDispatcher()
			return extensionResult{}, fmt.Errorf("create XDP link: %w", err)
		}
		linkID = result.LinkID
	} else {
		if err := e.kernel.UpdateXDPDispatcherLink(ctx, linkPinPath, dispProgPinPath); err != nil {
			cleanupExtensions()
			cleanupNewDispatcher()
			return extensionResult{}, fmt.Errorf("update XDP dispatcher link: %w", err)
		}
		linkID = existingState.LinkID
	}

	// Save dispatcher state and update existing extension link
	// detail records atomically. Without a transaction, a
	// concurrent reader (e.g., CLI "link get") can observe the
	// intermediate state where old details have been deleted but
	// new details have not yet been inserted.
	newState := dispatcher.State{
		Type:      dispType,
		Nsid:      nsid,
		Ifindex:   ops.ifindex,
		Revision:  revision,
		ProgramID: dispatcherID,
		LinkID:    linkID,
	}
	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		if err := tx.SaveDispatcher(ctx, newState); err != nil {
			return fmt.Errorf("save XDP dispatcher: %w", err)
		}
		if !firstAttach {
			if err := tx.DeleteDispatcherLinkDetails(ctx, dispatcherID); err != nil {
				return fmt.Errorf("delete stale XDP link details: %w", err)
			}
			for i, slot := range allSlots {
				if slot.LinkID == 0 {
					continue // new extension, saved by saveLinkNode
				}
				pinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ops.ifindex, revision, i)
				if err := tx.SaveLink(ctx, bpfman.NewPinnedLinkRecord(
					slot.LinkID,
					slot.ProgramID,
					bpfman.XDPDetails{
						Interface:    slot.Ifname,
						Ifindex:      ops.ifindex,
						Priority:     int32(slot.Priority),
						Position:     int32(i),
						ProceedOn:    bitmaskToActions(slot.ProceedOn),
						Nsid:         nsid,
						DispatcherID: dispatcherID,
						Revision:     revision,
					},
					bpfman.LinkPath(pinPath),
					time.Now(),
				)); err != nil {
					return fmt.Errorf("re-insert XDP link detail for link %d at position %d: %w", slot.LinkID, i, err)
				}
			}
		}
		return nil
	}); err != nil {
		e.logger.ErrorContext(ctx, "persist failed, rolling back XDP dispatcher",
			"ifindex", ops.ifindex, "error", err)
		cleanupExtensions()
		if firstAttach {
			if rbErr := e.kernel.DetachLink(ctx, linkPinPath); rbErr != nil {
				e.logger.ErrorContext(ctx, "rollback: detach dispatcher link failed",
					"path", linkPinPath, "error", rbErr)
			}
		}
		cleanupNewDispatcher()
		return extensionResult{}, err
	}

	// Clean up old revision directory (if not first-attach).
	if !firstAttach {
		oldRevDir := e.bpffs.DispatcherRevisionDir(dispType, nsid, ops.ifindex, existingState.Revision)
		if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old revision directory",
				"path", oldRevDir, "error", err)
		}
	}

	// Find the new extension's attach output.
	if newSlotPosition < 0 {
		// Shouldn't happen, but be defensive.
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

	return extensionResult{
		out:      newExt.out,
		disp:     newState,
		position: newSlotPosition,
		pinPath:  newExt.pinPath,
	}, nil
}

// rebuildTCDispatcher performs a full TC dispatcher rebuild.
// Same semantics as rebuildXDPDispatcher but for TC dispatchers.
func (e *executor) rebuildTCDispatcher(
	ctx context.Context,
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
		Ifname:      ops.ifname,
	}

	nsid, err := netns.GetNsid(ops.netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("get nsid: %w", err)
	}

	dispType := ops.dispType

	// Query existing dispatcher state (may not exist).
	existingState, err := e.store.GetDispatcher(ctx, dispType, nsid, ops.ifindex)
	firstAttach := false
	if err != nil {
		if !isNotFound(err) {
			return extensionResult{}, fmt.Errorf("get dispatcher: %w", err)
		}
		firstAttach = true
	}

	// Query existing extension slots.
	var existingSlots []platform.DispatcherSlot
	if !firstAttach {
		existingSlots, err = e.store.ListDispatcherSlots(ctx, existingState.ProgramID)
		if err != nil {
			return extensionResult{}, fmt.Errorf("list dispatcher slots: %w", err)
		}
		e.logger.InfoContext(ctx, "TC rebuild: existing slots",
			"dispatcher_program_id", existingState.ProgramID,
			"num_existing", len(existingSlots),
			"new_program_id", newSlot.ProgramID,
			"new_priority", newSlot.Priority)
		for i, s := range existingSlots {
			e.logger.InfoContext(ctx, "TC rebuild: existing slot",
				"index", i,
				"link_id", s.LinkID,
				"program_id", s.ProgramID,
				"priority", s.Priority)
		}
	}

	// Build the full set of extensions (existing + new).
	allSlots := make([]rebuildSlot, 0, len(existingSlots)+1)
	for _, s := range existingSlots {
		allSlots = append(allSlots, rebuildSlot{
			ProgPinPath: s.ProgPinPath,
			ProgramName: s.ProgramName,
			Priority:    s.Priority,
			ProceedOn:   s.ProceedOn,
			LinkID:      s.LinkID,
			ProgramID:   s.ProgramID,
			Ifname:      s.Ifname,
		})
	}
	allSlots = append(allSlots, newSlot)

	if len(allSlots) > dispatcher.MaxPrograms {
		return extensionResult{}, fmt.Errorf("no free dispatcher slots (all %d occupied)", dispatcher.MaxPrograms)
	}

	// Sort by (priority, programName) to determine positions.
	sortRebuildSlots(allSlots)

	// Compute .rodata config.
	cfg := dispatcher.NewTCConfig(len(allSlots))
	for i, slot := range allSlots {
		// TC dispatchers check (1 << (ret + 1)) for chain_call_actions
		// to handle TC_ACT_UNSPEC = -1.
		cfg.ChainCallActions[i] = slot.ProceedOn << dispType.ChainCallShift()
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	// Compute new revision.
	var revision uint32
	if firstAttach {
		revision = 1
	} else {
		revision, err = e.store.IncrementRevision(ctx, dispType, nsid, ops.ifindex)
		if err != nil {
			return extensionResult{}, fmt.Errorf("increment revision: %w", err)
		}
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
		pinPath  string
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

	// Atomic swap: create filter (first-attach) or swap (add new, remove old).
	// For TC, record the old handle before creating the new filter so we can
	// remove the old one after the new one is in place.
	var oldHandle uint32
	if !firstAttach {
		parent := dispatcher.TCParentHandle(dispType)
		handle, err := e.kernel.FindTCFilterHandle(ctx, int(ops.ifindex), parent, existingState.Priority)
		if err != nil {
			e.logger.WarnContext(ctx, "failed to find old TC filter handle",
				"error", err)
		} else {
			oldHandle = handle
		}
	}

	result, err := e.kernel.CreateTCFilter(ctx, dispProgPinPath, int(ops.ifindex), ops.ifname, ops.direction, ops.netnsPath)
	if err != nil {
		cleanupExtensions()
		cleanupNewDispatcher()
		return extensionResult{}, fmt.Errorf("create TC filter: %w", err)
	}

	// Remove old filter after new one is in place.
	if !firstAttach && oldHandle != 0 {
		parent := dispatcher.TCParentHandle(dispType)
		if err := e.kernel.DetachTCFilter(ctx, int(ops.ifindex), ops.ifname, parent, existingState.Priority, oldHandle); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old TC filter",
				"handle", fmt.Sprintf("%x", oldHandle), "error", err)
		}
	}

	// Save dispatcher state and update existing extension link
	// detail records atomically. See rebuildXDPDispatcher for
	// rationale.
	newState := dispatcher.State{
		Type:      dispType,
		Nsid:      nsid,
		Ifindex:   ops.ifindex,
		Revision:  revision,
		ProgramID: dispatcherID,
		Priority:  result.Priority,
	}
	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		if err := tx.SaveDispatcher(ctx, newState); err != nil {
			return fmt.Errorf("save TC dispatcher: %w", err)
		}
		if !firstAttach {
			if err := tx.DeleteDispatcherLinkDetails(ctx, dispatcherID); err != nil {
				return fmt.Errorf("delete stale TC link details: %w", err)
			}
			for i, slot := range allSlots {
				if slot.LinkID == 0 {
					continue // new extension, saved by saveLinkNode
				}
				pinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ops.ifindex, revision, i)
				if err := tx.SaveLink(ctx, bpfman.NewPinnedLinkRecord(
					slot.LinkID,
					slot.ProgramID,
					bpfman.TCDetails{
						Interface:    slot.Ifname,
						Ifindex:      ops.ifindex,
						Direction:    ops.direction,
						Priority:     int32(slot.Priority),
						Position:     int32(i),
						ProceedOn:    bitmaskToActions(slot.ProceedOn),
						Nsid:         nsid,
						DispatcherID: dispatcherID,
						Revision:     revision,
					},
					bpfman.LinkPath(pinPath),
					time.Now(),
				)); err != nil {
					return fmt.Errorf("re-insert TC link detail for link %d at position %d: %w", slot.LinkID, i, err)
				}
			}
		}
		return nil
	}); err != nil {
		e.logger.ErrorContext(ctx, "persist failed, rolling back TC dispatcher",
			"ifindex", ops.ifindex, "error", err)
		cleanupExtensions()
		cleanupNewDispatcher()
		return extensionResult{}, err
	}

	// Clean up old revision directory.
	if !firstAttach {
		oldRevDir := e.bpffs.DispatcherRevisionDir(dispType, nsid, ops.ifindex, existingState.Revision)
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

	return extensionResult{
		out:      newExt.out,
		disp:     newState,
		position: newSlotPosition,
		pinPath:  newExt.pinPath,
	}, nil
}

// rebuildDispatcherForDetach rebuilds the dispatcher after an
// extension has been detached. If no extensions remain, the
// dispatcher is removed entirely.
func (e *executor) rebuildDispatcherForDetach(ctx context.Context, state dispatcher.State) error {
	remaining, err := e.store.CountDispatcherLinks(ctx, state.ProgramID)
	if err != nil {
		return fmt.Errorf("count dispatcher links: %w", err)
	}

	if remaining == 0 {
		return e.removeEmptyDispatcher(ctx, state)
	}

	// Extensions remain: rebuild the dispatcher with the remaining slots.
	slots, err := e.store.ListDispatcherSlots(ctx, state.ProgramID)
	if err != nil {
		return fmt.Errorf("list dispatcher slots for rebuild: %w", err)
	}

	// Sort by (priority, programName) for position assignment.
	rebuildSlots := make([]rebuildSlot, len(slots))
	for i, s := range slots {
		rebuildSlots[i] = rebuildSlot{
			ProgPinPath: s.ProgPinPath,
			ProgramName: s.ProgramName,
			Priority:    s.Priority,
			ProceedOn:   s.ProceedOn,
			LinkID:      s.LinkID,
			ProgramID:   s.ProgramID,
			Ifname:      s.Ifname,
		}
	}
	sortRebuildSlots(rebuildSlots)

	nsid := state.Nsid
	ifindex := state.Ifindex
	dispType := state.Type

	// Compute new revision.
	revision, err := e.store.IncrementRevision(ctx, dispType, nsid, ifindex)
	if err != nil {
		return fmt.Errorf("increment revision: %w", err)
	}

	progPinPath := e.bpffs.DispatcherProgPath(dispType, nsid, ifindex, revision)

	e.logger.InfoContext(ctx, "rebuilding dispatcher for detach",
		"type", dispType,
		"nsid", nsid,
		"ifindex", ifindex,
		"revision", revision,
		"remaining", len(rebuildSlots))

	if dispType == dispatcher.DispatcherTypeXDP {
		return e.rebuildXDPForDetach(ctx, state, rebuildSlots, revision, progPinPath)
	}
	return e.rebuildTCForDetach(ctx, state, rebuildSlots, revision, progPinPath)
}

// rebuildXDPForDetach handles the XDP-specific rebuild after detach.
func (e *executor) rebuildXDPForDetach(
	ctx context.Context,
	state dispatcher.State,
	slots []rebuildSlot,
	revision uint32,
	progPinPath string,
) error {
	dispType := state.Type
	nsid := state.Nsid
	ifindex := state.Ifindex

	const xdpDispatcherRetval = 31
	cfg := dispatcher.NewXDPConfig(len(slots))
	for i, slot := range slots {
		cfg.ChainCallActions[i] = slot.ProceedOn | (1 << xdpDispatcherRetval)
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	dispatcherID, err := e.kernel.LoadAndPinXDPDispatcher(ctx, cfg, progPinPath)
	if err != nil {
		return fmt.Errorf("load XDP dispatcher for detach rebuild: %w", err)
	}

	// Attach remaining extensions.
	for i, slot := range slots {
		linkPinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ifindex, revision, i)
		_, err := e.kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
			DispatcherPinPath: progPinPath,
			ProgPinPath:       slot.ProgPinPath,
			ProgramName:       slot.ProgramName,
			Position:          i,
			LinkPinPath:       linkPinPath,
		})
		if err != nil {
			return fmt.Errorf("re-attach XDP extension %s at position %d: %w", slot.ProgramName, i, err)
		}
	}

	// Swap link.
	linkPinPath := e.bpffs.DispatcherLinkPath(dispType, nsid, ifindex)
	if err := e.kernel.UpdateXDPDispatcherLink(ctx, linkPinPath, progPinPath); err != nil {
		return fmt.Errorf("update XDP dispatcher link: %w", err)
	}

	// Update store.
	newState := dispatcher.State{
		Type:      dispType,
		Nsid:      nsid,
		Ifindex:   ifindex,
		Revision:  revision,
		ProgramID: dispatcherID,
		LinkID:    state.LinkID,
	}
	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		if err := tx.SaveDispatcher(ctx, newState); err != nil {
			return fmt.Errorf("save dispatcher after detach rebuild: %w", err)
		}
		if err := tx.DeleteDispatcherLinkDetails(ctx, dispatcherID); err != nil {
			return fmt.Errorf("delete stale XDP link details for detach rebuild: %w", err)
		}
		for i, slot := range slots {
			pinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ifindex, revision, i)
			if err := tx.SaveLink(ctx, bpfman.NewPinnedLinkRecord(
				slot.LinkID,
				slot.ProgramID,
				bpfman.XDPDetails{
					Interface:    slot.Ifname,
					Ifindex:      ifindex,
					Priority:     int32(slot.Priority),
					Position:     int32(i),
					ProceedOn:    bitmaskToActions(slot.ProceedOn),
					Nsid:         nsid,
					DispatcherID: dispatcherID,
					Revision:     revision,
				},
				bpfman.LinkPath(pinPath),
				time.Now(),
			)); err != nil {
				return fmt.Errorf("re-insert XDP link detail for link %d at position %d: %w", slot.LinkID, i, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Clean up old revision.
	oldRevDir := e.bpffs.DispatcherRevisionDir(dispType, nsid, ifindex, state.Revision)
	if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
		e.logger.WarnContext(ctx, "failed to remove old revision directory",
			"path", oldRevDir, "error", err)
	}

	return nil
}

// rebuildTCForDetach handles the TC-specific rebuild after detach.
func (e *executor) rebuildTCForDetach(
	ctx context.Context,
	state dispatcher.State,
	slots []rebuildSlot,
	revision uint32,
	progPinPath string,
) error {
	dispType := state.Type
	nsid := state.Nsid
	ifindex := state.Ifindex

	cfg := dispatcher.NewTCConfig(len(slots))
	for i, slot := range slots {
		cfg.ChainCallActions[i] = slot.ProceedOn << dispType.ChainCallShift()
		cfg.RunPrios[i] = uint32(effectivePriority(slot.Priority))
	}

	_, err := e.kernel.LoadAndPinTCDispatcher(ctx, cfg, progPinPath)
	if err != nil {
		return fmt.Errorf("load TC dispatcher for detach rebuild: %w", err)
	}

	// Attach remaining extensions.
	for i, slot := range slots {
		linkPinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ifindex, revision, i)
		_, err := e.kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
			DispatcherPinPath: progPinPath,
			ProgPinPath:       slot.ProgPinPath,
			ProgramName:       slot.ProgramName,
			Position:          i,
			LinkPinPath:       linkPinPath,
		})
		if err != nil {
			return fmt.Errorf("re-attach TC extension %s at position %d: %w", slot.ProgramName, i, err)
		}
	}

	// Record old handle before swap.
	var oldHandle uint32
	parent := dispatcher.TCParentHandle(dispType)
	handle, err := e.kernel.FindTCFilterHandle(ctx, int(ifindex), parent, state.Priority)
	if err != nil {
		e.logger.WarnContext(ctx, "failed to find old TC filter handle for detach rebuild",
			"error", err)
	} else {
		oldHandle = handle
	}

	// Determine direction from dispatcher type.
	var direction bpfman.TCDirection
	if dispType == dispatcher.DispatcherTypeTCIngress {
		direction = bpfman.TCDirectionIngress
	} else {
		direction = bpfman.TCDirectionEgress
	}

	// Create new filter.
	result, err := e.kernel.CreateTCFilter(ctx, progPinPath, int(ifindex), "", direction, "")
	if err != nil {
		return fmt.Errorf("create TC filter for detach rebuild: %w", err)
	}

	// Remove old filter.
	if oldHandle != 0 {
		if err := e.kernel.DetachTCFilter(ctx, int(ifindex), "", parent, state.Priority, oldHandle); err != nil {
			e.logger.WarnContext(ctx, "failed to remove old TC filter after detach rebuild",
				"handle", fmt.Sprintf("%x", oldHandle), "error", err)
		}
	}

	// Update store.
	newState := dispatcher.State{
		Type:      dispType,
		Nsid:      nsid,
		Ifindex:   ifindex,
		Revision:  revision,
		ProgramID: result.DispatcherID,
		Priority:  result.Priority,
	}
	if err := e.store.RunInTransaction(ctx, func(tx platform.Store) error {
		if err := tx.SaveDispatcher(ctx, newState); err != nil {
			return fmt.Errorf("save dispatcher after TC detach rebuild: %w", err)
		}
		if err := tx.DeleteDispatcherLinkDetails(ctx, result.DispatcherID); err != nil {
			return fmt.Errorf("delete stale TC link details for detach rebuild: %w", err)
		}
		for i, slot := range slots {
			pinPath := e.bpffs.ExtensionLinkPath(dispType, nsid, ifindex, revision, i)
			if err := tx.SaveLink(ctx, bpfman.NewPinnedLinkRecord(
				slot.LinkID,
				slot.ProgramID,
				bpfman.TCDetails{
					Interface:    slot.Ifname,
					Ifindex:      ifindex,
					Direction:    direction,
					Priority:     int32(slot.Priority),
					Position:     int32(i),
					ProceedOn:    bitmaskToActions(slot.ProceedOn),
					Nsid:         nsid,
					DispatcherID: result.DispatcherID,
					Revision:     revision,
				},
				bpfman.LinkPath(pinPath),
				time.Now(),
			)); err != nil {
				return fmt.Errorf("re-insert TC link detail for link %d at position %d: %w", slot.LinkID, i, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Clean up old revision.
	oldRevDir := e.bpffs.DispatcherRevisionDir(dispType, nsid, ifindex, state.Revision)
	if err := e.Execute(ctx, action.RemoveDispatcherRevDir{Path: oldRevDir}); err != nil {
		e.logger.WarnContext(ctx, "failed to remove old TC revision directory",
			"path", oldRevDir, "error", err)
	}

	return nil
}

// removeEmptyDispatcher removes a dispatcher when no extensions remain.
func (e *executor) removeEmptyDispatcher(ctx context.Context, state dispatcher.State) error {
	e.logger.DebugContext(ctx, "removing empty dispatcher",
		"type", state.Type,
		"nsid", state.Nsid,
		"ifindex", state.Ifindex,
		"program_id", state.ProgramID,
		"link_id", state.LinkID)

	// For TC dispatchers, query the kernel for the filter handle.
	var tcHandle uint32
	if state.Type == dispatcher.DispatcherTypeTCIngress || state.Type == dispatcher.DispatcherTypeTCEgress {
		parent := dispatcher.TCParentHandle(state.Type)
		handle, err := e.kernel.FindTCFilterHandle(ctx, int(state.Ifindex), parent, state.Priority)
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

// sortRebuildSlots sorts rebuild slots by (effectivePriority ASC, programName ASC).
func sortRebuildSlots(slots []rebuildSlot) {
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0; j-- {
			pi := effectivePriority(slots[j].Priority)
			pj := effectivePriority(slots[j-1].Priority)
			if pi < pj || (pi == pj &&
				slots[j].ProgramName < slots[j-1].ProgramName) {
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
