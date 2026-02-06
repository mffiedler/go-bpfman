package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/netns"
	"github.com/frobware/go-bpfman/outcome"
)

// XDP proceed-on action bits (matches XDP return codes).
const (
	xdpProceedOnPass = 1 << 2 // Continue to next program on XDP_PASS
)

// AttachXDP attaches an XDP program to a network interface using the
// dispatcher model for multi-program chaining.
//
// The dispatcher is created automatically if it doesn't exist for the interface.
// Programs are attached as extensions (freplace) to dispatcher slots.
// The program is reloaded from its original ObjectPath as Extension type.
//
// Pin paths follow the Rust bpfman convention:
//   - Dispatcher link: /sys/fs/bpf/bpfman/xdp/dispatcher_{nsid}_{ifindex}_link
//   - Dispatcher prog: /sys/fs/bpf/bpfman/xdp/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher
//   - Extension links: /sys/fs/bpf/bpfman/xdp/dispatcher_{nsid}_{ifindex}_{revision}/link_{position}
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachXDP(ctx context.Context, spec bpfman.XDPAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
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
	netnsPath := spec.Netns()
	target := ifname + ":xdp"

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

	// FETCH: Look up existing dispatcher or create new one.
	dispState, err := m.store.GetDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), nsid, uint32(ifindex))
	if errors.Is(err, store.ErrNotFound) {
		// KERNEL I/O + EXECUTE: Create new dispatcher
		dispState, err = m.createXDPDispatcher(ctx, nsid, uint32(ifindex), netnsPath)
		if err != nil {
			primaryErr := fmt.Errorf("create XDP dispatcher for %s: %w", ifname, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachXDPDispatcher,
				Target: target,
				Details: outcome.DispatcherDetails{
					Interface: ifname,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		// Record dispatcher creation
		_ = rec.Complete(outcome.Step{
			Kind:   outcome.StepKindAttachXDPDispatcher,
			Target: target,
			Details: outcome.DispatcherDetails{
				DispatcherID: dispState.KernelID,
				Interface:    ifname,
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

	m.logger.DebugContext(ctx, "using dispatcher",
		"interface", ifname,
		"nsid", nsid,
		"ifindex", ifindex,
		"revision", dispState.Revision,
		"dispatcher_id", dispState.KernelID)

	// COMPUTE: Calculate extension link path from conventions
	revisionDir := dispatcher.DispatcherRevisionDir(m.root.BPFFS().MountPoint(), dispatcher.DispatcherTypeXDP, nsid, uint32(ifindex), dispState.Revision)
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
	extSpec := dispatcher.XDPExtensionAttachSpec{
		DispatcherPinPath: progPinPath,
		ObjectPath:        prog.Load.ObjectPath(),
		ProgramName:       prog.Meta.Name,
		Position:          position,
		LinkPinPath:       linkPinPath,
		MapPinDir:         mapPinDir,
	}
	attachOut, err := m.kernel.AttachXDPExtension(ctx, extSpec)
	if err != nil {
		// Stale dispatcher recovery: the DB record exists but the
		// bpffs pin is gone (e.g., fresh mount after pod restart while
		// the kernel program survives via XDP link). Delete the stale
		// record and retry with a fresh dispatcher.
		if !errors.Is(err, os.ErrNotExist) {
			primaryErr := fmt.Errorf("attach XDP extension to %s slot %d: %w", ifname, position, err)
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
		if delErr := m.store.DeleteDispatcher(ctx, string(dispatcher.DispatcherTypeXDP), nsid, uint32(ifindex)); delErr != nil {
			primaryErr := fmt.Errorf("delete stale XDP dispatcher: %w", delErr)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindStoreDeleteDispatcher,
				Target: target,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		dispState, err = m.createXDPDispatcher(ctx, nsid, uint32(ifindex), netnsPath)
		if err != nil {
			primaryErr := fmt.Errorf("recreate XDP dispatcher for %s: %w", ifname, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachXDPDispatcher,
				Target: target,
				Details: outcome.DispatcherDetails{
					Interface: ifname,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		revisionDir = dispatcher.DispatcherRevisionDir(m.root.BPFFS().MountPoint(), dispatcher.DispatcherTypeXDP, nsid, uint32(ifindex), dispState.Revision)
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
		extSpec = dispatcher.XDPExtensionAttachSpec{
			DispatcherPinPath: progPinPath,
			ObjectPath:        prog.Load.ObjectPath(),
			ProgramName:       prog.Meta.Name,
			Position:          position,
			LinkPinPath:       linkPinPath,
			MapPinDir:         mapPinDir,
		}
		attachOut, err = m.kernel.AttachXDPExtension(ctx, extSpec)
		if err != nil {
			primaryErr := fmt.Errorf("attach XDP extension to %s slot %d (after recreate): %w", ifname, position, err)
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

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.XDPDetails{
			Interface:    ifname,
			Ifindex:      uint32(ifindex),
			Priority:     50, // Default priority
			Position:     int32(position),
			ProceedOn:    []int32{2}, // XDP_PASS
			Nsid:         nsid,
			DispatcherID: dispState.KernelID,
			Revision:     dispState.Revision,
		},
		*bpffs.NewLinkPath(linkPinPath),
		time.Now(),
	)

	// Construct Link with Status from AttachOutput
	link := bpfman.Link{
		Record: linkRecord,
		Status: bpfman.LinkStatus{
			Kernel:     attachOut.KernelLink,
			KernelSeen: attachOut.KernelLink != nil,
			PinPresent: attachOut.PinPath != "",
		},
	}

	// Record successful extension attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachExtension,
		Target: target,
		Details: outcome.LinkDetails{
			LinkID:       attachOut.LinkID,
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

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
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
				Target: fmt.Sprintf("%d", link.Record.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.Record.ID),
					PinPath: linkPinPath,
				},
				Error: rbErrs[0].Err.Error(),
			})
		} else {
			_ = rec.RollbackComplete(outcome.Step{
				Kind:   outcome.StepKindKernelDetachLink,
				Target: fmt.Sprintf("%d", link.Record.ID),
				Details: outcome.LinkDetails{
					LinkID:  uint32(link.Record.ID),
					PinPath: linkPinPath,
				},
			})
		}
		return fail(storeErr)
	}

	// Record successful store save
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindStoreSaveLink,
		Target: fmt.Sprintf("%d", link.Record.ID),
		Details: outcome.LinkDetails{
			LinkID:    uint32(link.Record.ID),
			ProgramID: programKernelID,
			Interface: ifname,
			PinPath:   linkPinPath,
		},
	})

	m.logger.InfoContext(ctx, "attached XDP via dispatcher",
		"link_id", link.Record.ID,
		"program_id", programKernelID,
		"interface", ifname,
		"ifindex", ifindex,
		"nsid", nsid,
		"position", position,
		"revision", dispState.Revision,
		"pin_path", linkPinPath)

	return link, nil
}

// createXDPDispatcher creates a new XDP dispatcher for the given interface.
//
// Pattern: COMPUTE -> KERNEL I/O -> COMPUTE -> EXECUTE
func (m *Manager) createXDPDispatcher(ctx context.Context, nsid uint64, ifindex uint32, netnsPath string) (dispatcher.State, error) {
	// COMPUTE: Calculate paths according to Rust bpfman convention
	revision := uint32(1)
	linkPinPath := dispatcher.DispatcherLinkPath(m.root.BPFFS().MountPoint(), dispatcher.DispatcherTypeXDP, nsid, ifindex)
	revisionDir := dispatcher.DispatcherRevisionDir(m.root.BPFFS().MountPoint(), dispatcher.DispatcherTypeXDP, nsid, ifindex, revision)
	progPinPath := dispatcher.DispatcherProgPath(revisionDir)

	m.logger.InfoContext(ctx, "creating XDP dispatcher",
		"nsid", nsid,
		"ifindex", ifindex,
		"netns", netnsPath,
		"revision", revision,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPinPath)

	// KERNEL I/O: Create dispatcher (returns IDs)
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
	result, err := m.kernel.AttachXDPDispatcher(ctx, spec)
	if err != nil {
		return dispatcher.State{}, err
	}

	// ROLLBACK: If the store write fails, undo kernel state.
	// Order: remove prog pin first, then detach the dispatcher link.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.RemovePin(ctx, progPinPath)
	})
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// COMPUTE: Build save action from kernel result
	state := computeXDPDispatcherState(dispatcher.DispatcherTypeXDP, nsid, ifindex, revision, result)
	saveAction := action.SaveDispatcher{State: state}

	// EXECUTE: Save through executor
	if err := m.executor.Execute(ctx, saveAction); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back XDP dispatcher", "ifindex", ifindex, "error", err)
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
			}
			return dispatcher.State{}, errors.Join(fmt.Errorf("save dispatcher: %w", err), errors.New("rollback failed"))
		}
		return dispatcher.State{}, fmt.Errorf("save dispatcher: %w", err)
	}

	m.logger.InfoContext(ctx, "created XDP dispatcher",
		"nsid", nsid,
		"ifindex", ifindex,
		"dispatcher_id", result.DispatcherID,
		"link_id", result.LinkID,
		"prog_pin_path", progPinPath,
		"link_pin_path", linkPinPath)

	return state, nil
}

// computeXDPDispatcherState is a pure function that builds a DispatcherState
// from kernel attach results.
func computeXDPDispatcherState(
	dispType dispatcher.DispatcherType,
	nsid uint64,
	ifindex, revision uint32,
	result *interpreter.XDPDispatcherResult,
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
