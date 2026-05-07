package manager

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/inspect"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/platform"
)

// dispKey identifies a dispatcher for deduplication in GC.
type dispKey struct {
	Type    dispatcher.DispatcherType
	Nsid    uint64
	Ifindex uint32
}

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
	dispatchers []platform.DispatcherSummary,
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
	deletedDispatchers := make(map[dispKey]bool)
	for _, disp := range dispatchers {
		if !kernelPrograms[disp.Runtime.ProgramID] {
			actions = append(actions, action.DeleteDispatcher{
				Type:    disp.Key.Type,
				Nsid:    disp.Key.Nsid,
				Ifindex: disp.Key.Ifindex,
			})
			deletedDispatchers[dispKey{disp.Key.Type, disp.Key.Nsid, disp.Key.Ifindex}] = true
		}
	}

	// Phase 3: stale non-synthetic, non-dispatcher-backed links.
	// Dispatcher-backed links (XDP/TC) are not deleted here; their
	// lifecycle is owned by DispatcherStore operations (phase 2
	// handles dead dispatchers, phase 4 handles orphaned ones).
	deletedLinks := make(map[kernel.LinkID]bool)
	for _, link := range links {
		if link.IsSynthetic() {
			continue
		}
		if link.Kind == bpfman.LinkKindXDP || link.Kind == bpfman.LinkKindTC {
			continue
		}
		if !kernelLinks[link.ID] {
			actions = append(actions, action.DeleteLink{LinkID: link.ID})
			deletedLinks[link.ID] = true
		}
	}

	// Phase 4: orphaned dispatchers. Only needed if links were
	// deleted in phase 3, because that is the only way a
	// dispatcher can lose its last extension link within a single
	// GC pass. Extension links deleted by phase 2 (dead
	// dispatchers) are handled by DeleteDispatcherSnapshot.
	if len(deletedLinks) == 0 {
		return actions
	}
	for _, disp := range dispatchers {
		dk := dispKey{disp.Key.Type, disp.Key.Nsid, disp.Key.Ifindex}
		if deletedDispatchers[dk] {
			continue // already deleted in phase 2
		}
		if countExtensionLinks(links, deletedLinks, disp.Runtime.ProgramID) == 0 {
			actions = append(actions, action.DeleteDispatcher{
				Type:    disp.Key.Type,
				Nsid:    disp.Key.Nsid,
				Ifindex: disp.Key.Ifindex,
			})
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

// storeGCInputs holds the data extracted from an inspect.Observation that
// computeStoreGC needs to decide which store entries are stale.
type storeGCInputs struct {
	programs       map[kernel.ProgramID]bpfman.ProgramRecord
	dispatchers    []platform.DispatcherSummary
	links          []bpfman.LinkRecord
	kernelPrograms map[kernel.ProgramID]bool
	kernelLinks    map[kernel.LinkID]bool
}

// deriveStoreGCInputs extracts the inputs for computeStoreGC from
// an inspect.Observation snapshot. For store-managed programs with a live
// kernel ID but a missing pin, the kernel ID is excluded from the
// alive set; the ID may have been recycled to a program we do not
// own.
func deriveStoreGCInputs(obs *inspect.Observation) storeGCInputs {
	in := storeGCInputs{
		kernelPrograms: make(map[kernel.ProgramID]bool),
		programs:       make(map[kernel.ProgramID]bpfman.ProgramRecord),
		kernelLinks:    make(map[kernel.LinkID]bool),
	}

	for _, p := range obs.Programs {
		if p.Presence.InStore && p.Managed != nil {
			in.programs[p.ProgramID] = *p.Managed
		}
		if p.Presence.InKernel {
			// For store-managed programs, a missing pin means
			// the kernel ID may have been recycled. Exclude it
			// from the alive set so the store entry is reaped.
			if p.Presence.InStore && !p.Presence.InFS {
				continue
			}
			in.kernelPrograms[p.ProgramID] = true
		}
	}

	for _, d := range obs.ManagedDispatchers() {
		if d.Managed != nil {
			in.dispatchers = append(in.dispatchers, *d.Managed)
		}
	}

	for _, l := range obs.Links {
		if l.Presence.InKernel {
			if id := l.KernelLinkID(); id != nil {
				in.kernelLinks[*id] = true
			}
		}
		if l.Presence.InStore && l.Managed != nil {
			in.links = append(in.links, *l.Managed)
		}
	}

	return in
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
