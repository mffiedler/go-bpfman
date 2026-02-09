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

// dispatcherAttach implements the common skeleton for dispatcher-based
// attach types (XDP, TC).
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// The operation creates a dispatcher if none exists, attaches the user
// program as an extension to a dispatcher slot, and persists the link
// metadata. On extension attach failure due to a missing dispatcher
// pin (stale record after a fresh bpffs mount), the dispatcher is
// recreated and the attach retried once.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) dispatcherAttach(ctx context.Context, p dispatcherAttachParams) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	// FETCH: Get program metadata to access ObjectPath and ProgramName
	prog, err := m.getProgram(ctx, p.programKernelID)
	if err != nil {
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: p.target,
			Error:  err.Error(),
		})
		return fail(err)
	}

	// FETCH: Get network namespace ID (from target namespace if specified)
	nsid, err := netns.GetNsid(p.netnsPath)
	if err != nil {
		primaryErr := fmt.Errorf("get nsid: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: p.target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	ifindex := uint32(p.ifindex)

	// FETCH: Look up existing dispatcher or create new one.
	dispState, err := m.store.GetDispatcher(ctx, string(p.dispType), nsid, ifindex)
	if errors.Is(err, store.ErrNotFound) {
		// KERNEL I/O + EXECUTE: Create new dispatcher
		dispState, err = p.createDispatcher(ctx, nsid, ifindex, p.netnsPath)
		if err != nil {
			primaryErr := fmt.Errorf("create %s dispatcher for %s: %w", p.dispType, p.target, err)
			_ = rec.Fail(outcome.Step{
				Kind:   p.dispStepKind,
				Target: p.target,
				Details: outcome.DispatcherDetails{
					Interface: p.ifname,
					Direction: p.direction,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		// Record dispatcher creation
		_ = rec.Complete(outcome.Step{
			Kind:   p.dispStepKind,
			Target: p.target,
			Details: outcome.DispatcherDetails{
				DispatcherID: dispState.KernelID,
				Interface:    p.ifname,
				Direction:    p.direction,
			},
		})
	} else if err != nil {
		primaryErr := fmt.Errorf("get dispatcher: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: p.target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	m.logger.DebugContext(ctx, "using dispatcher",
		"type", p.dispType,
		"interface", p.ifname,
		"nsid", nsid,
		"ifindex", p.ifindex,
		"revision", dispState.Revision,
		"dispatcher_id", dispState.KernelID)

	// COMPUTE: Calculate extension link path from conventions
	fs := m.fsctx.BPFFS()
	position, err := m.store.CountDispatcherLinks(ctx, dispState.KernelID)
	if err != nil {
		primaryErr := fmt.Errorf("count dispatcher links: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: p.target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}
	linkPinPath := fs.ExtensionLinkPath(p.dispType, nsid, ifindex, dispState.Revision, position)

	// COMPUTE: Use the program's MapPinPath which points to the correct maps
	// directory (either the program's own or the map owner's if sharing).
	mapPinDir := prog.Handles.MapPinPath

	// KERNEL I/O: Attach user program as extension
	progPinPath := fs.DispatcherProgPath(p.dispType, nsid, ifindex, dispState.Revision)
	attachOut, err := p.attachExtension(ctx, progPinPath, prog.Load.ObjectPath(), prog.Meta.Name, position, linkPinPath, mapPinDir)
	if err != nil {
		// Stale dispatcher recovery: the DB record exists but the
		// bpffs pin is gone (e.g., fresh mount after pod restart while
		// the kernel program survives). Delete the stale record and
		// retry with a fresh dispatcher.
		if !errors.Is(err, os.ErrNotExist) {
			primaryErr := fmt.Errorf("attach extension to %s slot %d: %w", p.target, position, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachExtension,
				Target: p.target,
				Details: outcome.LinkDetails{
					ProgramID:    p.programKernelID,
					Interface:    p.ifname,
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
		if delErr := m.store.DeleteDispatcher(ctx, string(p.dispType), nsid, ifindex); delErr != nil {
			primaryErr := fmt.Errorf("delete stale %s dispatcher: %w", p.dispType, delErr)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindStoreDeleteDispatcher,
				Target: p.target,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		dispState, err = p.createDispatcher(ctx, nsid, ifindex, p.netnsPath)
		if err != nil {
			primaryErr := fmt.Errorf("recreate %s dispatcher for %s: %w", p.dispType, p.target, err)
			_ = rec.Fail(outcome.Step{
				Kind:   p.dispStepKind,
				Target: p.target,
				Details: outcome.DispatcherDetails{
					Interface: p.ifname,
					Direction: p.direction,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		position, err = m.store.CountDispatcherLinks(ctx, dispState.KernelID)
		if err != nil {
			primaryErr := fmt.Errorf("count dispatcher links after recreate: %w", err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: p.target,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		linkPinPath = fs.ExtensionLinkPath(p.dispType, nsid, ifindex, dispState.Revision, position)
		progPinPath = fs.DispatcherProgPath(p.dispType, nsid, ifindex, dispState.Revision)
		attachOut, err = p.attachExtension(ctx, progPinPath, prog.Load.ObjectPath(), prog.Meta.Name, position, linkPinPath, mapPinDir)
		if err != nil {
			primaryErr := fmt.Errorf("attach extension to %s slot %d (after recreate): %w", p.target, position, err)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindAttachExtension,
				Target: p.target,
				Details: outcome.LinkDetails{
					ProgramID:    p.programKernelID,
					Interface:    p.ifname,
					PinPath:      linkPinPath,
					DispatcherID: dispState.KernelID,
				},
				Error: primaryErr.Error(),
			})
			return fail(primaryErr)
		}
	}

	// COMPUTE: Construct LinkRecord from attach output
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		p.programKernelID,
		p.buildLinkDetails(nsid, position, dispState),
		*bpffs.NewLinkPath(linkPinPath),
		time.Now(),
	)

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
		Target: p.target,
		Details: outcome.LinkDetails{
			LinkID:       attachOut.LinkID,
			ProgramID:    p.programKernelID,
			Interface:    p.ifname,
			PinPath:      linkPinPath,
			DispatcherID: dispState.KernelID,
		},
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
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: p.programKernelID,
				Interface: p.ifname,
				PinPath:   linkPinPath,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed",
					"step", f.Step, "error", f.Err)
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
			ProgramID: p.programKernelID,
			Interface: p.ifname,
			PinPath:   linkPinPath,
		},
	})

	m.logger.InfoContext(ctx, "attached via dispatcher",
		"type", p.dispType,
		"link_id", link.Record.ID,
		"program_id", p.programKernelID,
		"interface", p.ifname,
		"ifindex", p.ifindex,
		"nsid", nsid,
		"position", position,
		"revision", dispState.Revision,
		"pin_path", linkPinPath)

	return link, nil
}
