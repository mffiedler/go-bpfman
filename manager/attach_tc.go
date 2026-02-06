package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/netns"
	"github.com/frobware/go-bpfman/outcome"
)

// TC proceed-on action bits (matches TC_ACT_* return codes).
const (
	tcProceedOnOK               = 1 << 0  // TC_ACT_OK
	tcProceedOnPipe             = 1 << 3  // TC_ACT_PIPE
	tcProceedOnDispatcherReturn = 1 << 30 // bpfman-specific sentinel
)

// DefaultTCProceedOn is the default bitmask for TC proceed-on actions.
var DefaultTCProceedOn = tcProceedOnOK | tcProceedOnPipe | tcProceedOnDispatcherReturn

// AttachTC attaches a TC program to a network interface using the
// dispatcher model for multi-program chaining.
//
// The dispatcher is created automatically if it doesn't exist for the interface
// and direction combination. Programs are attached as extensions (freplace) to
// dispatcher slots.
//
// Pin paths follow the Rust bpfman convention:
//   - Dispatcher link: /sys/fs/bpf/bpfman/tc-{direction}/dispatcher_{nsid}_{ifindex}_link
//   - Dispatcher prog: /sys/fs/bpf/bpfman/tc-{direction}/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher
//   - Extension links: /sys/fs/bpf/bpfman/tc-{direction}/dispatcher_{nsid}_{ifindex}_{revision}/link_{position}
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachTC(ctx context.Context, spec bpfman.TCAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()
	ifindex := spec.Ifindex()
	ifname := spec.Ifname()
	direction := spec.Direction()
	priority := spec.Priority()
	proceedOn := spec.ProceedOn()
	netnsPath := spec.Netns()
	target := ifname + ":" + string(direction)

	// FETCH: Get program metadata to access ObjectPath and ProgramName
	prog, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// FETCH: Get network namespace ID (from target namespace if specified)
	nsid, err := netns.GetNsid(netnsPath)
	if err != nil {
		primaryErr := fmt.Errorf("get nsid: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// Determine dispatcher type based on direction
	var dispType dispatcher.DispatcherType
	if direction == bpfman.TCDirectionIngress {
		dispType = dispatcher.DispatcherTypeTCIngress
	} else {
		dispType = dispatcher.DispatcherTypeTCEgress
	}

	// FETCH: Look up existing dispatcher or create new one.
	dispState, err := m.store.GetDispatcher(ctx, string(dispType), nsid, uint32(ifindex))
	if errors.Is(err, store.ErrNotFound) {
		// KERNEL I/O + EXECUTE: Create new dispatcher
		dispState, err = m.createTCDispatcher(ctx, nsid, uint32(ifindex), ifname, direction, dispType, netnsPath)
		if err != nil {
			primaryErr := fmt.Errorf("create TC dispatcher for %s %s: %w", ifname, direction, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachTCDispatcher,
				Target: target,
				Details: outcome.DispatcherDetails{
					Interface: ifname,
					Direction: string(direction),
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		// Record dispatcher creation
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindAttachTCDispatcher,
			Target: target,
			Details: outcome.DispatcherDetails{
				DispatcherID: dispState.KernelID,
				Interface:    ifname,
				Direction:    string(direction),
			},
		})
	} else if err != nil {
		primaryErr := fmt.Errorf("get dispatcher: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	m.logger.DebugContext(ctx, "using TC dispatcher",
		"interface", ifname,
		"direction", direction,
		"nsid", nsid,
		"ifindex", ifindex,
		"revision", dispState.Revision,
		"dispatcher_id", dispState.KernelID)

	// COMPUTE: Calculate extension link path from conventions
	revisionDir := dispatcher.DispatcherRevisionDir(m.root.BPFFS().FS(), dispType, nsid, uint32(ifindex), dispState.Revision)
	position, err := m.store.CountDispatcherLinks(ctx, dispState.KernelID)
	if err != nil {
		primaryErr := fmt.Errorf("count dispatcher links: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}
	linkPinPath := dispatcher.ExtensionLinkPath(revisionDir, position)

	// COMPUTE: Use the program's MapPinPath which points to the correct maps
	// directory (either the program's own or the map owner's if sharing).
	mapPinDir := prog.Handles.MapPinPath

	// KERNEL I/O: Attach user program as extension
	progPinPath := dispatcher.DispatcherProgPath(revisionDir)
	extSpec := dispatcher.TCExtensionAttachSpec{
		DispatcherPinPath: progPinPath,
		ObjectPath:        prog.Load.ObjectPath(),
		ProgramName:       prog.Meta.Name,
		Position:          position,
		LinkPinPath:       linkPinPath,
		MapPinDir:         mapPinDir,
	}
	link, err := m.kernel.AttachTCExtension(ctx, extSpec)
	if err != nil {
		// The dispatcher DB record may be stale: the kernel program
		// survives (held by a tc filter) but its bpffs pin is gone
		// after a fresh mount. GC keeps the record because the kernel
		// program exists, but the pin path is invalid. Delete the
		// stale record and retry once with a fresh dispatcher.
		if !errors.Is(err, os.ErrNotExist) {
			primaryErr := fmt.Errorf("attach TC extension to %s %s slot %d: %w", ifname, direction, position, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachExtension,
				Target: target,
				Details: outcome.LinkDetails{
					ProgramID:    programKernelID,
					Interface:    ifname,
					PinPath:      linkPinPath,
					DispatcherID: dispState.KernelID,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		m.logger.WarnContext(ctx, "dispatcher pin missing, recreating",
			"prog_pin_path", progPinPath,
			"dispatcher_id", dispState.KernelID,
			"error", err)
		if delErr := m.store.DeleteDispatcher(ctx, string(dispType), nsid, uint32(ifindex)); delErr != nil {
			primaryErr := fmt.Errorf("delete stale TC dispatcher: %w", delErr)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindStoreDeleteDispatcher,
				Target: target,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		dispState, err = m.createTCDispatcher(ctx, nsid, uint32(ifindex), ifname, direction, dispType, netnsPath)
		if err != nil {
			primaryErr := fmt.Errorf("recreate TC dispatcher for %s %s: %w", ifname, direction, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachTCDispatcher,
				Target: target,
				Details: outcome.DispatcherDetails{
					Interface: ifname,
					Direction: string(direction),
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		// Recalculate paths for the fresh dispatcher
		revisionDir = dispatcher.DispatcherRevisionDir(m.root.BPFFS().FS(), dispType, nsid, uint32(ifindex), dispState.Revision)
		position, err = m.store.CountDispatcherLinks(ctx, dispState.KernelID)
		if err != nil {
			primaryErr := fmt.Errorf("count dispatcher links after recreate: %w", err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: target,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		linkPinPath = dispatcher.ExtensionLinkPath(revisionDir, position)
		progPinPath = dispatcher.DispatcherProgPath(revisionDir)
		extSpec = dispatcher.TCExtensionAttachSpec{
			DispatcherPinPath: progPinPath,
			ObjectPath:        prog.Load.ObjectPath(),
			ProgramName:       prog.Meta.Name,
			Position:          position,
			LinkPinPath:       linkPinPath,
			MapPinDir:         mapPinDir,
		}
		link, err = m.kernel.AttachTCExtension(ctx, extSpec)
		if err != nil {
			primaryErr := fmt.Errorf("attach TC extension to %s %s slot %d (after recreate): %w", ifname, direction, position, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachExtension,
				Target: target,
				Details: outcome.LinkDetails{
					ProgramID:    programKernelID,
					Interface:    ifname,
					PinPath:      linkPinPath,
					DispatcherID: dispState.KernelID,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
	}

	// Record successful extension attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachExtension,
		Target: target,
		Details: outcome.LinkDetails{
			LinkID:       uint32(link.Spec.ID),
			ProgramID:    programKernelID,
			Interface:    ifname,
			PinPath:      linkPinPath,
			DispatcherID: dispState.KernelID,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// COMPUTE: Update link record with TC details
	// The kernel attach function populates ID, Kind, PinPath, CreatedAt
	// We need to add the TC-specific details
	link.Spec.Details = bpfman.TCDetails{
		Interface:    ifname,
		Ifindex:      uint32(ifindex),
		Direction:    direction,
		Priority:     int32(priority),
		Position:     int32(position),
		ProceedOn:    proceedOn,
		Nsid:         nsid,
		DispatcherID: dispState.KernelID,
		Revision:     dispState.Revision,
	}

	// EXECUTE: Save link metadata directly to store
	// The link ID is populated by the kernel attach function (kernel-assigned for real links)
	// Set the program ID before saving (kernel adapter doesn't know it)
	link.Spec.ProgramID = programKernelID
	if err := m.store.SaveLink(ctx, link.Spec); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Spec.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Spec.ID),
				ProgramID: programKernelID,
				Interface: ifname,
				PinPath:   linkPinPath,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
			}
			rec.SetRollbackErrors(toOutcomeErrors(rbErrs))
			_ = rec.RollbackFail(outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: fmt.Sprintf("%d", link.Spec.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.Spec.ID),
					PinPath: linkPinPath,
				},
				Error: rbErrs[0].Err.Error(),
			})
		} else {
			_ = rec.RollbackComplete(outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: fmt.Sprintf("%d", link.Spec.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.Spec.ID),
					PinPath: linkPinPath,
				},
			})
		}
		return fail(storeErr)
	}

	// Record successful store save
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreSaveLink,
		Target: fmt.Sprintf("%d", link.Spec.ID),
		Details: outcome.LinkDetails{
			LinkID:    uint32(link.Spec.ID),
			ProgramID: programKernelID,
			Interface: ifname,
			PinPath:   linkPinPath,
		},
	})

	m.logger.InfoContext(ctx, "attached TC via dispatcher",
		"link_id", link.Spec.ID,
		"program_id", programKernelID,
		"interface", ifname,
		"direction", direction,
		"ifindex", ifindex,
		"nsid", nsid,
		"position", position,
		"revision", dispState.Revision,
		"pin_path", linkPinPath)

	return link, nil
}

// AttachTCX attaches a TCX program to a network interface using native
// kernel multi-program support. Unlike TC, TCX doesn't use dispatchers.
//
// Pin paths follow the convention:
//   - Link: /sys/fs/bpf/bpfman/tcx-{direction}/link_{nsid}_{ifindex}_{linkid}
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachTCX(ctx context.Context, spec bpfman.TCXAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()
	ifindex := spec.Ifindex()
	ifname := spec.Ifname()
	direction := spec.Direction()
	priority := spec.Priority()
	netnsPath := spec.Netns()
	target := ifname + ":" + string(direction)

	// FETCH: Get program metadata to find pin path
	prog, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// Verify program type is TCX
	if prog.Load.ProgramType() != bpfman.ProgramTypeTCX {
		primaryErr := fmt.Errorf("program %d is type %s, not tcx", programKernelID, prog.Load.ProgramType())
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// FETCH: Get network namespace ID (from target namespace if specified)
	nsid, err := netns.GetNsid(netnsPath)
	if err != nil {
		primaryErr := fmt.Errorf("get nsid: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Calculate link pin path from conventions.
	// The path must be unique per program to support multiple TCX programs
	// on the same interface — each needs its own pinned link to keep the
	// kernel attachment alive.
	dirName := fmt.Sprintf("tcx-%s", direction)
	linkPinPath := filepath.Join(m.root.BPFFS().FS(), dirName, fmt.Sprintf("link_%d_%d_%d", nsid, ifindex, programKernelID))

	// KERNEL I/O: Remove stale pin if it exists from a previous daemon run.
	if _, statErr := os.Stat(linkPinPath); statErr == nil {
		m.logger.WarnContext(ctx, "removing stale TCX link pin", "path", linkPinPath)
		if removeErr := os.Remove(linkPinPath); removeErr != nil {
			primaryErr := fmt.Errorf("remove stale TCX link pin %s: %w", linkPinPath, removeErr)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindKernelRemovePin,
				Target: linkPinPath,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
	}

	// COMPUTE: Use the stored program pin path directly
	progPinPath := prog.Handles.PinPath

	// FETCH: Get existing TCX links for this interface/direction to compute order
	existingLinks, err := m.store.ListTCXLinksByInterface(ctx, nsid, uint32(ifindex), string(direction))
	if err != nil {
		primaryErr := fmt.Errorf("list existing TCX links: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Determine attach order based on priority
	// Lower priority values should run first (earlier in chain).
	// We need to find where to insert this program in the priority-sorted chain.
	order := computeTCXAttachOrder(existingLinks, int32(priority))

	m.logger.DebugContext(ctx, "computed TCX attach order",
		"program_id", programKernelID,
		"priority", priority,
		"existing_links", len(existingLinks),
		"order", order)

	// KERNEL I/O: Attach program using TCX link with computed order
	link, err := m.kernel.AttachTCX(ctx, ifindex, string(direction), progPinPath, linkPinPath, netnsPath, order)
	if err != nil {
		primaryErr := fmt.Errorf("attach TCX to %s %s: %w", ifname, direction, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindAttachTCX,
			Target: target,
			Details: outcome.LinkDetails{
				ProgramID: programKernelID,
				Interface: ifname,
				PinPath:   linkPinPath,
			},
			Error: primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// Record successful TCX attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachTCX,
		Target: target,
		Details: outcome.LinkDetails{
			LinkID:    uint32(link.Spec.ID),
			ProgramID: programKernelID,
			Interface: ifname,
			PinPath:   linkPinPath,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// COMPUTE: Update link record with TCX details
	// The kernel attach function populates ID, Kind, PinPath, CreatedAt
	// We need to add the TCX-specific details
	link.Spec.Details = bpfman.TCXDetails{
		Interface: ifname,
		Ifindex:   uint32(ifindex),
		Direction: direction,
		Priority:  int32(priority),
		Nsid:      nsid,
	}

	// EXECUTE: Save link metadata directly to store
	// The link ID is populated by the kernel attach function (kernel-assigned for real links)
	// Set the program ID before saving (kernel adapter doesn't know it)
	link.Spec.ProgramID = programKernelID
	if err := m.store.SaveLink(ctx, link.Spec); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save TCX link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Spec.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Spec.ID),
				ProgramID: programKernelID,
				Interface: ifname,
				PinPath:   linkPinPath,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
			}
			rec.SetRollbackErrors(toOutcomeErrors(rbErrs))
			_ = rec.RollbackFail(outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: fmt.Sprintf("%d", link.Spec.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.Spec.ID),
					PinPath: linkPinPath,
				},
				Error: rbErrs[0].Err.Error(),
			})
		} else {
			_ = rec.RollbackComplete(outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: fmt.Sprintf("%d", link.Spec.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.Spec.ID),
					PinPath: linkPinPath,
				},
			})
		}
		return fail(storeErr)
	}

	// Record successful store save
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreSaveLink,
		Target: fmt.Sprintf("%d", link.Spec.ID),
		Details: outcome.LinkDetails{
			LinkID:    uint32(link.Spec.ID),
			ProgramID: programKernelID,
			Interface: ifname,
			PinPath:   linkPinPath,
		},
	})

	m.logger.InfoContext(ctx, "attached TCX program",
		"link_id", link.Spec.ID,
		"program_id", programKernelID,
		"interface", ifname,
		"direction", direction,
		"ifindex", ifindex,
		"nsid", nsid,
		"priority", priority,
		"pin_path", linkPinPath)

	return link, nil
}

// computeTCXAttachOrder determines where to insert a new TCX program in the chain
// based on its priority relative to existing programs. Lower priority values run first.
//
// The algorithm:
// 1. If no existing links, attach at head (first)
// 2. Find the first existing link with priority > newPriority, attach before it
// 3. If all existing links have priority <= newPriority, attach after the last one
//
// This ensures programs are ordered by priority, with ties broken by insertion order.
func computeTCXAttachOrder(existingLinks []bpfman.TCXLinkInfo, newPriority int32) bpfman.TCXAttachOrder {
	if len(existingLinks) == 0 {
		// No existing links, attach at head
		return bpfman.TCXAttachFirst()
	}

	// Links are already sorted by priority ASC from the query
	// Find the first link with higher priority (should come after us)
	for _, link := range existingLinks {
		if link.Priority > newPriority {
			// This link has higher priority (runs later), we should attach before it
			return bpfman.TCXAttachBefore(link.KernelProgramID)
		}
	}

	// All existing links have priority <= ours, attach after the last one
	lastLink := existingLinks[len(existingLinks)-1]
	return bpfman.TCXAttachAfter(lastLink.KernelProgramID)
}

// createTCDispatcher creates a new TC dispatcher for the given interface and direction.
// The dispatcher is attached via legacy netlink TC (clsact qdisc + BPF filter),
// matching the upstream Rust bpfman approach.
//
// Pattern: COMPUTE -> KERNEL I/O -> COMPUTE -> EXECUTE
func (m *Manager) createTCDispatcher(ctx context.Context, nsid uint64, ifindex uint32, ifname string, direction bpfman.TCDirection, dispType dispatcher.DispatcherType, netnsPath string) (dispatcher.State, error) {
	// COMPUTE: Calculate paths according to Rust bpfman convention.
	// TC dispatchers do not use a link pin — legacy netlink TC has no
	// BPF link to pin. The filter is identified by handle + priority.
	revision := uint32(1)
	revisionDir := dispatcher.DispatcherRevisionDir(m.root.BPFFS().FS(), dispType, nsid, ifindex, revision)
	progPinPath := dispatcher.DispatcherProgPath(revisionDir)

	m.logger.InfoContext(ctx, "creating TC dispatcher",
		"direction", direction,
		"nsid", nsid,
		"ifindex", ifindex,
		"ifname", ifname,
		"netns", netnsPath,
		"revision", revision,
		"prog_pin_path", progPinPath)

	// KERNEL I/O: Create TC dispatcher using legacy netlink TC
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
	result, err := m.kernel.AttachTCDispatcher(ctx, spec)
	if err != nil {
		return dispatcher.State{}, err
	}

	// ROLLBACK: If the store write fails, undo kernel state.
	// Order: remove prog pin first, then detach the TC filter.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.RemovePin(ctx, progPinPath)
	})
	undo.push(func() error {
		return m.kernel.DetachTCFilter(ctx, int(ifindex), ifname, tcParentHandle(dispType), result.Priority, result.Handle)
	})

	// COMPUTE: Build save action from kernel result
	state := computeTCDispatcherState(dispType, nsid, ifindex, revision, result)
	saveAction := action.SaveDispatcher{State: state}

	// EXECUTE: Save through executor
	if err := m.executor.Execute(ctx, saveAction); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back TC dispatcher", "ifname", ifname, "error", err)
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
			}
			return dispatcher.State{}, errors.Join(fmt.Errorf("save TC dispatcher: %w", err), errors.New("rollback failed"))
		}
		return dispatcher.State{}, fmt.Errorf("save TC dispatcher: %w", err)
	}

	m.logger.InfoContext(ctx, "created TC dispatcher",
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

// computeTCDispatcherState is a pure function that builds a DispatcherState
// from TC kernel attach results.
func computeTCDispatcherState(
	dispType dispatcher.DispatcherType,
	nsid uint64,
	ifindex, revision uint32,
	result *interpreter.TCDispatcherResult,
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
