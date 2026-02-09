package manager

import (
	"log/slog"

	"github.com/frobware/go-bpfman/outcome"
)

// undoStack accumulates rollback closures that are executed in reverse
// order when a multi-step operation fails partway through. Each
// closure should undo one kernel-side effect (detach a link, remove a
// pin, etc.).
type undoStack []func() error

// RollbackError pairs a failure with its position in the undo stack.
type RollbackError struct {
	Step int
	Err  error
}

// toOutcomeErrors converts internal RollbackErrors to outcome package format.
func toOutcomeErrors(errs []RollbackError) []outcome.RollbackError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]outcome.RollbackError, len(errs))
	for i, e := range errs {
		out[i] = outcome.RollbackError{Step: e.Step, Err: e.Err.Error()}
	}
	return out
}

// push appends a rollback closure to the stack.
func (u *undoStack) push(fn func() error) {
	*u = append(*u, fn)
}

// rollback executes all closures in reverse order, collecting any
// errors. Returns nil if every closure succeeds.
func (u undoStack) rollback() []RollbackError {
	var errs []RollbackError
	for i := len(u) - 1; i >= 0; i-- {
		if err := u[i](); err != nil {
			errs = append(errs, RollbackError{Step: i, Err: err})
		}
	}
	return errs
}

// recordRollback executes the undo stack and records the rollback
// outcome. The step describes the rollback operation for the
// timeline. Returns true if any rollback step failed.
func recordRollback(rec *outcome.ManagerOperationRecorder, undo undoStack, step outcome.Step, logger *slog.Logger) bool {
	rec.BeginRollback()
	rbErrs := undo.rollback()
	if len(rbErrs) > 0 {
		for _, f := range rbErrs {
			logger.Error("rollback step failed", "step", f.Step, "error", f.Err)
		}
		rec.SetRollbackErrors(toOutcomeErrors(rbErrs))
		step.Error = rbErrs[0].Err.Error()
		if _, recErr := rec.RollbackFail(step); recErr != nil {
			logger.Error("outcome recorder: invariant violation", "error", recErr)
		}
		return true
	}
	if _, recErr := rec.RollbackComplete(step); recErr != nil {
		logger.Error("outcome recorder: invariant violation", "error", recErr)
	}
	return false
}
