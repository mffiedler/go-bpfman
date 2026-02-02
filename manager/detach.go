package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/outcome"
)

// isNotFoundError returns true if err wraps store.ErrNotFound.
func isNotFoundError(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// Detach removes a link by link ID.
//
// This detaches the link from the kernel (if pinned) and removes it from the
// store. The associated program remains loaded.
//
// For XDP and TC links attached via dispatchers, the dispatcher link count
// is queried after the link is removed. If no extensions remain, the
// dispatcher is cleaned up automatically (pins removed, deleted from store).
//
// Pattern: FETCH -> EXECUTE (detach link) -> QUERY -> EXECUTE (cleanup)
func (m *Manager) Detach(ctx context.Context, linkID bpfman.LinkID) (result DetachResult, retErr error) {
	rec := outcome.NewRecorder(&result.Outcome)
	defer func() { rec.Finalise() }()
	target := fmt.Sprintf("%d", linkID)

	// FETCH: Get link record (includes details)
	record, err := m.store.GetLink(ctx, linkID)
	if err != nil {
		if isNotFoundError(err) {
			// Check if link exists in kernel but isn't managed by bpfman
			if _, kerr := m.kernel.GetLinkByID(ctx, uint32(linkID)); kerr == nil {
				retErr = bpfman.ErrLinkNotManaged{LinkID: linkID}
			} else {
				retErr = bpfman.ErrLinkNotFound{LinkID: linkID}
			}
		} else {
			retErr = fmt.Errorf("get link %d: %w", linkID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  retErr.Error(),
		})
		result.Outcome.PrimaryError = retErr.Error()
		return
	}

	// FETCH: Get dispatcher state if this is a dispatcher-based link
	var dispState *dispatcher.State
	if record.Kind == bpfman.LinkKindXDP || record.Kind == bpfman.LinkKindTC {
		dispType, nsid, ifindex, err := extractDispatcherKey(record.Details)
		if err != nil {
			retErr = fmt.Errorf("extract dispatcher key: %w", err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: target,
				Error:  retErr.Error(),
			})
			result.Outcome.PrimaryError = retErr.Error()
			return
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

	// Log before executing
	m.logger.InfoContext(ctx, "detaching link",
		"link_id", linkID,
		"kind", record.Kind,
		"pin_path", record.PinPath)

	// Phase 1: Detach the link and delete it from the store.
	linkActions := computeDetachLinkActions(record)
	linkSteps := computeDetachLinkSteps(record)
	if err := m.executor.ExecuteAll(ctx, linkActions); err != nil {
		retErr = fmt.Errorf("execute detach link actions: %w", err)
		// Record the failed step (we don't know which one failed, so mark the first)
		if len(linkSteps) > 0 {
			failedStep := linkSteps[0]
			failedStep.Error = retErr.Error()
			_ = rec.Fail(failedStep)
		} else {
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: target,
				Error:  retErr.Error(),
			})
		}
		result.Outcome.PrimaryError = retErr.Error()
		return
	}

	// Record successful detach steps
	for _, step := range linkSteps {
		_ = rec.Complete(step)
	}

	// Phase 2: If this was a dispatcher-based link, check whether the
	// dispatcher has any remaining extensions. Clean up if empty.
	if dispState != nil {
		if err := m.cleanupEmptyDispatcher(ctx, *dispState); err != nil {
			retErr = err
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindStoreDeleteDispatcher,
				Target: fmt.Sprintf("%s:%d:%d", dispState.Type, dispState.Nsid, dispState.Ifindex),
				Details: outcome.DispatcherDetails{
					DispatcherID: dispState.KernelID,
				},
				Error: retErr.Error(),
			})
			result.Outcome.PrimaryError = retErr.Error()
			return
		}
	}

	m.logger.InfoContext(ctx, "removed link", "link_id", linkID, "kind", record.Kind)
	return
}

// computeDetachLinkSteps generates outcome.Step entries corresponding to computeDetachLinkActions.
func computeDetachLinkSteps(record bpfman.LinkSpec) []outcome.Step {
	var steps []outcome.Step

	// Detach link from kernel if pinned
	if record.PinPath != nil {
		steps = append(steps, outcome.Step{
			Kind:   outcome.StepKindKernelDetachLink,
			Target: fmt.Sprintf("%d", record.ID),
			Details: outcome.LinkDetails{
				LinkID:  uint32(record.ID),
				PinPath: record.PinPath.String(),
			},
		})
	}

	// Delete link from store
	steps = append(steps, outcome.Step{
		Kind:   outcome.StepKindStoreDeleteLink,
		Target: fmt.Sprintf("%d", record.ID),
		Details: outcome.LinkDetails{
			LinkID: uint32(record.ID),
		},
	})

	return steps
}

// computeDetachLinkActions is a pure function that computes the actions
// needed to detach and delete a link from the store.
func computeDetachLinkActions(record bpfman.LinkSpec) []action.Action {
	var actions []action.Action

	// Detach link from kernel if pinned
	if record.PinPath != nil {
		actions = append(actions, action.DetachLink{PinPath: record.PinPath.String()})
	}

	// Delete link from store
	actions = append(actions, action.DeleteLink{LinkID: record.ID})

	return actions
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

	cleanupActions := computeDispatcherCleanupActions(m.dirs.FS(), state, tcHandle)
	if err := m.executor.ExecuteAll(ctx, cleanupActions); err != nil {
		return fmt.Errorf("execute dispatcher cleanup actions: %w", err)
	}
	return nil
}

// collectDispatcherKeys examines links for dispatcher associations and
// returns a deduplicated set of dispatcher keys. This must be called
// before the links are deleted from the store.
func (m *Manager) collectDispatcherKeys(ctx context.Context, links []bpfman.LinkSpec) map[dispatcher.Key]struct{} {
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
func computeDispatcherCleanupActions(bpffsRoot string, state dispatcher.State, tcHandle uint32) []action.Action {
	revisionDir := dispatcher.DispatcherRevisionDir(bpffsRoot, state.Type, state.Nsid, state.Ifindex, state.Revision)
	progPinPath := dispatcher.DispatcherProgPath(revisionDir)
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
		linkPinPath := dispatcher.DispatcherLinkPath(bpffsRoot, state.Type, state.Nsid, state.Ifindex)
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
