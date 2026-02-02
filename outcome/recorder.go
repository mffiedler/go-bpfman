package outcome

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrAlreadyFailed is returned when attempting to record a step after failure.
	ErrAlreadyFailed = errors.New("outcome already failed")

	// ErrRollbackNotActive is returned when recording rollback steps without BeginRollback.
	ErrRollbackNotActive = errors.New("rollback not active")
)

// ManagerOperationRecorder appends steps to a ManagerOperationOutcome while enforcing invariants.
//
// The recorder is intentionally tiny: it does not know about manager
// semantics, rollback policy, or step taxonomies. It only ensures the
// ManagerOperationOutcome structure cannot contradict itself.
type ManagerOperationRecorder struct {
	o *ManagerOperationOutcome
}

// NewRecorder initialises a ManagerOperationOutcome in a consistent state.
// Status defaults to success; failure flips status and sets Error at the
// boundary (manager), not here.
func NewRecorder(o *ManagerOperationOutcome) ManagerOperationRecorder {
	if o.Status == "" {
		o.Status = StatusSuccess
	}
	return ManagerOperationRecorder{o: o}
}

// Outcome returns the underlying ManagerOperationOutcome.
func (r ManagerOperationRecorder) Outcome() *ManagerOperationOutcome {
	return r.o
}

// Started reports whether any step was recorded (completed/failed/skipped).
func (r ManagerOperationRecorder) Started() bool {
	return len(r.o.Completed) > 0 || r.o.Failed != nil || len(r.o.Skipped) > 0
}

// Complete appends a completed step. Returns error if already failed.
func (r ManagerOperationRecorder) Complete(step Step) error {
	if r.o.Status == StatusFailure {
		return ErrAlreadyFailed
	}
	r.o.Completed = append(r.o.Completed, step)
	return nil
}

// Skip appends a skipped step. Returns error if already failed.
func (r ManagerOperationRecorder) Skip(step Step) error {
	if r.o.Status == StatusFailure {
		return ErrAlreadyFailed
	}
	r.o.Skipped = append(r.o.Skipped, step)
	return nil
}

// Fail sets the failed step and flips status to failure.
// Does NOT set ManagerOperationOutcome.Error - that's the manager boundary's job.
func (r ManagerOperationRecorder) Fail(step Step) error {
	if r.o.Status == StatusFailure {
		return ErrAlreadyFailed
	}
	if r.o.Failed != nil {
		return fmt.Errorf("failed step already set: %w", ErrAlreadyFailed)
	}
	r.o.Status = StatusFailure
	r.o.Failed = &step
	return nil
}

// BeginRollback initialises Rollback if needed. Idempotent.
func (r ManagerOperationRecorder) BeginRollback() {
	if r.o.Rollback == nil {
		r.o.Rollback = &RollbackOutcome{Status: StatusSuccess}
	}
}

// RollbackComplete records a successful cleanup step.
func (r ManagerOperationRecorder) RollbackComplete(step Step) error {
	if r.o.Rollback == nil {
		return ErrRollbackNotActive
	}
	r.o.Rollback.Completed = append(r.o.Rollback.Completed, step)
	return nil
}

// RollbackFail records a failed cleanup step and flips cleanup status.
func (r ManagerOperationRecorder) RollbackFail(step Step) error {
	if r.o.Rollback == nil {
		return ErrRollbackNotActive
	}
	r.o.Rollback.Status = StatusFailure
	r.o.Rollback.Failed = append(r.o.Rollback.Failed, step)
	return nil
}

// SetObserved records observed residue and/or observation error.
func (r ManagerOperationRecorder) SetObserved(artefacts []Artefact, observeErr error) {
	r.o.Observed = artefacts
	if observeErr != nil {
		r.o.ObservedError = observeErr.Error()
	}
}

// Validate enforces cheap invariants. Call in tests and debug builds.
func (r ManagerOperationRecorder) Validate() error {
	o := r.o
	if o.Status != StatusSuccess && o.Status != StatusFailure {
		return fmt.Errorf("invalid status: %q", o.Status)
	}
	if o.Status == StatusSuccess && o.Failed != nil {
		return errors.New("success outcome has failed step")
	}
	if o.Status == StatusFailure && o.Failed == nil && o.Error == "" {
		return errors.New("failure outcome has neither failed step nor error")
	}
	if o.Rollback != nil {
		if o.Rollback.Status == StatusSuccess && len(o.Rollback.Failed) != 0 {
			return errors.New("cleanup success has failed steps")
		}
	}

	// JSON sanity for Details - ALL steps, not just Completed
	validateStep := func(s Step, loc string) error {
		if s.Details == nil {
			return nil
		}
		if _, err := json.Marshal(s.Details); err != nil {
			return fmt.Errorf("%s step details not json-safe: %w", loc, err)
		}
		return nil
	}

	for _, s := range o.Completed {
		if err := validateStep(s, "completed"); err != nil {
			return err
		}
	}
	for _, s := range o.Skipped {
		if err := validateStep(s, "skipped"); err != nil {
			return err
		}
	}
	if o.Failed != nil {
		if err := validateStep(*o.Failed, "failed"); err != nil {
			return err
		}
	}
	if o.Rollback != nil {
		for _, s := range o.Rollback.Completed {
			if err := validateStep(s, "cleanup.completed"); err != nil {
				return err
			}
		}
		for _, s := range o.Rollback.Failed {
			if err := validateStep(s, "cleanup.failed"); err != nil {
				return err
			}
		}
	}
	return nil
}

// FailFromErr is a helper to create a failed step from an error.
func FailFromErr(kind StepKind, target string, err error) Step {
	return Step{Kind: kind, Target: target, Error: err.Error()}
}
