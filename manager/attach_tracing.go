package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/bpffs"
	"github.com/frobware/go-bpfman/interpreter/store"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/outcome"
)

// AttachTracepoint attaches a pinned program to a tracepoint.
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachTracepoint(ctx context.Context, spec bpfman.TracepointAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()
	group := spec.Group()
	name := spec.Name()
	target := group + "/" + name

	// FETCH: Verify program exists in store
	_, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: target,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct paths from convention (kernel ID + bpffs root)
	progPinPath := m.root.BPFFS().ProgPinPath(programKernelID)

	// COMPUTE: Calculate link pin path from conventions
	linkName := fmt.Sprintf("%s_%s", group, name)
	linksDir := m.root.BPFFS().LinkPinDir(programKernelID)
	linkPinPath := filepath.Join(linksDir, linkName)

	// KERNEL I/O: Attach to the kernel
	attachOut, err := m.kernel.AttachTracepoint(ctx, progPinPath, group, name, linkPinPath)
	if err != nil {
		primaryErr := fmt.Errorf("attach tracepoint %s/%s: %w", group, name, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindAttachTracepoint,
			Target: target,
			Details: outcome.LinkDetails{
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
			},
			Error: primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.TracepointDetails{Group: group, Name: name},
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

	// Record successful kernel attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachTracepoint,
		Target: target,
		Details: outcome.LinkDetails{
			LinkID:    attachOut.LinkID,
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
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
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
		},
	})

	m.logger.InfoContext(ctx, "attached tracepoint",
		"link_id", link.Record.ID,
		"program_id", programKernelID,
		"tracepoint", target,
		"pin_path", linkPinPath)

	return link, nil
}

// AttachKprobe attaches a pinned program to a kernel function.
// retprobe is derived from the program type stored in the database.
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachKprobe(ctx context.Context, spec bpfman.KprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()
	fnName := spec.FnName()
	offset := spec.Offset()

	// FETCH: Get program to determine if it's a kretprobe
	prog, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fnName,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// Derive retprobe from program type
	retprobe := prog.Load.ProgramType() == bpfman.ProgramTypeKretprobe

	// COMPUTE: Construct paths from convention (kernel ID + bpffs root)
	progPinPath := m.root.BPFFS().ProgPinPath(programKernelID)

	// COMPUTE: Calculate link pin path from conventions
	linkName := fnName
	if retprobe {
		linkName = "ret_" + linkName
	}
	linksDir := m.root.BPFFS().LinkPinDir(programKernelID)
	linkPinPath := filepath.Join(linksDir, linkName)

	// KERNEL I/O: Attach to the kernel
	attachOut, err := m.kernel.AttachKprobe(ctx, progPinPath, fnName, offset, retprobe, linkPinPath)
	if err != nil {
		primaryErr := fmt.Errorf("attach kprobe %s: %w", fnName, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindAttachKprobe,
			Target: fnName,
			Details: outcome.LinkDetails{
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.KprobeDetails{FnName: fnName, Offset: offset, Retprobe: retprobe},
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

	// Record successful kernel attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachKprobe,
		Target: fnName,
		Details: outcome.LinkDetails{
			LinkID:    attachOut.LinkID,
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
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
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	probeType := "kprobe"
	if retprobe {
		probeType = "kretprobe"
	}
	m.logger.InfoContext(ctx, "attached "+probeType,
		"link_id", link.Record.ID,
		"program_id", programKernelID,
		"fn_name", fnName,
		"offset", offset,
		"pin_path", linkPinPath)

	return link, nil
}

// AttachUprobe attaches a pinned program to a user-space function.
// retprobe is derived from the program type stored in the database.
//
// The scope parameter is required for container uprobes (containerPid > 0)
// to pass the lock fd to the helper subprocess. For local uprobes, scope
// is not used but accepted for API uniformity.
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachUprobe(ctx context.Context, scope lock.WriterScope, spec bpfman.UprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()
	binaryTarget := spec.Target()
	fnName := spec.FnName()
	offset := spec.Offset()
	containerPid := spec.ContainerPid()
	outcomeTarget := binaryTarget + ":" + fnName

	// FETCH: Get program to determine if it's a uretprobe
	prog, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: outcomeTarget,
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// Derive retprobe from program type
	retprobe := prog.Load.ProgramType() == bpfman.ProgramTypeUretprobe

	// COMPUTE: Construct paths from convention (kernel ID + bpffs root)
	progPinPath := m.root.BPFFS().ProgPinPath(programKernelID)

	// COMPUTE: Calculate link pin path from conventions
	linkName := fnName
	if retprobe {
		linkName = "ret_" + linkName
	}
	linksDir := m.root.BPFFS().LinkPinDir(programKernelID)
	linkPinPath := filepath.Join(linksDir, linkName)

	// KERNEL I/O: Choose local vs container method based on spec
	var attachOut bpfman.AttachOutput
	if containerPid > 0 {
		// Container uprobe - scope required
		if scope == nil {
			primaryErr := fmt.Errorf("container uprobe requires lock scope (containerPid=%d)", containerPid)
			_ = rec.Fail(outcome.Step{
				Kind:   outcome.StepKindPreflight,
				Target: outcomeTarget,
				Error:  primaryErr.Error(),
			})
			return fail(primaryErr)
		}
		attachOut, err = m.kernel.AttachUprobeContainer(ctx, scope, progPinPath, binaryTarget, fnName, offset, retprobe, linkPinPath, containerPid)
	} else {
		// Local uprobe - no scope needed
		attachOut, err = m.kernel.AttachUprobeLocal(ctx, progPinPath, binaryTarget, fnName, offset, retprobe, linkPinPath)
	}
	if err != nil {
		primaryErr := fmt.Errorf("attach uprobe %s to %s: %w", fnName, binaryTarget, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindAttachUprobe,
			Target: outcomeTarget,
			Details: outcome.LinkDetails{
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.UprobeDetails{Target: binaryTarget, FnName: fnName, Offset: offset, Retprobe: retprobe, ContainerPid: containerPid},
		*bpffs.NewLinkPath(linkPinPath),
		time.Now(),
	)

	// Construct Link with Status from AttachOutput
	link := bpfman.Link{
		Record: linkRecord,
		Status: bpfman.LinkStatus{
			Kernel:     attachOut.KernelLink,
			KernelSeen: attachOut.KernelLink != nil,
			PinPresent: attachOut.PinPath != "" && !attachOut.Synthetic,
		},
	}

	// Record successful kernel attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachUprobe,
		Target: outcomeTarget,
		Details: outcome.LinkDetails{
			LinkID:    attachOut.LinkID,
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
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
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	probeType := "uprobe"
	if retprobe {
		probeType = "uretprobe"
	}
	m.logger.InfoContext(ctx, "attached "+probeType,
		"link_id", link.Record.ID,
		"program_id", programKernelID,
		"target", binaryTarget,
		"fn_name", fnName,
		"offset", offset,
		"container_pid", containerPid,
		"pin_path", linkPinPath)

	return link, nil
}

// AttachFentry attaches a pinned fentry program to its target kernel function.
// The target function was specified at load time and stored in the program's AttachFunc.
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachFentry(ctx context.Context, spec bpfman.FentryAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()

	// FETCH: Get program metadata to access AttachFunc
	prog, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fmt.Sprintf("program_%d", programKernelID),
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	fnName := prog.Load.AttachFunc()
	if fnName == "" {
		primaryErr := fmt.Errorf("program %d has no attach function (fentry requires attach function at load time)", programKernelID)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fmt.Sprintf("program_%d", programKernelID),
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct paths from convention (kernel ID + bpffs root)
	progPinPath := m.root.BPFFS().ProgPinPath(programKernelID)

	// COMPUTE: Calculate link pin path from conventions
	linkName := "fentry_" + fnName
	linksDir := m.root.BPFFS().LinkPinDir(programKernelID)
	linkPinPath := filepath.Join(linksDir, linkName)

	// KERNEL I/O: Attach to the kernel
	attachOut, err := m.kernel.AttachFentry(ctx, progPinPath, fnName, linkPinPath)
	if err != nil {
		primaryErr := fmt.Errorf("attach fentry %s: %w", fnName, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindAttachFentry,
			Target: fnName,
			Details: outcome.LinkDetails{
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.FentryDetails{FnName: fnName},
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

	// Record successful kernel attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachFentry,
		Target: fnName,
		Details: outcome.LinkDetails{
			LinkID:    attachOut.LinkID,
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
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
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	m.logger.InfoContext(ctx, "attached fentry",
		"link_id", link.Record.ID,
		"program_id", programKernelID,
		"fn_name", fnName,
		"pin_path", linkPinPath)

	return link, nil
}

// AttachFexit attaches a pinned fexit program to its target kernel function.
// The target function was specified at load time and stored in the program's AttachFunc.
//
// Pattern: FETCH -> KERNEL I/O -> COMPUTE -> EXECUTE
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) AttachFexit(ctx context.Context, spec bpfman.FexitAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	var o outcome.OperationOutcome
	rec := outcome.NewRecorder(&o)

	fail := func(primaryErr error) (bpfman.Link, error) {
		o.PrimaryError = primaryErr.Error()
		rec.Finalise()
		return bpfman.Link{}, &ManagerError{Outcome: o, Cause: primaryErr}
	}

	programKernelID := spec.ProgramID()

	// FETCH: Get program metadata to access AttachFunc
	prog, err := m.store.Get(ctx, programKernelID)
	if err != nil {
		var primaryErr error
		if errors.Is(err, store.ErrNotFound) {
			primaryErr = bpfman.ErrProgramNotFound{ID: programKernelID}
		} else {
			primaryErr = fmt.Errorf("get program %d: %w", programKernelID, err)
		}
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fmt.Sprintf("program_%d", programKernelID),
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	fnName := prog.Load.AttachFunc()
	if fnName == "" {
		primaryErr := fmt.Errorf("program %d has no attach function (fexit requires attach function at load time)", programKernelID)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindPreflight,
			Target: fmt.Sprintf("program_%d", programKernelID),
			Error:  primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct paths from convention (kernel ID + bpffs root)
	progPinPath := m.root.BPFFS().ProgPinPath(programKernelID)

	// COMPUTE: Calculate link pin path from conventions
	linkName := "fexit_" + fnName
	linksDir := m.root.BPFFS().LinkPinDir(programKernelID)
	linkPinPath := filepath.Join(linksDir, linkName)

	// KERNEL I/O: Attach to the kernel
	attachOut, err := m.kernel.AttachFexit(ctx, progPinPath, fnName, linkPinPath)
	if err != nil {
		primaryErr := fmt.Errorf("attach fexit %s: %w", fnName, err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindAttachFexit,
			Target: fnName,
			Details: outcome.LinkDetails{
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: primaryErr.Error(),
		})
		return fail(primaryErr)
	}

	// COMPUTE: Construct LinkSpec from AttachSpec + AttachOutput
	linkRecord := bpfman.NewPinnedLinkRecord(
		bpfman.LinkID(attachOut.LinkID),
		programKernelID,
		bpfman.FexitDetails{FnName: fnName},
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

	// Record successful kernel attach
	_ = rec.Complete(outcome.Step{
		Kind:   outcome.StepKindAttachFexit,
		Target: fnName,
		Details: outcome.LinkDetails{
			LinkID:    attachOut.LinkID,
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	// ROLLBACK: If the store write fails, detach the link we just created.
	var undo undoStack
	undo.push(func() error {
		return m.kernel.DetachLink(ctx, linkPinPath)
	})

	// EXECUTE: Save link metadata directly to store
	if err := m.store.SaveLink(ctx, link.Record); err != nil {
		m.logger.ErrorContext(ctx, "persist failed, rolling back", "program_id", programKernelID, "error", err)

		storeErr := fmt.Errorf("save link metadata: %w", err)
		_ = rec.Fail(outcome.Step{
			Kind:   outcome.StepKindStoreSaveLink,
			Target: fmt.Sprintf("%d", link.Record.ID),
			Details: outcome.LinkDetails{
				LinkID:    uint32(link.Record.ID),
				ProgramID: programKernelID,
				PinPath:   linkPinPath,
				Function:  fnName,
			},
			Error: storeErr.Error(),
		})

		rec.BeginRollback()
		if rbErrs := undo.rollback(); len(rbErrs) > 0 {
			for _, f := range rbErrs {
				m.logger.ErrorContext(ctx, "rollback step failed", "step", f.Step, "error", f.Err)
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
			ProgramID: programKernelID,
			PinPath:   linkPinPath,
			Function:  fnName,
		},
	})

	m.logger.InfoContext(ctx, "attached fexit",
		"link_id", link.Record.ID,
		"program_id", programKernelID,
		"fn_name", fnName,
		"pin_path", linkPinPath)

	return link, nil
}
