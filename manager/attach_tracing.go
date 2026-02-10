package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager/action"
)

// attachTracepoint attaches a pinned program to a tracepoint.
func (m *Manager) attachTracepoint(ctx context.Context, spec bpfman.TracepointAttachSpec) (bpfman.Link, error) {
	group, name := spec.Group(), spec.Name()
	target := group + "/" + name
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		defaultTarget:   target,
		prepare: func(_ bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			return attachPlan{
				target:   target,
				linkName: fmt.Sprintf("%s_%s", group, name),
				details:  bpfman.TracepointDetails{Group: group, Name: name},
				attachAction: func(linkPinPath string) action.Action {
					return action.AttachTracepoint{
						ProgPinPath: progPinPath,
						Group:       group,
						Name:        name,
						LinkPinPath: linkPinPath,
					}
				},
			}, nil
		},
	})
}

// attachKprobe attaches a pinned program to a kernel function.
// retprobe is derived from the program type stored in the database.
func (m *Manager) attachKprobe(ctx context.Context, spec bpfman.KprobeAttachSpec) (bpfman.Link, error) {
	fnName, offset := spec.FnName(), spec.Offset()
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
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
				details:  bpfman.KprobeDetails{FnName: fnName, Offset: offset, Retprobe: retprobe},
				attachAction: func(linkPinPath string) action.Action {
					return action.AttachKprobe{
						ProgPinPath: progPinPath,
						FnName:      fnName,
						Offset:      offset,
						Retprobe:    retprobe,
						LinkPinPath: linkPinPath,
					}
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
func (m *Manager) attachUprobe(ctx context.Context, scope lock.WriterScope, spec bpfman.UprobeAttachSpec) (bpfman.Link, error) {
	binaryTarget := spec.Target()
	fnName := spec.FnName()
	offset := spec.Offset()
	containerPid := spec.ContainerPid()
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
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
			var attachFn func(linkPinPath string) action.Action
			if containerPid > 0 {
				attachFn = func(linkPinPath string) action.Action {
					return action.AttachUprobeContainer{
						Scope:        scope,
						ProgPinPath:  progPinPath,
						Target:       binaryTarget,
						FnName:       fnName,
						Offset:       offset,
						Retprobe:     retprobe,
						LinkPinPath:  linkPinPath,
						ContainerPid: containerPid,
					}
				}
			} else {
				attachFn = func(linkPinPath string) action.Action {
					return action.AttachUprobeLocal{
						ProgPinPath: progPinPath,
						Target:      binaryTarget,
						FnName:      fnName,
						Offset:      offset,
						Retprobe:    retprobe,
						LinkPinPath: linkPinPath,
					}
				}
			}
			return attachPlan{
				target:       binaryTarget + ":" + fnName,
				linkName:     linkName,
				details:      bpfman.UprobeDetails{Target: binaryTarget, FnName: fnName, Offset: offset, Retprobe: retprobe, ContainerPid: containerPid},
				attachAction: attachFn,
			}, nil
		},
	})
}

// attachFentry attaches a pinned fentry program to its target kernel function.
// The target function was specified at load time and stored in the program's AttachFunc.
func (m *Manager) attachFentry(ctx context.Context, spec bpfman.FentryAttachSpec) (bpfman.Link, error) {
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		prepare: func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			fnName := prog.Load.AttachFunc()
			if fnName == "" {
				return attachPlan{}, fmt.Errorf("program %d has no attach function (fentry requires attach function at load time)", spec.ProgramID())
			}
			return attachPlan{
				target:   fnName,
				linkName: "fentry_" + fnName,
				details:  bpfman.FentryDetails{FnName: fnName},
				attachAction: func(linkPinPath string) action.Action {
					return action.AttachFentry{
						ProgPinPath: progPinPath,
						FnName:      fnName,
						LinkPinPath: linkPinPath,
					}
				},
			}, nil
		},
	})
}

// attachFexit attaches a pinned fexit program to its target kernel function.
// The target function was specified at load time and stored in the program's AttachFunc.
func (m *Manager) attachFexit(ctx context.Context, spec bpfman.FexitAttachSpec) (bpfman.Link, error) {
	return m.simpleAttach(ctx, attachParams{
		programKernelID: spec.ProgramID(),
		prepare: func(prog bpfman.ProgramRecord, progPinPath string) (attachPlan, error) {
			fnName := prog.Load.AttachFunc()
			if fnName == "" {
				return attachPlan{}, fmt.Errorf("program %d has no attach function (fexit requires attach function at load time)", spec.ProgramID())
			}
			return attachPlan{
				target:   fnName,
				linkName: "fexit_" + fnName,
				details:  bpfman.FexitDetails{FnName: fnName},
				attachAction: func(linkPinPath string) action.Action {
					return action.AttachFexit{
						ProgPinPath: progPinPath,
						FnName:      fnName,
						LinkPinPath: linkPinPath,
					}
				},
			}, nil
		},
	})
}
