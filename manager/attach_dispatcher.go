package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
	"github.com/frobware/go-bpfman/netns"
	"github.com/frobware/go-bpfman/outcome"
	"github.com/frobware/go-bpfman/platform/store"
)

// dispatcherAttachParams describes a dispatcher-based attach operation
// (XDP or TC). The closures capture the type-specific kernel calls and
// link construction while the shared skeleton handles outcome recording,
// stale dispatcher recovery, persistence, and rollback.
type dispatcherAttachParams struct {
	programKernelID uint32
	ifindex         int
	ifname          string
	netnsPath       string
	target          string // outcome target (e.g., "eth0:xdp", "eth0:ingress")
	direction       string // empty for XDP, "ingress"/"egress" for TC
	dispType        dispatcher.DispatcherType
	dispStepKind    outcome.StepKind

	// createDispatcher creates the kernel dispatcher and persists it.
	createDispatcher func(ctx context.Context, nsid uint64, ifindex uint32, netnsPath string) (dispatcher.State, error)

	// attachExtension attaches the user program as extension to a
	// dispatcher slot. The caller constructs the type-specific
	// extension spec internally.
	attachExtension func(ctx context.Context, dispatcherPinPath, objectPath, programName string, position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error)

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
	dispStateKey = operation.NewKey[dispatcher.State]("dispatcher-state")
	extResultKey = operation.NewKey[extensionResult]("extension-result")
)

// dispatcherAttach implements the common skeleton for dispatcher-based
// attach types (XDP, TC).
//
// The operation creates a dispatcher if none exists, attaches the user
// program as an extension to a dispatcher slot, and persists the link
// metadata. On extension attach failure due to a missing dispatcher
// pin (stale record after a fresh bpffs mount), the dispatcher is
// recreated and the attach retried once.
//
// Preflight failures (getProgram, GetNsid) return plain errors.
// Execution failures return *ManagerError with the full operation
// outcome.
func (m *Manager) dispatcherAttach(ctx context.Context, p dispatcherAttachParams) (bpfman.Link, error) {
	// --- Preflight (outside plan, plain errors) ---
	prog, err := m.getProgram(ctx, p.programKernelID)
	if err != nil {
		return bpfman.Link{}, err
	}
	nsid, err := netns.GetNsid(p.netnsPath)
	if err != nil {
		return bpfman.Link{}, fmt.Errorf("get nsid: %w", err)
	}

	// --- Build and execute plan ---
	plan := m.dispatcherAttachPlan(p, prog, nsid)
	begin := func(_ context.Context) *operation.RunState {
		return m.beginOp(ctx)
	}
	b, err := operation.Run(ctx, begin, m.executor, plan)
	if err != nil {
		return bpfman.Link{}, wrapOpErr(err)
	}

	link := operation.Get(b, linkKey)
	r := operation.Get(b, extResultKey)
	m.logger.InfoContext(ctx, "attached via dispatcher",
		"type", p.dispType,
		"link_id", link.Record.ID,
		"program_id", p.programKernelID,
		"interface", p.ifname,
		"ifindex", p.ifindex,
		"nsid", nsid,
		"position", r.position,
		"revision", r.disp.Revision,
		"pin_path", r.pinPath)

	return link, nil
}

// dispatcherAttachPlan builds the operation plan for a
// dispatcher-based attach.
//
// Nodes:
//  1. Produce dispStateKey -- dispatcher lookup or creation.
//  2. Produce extResultKey -- attach extension with internal retry on
//     stale dispatcher, with undo that detaches the link on failure.
//  3. Produce linkKey -- construct link record, save to store.
func (m *Manager) dispatcherAttachPlan(
	p dispatcherAttachParams,
	prog bpfman.ProgramRecord,
	nsid uint64,
) operation.Plan {
	ifindex := uint32(p.ifindex)
	fs := m.fsctx.BPFFS()
	mapPinDir := prog.Handles.MapPinPath

	return operation.Build(
		// Node 1: Dispatcher lookup or creation.
		operation.Produce(dispStateKey, p.dispStepKind, p.target,
			func(ctx context.Context, _ *operation.Bindings) (dispatcher.State, error) {
				state, err := m.store.GetDispatcher(ctx, string(p.dispType), nsid, ifindex)
				if errors.Is(err, store.ErrNotFound) {
					return p.createDispatcher(ctx, nsid, ifindex, p.netnsPath)
				}
				if err != nil {
					return dispatcher.State{}, fmt.Errorf("get dispatcher: %w", err)
				}
				return state, nil
			},
			operation.DetailsFn(func(b *operation.Bindings) any {
				ds := operation.Get(b, dispStateKey)
				return outcome.DispatcherDetails{
					DispatcherID: ds.KernelID,
					Interface:    p.ifname,
					Direction:    p.direction,
				}
			}),
		),

		// Node 2: Attach extension (with stale-dispatcher retry).
		operation.Produce(extResultKey, outcome.StepKindAttachExtension, p.target,
			func(ctx context.Context, b *operation.Bindings) (extensionResult, error) {
				ds := operation.Get(b, dispStateKey)
				return m.attachExtensionWithRetry(ctx, p, ds, nsid, ifindex, fs, mapPinDir, prog)
			},
			operation.DetailsFn(func(b *operation.Bindings) any {
				r := operation.Get(b, extResultKey)
				return outcome.LinkDetails{
					LinkID:       r.out.LinkID,
					ProgramID:    p.programKernelID,
					Interface:    p.ifname,
					PinPath:      r.pinPath,
					DispatcherID: r.disp.KernelID,
				}
			}),
			operation.UndoFrom(func(b *operation.Bindings) []operation.UndoEntry {
				r := operation.Get(b, extResultKey)
				return []operation.UndoEntry{{
					Action: action.DetachLink{PinPath: r.pinPath},
					Step: outcome.Step{
						Kind:   outcome.StepKindKernelDetachLink,
						Target: p.target,
						Details: outcome.LinkDetails{
							PinPath: r.pinPath,
						},
					},
				}}
			}),
		),

		// Node 3: Construct link record + save to store.
		operation.Produce(linkKey, outcome.StepKindStoreSaveLink, p.target,
			func(ctx context.Context, b *operation.Bindings) (bpfman.Link, error) {
				r := operation.Get(b, extResultKey)
				record := bpfman.NewPinnedLinkRecord(
					bpfman.LinkID(r.out.LinkID),
					p.programKernelID,
					p.buildLinkDetails(nsid, r.position, r.disp),
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
			operation.DetailsFn(func(b *operation.Bindings) any {
				link := operation.Get(b, linkKey)
				r := operation.Get(b, extResultKey)
				return outcome.LinkDetails{
					LinkID:       uint32(link.Record.ID),
					ProgramID:    p.programKernelID,
					Interface:    p.ifname,
					PinPath:      r.pinPath,
					DispatcherID: r.disp.KernelID,
				}
			}),
		),
	)
}

// attachExtensionWithRetry attaches the user program as an extension
// to a dispatcher slot. If the first attempt fails with
// os.ErrNotExist (stale dispatcher pin after bpffs remount), it
// deletes the stale dispatcher record, recreates the dispatcher, and
// retries once.
func (m *Manager) attachExtensionWithRetry(
	ctx context.Context,
	p dispatcherAttachParams,
	ds dispatcher.State,
	nsid uint64, ifindex uint32,
	fs bpfmanfs.BPFFS,
	mapPinDir string,
	prog bpfman.ProgramRecord,
) (extensionResult, error) {
	position, err := m.store.CountDispatcherLinks(ctx, ds.KernelID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links: %w", err)
	}
	linkPinPath := fs.ExtensionLinkPath(p.dispType, nsid, ifindex, ds.Revision, position)
	progPinPath := fs.DispatcherProgPath(p.dispType, nsid, ifindex, ds.Revision)

	out, err := p.attachExtension(ctx, progPinPath, prog.Load.ObjectPath(), prog.Meta.Name, position, linkPinPath, mapPinDir)
	if err == nil {
		return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
	}

	// Stale dispatcher recovery: pin missing after bpffs remount.
	if !errors.Is(err, os.ErrNotExist) {
		return extensionResult{}, fmt.Errorf("attach extension to %s slot %d: %w", p.target, position, err)
	}

	m.logger.WarnContext(ctx, "dispatcher pin missing, recreating",
		"prog_pin_path", progPinPath,
		"dispatcher_id", ds.KernelID,
		"error", err)

	if delErr := m.store.DeleteDispatcher(ctx, string(p.dispType), nsid, ifindex); delErr != nil {
		return extensionResult{}, fmt.Errorf("delete stale %s dispatcher: %w", p.dispType, delErr)
	}
	ds, err = p.createDispatcher(ctx, nsid, ifindex, p.netnsPath)
	if err != nil {
		return extensionResult{}, fmt.Errorf("recreate %s dispatcher for %s: %w", p.dispType, p.target, err)
	}
	position, err = m.store.CountDispatcherLinks(ctx, ds.KernelID)
	if err != nil {
		return extensionResult{}, fmt.Errorf("count dispatcher links after recreate: %w", err)
	}
	linkPinPath = fs.ExtensionLinkPath(p.dispType, nsid, ifindex, ds.Revision, position)
	progPinPath = fs.DispatcherProgPath(p.dispType, nsid, ifindex, ds.Revision)

	out, err = p.attachExtension(ctx, progPinPath, prog.Load.ObjectPath(), prog.Meta.Name, position, linkPinPath, mapPinDir)
	if err != nil {
		return extensionResult{}, fmt.Errorf("attach extension to %s slot %d (after recreate): %w", p.target, position, err)
	}
	return extensionResult{out: out, disp: ds, position: position, pinPath: linkPinPath}, nil
}
