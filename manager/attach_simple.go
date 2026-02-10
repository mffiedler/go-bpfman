package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/operation"
)

// Binding keys for simpleAttach plan nodes.
var (
	attachOutKey = operation.NewKey[bpfman.AttachOutput]("kernel-attach")
	linkKey      = operation.NewKey[bpfman.Link]("link")
)

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
	// programKernelID is the kernel ID of the program to attach.
	programKernelID uint32
	// defaultTarget is used for log messages that occur before
	// prepare runs. Falls back to "program_<id>" if empty.
	defaultTarget string
	// prepare inspects the program record and returns the plan.
	// progPinPath is the program's bpffs pin path, precomputed
	// from the kernel ID.
	prepare func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error)
}

// simpleAttach implements the common skeleton for non-dispatcher attach
// types (tracepoint, kprobe, uprobe, fentry, fexit).
//
// Preflight (getProgram + prepare) stays outside the plan and returns
// plain errors. The plan handles kernel I/O, link construction, and
// store persistence. On plan failure, the interpreter rolls back
// automatically.
func (m *Manager) simpleAttach(ctx context.Context, p attachParams) (bpfman.Link, error) {
	// Preflight: verify program exists in store.
	prog, err := m.getProgram(ctx, p.programKernelID)
	if err != nil {
		return bpfman.Link{}, err
	}

	// Prepare: caller-specific derivation.
	progPinPath := m.fsctx.BPFFS().ProgPinPath(p.programKernelID)
	ap, err := p.prepare(prog, progPinPath)
	if err != nil {
		return bpfman.Link{}, err
	}

	// Build and execute plan.
	plan := m.simpleAttachPlan(p, ap)
	b, err := operation.Run(ctx, m.logger, m.executor, plan)
	if err != nil {
		return bpfman.Link{}, err
	}

	link := operation.Get(b, linkKey)
	m.logger.InfoContext(ctx, "attached",
		"link_id", link.Record.ID,
		"program_id", p.programKernelID,
		"target", ap.target,
		"pin_path", m.fsctx.BPFFS().LinkPinPath(
			p.programKernelID, ap.linkName))

	return link, nil
}

// simpleAttachPlan builds the operation plan for a simple attach.
//
// Nodes:
//  1. Produce attachOutKey -- kernel attach, with undo that detaches.
//  2. Produce linkKey -- construct link record, save to store.
func (m *Manager) simpleAttachPlan(
	p attachParams, ap attachPlan,
) operation.Plan {
	linkPinPath := m.fsctx.BPFFS().LinkPinPath(
		p.programKernelID, ap.linkName)

	return operation.Build(
		operation.Produce(attachOutKey, ap.target,
			func(ctx context.Context, _ *operation.Bindings) (bpfman.AttachOutput, error) {
				return action.Produce[bpfman.AttachOutput](ctx, m.executor, ap.attachAction(linkPinPath))
			},
			operation.UndoFrom(func(_ *operation.Bindings) []action.Action {
				return []action.Action{
					action.DetachLink{PinPath: linkPinPath},
				}
			}),
		),

		operation.Produce(linkKey, ap.target,
			func(ctx context.Context, b *operation.Bindings) (bpfman.Link, error) {
				out := operation.Get(b, attachOutKey)
				record := bpfman.NewPinnedLinkRecord(
					bpfman.LinkID(out.LinkID),
					p.programKernelID,
					ap.details,
					*bpffs.NewLinkPath(linkPinPath),
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
				if err := m.executor.Execute(ctx, action.SaveLink{Record: record}); err != nil {
					return bpfman.Link{}, fmt.Errorf("save link metadata: %w", err)
				}
				return link, nil
			},
		),
	)
}
