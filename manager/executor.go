package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/store"
)

// executor interprets and executes actions.
type executor struct {
	store  platform.Store
	kernel platform.KernelOperations
	bcfs   bpfmanfs.BytecodeFS
}

// newExecutor creates a new action executor.
func newExecutor(store platform.Store, kernel platform.KernelOperations, bcfs bpfmanfs.BytecodeFS) action.Executor {
	return &executor{
		store:  store,
		kernel: kernel,
		bcfs:   bcfs,
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
	case action.CheckProgramNotInStore:
		if _, err := e.store.Get(ctx, a.KernelID); err == nil {
			return nil, fmt.Errorf("program %d already exists in database", a.KernelID)
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("check existing program %d: %w", a.KernelID, err)
		}
		return nil, nil

	case action.LoadProgram:
		return e.kernel.Load(ctx, a.Spec, a.BPFFS)

	case action.SaveProgram:
		return nil, e.store.RunInTransaction(ctx, func(tx platform.Store) error {
			return tx.Save(ctx, a.KernelID, a.Metadata)
		})

	case action.DeleteProgram:
		return nil, e.store.Delete(ctx, a.KernelID)

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

	case action.Batch:
		return nil, e.ExecuteAll(ctx, a.Actions)

	case action.Sequence:
		return nil, e.ExecuteAll(ctx, a.Actions)

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
		return nil, e.bcfs.PublishBytecode(a.KernelID, a.SourcePath, a.Provenance)

	case action.RemoveProgramDir:
		return nil, e.bcfs.RemoveProgram(a.KernelID)

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
