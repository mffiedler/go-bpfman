package manager

import "errors"

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

// joinRollbackErrors converts a slice of RollbackError to a single
// error using errors.Join. Returns nil if the slice is empty.
func joinRollbackErrors(errs []RollbackError) error {
	if len(errs) == 0 {
		return nil
	}
	unwrapped := make([]error, len(errs))
	for i, e := range errs {
		unwrapped[i] = e.Err
	}
	return errors.Join(unwrapped...)
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
