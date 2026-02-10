package manager

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/bpfmanfs"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/platform"
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

// Execute runs a single action.
func (e *executor) Execute(ctx context.Context, a action.Action) error {
	switch a := a.(type) {
	case action.SaveProgram:
		return e.store.RunInTransaction(ctx, func(tx platform.Store) error {
			return tx.Save(ctx, a.KernelID, a.Metadata)
		})

	case action.DeleteProgram:
		return e.store.Delete(ctx, a.KernelID)

	case action.SaveLink:
		return e.store.SaveLink(ctx, a.Record)

	case action.DeleteLink:
		return e.store.DeleteLink(ctx, a.LinkID)

	case action.LoadProgram:
		_, err := e.kernel.Load(ctx, a.Spec, a.BPFFS)
		return err

	case action.UnloadProgram:
		return e.kernel.Unload(ctx, a.PinPath)

	case action.Batch:
		return e.ExecuteAll(ctx, a.Actions)

	case action.Sequence:
		return e.ExecuteAll(ctx, a.Actions)

	case action.SaveDispatcher:
		return e.store.SaveDispatcher(ctx, a.State)

	case action.DeleteDispatcher:
		return e.store.DeleteDispatcher(ctx, a.Type, a.Nsid, a.Ifindex)

	case action.DetachLink:
		return e.kernel.DetachLink(ctx, a.PinPath)

	case action.RemovePin:
		return e.kernel.RemovePin(ctx, a.Path)

	case action.DetachTCFilter:
		return e.kernel.DetachTCFilter(ctx, a.Ifindex, a.Ifname, a.Parent, a.Priority, a.Handle)

	case action.PublishBytecode:
		return e.bcfs.PublishBytecode(a.KernelID, a.SourcePath, a.Provenance)

	case action.RemoveProgramDir:
		return e.bcfs.RemoveProgram(a.KernelID)

	default:
		return fmt.Errorf("unknown action type: %T", a)
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
