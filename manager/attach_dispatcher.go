package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
)

// dispatcherAttachParams describes a dispatcher-based attach operation
// (XDP or TC). The closures construct the type-specific deep actions
// while the shared skeleton handles the plan structure and result
// extraction.
type dispatcherAttachParams struct {
	programKernelID kernel.ProgramID
	ifindex         int
	ifname          string
	netnsPath       string
	target          string // target (e.g., "eth0:xdp", "eth0:ingress")
	dispType        dispatcher.DispatcherType

	// ensureAction constructs the Ensure action for this
	// dispatcher type.
	ensureAction func() action.Action

	// extensionAction constructs the Attach action given the
	// dispatcher state and program record.
	extensionAction func(ds dispatcher.State, prog bpfman.ProgramRecord) action.Action

	// buildLinkDetails constructs the sealed LinkDetails value for
	// the link record.
	buildLinkDetails func(nsid uint64, position int, dispState dispatcher.State) bpfman.LinkDetails
}

// extensionResult bundles the kernel attach output with the
// dispatcher state and position that were current at attach time.
// The dispatcher state may differ from the originally looked-up
// state if stale-dispatcher recovery ran.
type extensionResult struct {
	out      bpfman.AttachOutput
	disp     dispatcher.State
	position int
	pinPath  string
}

// Binding keys for dispatcherAttach plan nodes.
var (
	dispPreparedKey = operation.NewKey[dispPrepared]("disp-prepared")
	dispStateKey    = operation.NewKey[dispatcher.State]("dispatcher-state")
	extResultKey    = operation.NewKey[extensionResult]("extension-result")
)

// dispPrepared bundles the program record fetched in node 1 for use
// by subsequent nodes.
type dispPrepared struct {
	prog bpfman.ProgramRecord
}

// dispatcherAttach implements the common skeleton for dispatcher-based
// attach types (XDP, TC).
//
// The operation creates a dispatcher if none exists, attaches the user
// program as an extension to a dispatcher slot, and persists the link
// metadata. All cross-subsystem complexity (stale dispatcher recovery,
// kernel+store transactions) lives behind the deep executor actions.
func (m *Manager) dispatcherAttach(ctx context.Context, p dispatcherAttachParams) (bpfman.Link, error) {
	plan := m.dispatcherAttachPlan(p)
	b, err := operation.Run(ctx, m.logger, m.executor, plan)
	if err != nil {
		return bpfman.Link{}, err
	}

	link := operation.Get(b, linkKey)
	r := operation.Get(b, extResultKey)
	m.logger.InfoContext(ctx, "attached via dispatcher",
		"type", p.dispType,
		"link_id", link.Record.ID,
		"program_id", p.programKernelID,
		"interface", p.ifname,
		"ifindex", p.ifindex,
		"nsid", r.disp.Nsid,
		"position", r.position,
		"revision", r.disp.Revision,
		"pin_path", r.pinPath)

	return link, nil
}

// dispatcherAttachPlan builds the operation plan for a
// dispatcher-based attach.
//
// Nodes:
//  1. Produce dispPreparedKey -- fetch program record via executor.
//  2. Produce dispStateKey -- EnsureXDPDispatcher or
//     EnsureTCDispatcher via executor.
//  3. Produce extResultKey -- AttachXDPExtension or
//     AttachTCExtension via executor, with undo that detaches the
//     link on failure.
//  4. Produce linkKey -- construct link record, save to store.
func (m *Manager) dispatcherAttachPlan(p dispatcherAttachParams) operation.Plan {
	return operation.Build(
		// Node 1: Fetch program record.
		operation.Produce(dispPreparedKey, p.target,
			func(ctx context.Context, _ *operation.Bindings) (dispPrepared, error) {
				prog, err := action.Produce[bpfman.ProgramRecord](ctx, m.executor, action.GetProgramFromStore{KernelID: p.programKernelID})
				if err != nil {
					return dispPrepared{}, err
				}
				return dispPrepared{prog: prog}, nil
			},
		),

		// Node 2: Ensure dispatcher exists.
		operation.Produce(dispStateKey, p.target,
			func(ctx context.Context, _ *operation.Bindings) (dispatcher.State, error) {
				return action.Produce[dispatcher.State](ctx, m.executor, p.ensureAction())
			},
		),

		// Node 3: Attach extension (with stale-dispatcher retry inside the executor).
		operation.Produce(extResultKey, p.target,
			func(ctx context.Context, b *operation.Bindings) (extensionResult, error) {
				dp := operation.Get(b, dispPreparedKey)
				ds := operation.Get(b, dispStateKey)
				return action.Produce[extensionResult](ctx, m.executor, p.extensionAction(ds, dp.prog))
			},
			operation.UndoFrom(func(b *operation.Bindings) []action.Action {
				r := operation.Get(b, extResultKey)
				return []action.Action{
					action.DetachLink{PinPath: r.pinPath},
				}
			}),
		),

		// Node 4: Construct link record + save to store.
		operation.Produce(linkKey, p.target,
			func(ctx context.Context, b *operation.Bindings) (bpfman.Link, error) {
				r := operation.Get(b, extResultKey)
				record := bpfman.NewPinnedLinkRecord(
					bpfman.LinkID(r.out.LinkID),
					p.programKernelID,
					p.buildLinkDetails(r.disp.Nsid, r.position, r.disp),
					*bpffs.NewLinkPath(r.pinPath),
					time.Now(),
				)
				link := bpfman.Link{
					Record: record,
					Status: bpfman.LinkStatus{
						Kernel:     r.out.KernelLink,
						KernelSeen: r.out.KernelLink != nil,
						PinPresent: r.out.PinPath != "",
					},
				}
				if err := m.executor.Execute(ctx, action.SaveLink{Record: record}); err != nil {
					return bpfman.Link{}, fmt.Errorf("save link metadata: %w", err)
				}
				return link, nil
			},
		),
	)
}
