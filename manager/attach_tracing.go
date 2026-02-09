package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/outcome"
)

// attachTracepoint attaches a pinned program to a tracepoint.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachTracepoint(ctx context.Context, spec bpfman.TracepointAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	group, name := spec.Group(), spec.Name()
	target := group + "/" + name
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		stepKind:        outcome.StepKindAttachTracepoint,
		defaultTarget:   target,
		prepare: func(_ bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			return attachPlan{
				target:   target,
				linkName: fmt.Sprintf("%s_%s", group, name),
				details:  bpfman.TracepointDetails{Group: group, Name: name},
				attach: func(linkPinPath string) (bpfman.AttachOutput, error) {
					return m.kernel.AttachTracepoint(ctx, progPinPath, group, name, linkPinPath)
				},
			}, nil
		},
	})
}

// attachKprobe attaches a pinned program to a kernel function.
// retprobe is derived from the program type stored in the database.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachKprobe(ctx context.Context, spec bpfman.KprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	fnName, offset := spec.FnName(), spec.Offset()
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		stepKind:        outcome.StepKindAttachKprobe,
		defaultTarget:   fnName,
		prepare: func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			retprobe := prog.Load.ProgramType() == bpfman.ProgramTypeKretprobe
			linkName := fnName
			if retprobe {
				linkName = "ret_" + linkName
			}
			return attachPlan{
				target:   fnName,
				linkName: linkName,
				function: fnName,
				details:  bpfman.KprobeDetails{FnName: fnName, Offset: offset, Retprobe: retprobe},
				attach: func(linkPinPath string) (bpfman.AttachOutput, error) {
					return m.kernel.AttachKprobe(ctx, progPinPath, fnName, offset, retprobe, linkPinPath)
				},
			}, nil
		},
	})
}

// attachUprobe attaches a pinned program to a user-space function.
// retprobe is derived from the program type stored in the database.
//
// The scope parameter is required for container uprobes (containerPid > 0)
// to pass the lock fd to the helper subprocess. For local uprobes, scope
// is not used but accepted for API uniformity.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachUprobe(ctx context.Context, scope lock.WriterScope, spec bpfman.UprobeAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	binaryTarget := spec.Target()
	fnName := spec.FnName()
	offset := spec.Offset()
	containerPid := spec.ContainerPid()
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		stepKind:        outcome.StepKindAttachUprobe,
		defaultTarget:   binaryTarget + ":" + fnName,
		prepare: func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			retprobe := prog.Load.ProgramType() == bpfman.ProgramTypeUretprobe
			if containerPid > 0 && scope == nil {
				return attachPlan{}, fmt.Errorf("container uprobe requires lock scope (containerPid=%d)", containerPid)
			}
			linkName := fnName
			if retprobe {
				linkName = "ret_" + linkName
			}
			return attachPlan{
				target:   binaryTarget + ":" + fnName,
				linkName: linkName,
				function: fnName,
				details:  bpfman.UprobeDetails{Target: binaryTarget, FnName: fnName, Offset: offset, Retprobe: retprobe, ContainerPid: containerPid},
				attach: func(linkPinPath string) (bpfman.AttachOutput, error) {
					if containerPid > 0 {
						return m.kernel.AttachUprobeContainer(ctx, scope, progPinPath, binaryTarget, fnName, offset, retprobe, linkPinPath, containerPid)
					}
					return m.kernel.AttachUprobeLocal(ctx, progPinPath, binaryTarget, fnName, offset, retprobe, linkPinPath)
				},
			}, nil
		},
	})
}

// attachFentry attaches a pinned fentry program to its target kernel function.
// The target function was specified at load time and stored in the program's AttachFunc.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachFentry(ctx context.Context, spec bpfman.FentryAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		stepKind:        outcome.StepKindAttachFentry,
		prepare: func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			fnName := prog.Load.AttachFunc()
			if fnName == "" {
				return attachPlan{}, fmt.Errorf("program %d has no attach function (fentry requires attach function at load time)", spec.ProgramID())
			}
			return attachPlan{
				target:   fnName,
				linkName: "fentry_" + fnName,
				function: fnName,
				details:  bpfman.FentryDetails{FnName: fnName},
				attach: func(linkPinPath string) (bpfman.AttachOutput, error) {
					return m.kernel.AttachFentry(ctx, progPinPath, fnName, linkPinPath)
				},
			}, nil
		},
	})
}

// attachFexit attaches a pinned fexit program to its target kernel function.
// The target function was specified at load time and stored in the program's AttachFunc.
//
// On failure, returns a *ManagerError containing the full operation outcome.
func (m *Manager) attachFexit(ctx context.Context, spec bpfman.FexitAttachSpec, opts bpfman.AttachOpts) (bpfman.Link, error) {
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		stepKind:        outcome.StepKindAttachFexit,
		prepare: func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			fnName := prog.Load.AttachFunc()
			if fnName == "" {
				return attachPlan{}, fmt.Errorf("program %d has no attach function (fexit requires attach function at load time)", spec.ProgramID())
			}
			return attachPlan{
				target:   fnName,
				linkName: "fexit_" + fnName,
				function: fnName,
				details:  bpfman.FexitDetails{FnName: fnName},
				attach: func(linkPinPath string) (bpfman.AttachOutput, error) {
					return m.kernel.AttachFexit(ctx, progPinPath, fnName, linkPinPath)
				},
			}, nil
		},
	})
}
