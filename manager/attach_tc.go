package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
	"github.com/frobware/go-bpfman/ns/netns"
)

// TC proceed-on action bits (matches TC_ACT_* return codes).
const (
	tcProceedOnOK               = 1 << 0  // TC_ACT_OK
	tcProceedOnPipe             = 1 << 3  // TC_ACT_PIPE
	tcProceedOnDispatcherReturn = 1 << 30 // bpfman-specific sentinel
)

// DefaultTCProceedOn is the default bitmask for TC proceed-on actions.
var DefaultTCProceedOn = tcProceedOnOK | tcProceedOnPipe | tcProceedOnDispatcherReturn

// tcProceedOnBitmask converts a list of TC action codes to a bitmask.
// Each action code a produces bit (1 << a). If the list is empty, the
// default bitmask (OK|Pipe|DispatcherReturn) is returned.
//
// This must match the reconstruction in ListDispatcherSlots so that
// the BPF map written at initial attach is identical to the one
// produced when recomputing after detach.
func tcProceedOnBitmask(actions []int32) uint32 {
	if len(actions) == 0 {
		return uint32(DefaultTCProceedOn)
	}
	var mask uint32
	for _, a := range actions {
		if a >= 0 && a < 32 {
			mask |= 1 << uint(a)
		}
	}
	return mask
}

// attachTC attaches a TC program to a network interface using the
// dispatcher model for multi-program chaining.
//
// Every attach triggers a full dispatcher rebuild: a new dispatcher
// is loaded with updated .rodata config, all extensions are re-attached,
// and the TC filter is atomically swapped (or created for first attach).
//
// Pin paths follow the Rust bpfman convention:
//   - Dispatcher prog: /sys/fs/bpf/bpfman/tc-{direction}/dispatcher_{nsid}_{ifindex}_{revision}/dispatcher
//   - Extension links: /sys/fs/bpf/bpfman/tc-{direction}/dispatcher_{nsid}_{ifindex}_{revision}/link_{position}
func (m *Manager) attachTC(ctx context.Context, spec bpfman.TCAttachSpec) (bpfman.Link, error) {
	ifname := spec.Ifname()
	ifindex := spec.Ifindex()
	direction := spec.Direction()
	priority := spec.Priority()
	proceedOn := spec.ProceedOn()
	netnsPath := spec.Netns()

	var dispType dispatcher.DispatcherType
	if direction == bpfman.TCDirectionIngress {
		dispType = dispatcher.DispatcherTypeTCIngress
	} else {
		dispType = dispatcher.DispatcherTypeTCEgress
	}

	return m.dispatcherAttach(ctx, dispatcherAttachParams{
		programID: spec.ProgramID(),
		ifindex:   ifindex,
		ifname:    ifname,
		netnsPath: netnsPath,
		target:    ifname + ":" + direction.String(),
		dispType:  dispType,
		rebuildAction: func(prog bpfman.ProgramRecord) action.Action {
			return action.RebuildTCDispatcher{
				ProgramID:   spec.ProgramID(),
				Ifindex:     uint32(ifindex),
				Ifname:      ifname,
				Direction:   direction,
				DispType:    dispType,
				NetnsPath:   netnsPath,
				ProgPinPath: prog.Handles.PinPath,
				ProgramName: prog.Meta.Name,
				Priority:    priority,
				ProceedOn:   tcProceedOnBitmask(proceedOn),
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
// removal, link listing) return plain errors.
func (m *Manager) attachTCX(ctx context.Context, spec bpfman.TCXAttachSpec) (bpfman.Link, error) {
	// --- Preflight (outside plan, plain errors) ---
	programID := spec.ProgramID()
	ifindex := spec.Ifindex()
	ifname := spec.Ifname()
	direction := spec.Direction()
	priority := spec.Priority()
	netnsPath := spec.Netns()
	target := ifname + ":" + direction.String()

	prog, err := m.getProgram(ctx, programID)
	if err != nil {
		return bpfman.Link{}, err
	}
	if prog.Load.ProgramType() != bpfman.ProgramTypeTCX {
		return bpfman.Link{}, fmt.Errorf("program %d is type %s, not tcx", programID, prog.Load.ProgramType())
	}
	nsid, err := netns.GetNsid(netnsPath)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("get nsid: %w", err)
	}

	linkPinPath := m.rt.BPFFS().TCXLinkPath(direction.String(), nsid, uint32(ifindex), programID)

	// Stale pin removal (preflight I/O).
	if err := m.executor.Execute(ctx, action.RemovePin{Path: linkPinPath}); err != nil {
		return bpfman.Link{}, fmt.Errorf("remove stale TCX link pin %s: %w", linkPinPath, err)
	}

	progPinPath := prog.Handles.PinPath
	existingLinks, err := m.store.ListTCXLinksByInterface(ctx, nsid, uint32(ifindex), direction.String())
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("list existing TCX links: %w", err)
	}
	order := computeTCXAttachOrder(existingLinks, int32(priority))

	m.logger.DebugContext(ctx, "computed TCX attach order",
		"program_id", programID,
		"priority", priority,
		"existing_links", len(existingLinks),
		"order", order)

	// --- Build and execute plan ---
	plan := m.attachTCXPlan(programID, ifindex, ifname, direction, priority, nsid, netnsPath, linkPinPath, progPinPath, target, order)
	b, err := operation.Run(ctx, m.logger, m.executor, plan)
	if err != nil {
		return bpfman.Link{}, err
	}

	link := operation.Get(b, linkKey)
	m.logger.InfoContext(ctx, "attached TCX program",
		"link_id", link.Record.ID,
		"program_id", programID,
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
	programID kernel.ProgramID, ifindex int, ifname string,
	direction bpfman.TCDirection, priority int, nsid uint64,
	netnsPath, linkPinPath, progPinPath, target string,
	order bpfman.TCXAttachOrder,
) operation.Plan {
	return operation.Build(
		operation.Produce(attachOutKey, target,
			func(ctx context.Context, exec action.ExecutorWithResult, _ *operation.Bindings) (bpfman.AttachOutput, error) {
				return action.Produce[bpfman.AttachOutput](ctx, exec, action.AttachTCX{
					Ifindex:     ifindex,
					Direction:   direction.String(),
					ProgPinPath: progPinPath,
					LinkPinPath: linkPinPath,
					NetnsPath:   netnsPath,
					Order:       order,
				})
			},
			operation.UndoFrom(func(_ *operation.Bindings) []action.Action {
				return []action.Action{
					action.DetachLink{PinPath: linkPinPath},
				}
			}),
		),

		saveLinkNode(programID, target, func(b *operation.Bindings) (kernel.LinkID, bpfman.LinkDetails, string, bpfman.AttachOutput) {
			out := operation.Get(b, attachOutKey)
			return out.LinkID, bpfman.TCXDetails{
				Interface: ifname,
				Ifindex:   uint32(ifindex),
				Direction: direction,
				Priority:  int32(priority),
				Nsid:      nsid,
			}, linkPinPath, out
		}),
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
