package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/netns"
	"github.com/frobware/go-bpfman/outcome"
	"github.com/frobware/go-bpfman/platform"
)

// TC proceed-on action bits (matches TC_ACT_* return codes).
const (
	tcProceedOnOK               = 1 << 0  // TC_ACT_OK
	tcProceedOnPipe             = 1 << 3  // TC_ACT_PIPE
	tcProceedOnDispatcherReturn = 1 << 30 // bpfman-specific sentinel
)

// DefaultTCProceedOn is the default bitmask for TC proceed-on actions.
var DefaultTCProceedOn = tcProceedOnOK | tcProceedOnPipe | tcProceedOnDispatcherReturn

// attachTC attaches a TC program to a network interface using the
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
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachTC(ctx context.Context, spec bpfman.TCAttachSpec) (bpfman.Link, error) {
	ifname := spec.Ifname()
	ifindex := spec.Ifindex()
	direction := spec.Direction()
	priority := spec.Priority()
	proceedOn := spec.ProceedOn()

	var dispType dispatcher.DispatcherType
	if direction == bpfman.TCDirectionIngress {
		dispType = dispatcher.DispatcherTypeTCIngress
	} else {
		dispType = dispatcher.DispatcherTypeTCEgress
	}

	return m.dispatcherAttach(ctx, dispatcherAttachParams{
		programKernelID: spec.ProgramID(),
		ifindex:         ifindex,
		ifname:          ifname,
		netnsPath:       spec.Netns(),
		target:          ifname + ":" + string(direction),
		direction:       string(direction),
		dispType:        dispType,
		dispStepKind:    outcome.StepKindAttachTCDispatcher,
		createDispatcher: func(ctx context.Context, nsid uint64, ifindex uint32, netnsPath string) (dispatcher.State, error) {
			return m.createTCDispatcher(ctx, nsid, ifindex, ifname, direction, dispType, netnsPath)
		},
		attachExtension: func(ctx context.Context, dispatcherPinPath, objectPath, programName string, position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error) {
			return m.kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
				DispatcherPinPath: dispatcherPinPath,
				ObjectPath:        objectPath,
				ProgramName:       programName,
				Position:          position,
				LinkPinPath:       linkPinPath,
				MapPinDir:         mapPinDir,
			})
		},
		buildLinkDetails: func(nsid uint64, position int, dispState dispatcher.State) bpfman.LinkDetails {
			return bpfman.TCDetails{
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
		},
	})
}

// attachTCX attaches a TCX program to a network interface using native
// kernel multi-program support. Unlike TC, TCX doesn't use dispatchers.
//
// Pin paths follow the convention:
//   - Link: /sys/fs/bpf/bpfman/tcx-{direction}/link_{nsid}_{ifindex}_{linkid}
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachTCX(ctx context.Context, spec bpfman.TCXAttachSpec) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o, func(err error) {
		m.logger.Error("outcome recorder: invariant violation", "error", err)
	})

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
	prog, err := m.getProgram(ctx, programKernelID)
	if err != nil {
		return fail(rec.FailStep(outcome.StepKindPreflight, target, err))
	}

	// Verify program type is TCX
	if prog.Load.ProgramType() != bpfman.ProgramTypeTCX {
		primaryErr := fmt.Errorf("program %d is type %s, not tcx", programKernelID, prog.Load.ProgramType())
		return fail(rec.FailStep(outcome.StepKindPreflight, target, primaryErr))
	}

	// FETCH: Get network namespace ID (from target namespace if specified)
	nsid, err := netns.GetNsid(netnsPath)
	if err != nil {
		primaryErr := fmt.Errorf("get nsid: %w", err)
		return fail(rec.FailStep(outcome.StepKindPreflight, target, primaryErr))
	}

	// COMPUTE: Calculate link pin path from conventions.
	// The path must be unique per program to support multiple TCX programs
	// on the same interface -- each needs its own pinned link to keep the
	// kernel attachment alive.
	linkPinPath := m.fsctx.BPFFS().TCXLinkPath(string(direction), nsid, uint32(ifindex), programKernelID)

	// KERNEL I/O: Remove stale pin if it exists from a previous daemon run.
	if _, statErr := os.Stat(linkPinPath); statErr == nil {
		m.logger.WarnContext(ctx, "removing stale TCX link pin", "path", linkPinPath)
		if removeErr := os.Remove(linkPinPath); removeErr != nil {
			primaryErr := fmt.Errorf("remove stale TCX link pin %s: %w", linkPinPath, removeErr)
			return fail(rec.FailStep(outcome.StepKindKernelRemovePin, linkPinPath, primaryErr))
		}
	}

	// COMPUTE: Use the stored program pin path directly
	progPinPath := prog.Handles.PinPath

	// FETCH: Get existing TCX links for this interface/direction to compute order
	existingLinks, err := m.store.ListTCXLinksByInterface(ctx, nsid, uint32(ifindex), string(direction))
	if err != nil {
		primaryErr := fmt.Errorf("list existing TCX links: %w", err)
		return fail(rec.FailStep(outcome.StepKindPreflight, target, primaryErr))
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
	attachOut, err := m.kernel.AttachTCX(ctx, ifindex, string(direction), progPinPath, linkPinPath, netnsPath, order)
	if err != nil {
		primaryErr := fmt.Errorf("attach TCX to %s %s: %w", ifname, direction, err)
		return fail(rec.FailStep(outcome.StepKindAttachTCX, target, primaryErr, outcome.LinkDetails{
			ProgramID: programKernelID,
			Interface: ifname,
			PinPath:   linkPinPath,
		}))
	}

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.TCXDetails{
			Interface: ifname,
			Ifindex:   uint32(ifindex),
			Direction: direction,
			Priority:  int32(priority),
			Nsid:      nsid,
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

	// Record successful TCX attach
	rec.CompleteStep(outcome.StepKindAttachTCX, target, outcome.LinkDetails{
		LinkID:    attachOut.LinkID,
		ProgramID: programKernelID,
		Interface: ifname,
		PinPath:   linkPinPath,
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := rec.FailStep(outcome.StepKindStoreSaveLink, fmt.Sprintf("%d", link.Record.ID),
			fmt.Errorf("save TCX link metadata: %w", err), outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: programKernelID,
				Interface: ifname,
				PinPath:   linkPinPath,
			})
		recordRollback(&rec, undo, outcome.Step{
			Kind:   outcome.StepKindKernelDetachLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:  uint32(link.Record.ID),
				PinPath: linkPinPath,
			},
		}, m.logger)
		return fail(storeErr)
	}

	// Record successful store save
	rec.CompleteStep(outcome.StepKindStoreSaveLink, fmt.Sprintf("%d", link.Record.ID), outcome.LinkDetails{
		LinkID:    uint32(link.Record.ID),
		ProgramID: programKernelID,
		Interface: ifname,
		PinPath:   linkPinPath,
	})

	m.logger.InfoContext(ctx, "attached TCX program",
		"link_id", link.Record.ID,
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
	// TC dispatchers do not use a link pin -- legacy netlink TC has no
	// BPF link to pin. The filter is identified by handle + priority.
	fs := m.fsctx.BPFFS()
	revision := uint32(1)
	progPinPath := fs.DispatcherProgPath(dispType, nsid, ifindex, revision)

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
	saveAction := SaveDispatcher{State: state}

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
