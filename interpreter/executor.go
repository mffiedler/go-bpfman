package interpreter

import (
	"context"
	"fmt"

	"github.com/frobware/go-bpfman/action"
)

// ActionExecutor executes reified actions.
type ActionExecutor interface {
	Execute(ctx context.Context, a action.Action) error
	ExecuteAll(ctx context.Context, actions []action.Action) error
}

// ActionExecutorWithResult extends ActionExecutor with structured result.
// Manager can type-assert to this interface if it needs result info.
type ActionExecutorWithResult interface {
	ActionExecutor
	ExecuteAllWithResult(ctx context.Context, actions []action.Action) ActionExecutionResult
}

// ActionExecutionResult describes the outcome of executing a batch of
// actions. It does not try to interpret semantics (no StepKinds, no
// rollback); it only reports what was attempted and where it failed.
type ActionExecutionResult struct {
	// CompletedCount is the number of actions successfully executed.
	CompletedCount int

	// FailedIndex is the index of first failing action, or -1 on success.
	FailedIndex int

	// Error is the error from the failed action (nil on success).
	Error error

	// Actions is the original actions slice (for slicing the completed prefix).
	Actions []action.Action
}

// executor interprets and executes actions.
type executor struct {
	store  Store
	kernel KernelOperations
}

// NewExecutor creates a new action executor.
func NewExecutor(store Store, kernel KernelOperations) ActionExecutor {
	return &executor{
		store:  store,
		kernel: kernel,
	}
}

// Execute runs a single action.
func (e *executor) Execute(ctx context.Context, a action.Action) error {
	switch a := a.(type) {
	case action.SaveProgram:
		return e.store.Save(ctx, a.KernelID, a.Metadata)

	case action.DeleteProgram:
		return e.store.Delete(ctx, a.KernelID)

	case action.SaveLink:
		return e.store.SaveLink(ctx, a.Record)

	case action.DeleteLink:
		return e.store.DeleteLink(ctx, a.LinkID)

	case action.LoadProgram:
		_, err := e.kernel.Load(ctx, a.Spec, a.BpffsRoot)
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
func (e *executor) ExecuteAllWithResult(ctx context.Context, actions []action.Action) ActionExecutionResult {
	res := ActionExecutionResult{
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

// Ensure executor implements ActionExecutorWithResult.
var _ ActionExecutorWithResult = (*executor)(nil)
