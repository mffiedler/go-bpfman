package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/action"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/interpreter"
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
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachXDP(ctx context.Context, spec bpfman.XDPAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	ifname := spec.Ifname()
	ifindex := spec.Ifindex()
	return m.dispatcherAttach(ctx, dispatcherAttachParams{
		programKernelID: spec.ProgramID(),
		ifindex:         ifindex,
		ifname:          ifname,
		netnsPath:       spec.Netns(),
		target:          ifname + ":xdp",
		dispType:        dispatcher.DispatcherTypeXDP,
		dispStepKind:    outcome.StepKindAttachXDPDispatcher,
		createDispatcher: func(ctx context.Context, nsid uint64, ifindex uint32, netnsPath string) (dispatcher.State, error) {
			return m.createXDPDispatcher(ctx, nsid, ifindex, netnsPath)
		},
		attachExtension: func(ctx context.Context, dispatcherPinPath, objectPath, programName string, position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error) {
			return m.kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
				DispatcherPinPath: dispatcherPinPath,
				ObjectPath:        objectPath,
				ProgramName:       programName,
				Position:          position,
				LinkPinPath:       linkPinPath,
				MapPinDir:         mapPinDir,
			})
		},
		buildLinkDetails: func(nsid uint64, position int, dispState dispatcher.State) bpfman.LinkDetails {
			return bpfman.XDPDetails{
				Interface:    ifname,
				Ifindex:      uint32(ifindex),
				Priority:     50, // Default priority
				Position:     int32(position),
				ProceedOn:    []int32{2}, // XDP_PASS
				Nsid:         nsid,
				DispatcherID: dispState.KernelID,
				Revision:     dispState.Revision,
			}
		},
	})
}

// createXDPDispatcher creates a new XDP dispatcher for the given interface.
//
// Pattern: COMPUTE -> KERNEL I/O -> COMPUTE -> EXECUTE
func (m *Manager) createXDPDispatcher(ctx context.Context, nsid uint64, ifindex uint32, netnsPath string) (dispatcher.State, error) {
	// COMPUTE: Calculate paths according to Rust bpfman convention
	fs := m.fsctx.BPFFS()
	revision := uint32(1)
	linkPinPath := fs.DispatcherLinkPath(dispatcher.DispatcherTypeXDP, nsid, ifindex)
	progPinPath := fs.DispatcherProgPath(dispatcher.DispatcherTypeXDP, nsid, ifindex, revision)

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
