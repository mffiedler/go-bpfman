package manager

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
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
// Preflight failures (getProgram, type check, GetNsid, stale pin
// removal, link listing) return plain errors. Execution failures
// return *ManagerError with the full operation outcome.
func (m *Manager) attachTCX(ctx context.Context, spec bpfman.TCXAttachSpec) (bpfman.Link, error) {
	// --- Preflight (outside plan, plain errors) ---
	programKernelID := spec.ProgramID()
	ifindex := spec.Ifindex()
	ifname := spec.Ifname()
	direction := spec.Direction()
	priority := spec.Priority()
	netnsPath := spec.Netns()
	target := ifname + ":" + string(direction)

	prog, err := m.getProgram(ctx, programKernelID)
	if err != nil {
		return bpfman.Link{}, err
	}
	if prog.Load.ProgramType() != bpfman.ProgramTypeTCX {
		return bpfman.Link{}, fmt.Errorf("program %d is type %s, not tcx", programKernelID, prog.Load.ProgramType())
	}
	nsid, err := netns.GetNsid(netnsPath)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("get nsid: %w", err)
	}

	linkPinPath := m.fsctx.BPFFS().TCXLinkPath(string(direction), nsid, uint32(ifindex), programKernelID)

	// Stale pin removal (preflight I/O).
	if _, statErr := os.Stat(linkPinPath); statErr == nil {
		m.logger.WarnContext(ctx, "removing stale TCX link pin", "path", linkPinPath)
		if removeErr := os.Remove(linkPinPath); removeErr != nil {
			return bpfman.Link{}, fmt.Errorf("remove stale TCX link pin %s: %w", linkPinPath, removeErr)
		}
	}

	progPinPath := prog.Handles.PinPath
	existingLinks, err := m.store.ListTCXLinksByInterface(ctx, nsid, uint32(ifindex), string(direction))
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("list existing TCX links: %w", err)
	}
	order := computeTCXAttachOrder(existingLinks, int32(priority))

	m.logger.DebugContext(ctx, "computed TCX attach order",
		"program_id", programKernelID,
		"priority", priority,
		"existing_links", len(existingLinks),
		"order", order)

	// --- Build and execute plan ---
	plan := m.attachTCXPlan(programKernelID, ifindex, ifname, direction, priority, nsid, netnsPath, linkPinPath, progPinPath, target, order)
	begin := func(_ context.Context) *operation.RunState {
		return m.beginOp(ctx)
	}
	b, err := operation.Run(ctx, begin, m.executor, plan)
	if err != nil {
		return bpfman.Link{}, wrapOpErr(err)
	}

	link := operation.Get(b, linkKey)
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

// attachTCXPlan builds the operation plan for a TCX attach.
//
// Nodes:
//  1. Produce attachOutKey -- kernel attach via AttachTCX, with undo
//     that detaches the link on failure.
//  2. Produce linkKey -- construct link record, save to store.
func (m *Manager) attachTCXPlan(
	programKernelID uint32, ifindex int, ifname string,
	direction bpfman.TCDirection, priority int, nsid uint64,
	netnsPath, linkPinPath, progPinPath, target string,
	order bpfman.TCXAttachOrder,
) operation.Plan {
	return operation.Build(
		operation.Produce(attachOutKey, outcome.StepKindAttachTCX, target,
			func(ctx context.Context, _ *operation.Bindings) (bpfman.AttachOutput, error) {
				return m.kernel.AttachTCX(ctx, ifindex, string(direction), progPinPath, linkPinPath, netnsPath, order)
			},
			operation.DetailsFn(func(b *operation.Bindings) any {
				out := operation.Get(b, attachOutKey)
				return outcome.LinkDetails{
					LinkID:    out.LinkID,
					ProgramID: programKernelID,
					Interface: ifname,
					PinPath:   linkPinPath,
				}
			}),
			operation.UndoFrom(func(_ *operation.Bindings) []operation.UndoEntry {
				return []operation.UndoEntry{{
					Action: action.DetachLink{PinPath: linkPinPath},
					Step: outcome.Step{
						Kind:   outcome.StepKindKernelDetachLink,
						Target: target,
						Details: outcome.LinkDetails{
							PinPath: linkPinPath,
						},
					},
				}}
			}),
		),

		operation.Produce(linkKey, outcome.StepKindStoreSaveLink, target,
			func(ctx context.Context, b *operation.Bindings) (bpfman.Link, error) {
				out := operation.Get(b, attachOutKey)
				record := bpfman.NewPinnedLinkRecord(
					bpfman.LinkID(out.LinkID),
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
				link := bpfman.Link{
					Record: record,
					Status: bpfman.LinkStatus{
						Kernel:     out.KernelLink,
						KernelSeen: out.KernelLink != nil,
						PinPresent: out.PinPath != "",
					},
				}
				if err := m.executor.Execute(ctx, action.SaveLink{Record: record}); err != nil {
					return bpfman.Link{}, fmt.Errorf("save TCX link metadata: %w", err)
				}
				return link, nil
			},
			operation.DetailsFn(func(b *operation.Bindings) any {
				link := operation.Get(b, linkKey)
				return outcome.LinkDetails{
					LinkID:    uint32(link.Record.ID),
					ProgramID: programKernelID,
					Interface: ifname,
					PinPath:   linkPinPath,
				}
			}),
		),
	)
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

	// COMPUTE: Build save action from kernel result
	state := computeTCDispatcherState(dispType, nsid, ifindex, revision, result)
	saveAction := action.SaveDispatcher{State: state}

	// EXECUTE: Save through executor
	if err := m.executor.Execute(ctx, saveAction); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back TC dispatcher",
			"ifname", ifname, "error", err)
		// Rollback: detach TC filter first, then remove prog pin.
		if rbErr := m.executor.Execute(ctx, action.DetachTCFilter{
			Ifindex:  int(ifindex),
			Ifname:   ifname,
			Parent:   tcParentHandle(dispType),
			Priority: result.Priority,
			Handle:   result.Handle,
		}); rbErr != nil {
			m.logger.ErrorContext(ctx, "rollback: detach TC filter failed",
				"ifname", ifname, "error", rbErr)
		}
		if rbErr := m.executor.Execute(ctx, action.RemovePin{Path: progPinPath}); rbErr != nil {
			m.logger.ErrorContext(ctx, "rollback: remove prog pin failed",
				"path", progPinPath, "error", rbErr)
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
