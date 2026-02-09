package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/outcome"
)

// attachPlan captures the variable parts of a simple attach operation.
// Returned by the prepare closure after inspecting the program record.
type attachPlan struct {
	// target is the outcome recording target (e.g., "sched/sched_switch").
	target string
	// linkName determines the bpffs pin path for the link.
	linkName string
	// function is an optional function name included in outcome details.
	function string
	// details is the sealed LinkDetails value for the link record.
	details bpfman.LinkDetails
	// attach performs the kernel I/O and returns the attach output.
	attach func(linkPinPath string) (bpfman.AttachOutput, error)
}

// attachParams describes a non-dispatcher attach operation.
type attachParams struct {
	// programKernelID is the kernel ID of the program to attach.
	programKernelID uint32
	// stepKind is the outcome step kind for the kernel attach step.
	stepKind outcome.StepKind
	// defaultTarget is used for outcome steps that occur before
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
// Pattern: FETCH -> PREPARE -> KERNEL I/O -> CONSTRUCT -> PERSIST
//
// The prepare closure supplies the varying parts: link naming, kernel
// attach call, and link details construction. Everything else—outcome
// recording, store persistence, and rollback—is handled here once.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) simpleAttach(ctx context.Context, p attachParams) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o, func(err error) {
		m.logger.Error("outcome recorder: invariant violation", "error", err)
	})

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	preTarget := p.defaultTarget
	if preTarget == "" {
		preTarget = fmt.Sprintf("program_%d", p.programKernelID)
	}

	// FETCH: Verify program exists in store.
	prog, err := m.getProgram(ctx, p.programKernelID)
	if err != nil {
		rec.FailStep(outcome.StepKindPreflight, preTarget, err)
		return fail(err)
	}

	progPinPath := m.fsctx.BPFFS().ProgPinPath(p.programKernelID)

	// PREPARE: Caller-specific derivation.
	plan, err := p.prepare(prog, progPinPath)
	if err != nil {
		target := plan.target
		if target == "" {
			target = preTarget
		}
		rec.FailStep(outcome.StepKindPreflight, target, err)
		return fail(err)
	}

	linkPinPath := m.fsctx.BPFFS().LinkPinPath(p.programKernelID, plan.linkName)

	// KERNEL I/O
	attachOut, err := plan.attach(linkPinPath)
	if err != nil {
		primaryErr := fmt.Errorf("attach %s: %w", plan.target, err)
		rec.FailStep(p.stepKind, plan.target, primaryErr, outcome.LinkDetails{
			ProgramID: p.programKernelID,
			PinPath:   linkPinPath,
			Function:  plan.function,
		})
		return fail(primaryErr)
	}

	// CONSTRUCT
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		p.programKernelID,
		plan.details,
		*bpffs.NewLinkPath(linkPinPath),
		time.Now(),
	)

	link := bpfman.Link{
		Record: linkRecord,
		Status: bpfman.LinkStatus{
			Kernel:     attachOut.KernelLink,
			KernelSeen: attachOut.KernelLink != nil,
			PinPresent: attachOut.PinPath != "" && !attachOut.Synthetic,
		},
	}

	rec.CompleteStep(p.stepKind, plan.target, outcome.LinkDetails{
		LinkID:    attachOut.LinkID,
		ProgramID: p.programKernelID,
		PinPath:   linkPinPath,
		Function:  plan.function,
	})

	// PERSIST with rollback.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back",
			"program_id", p.programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		rec.FailStep(outcome.StepKindStoreSaveLink, fmt.Sprintf("%d", link.Record.ID),
			storeErr, outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: p.programKernelID,
				PinPath:   linkPinPath,
				Function:  plan.function,
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

	rec.CompleteStep(outcome.StepKindStoreSaveLink, fmt.Sprintf("%d", link.Record.ID), outcome.LinkDetails{
		LinkID:    uint32(link.Record.ID),
		ProgramID: p.programKernelID,
		PinPath:   linkPinPath,
		Function:  plan.function,
	})

	m.logger.InfoContext(ctx, "attached",
		"link_id", link.Record.ID,
		"program_id", p.programKernelID,
		"target", plan.target,
		"pin_path", linkPinPath)

	return link, nil
}
