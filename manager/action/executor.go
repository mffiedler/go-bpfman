package action

import "context"

// Executor executes reified actions.
type Executor interface {
	Execute(ctx context.Context, a Action) error
	ExecuteAll(ctx context.Context, actions []Action) error
}

// ExecutorWithResult extends Executor with structured result.
// Manager can type-assert to this interface if it needs result info.
type ExecutorWithResult interface {
	Executor
	ExecuteAllWithResult(ctx context.Context, actions []Action) ExecutionResult
}

// ExecutionResult describes the outcome of executing a batch of
// actions. It does not try to interpret semantics (no StepKinds, no
// rollback); it only reports what was attempted and where it failed.
type ExecutionResult struct {
	// CompletedCount is the number of actions successfully executed.
	CompletedCount int

	// FailedIndex is the index of first failing action, or -1 on success.
	FailedIndex int

	// Error is the error from the failed action (nil on success).
	Error error

	// Actions is the original actions slice (for slicing the completed prefix).
	Actions []Action
}
