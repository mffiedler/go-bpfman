// Package reconcile contains pure functions for computing reconciliation actions.
// Functions in this package perform no I/O - they compare observed state and
// produce actions to bring the system into a consistent state.
package reconcile

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
)

// ReconcileActions computes the actions needed to reconcile store state
// with kernel state. This is a pure function - no I/O.
//
// The returned actions are ordered to respect foreign key constraints:
// programs that reference a map owner (MapOwnerID != 0) are deleted before
// the programs they reference (MapOwnerID == 0). This ensures the schema's
// ON DELETE RESTRICT constraint on map_owner_id is satisfied.
func ReconcileActions(
	stored map[uint32]bpfman.ProgramRecord,
	kps []kernel.Program,
) []action.Action {
	// Build set of kernel program IDs
	kernelIDs := make(map[uint32]bool, len(kps))
	for _, kp := range kps {
		kernelIDs[kp.ID] = true
	}

	// Collect stale programs, separating dependents from owners
	var dependents, owners []uint32
	for id, prog := range stored {
		if !kernelIDs[id] {
			if prog.Handles.MapOwnerID != nil {
				dependents = append(dependents, id)
			} else {
				owners = append(owners, id)
			}
		}
	}

	// Build actions: dependents first, then owners
	actions := make([]action.Action, 0, len(dependents)+len(owners))
	for _, id := range dependents {
		actions = append(actions, action.DeleteProgram{KernelID: id})
	}
	for _, id := range owners {
		actions = append(actions, action.DeleteProgram{KernelID: id})
	}

	return actions
}

// OrphanedPrograms returns IDs of programs in store that no longer exist in kernel.
// Pure function.
func OrphanedPrograms(
	stored map[uint32]bpfman.ProgramRecord,
	kps []kernel.Program,
) []uint32 {
	kernelIDs := make(map[uint32]bool, len(kps))
	for _, kp := range kps {
		kernelIDs[kp.ID] = true
	}

	var orphaned []uint32
	for id := range stored {
		if !kernelIDs[id] {
			orphaned = append(orphaned, id)
		}
	}
	return orphaned
}

// UnmanagedPrograms returns kernel programs not tracked in the store.
// Pure function.
func UnmanagedPrograms(
	stored map[uint32]bpfman.ProgramRecord,
	kps []kernel.Program,
) []kernel.Program {
	var unmanaged []kernel.Program
	for _, kp := range kps {
		if _, exists := stored[kp.ID]; !exists {
			unmanaged = append(unmanaged, kp)
		}
	}
	return unmanaged
}

// ReconcileDispatcherActions computes actions to remove stale dispatcher entries.
// A dispatcher is stale if its KernelID doesn't exist in the kernel program set.
// Pure function - no I/O.
func ReconcileDispatcherActions(
	dispatchers []dispatcher.State,
	kernelPrograms []kernel.Program,
) []action.Action {
	// Build set of kernel program IDs
	kernelIDs := make(map[uint32]bool, len(kernelPrograms))
	for _, kp := range kernelPrograms {
		kernelIDs[kp.ID] = true
	}

	var actions []action.Action
	for _, disp := range dispatchers {
		if !kernelIDs[disp.KernelID] {
			actions = append(actions, action.DeleteDispatcher{
				Type:    string(disp.Type),
				Nsid:    disp.Nsid,
				Ifindex: disp.Ifindex,
			})
		}
	}
	return actions
}

// ReconcileLinkActions computes actions to remove stale link entries.
// A link is stale if its ID (which is the kernel link ID for non-synthetic
// links) doesn't exist in the kernel link set.
// Synthetic links (perf_event-based) are skipped since they don't have
// kernel link IDs.
// Pure function - no I/O.
func ReconcileLinkActions(
	links []bpfman.LinkSpec,
	kernelLinks []kernel.Link,
) []action.Action {
	// Build set of kernel link IDs
	kernelIDs := make(map[uint32]bool, len(kernelLinks))
	for _, kl := range kernelLinks {
		kernelIDs[kl.ID] = true
	}

	var actions []action.Action
	for _, link := range links {
		// Skip synthetic links (perf_event-based) - they don't have kernel link IDs
		if link.IsSynthetic() {
			continue
		}
		// For non-synthetic links, ID is the kernel link ID
		if !kernelIDs[uint32(link.ID)] {
			actions = append(actions, action.DeleteLink{LinkID: link.ID})
		}
	}
	return actions
}
