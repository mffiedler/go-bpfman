package manager

import (
	"context"
	"fmt"
	"io"

	"github.com/frobware/go-bpfman/manager/action"
)

// verboseExecutor wraps an executor and prints a human-readable
// description of each action before delegating. When the writer is
// io.Discard the overhead is negligible.
type verboseExecutor struct {
	real action.ExecutorWithResult
	w    io.Writer
}

func (v *verboseExecutor) Execute(ctx context.Context, a action.Action) error {
	fmt.Fprintf(v.w, "  %s\n", action.Describe(a))
	return v.real.Execute(ctx, a)
}

func (v *verboseExecutor) ExecuteResult(ctx context.Context, a action.Action) (any, error) {
	fmt.Fprintf(v.w, "  %s\n", action.Describe(a))
	return v.real.ExecuteResult(ctx, a)
}

func (v *verboseExecutor) ExecuteAll(ctx context.Context, actions []action.Action) error {
	for _, a := range actions {
		fmt.Fprintf(v.w, "  %s\n", action.Describe(a))
	}
	return v.real.ExecuteAll(ctx, actions)
}

func (v *verboseExecutor) ExecuteAllWithResult(ctx context.Context, actions []action.Action) action.ExecutionResult {
	for _, a := range actions {
		fmt.Fprintf(v.w, "  %s\n", action.Describe(a))
	}
	return v.real.ExecuteAllWithResult(ctx, actions)
}
