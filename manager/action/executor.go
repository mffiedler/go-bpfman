package action

import (
	"context"
	"fmt"
)

// Executor executes reified actions.
type Executor interface {
	// Execute runs a single action, discarding any result.
	Execute(ctx context.Context, a Action) error

	// ExecuteResult runs a single action and returns its result.
	// Actions that produce no value return (nil, error).
	ExecuteResult(ctx context.Context, a Action) (any, error)

	ExecuteAll(ctx context.Context, actions []Action) error
}

// ExecutorWithResult extends Executor with structured result.
// Manager can type-assert to this interface if it needs result info.
type ExecutorWithResult interface {
	Executor
	ExecuteAllWithResult(ctx context.Context, actions []Action) ExecutionResult
}

// Produce executes an action and returns the typed result. It
// provides compile-time type safety over the raw any return from
// ExecuteResult.
func Produce[T any](ctx context.Context, exec Executor, a Action) (T, error) {
	result, err := exec.ExecuteResult(ctx, a)
	if err != nil {
		var zero T
		return zero, err
	}
	if result == nil {
		var zero T
		return zero, nil
	}
	typed, ok := result.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("action %T produced %T, want %T", a, result, zero)
	}
	return typed, nil
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
