package operation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/outcome"
)

// Key is a typed reference to a plan binding. The type parameter
// ensures that Get returns the correct type without requiring callers
// to assert.
type Key[T any] struct{ name string }

// NewKey creates a binding key with the given name.
func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

// Bindings stores values produced by Produce nodes during execution.
type Bindings struct{ m map[string]any }

func newBindings() *Bindings { return &Bindings{m: make(map[string]any)} }

// Get retrieves a typed value from bindings. Panics if the key is
// absent; this is a programming error indicating the Produce node was
// skipped or has not run yet.
func Get[T any](b *Bindings, key Key[T]) T {
	v, ok := b.m[key.name]
	if !ok {
		panic(fmt.Sprintf("operation.Get: key %q not bound", key.name))
	}
	val, ok2 := v.(T)
	if !ok2 {
		panic(fmt.Sprintf("operation.Get: key %q has type %T, not %T", key.name, v, val))
	}
	return val
}

// RollbackSeverity controls how a rollback failure is reported.
// All entries are always attempted regardless of severity.
type RollbackSeverity uint8

const (
	SeverityError RollbackSeverity = iota
	SeverityWarning
)

// UndoEntry describes a single rollback action to execute on failure.
type UndoEntry struct {
	Action   action.Action
	Step     outcome.Step
	Severity RollbackSeverity
}

// ResidualProbe inspects actual kernel/filesystem state to discover
// leftover artefacts after a failed operation (including rollback).
type ResidualProbe func(ctx context.Context) ([]outcome.Artefact, error)

// RunState holds the per-operation state that the interpreter needs.
// Created by BeginFunc, which is owned by the manager layer.
type RunState struct {
	Outcome *outcome.OperationOutcome
	Rec     outcome.ManagerOperationRecorder
	Logger  *slog.Logger
	Probe   ResidualProbe
}

// BeginFunc initialises a RunState for each operation invocation. The
// manager provides this; the operation package never creates recorders
// or outcomes directly.
type BeginFunc func(ctx context.Context) *RunState

// OperationError wraps a failed OperationOutcome so that callers can
// inspect the full timeline via errors.As. The manager boundary
// converts this to its own ManagerError type.
type OperationError struct {
	Outcome outcome.OperationOutcome
	Cause   error
}

func (e *OperationError) Error() string { return e.Outcome.PrimaryError }
func (e *OperationError) Unwrap() error { return e.Cause }

// ensure OperationError satisfies the error interface.
var _ error = (*OperationError)(nil)

// ensure OperationError is unwrappable.
var _ interface{ Unwrap() error } = (*OperationError)(nil)

// AsOperationError extracts an *OperationError from err, if present.
func AsOperationError(err error) (*OperationError, bool) {
	var opErr *OperationError
	if errors.As(err, &opErr) {
		return opErr, true
	}
	return nil, false
}
