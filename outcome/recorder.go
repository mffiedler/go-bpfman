package outcome

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAlreadyFailed is returned when attempting to record a primary step after failure.
	ErrAlreadyFailed = errors.New("outcome already failed")

	// ErrRollbackNotActive is returned when recording rollback steps without BeginRollback.
	ErrRollbackNotActive = errors.New("rollback not active")
)

// ManagerOperationRecorder appends steps to a OperationOutcome while enforcing invariants.
//
// The recorder is intentionally tiny: it does not know about manager
// semantics, rollback policy, or step taxonomies. It only ensures the
// OperationOutcome structure cannot contradict itself.
type ManagerOperationRecorder struct {
	o              *OperationOutcome
	seq            int         // current sequence number
	inRollback     bool        // whether we're in rollback phase
	rollbackFailed bool        // whether any rollback step has failed
	onErr          func(error) // called on invariant violation
}

// NewRecorder initialises a OperationOutcome in a consistent state.
// Status defaults to success; failure flips status and sets PrimaryError at the
// boundary (manager), not here.
//
// The onErr closure is called when an invariant violation occurs in
// convenience methods (FailStep, CompleteStep). These errors are
// programming errors, not runtime failures; the closure decides how
// to handle them (e.g., log via the caller's logger).
func NewRecorder(o *OperationOutcome, onErr func(error)) ManagerOperationRecorder {
	if o.Status == "" {
		o.Status = StatusSuccess
	}
	return ManagerOperationRecorder{o: o, seq: 0, onErr: onErr}
}

// Outcome returns the underlying OperationOutcome.
func (r ManagerOperationRecorder) Outcome() *OperationOutcome {
	return r.o
}

// Started reports whether any step was recorded.
func (r ManagerOperationRecorder) Started() bool {
	return len(r.o.Timeline) > 0
}

// nextSeq returns the next sequence number and increments the counter.
func (r *ManagerOperationRecorder) nextSeq() int {
	r.seq++
	return r.seq
}

// stepTimestamp returns the step's timestamp if set, otherwise time.Now().
func stepTimestamp(step Step) time.Time {
	if step.Timestamp.IsZero() {
		return time.Now()
	}
	return step.Timestamp
}

// Complete appends a completed step to the timeline. Returns error if already failed.
func (r *ManagerOperationRecorder) Complete(step Step) error {
	if r.o.Status == StatusFailure && !r.inRollback {
		return ErrAlreadyFailed
	}
	r.o.Timeline = append(r.o.Timeline, TimelineEntry{
		Seq:       r.nextSeq(),
		Timestamp: stepTimestamp(step),
		Phase:     r.currentPhase(),
		Status:    StepStatusCompleted,
		Kind:      step.Kind,
		Target:    step.Target,
		Details:   step.Details,
	})
	return nil
}

// Skip appends a skipped step to the timeline. Returns error if already failed.
func (r *ManagerOperationRecorder) Skip(step Step) error {
	if r.o.Status == StatusFailure && !r.inRollback {
		return ErrAlreadyFailed
	}
	r.o.Timeline = append(r.o.Timeline, TimelineEntry{
		Seq:       r.nextSeq(),
		Timestamp: stepTimestamp(step),
		Phase:     r.currentPhase(),
		Status:    StepStatusSkipped,
		Kind:      step.Kind,
		Target:    step.Target,
		Error:     step.Error,
	})
	return nil
}

// Fail sets the failed step and flips status to failure.
// Does NOT set OperationOutcome.PrimaryError - that's the manager boundary's job.
func (r *ManagerOperationRecorder) Fail(step Step) error {
	if r.o.Status == StatusFailure && !r.inRollback {
		return ErrAlreadyFailed
	}
	r.o.Status = StatusFailure
	r.o.Timeline = append(r.o.Timeline, TimelineEntry{
		Seq:       r.nextSeq(),
		Timestamp: stepTimestamp(step),
		Phase:     r.currentPhase(),
		Status:    StepStatusFailed,
		Kind:      step.Kind,
		Target:    step.Target,
		Error:     step.Error,
		Details:   step.Details,
	})
	return nil
}

// currentPhase returns the current phase (primary or rollback).
func (r ManagerOperationRecorder) currentPhase() Phase {
	if r.inRollback {
		return PhaseRollback
	}
	return PhasePrimary
}

// BeginRollback transitions to the rollback phase. Idempotent.
func (r *ManagerOperationRecorder) BeginRollback() {
	r.inRollback = true
}

// RollbackComplete records a successful rollback step.
func (r *ManagerOperationRecorder) RollbackComplete(step Step) error {
	if !r.inRollback {
		return ErrRollbackNotActive
	}
	r.o.Timeline = append(r.o.Timeline, TimelineEntry{
		Seq:       r.nextSeq(),
		Timestamp: stepTimestamp(step),
		Phase:     PhaseRollback,
		Status:    StepStatusCompleted,
		Kind:      step.Kind,
		Target:    step.Target,
		Details:   step.Details,
	})
	return nil
}

// RollbackFail records a failed rollback step and marks rollback as failed.
func (r *ManagerOperationRecorder) RollbackFail(step Step) error {
	if !r.inRollback {
		return ErrRollbackNotActive
	}
	r.rollbackFailed = true
	r.o.Timeline = append(r.o.Timeline, TimelineEntry{
		Seq:       r.nextSeq(),
		Timestamp: stepTimestamp(step),
		Phase:     PhaseRollback,
		Status:    StepStatusFailed,
		Kind:      step.Kind,
		Target:    step.Target,
		Error:     step.Error,
		Details:   step.Details,
	})
	return nil
}

// RollbackFailed returns true if any rollback step has failed.
func (r ManagerOperationRecorder) RollbackFailed() bool {
	return r.rollbackFailed
}

// SetResidual records residual artefacts and/or observation error.
func (r *ManagerOperationRecorder) SetResidual(artefacts []Artefact, observeErr error) {
	r.o.Residual = artefacts
	if observeErr != nil {
		r.o.ResidualError = observeErr.Error()
	}
}

// SetRollbackErrors records structured rollback errors.
func (r *ManagerOperationRecorder) SetRollbackErrors(errs []RollbackError) {
	r.o.RollbackErrors = errs
}

// Finalise computes and stores the derived fields (SystemState, ManualCleanupRequired,
// ManualCleanupCommands). Call this before returning the outcome to the caller.
func (r *ManagerOperationRecorder) Finalise() {
	r.o.SystemState = ComputeSystemState(r.o.Status, r.o.Residual, r.o.ResidualError)
	r.o.ManualCleanupRequired = ComputeManualCleanupRequired(r.o.Status, r.o.SystemState)
	r.o.ManualCleanupCommands = ComputeManualCleanupCommands(r.o.SystemState, r.o.Residual)
}

// Validate enforces cheap invariants. Call in tests and debug builds.
func (r ManagerOperationRecorder) Validate() error {
	o := r.o
	if o.Status != StatusSuccess && o.Status != StatusFailure {
		return fmt.Errorf("invalid status: %q", o.Status)
	}

	// Check timeline consistency
	hasPrimaryFailed := false
	hasRollbackFailed := false
	for _, entry := range o.Timeline {
		if entry.Phase == PhasePrimary && entry.Status == StepStatusFailed {
			hasPrimaryFailed = true
		}
		if entry.Phase == PhaseRollback && entry.Status == StepStatusFailed {
			hasRollbackFailed = true
		}
	}

	if o.Status == StatusSuccess && hasPrimaryFailed {
		return errors.New("success outcome has failed primary step")
	}
	if o.Status == StatusFailure && !hasPrimaryFailed && o.PrimaryError == "" {
		return errors.New("failure outcome has neither failed primary step nor primary error")
	}
	if hasRollbackFailed && len(o.RollbackErrors) == 0 {
		return errors.New("rollback failed but no rollback errors set")
	}

	// JSON sanity for Details
	for i, entry := range o.Timeline {
		if entry.Details == nil {
			continue
		}
		if _, err := json.Marshal(entry.Details); err != nil {
			return fmt.Errorf("timeline[%d] details not json-safe: %w", i, err)
		}
	}

	return nil
}

// FailStep records a failed step and returns err for convenient chaining.
//
// An optional single Details value may be provided.
//
//	return fail(rec.FailStep(kind, target, err))
//	return fail(rec.FailStep(kind, target, err, outcome.LinkDetails{...}))
func (r *ManagerOperationRecorder) FailStep(kind StepKind, target string, err error, details ...any) error {
	step := Step{Kind: kind, Target: target, Error: err.Error()}
	if len(details) > 0 {
		step.Details = details[0]
	}
	if recErr := r.Fail(step); recErr != nil {
		r.onErr(recErr)
	}
	return err
}

// CompleteStep records a completed step.
//
// An optional single Details value may be provided.
//
//	rec.CompleteStep(kind, target)
//	rec.CompleteStep(kind, target, outcome.ProgramDetails{...})
func (r *ManagerOperationRecorder) CompleteStep(kind StepKind, target string, details ...any) {
	step := Step{Kind: kind, Target: target}
	if len(details) > 0 {
		step.Details = details[0]
	}
	if recErr := r.Complete(step); recErr != nil {
		r.onErr(recErr)
	}
}

// FailFromErr is a helper to create a failed step from an error.
//
// An optional single Details value may be provided.
func FailFromErr(kind StepKind, target string, err error, details ...any) Step {
	step := Step{Kind: kind, Target: target, Error: err.Error()}
	if len(details) > 0 {
		step.Details = details[0]
	}
	return step
}
