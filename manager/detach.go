package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
)

// Detach removes a link by link ID.
//
// This detaches the link from the kernel (if pinned) and removes it
// from the store. The associated program remains loaded.
//
// For XDP and TC links attached via dispatchers, the dispatcher link
// count is queried after the link is removed. If no extensions
// remain, the dispatcher is cleaned up automatically (pins removed,
// deleted from store).
//
// Preflight failures (store lookup, not-managed check, dispatcher key
// extraction) return plain errors.
func (m *Manager) Detach(ctx context.Context, linkID kernel.LinkID) error {
	// Preflight: get link record.
	record, err := m.getLink(ctx, linkID)
	if err != nil {
		var notFound bpfman.ErrLinkNotFound
		if errors.As(err, &notFound) {
			if _, kerr := m.kernel.GetLinkByID(ctx, linkID); kerr == nil {
				return bpfman.ErrLinkNotManaged{LinkID: linkID}
			}
		}
		return err
	}

	// Preflight: get dispatcher state for post-detach cleanup.
	var dispState *dispatcher.State
	if record.Kind == bpfman.LinkKindXDP || record.Kind == bpfman.LinkKindTC {
		dispType, nsid, ifindex, err := extractDispatcherKey(record.Details)
		if err != nil {
			return fmt.Errorf("extract dispatcher key: %w", err)
		}
		if dispType != "" {
			state, err := m.store.GetDispatcher(ctx, string(dispType), nsid, ifindex)
			if err != nil {
				m.logger.WarnContext(ctx, "failed to get dispatcher for cleanup", "error", err)
			} else {
				dispState = &state
			}
		}
	}

	m.logger.InfoContext(ctx, "detaching link",
		"link_id", linkID, "kind", record.Kind, "pin_path", record.PinPath)

	plan := m.detachPlan(record, dispState)
	if err := operation.Run0(ctx, m.logger, m.executor, plan); err != nil {
		return err
	}

	m.logger.InfoContext(ctx, "removed link", "link_id", linkID, "kind", record.Kind)
	return nil
}

// detachPlan builds the operation plan for detaching a single link.
//
// Nodes:
//  1. (conditional) Do "detach-link" -- kernel detach via DetachLink.
//     Only included when the link has a pin path.
//  2. Do "delete-link" -- store delete via DeleteLink.
//  3. (conditional) Do "dispatcher-cleanup" -- calls
//     cleanupEmptyDispatcher. Only included when the link uses a
//     dispatcher (XDP/TC).
//
// No undo entries on any node. Detach is destructive and
// non-reversible.
func (m *Manager) detachPlan(
	record bpfman.LinkRecord, dispState *dispatcher.State,
) operation.Plan {
	var nodes []operation.Node
	target := fmt.Sprintf("%d", record.ID)

	if record.PinPath != nil {
		pinPath := record.PinPath.String()
		nodes = append(nodes, operation.Do(
			"detach-link", target,
			func(ctx context.Context, _ *operation.Bindings) error {
				return m.executor.Execute(ctx, action.DetachLink{PinPath: pinPath})
			},
		))
	}

	nodes = append(nodes, operation.Do(
		"delete-link", target,
		func(ctx context.Context, _ *operation.Bindings) error {
			return m.executor.Execute(ctx, action.DeleteLink{LinkID: record.ID})
		},
	))

	if dispState != nil {
		ds := *dispState
		nodes = append(nodes, operation.Do(
			"dispatcher-cleanup",
			fmt.Sprintf("%s:%d:%d", ds.Type, ds.Nsid, ds.Ifindex),
			func(ctx context.Context, _ *operation.Bindings) error {
				return m.cleanupEmptyDispatcher(ctx, ds)
			},
		))
	}

	return operation.Build(nodes...)
}

// extractDispatcherKey extracts dispatcher identification from link details.
// Returns empty dispType if the link type doesn't use dispatchers.
func extractDispatcherKey(details bpfman.LinkDetails) (dispType dispatcher.DispatcherType, nsid uint64, ifindex uint32, err error) {
	switch d := details.(type) {
	case bpfman.XDPDetails:
		return dispatcher.DispatcherTypeXDP, d.Nsid, d.Ifindex, nil
	case bpfman.TCDetails:
		switch d.Direction {
		case "ingress":
			return dispatcher.DispatcherTypeTCIngress, d.Nsid, d.Ifindex, nil
		case "egress":
			return dispatcher.DispatcherTypeTCEgress, d.Nsid, d.Ifindex, nil
		default:
			return "", 0, 0, fmt.Errorf("unknown TC direction: %s", d.Direction)
		}
	default:
		return "", 0, 0, nil
	}
}

// tcParentHandle returns the netlink parent handle for a TC dispatcher type.
func tcParentHandle(dispType dispatcher.DispatcherType) uint32 {
	switch dispType {
	case dispatcher.DispatcherTypeTCIngress:
		return netlink.HANDLE_MIN_INGRESS
	case dispatcher.DispatcherTypeTCEgress:
		return netlink.HANDLE_MIN_EGRESS
	default:
		return 0
	}
}

// cleanupEmptyDispatcher checks whether a dispatcher has any remaining
// extension links and, if not, removes it from both the kernel and the
// store. This is called after link removal to eagerly reclaim
// dispatchers that are no longer needed.
func (m *Manager) cleanupEmptyDispatcher(ctx context.Context, state dispatcher.State) error {
	remaining, err := m.store.CountDispatcherLinks(ctx, state.KernelID)
	if err != nil {
		m.logger.WarnContext(ctx, "failed to count dispatcher links", "error", err)
		return nil
	}
	if remaining > 0 {
		return nil
	}

	// For TC dispatchers, query the kernel for the filter handle
	// since it is no longer stored.
	var tcHandle uint32
	if state.Type == dispatcher.DispatcherTypeTCIngress || state.Type == dispatcher.DispatcherTypeTCEgress {
		parent := tcParentHandle(state.Type)
		handle, err := m.kernel.FindTCFilterHandle(ctx, int(state.Ifindex), parent, state.Priority)
		if err != nil {
			m.logger.WarnContext(ctx, "failed to find TC filter handle", "error", err)
		} else {
			tcHandle = handle
		}
	}

	cleanupActions := computeDispatcherCleanupActions(m.rt.BPFFS(), state, tcHandle)
	if err := m.executor.ExecuteAll(ctx, cleanupActions); err != nil {
		return fmt.Errorf("execute dispatcher cleanup actions: %w", err)
	}
	return nil
}

// collectDispatcherKeys examines links for dispatcher associations and
// returns a deduplicated set of dispatcher keys. This must be called
// before the links are deleted from the store.
func (m *Manager) collectDispatcherKeys(ctx context.Context, links []bpfman.LinkRecord) map[dispatcher.Key]struct{} {
	keys := make(map[dispatcher.Key]struct{})
	for _, link := range links {
		if link.Kind != bpfman.LinkKindTC && link.Kind != bpfman.LinkKindXDP {
			continue
		}
		record, err := m.store.GetLink(ctx, link.ID)
		if err != nil {
			m.logger.WarnContext(ctx, "failed to get link details for dispatcher cleanup",
				"link_id", link.ID, "error", err)
			continue
		}
		dispType, nsid, ifindex, err := extractDispatcherKey(record.Details)
		if err != nil {
			m.logger.WarnContext(ctx, "failed to extract dispatcher key",
				"link_id", link.ID, "error", err)
			continue
		}
		if dispType != "" {
			keys[dispatcher.Key{Type: dispType, Nsid: nsid, Ifindex: ifindex}] = struct{}{}
		}
	}
	return keys
}

// cleanupEmptyDispatchers checks each dispatcher in the set and
// removes any that no longer have extension links. Errors are logged
// but do not prevent cleanup of remaining dispatchers.
func (m *Manager) cleanupEmptyDispatchers(ctx context.Context, dispatchers map[dispatcher.Key]struct{}) {
	for key := range dispatchers {
		state, err := m.store.GetDispatcher(ctx, string(key.Type), key.Nsid, key.Ifindex)
		if err != nil {
			// Already gone (e.g., cleaned up by a concurrent
			// Detach call or GC). Nothing to do.
			continue
		}
		if err := m.cleanupEmptyDispatcher(ctx, state); err != nil {
			m.logger.WarnContext(ctx, "dispatcher cleanup failed",
				"type", key.Type,
				"nsid", key.Nsid,
				"ifindex", key.Ifindex,
				"error", err)
		}
	}
}

// computeDispatcherCleanupActions computes the actions needed to fully
// remove a dispatcher. It is only called when no extension links remain.
// For TC dispatchers, tcHandle is the kernel-assigned filter handle
// (queried at detach time); it is zero for XDP dispatchers.
func computeDispatcherCleanupActions(bpffs fs.BPFFS, state dispatcher.State, tcHandle uint32) []action.Action {
	progPinPath := bpffs.DispatcherProgPath(state.Type, state.Nsid, state.Ifindex, state.Revision)
	revisionDir := bpffs.DispatcherRevisionDir(state.Type, state.Nsid, state.Ifindex, state.Revision)
	var actions []action.Action

	// TC dispatchers use legacy netlink and must be detached via
	// RTM_DELTFILTER. XDP dispatchers use BPF links and are
	// detached by removing the link pin.
	if state.Type == dispatcher.DispatcherTypeTCIngress || state.Type == dispatcher.DispatcherTypeTCEgress {
		if tcHandle != 0 {
			actions = append(actions, action.DetachTCFilter{
				Ifindex:  int(state.Ifindex),
				Parent:   tcParentHandle(state.Type),
				Priority: state.Priority,
				Handle:   tcHandle,
			})
		}
	} else {
		linkPinPath := bpffs.DispatcherLinkPath(state.Type, state.Nsid, state.Ifindex)
		actions = append(actions, action.RemovePin{Path: linkPinPath})
	}

	actions = append(actions,
		action.RemovePin{Path: progPinPath},
		action.RemovePin{Path: revisionDir},
		action.DeleteDispatcher{
			Type:    string(state.Type),
			Nsid:    state.Nsid,
			Ifindex: state.Ifindex,
		},
	)

	return actions
}
