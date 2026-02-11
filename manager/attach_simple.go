package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
)

// Binding keys for simpleAttach plan nodes.
var (
	preparedKey  = operation.NewKey[preparedAttach]("prepared")
	attachOutKey = operation.NewKey[bpfman.AttachOutput]("kernel-attach")
	linkKey      = operation.NewKey[bpfman.Link]("link")
)

// preparedAttach bundles the output of getProgram + prepare for use
// by subsequent plan nodes via bindings.
type preparedAttach struct {
	plan        attachPlan
	linkPinPath string
}

// attachPlan captures the variable parts of a simple attach operation.
// Returned by the prepare closure after inspecting the program record.
type attachPlan struct {
	// target is the recording target (e.g., "sched/sched_switch").
	target string
	// linkName determines the bpffs pin path for the link.
	linkName string
	// details is the sealed LinkDetails value for the link record.
	details bpfman.LinkDetails
	// attachAction constructs the kernel attach action for the given link pin path.
	attachAction func(linkPinPath string) action.Action
}

// attachParams describes a non-dispatcher attach operation.
type attachParams struct {
	// programID is the kernel ID of the program to attach.
	programID kernel.ProgramID
	// defaultTarget is used for plan node labels. The actual target
	// may differ once the program record is fetched (e.g., fentry
	// resolves the function name from the record).
	defaultTarget string
	// prepare inspects the program record and returns the plan.
	// progPinPath is the program's bpffs pin path, precomputed
	// from the kernel ID.
	prepare func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error)
}

// simpleAttach implements the common skeleton for non-dispatcher attach
// types (tracepoint, kprobe, uprobe, fentry, fexit).
//
// It builds a plan via simpleAttachPlan and delegates to
// operation.Run. The plan interpreter walks each node in sequence,
// executing I/O through the executor as reified actions and
// accumulating undo actions for automatic rollback on failure. Node
// closures never call store or kernel methods directly; they
// construct action values and hand them to the executor.
func (m *Manager) simpleAttach(ctx context.Context, p attachParams) (bpfman.Link, error) {
	plan := m.simpleAttachPlan(p)
	b, err := operation.Run(ctx, m.logger, m.executor, plan)
	if err != nil {
		return bpfman.Link{}, err
	}

	pa := operation.Get(b, preparedKey)
	link := operation.Get(b, linkKey)
	m.logger.InfoContext(ctx, "attached",
		"link_id", link.Record.ID,
		"program_id", p.programID,
		"target", pa.plan.target,
		"pin_path", pa.linkPinPath)

	return link, nil
}

// simpleAttachPlan builds the operation plan for a simple attach.
//
// Nodes:
//  1. Produce preparedKey -- fetch program record, run prepare,
//     compute link pin path.
//  2. Produce attachOutKey -- kernel attach via action, with undo
//     that detaches.
//  3. Produce linkKey -- construct link record, save to store.
func (m *Manager) simpleAttachPlan(p attachParams) operation.Plan {
	return operation.Build(
		operation.Produce(preparedKey, p.defaultTarget,
			func(ctx context.Context, exec action.ExecutorWithResult, _ *operation.Bindings) (preparedAttach, error) {
				prog, err := action.Produce[bpfman.ProgramRecord](ctx, exec, action.GetProgramFromStore{ProgramID: p.programID})
				if err != nil {
					return preparedAttach{}, err
				}
				progPinPath := m.rt.BPFFS().ProgPinPath(p.programID)
				ap, err := p.prepare(prog, progPinPath)
				if err != nil {
					return preparedAttach{}, err
				}
				linkPinPath := m.rt.BPFFS().LinkPinPath(p.programID, ap.linkName)
				return preparedAttach{plan: ap, linkPinPath: linkPinPath}, nil
			},
		),

		operation.Produce(attachOutKey, p.defaultTarget,
			func(ctx context.Context, exec action.ExecutorWithResult, b *operation.Bindings) (bpfman.AttachOutput, error) {
				pa := operation.Get(b, preparedKey)
				return action.Produce[bpfman.AttachOutput](ctx, exec, pa.plan.attachAction(pa.linkPinPath))
			},
			operation.UndoFrom(func(b *operation.Bindings) []action.Action {
				pa := operation.Get(b, preparedKey)
				return []action.Action{
					action.DetachLink{PinPath: pa.linkPinPath},
				}
			}),
		),

		operation.Produce(linkKey, p.defaultTarget,
			func(ctx context.Context, exec action.ExecutorWithResult, b *operation.Bindings) (bpfman.Link, error) {
				pa := operation.Get(b, preparedKey)
				out := operation.Get(b, attachOutKey)
				record := bpfman.NewPinnedLinkRecord(
					out.LinkID,
					p.programID,
					pa.plan.details,
					*bpfman.NewLinkPath(pa.linkPinPath),
					time.Now(),
				)
				link := bpfman.Link{
					Record: record,
					Status: bpfman.LinkStatus{
						Kernel:     out.KernelLink,
						KernelSeen: out.KernelLink != nil,
						PinPresent: out.PinPath != "" && !out.Synthetic,
					},
				}
				if err := exec.Execute(ctx, action.SaveLink{Record: record}); err != nil {
					return bpfman.Link{}, fmt.Errorf("save link metadata: %w", err)
				}
				return link, nil
			},
		),
	)
}
