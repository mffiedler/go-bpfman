package manager

import (
	"errors"
	"testing"
)

func TestUndoStack_ReverseOrder(t *testing.T) {
	var order []int
	var undo undoStack
	for i := 0; i < 3; i++ {
		undo.push(func() error {
			order = append(order, i)
			return nil
		})
	}
	if errs := undo.rollback(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(order) != 3 || order[0] != 2 || order[1] != 1 || order[2] != 0 {
		t.Fatalf("expected reverse order [2 1 0], got %v", order)
	}
}

func TestUndoStack_CollectsErrors(t *testing.T) {
	errA := errors.New("a")
	errB := errors.New("b")
	var undo undoStack
	undo.push(func() error { return errA })
	undo.push(func() error { return errB })

	errs := undo.rollback()
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(errs))
	}
	// Executed in reverse: step 1 (errB) fails first, then step 0 (errA)
	if errs[0].Step != 1 || !errors.Is(errs[0].Err, errB) {
		t.Fatalf("expected step 1 with errB, got step %d with %v", errs[0].Step, errs[0].Err)
	}
	if errs[1].Step != 0 || !errors.Is(errs[1].Err, errA) {
		t.Fatalf("expected step 0 with errA, got step %d with %v", errs[1].Step, errs[1].Err)
	}
}

func TestUndoStack_EmptyIsNoop(t *testing.T) {
	var undo undoStack
	if errs := undo.rollback(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestUndoStack_PartialFailure(t *testing.T) {
	errOnly := errors.New("only this fails")
	var undo undoStack
	undo.push(func() error { return nil })
	undo.push(func() error { return errOnly })
	undo.push(func() error { return nil })

	errs := undo.rollback()
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if errs[0].Step != 1 || !errors.Is(errs[0].Err, errOnly) {
		t.Fatalf("expected step 1 with errOnly, got step %d with %v", errs[0].Step, errs[0].Err)
	}
}
