package manager

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
)

// computeStoreGC is a pure function that decides which store entries
// to delete based on a snapshot of the database and the set of
// kernel-alive IDs. It returns a sequence of actions that, when
// executed, remove every stale entry.
//
// The four phases mirror the previous store.GC implementation:
//
//  1. Stale programs (dependents before owners for FK ordering).
//  2. Stale dispatchers (kernel ID not in alive set).
//  3. Stale non-synthetic links (link ID not in alive set).
//  4. Orphaned dispatchers (surviving dispatchers with no remaining
//     extension links after phase 3).
func computeStoreGC(
	programs map[kernel.ProgramID]bpfman.ProgramRecord,
	dispatchers []dispatcher.State,
	links []bpfman.LinkRecord,
	kernelPrograms map[kernel.ProgramID]bool,
	kernelLinks map[kernel.LinkID]bool,
) []action.Action {
	var actions []action.Action

	// Phase 1: stale programs. Delete dependents (MapOwnerID != nil)
	// before owners so FK constraints are respected.
	var dependents, owners []kernel.ProgramID
	for id, prog := range programs {
		if kernelPrograms[id] {
			continue
		}
		if prog.Handles.MapOwnerID != nil {
			dependents = append(dependents, id)
		} else {
			owners = append(owners, id)
		}
	}
	for _, id := range dependents {
		actions = append(actions, action.DeleteProgram{ProgramID: id})
	}
	for _, id := range owners {
		actions = append(actions, action.DeleteProgram{ProgramID: id})
	}

	// Phase 2: stale dispatchers. Track which dispatcher keys were
	// deleted so phase 4 can skip them.
	type dispKey struct {
		Type    dispatcher.DispatcherType
		Nsid    uint64
		Ifindex uint32
	}
	deletedDispatchers := make(map[dispKey]bool)
	for _, disp := range dispatchers {
		if !kernelPrograms[disp.ProgramID] {
			actions = append(actions, action.DeleteDispatcher{
				Type:    disp.Type,
				Nsid:    disp.Nsid,
				Ifindex: disp.Ifindex,
			})
			deletedDispatchers[dispKey{disp.Type, disp.Nsid, disp.Ifindex}] = true
		}
	}

	// Phase 3: stale non-synthetic links.
	deletedLinks := make(map[kernel.LinkID]bool)
	for _, link := range links {
		if link.IsSynthetic() {
			continue
		}
		if !kernelLinks[link.ID] {
			actions = append(actions, action.DeleteLink{LinkID: link.ID})
			deletedLinks[link.ID] = true
		}
	}

	// Phase 4: orphaned dispatchers. Only needed if links were
	// deleted, because that is the only way a dispatcher can lose
	// its last extension link.
	if len(deletedLinks) > 0 {
		for _, disp := range dispatchers {
			dk := dispKey{disp.Type, disp.Nsid, disp.Ifindex}
			if deletedDispatchers[dk] {
				continue // already deleted in phase 2
			}
			if countExtensionLinks(links, deletedLinks, disp.ProgramID) == 0 {
				actions = append(actions, action.DeleteDispatcher{
					Type:    disp.Type,
					Nsid:    disp.Nsid,
					Ifindex: disp.Ifindex,
				})
			}
		}
	}

	return actions
}

// countByType counts the number of actions in the slice that match
// the given concrete type T.
func countByType[T action.Action](actions []action.Action) int {
	n := 0
	for _, a := range actions {
		if _, ok := a.(T); ok {
			n++
		}
	}
	return n
}

// countExtensionLinks counts the non-deleted extension links that
// reference the given dispatcher kernel ID. An extension link is an
// XDP or TC link whose DispatcherID matches.
func countExtensionLinks(links []bpfman.LinkRecord, deletedLinks map[kernel.LinkID]bool, dispatcherProgramID kernel.ProgramID) int {
	count := 0
	for _, link := range links {
		if deletedLinks[link.ID] {
			continue
		}
		switch d := link.Details.(type) {
		case bpfman.XDPDetails:
			if d.DispatcherID == dispatcherProgramID {
				count++
			}
		case bpfman.TCDetails:
			if d.DispatcherID == dispatcherProgramID {
				count++
			}
		}
	}
	return count
}
