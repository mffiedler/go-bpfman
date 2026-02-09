package operation

import (
	"context"
	"errors"
	"log/slog"

	"github.com/frobware/go-bpfman/manager/action"
)

// Run executes a plan and returns the bindings on success. On
// failure it returns an *OperationError wrapping the full outcome.
func Run(
	ctx context.Context,
	begin BeginFunc,
	exec action.ExecutorWithResult,
	plan Plan,
) (*Bindings, error) {
	rs := begin(ctx)
	bindings := newBindings()

	undos := interpret(ctx, rs, plan, bindings)

	if rs.Outcome.PrimaryError != "" {
		executeRollback(ctx, rs, exec, undos)
		rs.Rec.Finalise()
		return nil, &OperationError{
			Outcome: *rs.Outcome,
			Cause:   errors.New(rs.Outcome.PrimaryError),
		}
	}

	rs.Rec.Finalise()
	return bindings, nil
}

// Run0 executes a plan that produces no needed result bindings.
func Run0(
	ctx context.Context,
	begin BeginFunc,
	exec action.ExecutorWithResult,
	plan Plan,
) error {
	_, err := Run(ctx, begin, exec, plan)
	return err
}

// interpret walks the plan nodes in order. On the first failure it
// sets the primary error and auto-skips all remaining nodes. Returns
// the accumulated undo entries (in forward order; the caller reverses
// them for rollback).
func interpret(
	ctx context.Context,
	rs *RunState,
	plan Plan,
	bindings *Bindings,
) []UndoEntry {
	var (
		undos []UndoEntry
		opErr error
	)

	for _, n := range plan.nodes {
		if opErr != nil {
			rs.Rec.SkipStep(n.kind, n.target, "prior failure")
			continue
		}

		switch n.flavour {
		case flavourValidate, flavourDo:
			err := n.execFn(ctx, bindings)
			if err != nil {
				opErr = err
				rs.Rec.FailStep(n.kind, n.target, err)
			} else {
				h := rs.Rec.CompleteStep(n.kind, n.target)
				if n.detailsFn != nil {
					rs.Rec.SetDetails(h, n.detailsFn(bindings))
				}
				undos = appendUndos(undos, &n, bindings)
			}

		case flavourProduce:
			val, err := n.produceFn(ctx, bindings)
			if err != nil {
				opErr = err
				rs.Rec.FailStep(n.kind, n.target, err)
			} else {
				bindings.m[n.bindKey] = val
				h := rs.Rec.CompleteStep(n.kind, n.target)
				if n.detailsFn != nil {
					rs.Rec.SetDetails(h, n.detailsFn(bindings))
				}
				undos = appendUndos(undos, &n, bindings)
			}

		case flavourTry:
			err := n.execFn(ctx, bindings)
			if err != nil {
				rs.Rec.WarnStep(n.kind, n.target, err)
			} else {
				rs.Rec.CompleteStep(n.kind, n.target)
			}
		}
	}

	if opErr != nil {
		rs.Outcome.PrimaryError = opErr.Error()
	}

	return undos
}

// appendUndos evaluates late-bind undo closures or appends a static
// undo entry. Called only on successful execution of a node.
func appendUndos(undos []UndoEntry, n *node, bindings *Bindings) []UndoEntry {
	if n.undoFn != nil {
		return append(undos, n.undoFn(bindings)...)
	}
	if n.staticUndo != nil {
		return append(undos, *n.staticUndo)
	}
	return undos
}

// executeRollback walks the undo entries in reverse order, executing
// every entry regardless of individual failures. Failures are
// recorded in the outcome timeline and logged at the severity level
// declared in the entry.
func executeRollback(
	ctx context.Context,
	rs *RunState,
	exec action.ExecutorWithResult,
	undos []UndoEntry,
) {
	if len(undos) == 0 {
		return
	}

	rs.Rec.BeginRollback()
	anyFailed := false

	for i := len(undos) - 1; i >= 0; i-- {
		e := undos[i]
		err := exec.Execute(ctx, e.Action)

		if err == nil {
			if _, recErr := rs.Rec.RollbackComplete(e.Step); recErr != nil {
				rs.Logger.Log(ctx, slog.LevelError, "rollback recording error",
					"kind", e.Step.Kind, "target", e.Step.Target,
					"error", recErr)
			}
			continue
		}

		failed := e.Step
		failed.Error = err.Error()
		if _, recErr := rs.Rec.RollbackFail(failed); recErr != nil {
			rs.Logger.Log(ctx, slog.LevelError, "rollback recording error",
				"kind", failed.Kind, "target", failed.Target,
				"error", recErr)
		}
		anyFailed = true

		level := slog.LevelError
		if e.Severity == SeverityWarning {
			level = slog.LevelWarn
		}
		rs.Logger.Log(ctx, level, "rollback step failed",
			"kind", e.Step.Kind, "target", e.Step.Target,
			"error", err)
	}

	if anyFailed && rs.Probe != nil {
		arts, probeErr := rs.Probe(ctx)
		rs.Rec.SetResidual(arts, probeErr)
	}
}
