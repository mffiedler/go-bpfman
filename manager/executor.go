package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/fs"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
)

// executor interprets and executes actions.
type executor struct {
	store  platform.Store
	kernel platform.KernelOperations
	bcfs   fs.Bytecode
	bpffs  fs.BPFFS
	logger *slog.Logger
}

// newExecutor creates a new action executor.
func newExecutor(store platform.Store, kernel platform.KernelOperations, bcfs fs.Bytecode, bpffs fs.BPFFS, logger *slog.Logger) action.Executor {
	return &executor{
		store:  store,
		kernel: kernel,
		bcfs:   bcfs,
		bpffs:  bpffs,
		logger: logger,
	}
}

// Execute runs a single action, discarding any result.
func (e *executor) Execute(ctx context.Context, a action.Action) error {
	_, err := e.ExecuteResult(ctx, a)
	return err
}

// ExecuteResult runs a single action and returns its result.
// Actions that produce no value return (nil, error).
func (e *executor) ExecuteResult(ctx context.Context, a action.Action) (any, error) {
	switch a := a.(type) {
	case action.GetProgramFromStore:
		rec, err := e.store.Get(ctx, a.ProgramID)
		if err != nil {
			if errors.Is(err, platform.ErrRecordNotFound) {
				return nil, bpfman.ErrProgramNotFound{ID: a.ProgramID}
			}
			return nil, fmt.Errorf("get program %d: %w", a.ProgramID, err)
		}
		return rec, nil

	case action.CheckProgramNotInStore:
		if _, err := e.store.Get(ctx, a.ProgramID); err == nil {
			return nil, fmt.Errorf("program %d already exists in database", a.ProgramID)
		} else if !errors.Is(err, platform.ErrRecordNotFound) {
			return nil, fmt.Errorf("check existing program %d: %w", a.ProgramID, err)
		}
		return nil, nil

	case action.LoadProgram:
		return e.kernel.Load(ctx, a.Spec, a.BPFFS)

	case action.SaveProgram:
		return nil, e.store.RunInTransaction(ctx, func(tx platform.Store) error {
			return tx.Save(ctx, a.ProgramID, a.Metadata)
		})

	case action.DeleteProgram:
		return nil, e.store.Delete(ctx, a.ProgramID)

	case action.SaveLink:
		return nil, e.store.SaveLink(ctx, a.Record)

	case action.DeleteLink:
		return nil, e.store.DeleteLink(ctx, a.LinkID)

	case action.UnloadProgram:
		return nil, e.kernel.Unload(ctx, a.PinPath)

	case action.AttachTracepoint:
		return e.kernel.AttachTracepoint(ctx, a.ProgPinPath, a.Group, a.Name, a.LinkPinPath)

	case action.AttachKprobe:
		return e.kernel.AttachKprobe(ctx, a.ProgPinPath, a.FnName, a.Offset, a.Retprobe, a.LinkPinPath)

	case action.AttachUprobeLocal:
		return e.kernel.AttachUprobeLocal(ctx, a.ProgPinPath, a.Target, a.FnName, a.Offset, a.Retprobe, a.LinkPinPath)

	case action.AttachUprobeContainer:
		return e.kernel.AttachUprobeContainer(ctx, a.Scope, a.ProgPinPath, a.Target, a.FnName, a.Offset, a.Retprobe, a.LinkPinPath, a.ContainerPid)

	case action.AttachFentry:
		return e.kernel.AttachFentry(ctx, a.ProgPinPath, a.FnName, a.LinkPinPath)

	case action.AttachFexit:
		return e.kernel.AttachFexit(ctx, a.ProgPinPath, a.FnName, a.LinkPinPath)

	case action.AttachTCX:
		return e.kernel.AttachTCX(ctx, a.Ifindex, a.Direction, a.ProgPinPath, a.LinkPinPath, a.NetnsPath, a.Order)

	case action.SaveDispatcher:
		return nil, e.store.SaveDispatcher(ctx, a.State)

	case action.DeleteDispatcher:
		return nil, e.store.DeleteDispatcher(ctx, a.Type, a.Nsid, a.Ifindex)

	case action.DetachLink:
		return nil, e.kernel.DetachLink(ctx, a.PinPath)

	case action.RemovePin:
		return nil, e.kernel.RemovePin(ctx, a.Path)

	case action.DetachTCFilter:
		return nil, e.kernel.DetachTCFilter(ctx, a.Ifindex, a.Ifname, a.Parent, a.Priority, a.Handle)

	case action.PublishBytecode:
		return nil, e.bcfs.PublishBytecode(a.ProgramID, a.SourcePath, a.Provenance)

	case action.RemoveProgramDir:
		return nil, e.bcfs.RemoveProgram(a.ProgramID)

	case action.RemoveProgPin:
		return nil, e.bpffs.RemoveProgPin(a.Path)

	case action.RemoveLinkDir:
		return nil, e.bpffs.RemoveLinkDir(a.Path)

	case action.RemoveMapDir:
		return nil, e.bpffs.RemoveMapDir(a.Path)

	case action.RemoveDispatcherProgPin:
		return nil, e.bpffs.RemoveDispatcherProgPin(a.Path)

	case action.RemoveDispatcherRevDir:
		return nil, e.bpffs.RemoveDispatcherRevDir(a.Path)

	case action.RemoveDispatcherLinkPin:
		return nil, e.bpffs.RemoveDispatcherLinkPin(a.Path)

	case action.RemoveProgramDirByPath:
		return nil, e.bcfs.RemoveProgramDir(a.Path)

	case action.RemoveStagingDir:
		return nil, e.bcfs.RemoveStagingDir(a.Path)

	case action.EnsureXDPDispatcher:
		return e.ensureDispatcher(ctx, dispatcher.DispatcherTypeXDP, a.Ifindex, a.NetnsPath,
			func(nsid uint64) (dispatcher.State, error) {
				return createXDPDispatcherHelper(ctx, e.store, e.kernel, e.bpffs, e.logger, nsid, a.Ifindex, a.NetnsPath)
			})

	case action.EnsureTCDispatcher:
		return e.ensureDispatcher(ctx, a.DispType, a.Ifindex, a.NetnsPath,
			func(nsid uint64) (dispatcher.State, error) {
				return createTCDispatcherHelper(ctx, e.store, e.kernel, e.bpffs, e.logger, nsid, a.Ifindex, a.Ifname, a.Direction, a.DispType, a.NetnsPath)
			})

	case action.AttachXDPExtension:
		return attachExtensionWithRetry(ctx, e.store, e.bpffs, e.logger,
			extensionOps{
				label:    "XDP",
				dispType: dispatcher.DispatcherTypeXDP,
				attach: func(ctx context.Context, dispatcherPinPath, objectPath, programName string, position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error) {
					return e.kernel.AttachXDPExtension(ctx, dispatcher.XDPExtensionAttachSpec{
						DispatcherPinPath: dispatcherPinPath,
						ObjectPath:        objectPath,
						ProgramName:       programName,
						Position:          position,
						LinkPinPath:       linkPinPath,
						MapPinDir:         mapPinDir,
					})
				},
				recreate: func(ctx context.Context, nsid uint64, ifindex uint32) (dispatcher.State, error) {
					return createXDPDispatcherHelper(ctx, e.store, e.kernel, e.bpffs, e.logger, nsid, ifindex, a.NetnsPath)
				},
			},
			a.DispState, a.ObjectPath, a.ProgramName, a.MapPinDir)

	case action.AttachTCExtension:
		return attachExtensionWithRetry(ctx, e.store, e.bpffs, e.logger,
			extensionOps{
				label:    "TC",
				dispType: a.DispType,
				attach: func(ctx context.Context, dispatcherPinPath, objectPath, programName string, position int, linkPinPath, mapPinDir string) (bpfman.AttachOutput, error) {
					return e.kernel.AttachTCExtension(ctx, dispatcher.TCExtensionAttachSpec{
						DispatcherPinPath: dispatcherPinPath,
						ObjectPath:        objectPath,
						ProgramName:       programName,
						Position:          position,
						LinkPinPath:       linkPinPath,
						MapPinDir:         mapPinDir,
					})
				},
				recreate: func(ctx context.Context, nsid uint64, ifindex uint32) (dispatcher.State, error) {
					return createTCDispatcherHelper(ctx, e.store, e.kernel, e.bpffs, e.logger, nsid, ifindex, a.Ifname, a.Direction, a.DispType, a.NetnsPath)
				},
			},
			a.DispState, a.ObjectPath, a.ProgramName, a.MapPinDir)

	case action.CleanupEmptyDispatcher:
		return nil, e.cleanupEmptyDispatcher(ctx, a.State)

	default:
		return nil, fmt.Errorf("unknown action type: %T", a)
	}
}

// ExecuteAll runs multiple actions, stopping on first error.
func (e *executor) ExecuteAll(ctx context.Context, actions []action.Action) error {
	return e.ExecuteAllWithResult(ctx, actions).Error
}

// ExecuteAllWithResult runs multiple actions, stopping on first error,
// and returns structured information about what completed and what failed.
func (e *executor) ExecuteAllWithResult(ctx context.Context, actions []action.Action) action.ExecutionResult {
	res := action.ExecutionResult{
		CompletedCount: 0,
		FailedIndex:    -1,
		Error:          nil,
		Actions:        actions,
	}

	for i, a := range actions {
		if err := e.Execute(ctx, a); err != nil {
			res.FailedIndex = i
			res.Error = err
			return res
		}
		res.CompletedCount++
	}

	return res
}

// ensureDispatcher looks up an existing dispatcher by type, namespace,
// and interface index. If none exists, it calls create to provision a
// new one. The nsid is resolved from netnsPath before the lookup.
func (e *executor) ensureDispatcher(
	ctx context.Context,
	dispType dispatcher.DispatcherType,
	ifindex uint32,
	netnsPath string,
	create func(nsid uint64) (dispatcher.State, error),
) (dispatcher.State, error) {
	nsid, err := netns.GetNsid(netnsPath)
	if err != nil {
		return dispatcher.State{}, fmt.Errorf("get nsid: %w", err)
	}
	state, err := e.store.GetDispatcher(ctx, dispType, nsid, ifindex)
	if err == nil {
		return state, nil
	}
	if !errors.Is(err, platform.ErrRecordNotFound) {
		return dispatcher.State{}, fmt.Errorf("get dispatcher: %w", err)
	}
	return create(nsid)
}

// cleanupEmptyDispatcher checks whether a dispatcher has any
// remaining extension links and, if not, removes it from both the
// kernel and the store.
func (e *executor) cleanupEmptyDispatcher(ctx context.Context, state dispatcher.State) error {
	remaining, err := e.store.CountDispatcherLinks(ctx, state.ProgramID)
	if err != nil {
		e.logger.WarnContext(ctx, "failed to count dispatcher links", "error", err)
		return nil
	}
	if remaining > 0 {
		return nil
	}

	// For TC dispatchers, query the kernel for the filter handle
	// since it is no longer stored.
	var tcHandle uint32
	if state.Type == dispatcher.DispatcherTypeTCIngress || state.Type == dispatcher.DispatcherTypeTCEgress {
		parent := dispatcher.TCParentHandle(state.Type)
		handle, err := e.kernel.FindTCFilterHandle(ctx, int(state.Ifindex), parent, state.Priority)
		if err != nil {
			e.logger.WarnContext(ctx, "failed to find TC filter handle", "error", err)
		} else {
			tcHandle = handle
		}
	}

	cleanupActions := computeDispatcherCleanupActions(e.bpffs, state, tcHandle)
	if err := e.ExecuteAll(ctx, cleanupActions); err != nil {
		return fmt.Errorf("execute dispatcher cleanup actions: %w", err)
	}
	return nil
}

// Ensure executor implements action.ExecutorWithResult.
var _ action.ExecutorWithResult = (*executor)(nil)
