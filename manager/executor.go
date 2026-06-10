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
		return nil, e.store.RunInTransaction(ctx, "save_program", func(tx platform.Store) error {
			return tx.Save(ctx, a.ProgramID, a.Metadata)
		})

	case action.DeleteProgram:
		return nil, e.store.Delete(ctx, a.ProgramID)

	case action.CreateLink:
		var record bpfman.LinkRecord
		err := e.store.RunInTransaction(ctx, "create_link", func(tx platform.Store) error {
			var err error
			record, err = tx.CreateLink(ctx, a.Spec)
			return err
		})
		if err != nil {
			return nil, err
		}
		return record, nil

	case action.DeleteLink:
		return nil, e.store.DeleteLink(ctx, a.LinkID)

	case action.UnloadProgram:
		return nil, e.kernel.Unload(ctx, a.PinPath.String())

	case action.RemoveMapsPins:
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

	case action.DeleteDispatcher:
		return nil, e.store.DeleteDispatcherSnapshot(ctx, dispatcher.Key{
			Type: a.Type, Nsid: a.Nsid, Ifindex: a.Ifindex,
		})

	case action.DetachLink:
		return nil, e.kernel.DetachLink(ctx, a.PinPath)

	case action.DetachTCFilter:
		return nil, e.kernel.DetachTCFilter(ctx, a.Ifindex, a.Ifname, a.Parent, a.Priority, a.Handle, a.NetnsPath)

	case action.PublishBytecode:
		return nil, e.bcfs.PublishBytecode(a.ProgramID, a.SourcePath, a.Provenance)

	case action.RemoveProgramDir:
		return nil, e.bcfs.RemoveProgramDir(a.Path)

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

	case action.RemoveStagingDir:
		return nil, e.bcfs.RemoveStagingDir(a.Path)

	case action.SaveSharedMapPins:
		return nil, e.store.RunInTransaction(ctx, "save_shared_map_pins", func(tx platform.Store) error {
			return tx.SaveSharedMapPins(ctx, a.ProgramID, a.MapNames)
		})

	case action.CleanupSharedMapPins:
		var orphaned []string
		if err := e.store.RunInTransaction(ctx, "cleanup_shared_map_pins", func(tx platform.Store) error {
			var txErr error
			orphaned, txErr = tx.DeleteSharedMapPins(ctx, a.ProgramID)
			return txErr
		}); err != nil {
			return nil, err
		}
		for _, mapName := range orphaned {
			path := e.bpffs.SharedMapPin(mapName)
			if rmErr := e.bpffs.RemoveSharedMapPin(path); rmErr != nil {
				e.logger.Warn("failed to remove orphaned shared map pin",
					"path", path, "error", rmErr)
			}
		}
		return nil, nil

	case action.RemoveSharedMapPin:
		return nil, e.bpffs.RemoveSharedMapPin(a.Path)

	case action.RebuildXDPDispatcher:
		return e.rebuildXDPDispatcher(ctx, a.ProgramID,
			xdpRebuildOps{ifindex: a.Ifindex, ifname: a.Ifname, netnsPath: a.NetnsPath},
			a.ProgPinPath, a.ProgramName, a.Priority, a.ProceedOn)

	case action.RebuildTCDispatcher:
		return e.rebuildTCDispatcher(ctx, a.ProgramID,
			tcRebuildOps{
				ifindex:   a.Ifindex,
				ifname:    a.Ifname,
				direction: a.Direction,
				dispType:  a.DispType,
				netnsPath: a.NetnsPath,
			},
			a.ProgPinPath, a.ProgramName, a.Priority, a.ProceedOn)

	case action.RebuildDispatcherForDetach:
		return nil, e.rebuildDispatcherForDetach(ctx, a.Key, a.ExcludeLinkID)

	case action.RemoveDispatcher:
		return nil, e.removeDispatcherIfEmpty(ctx, a.Key)

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

// Ensure executor implements action.ExecutorWithResult.
var _ action.ExecutorWithResult = (*executor)(nil)
