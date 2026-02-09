package operation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/outcome"
)

// testAction creates a labelled action using action.RemovePin. The
// Path field serves as the label for matching in the fake executor.
func testAction(label string) action.Action {
	return action.RemovePin{Path: label}
}

// fakeExecutor lets tests configure per-action success or failure.
type fakeExecutor struct {
	errs     map[string]error
	executed []string
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{errs: make(map[string]error)}
}

func (f *fakeExecutor) failOn(label string, err error) {
	f.errs[label] = err
}

func (f *fakeExecutor) Execute(_ context.Context, a action.Action) error {
	rp, ok := a.(action.RemovePin)
	if !ok {
		return fmt.Errorf("unexpected action type: %T", a)
	}
	f.executed = append(f.executed, rp.Path)
	if err, ok := f.errs[rp.Path]; ok {
		return err
	}
	return nil
}

func (f *fakeExecutor) ExecuteAll(ctx context.Context, actions []action.Action) error {
	return f.ExecuteAllWithResult(ctx, actions).Error
}

func (f *fakeExecutor) ExecuteAllWithResult(ctx context.Context, actions []action.Action) action.ExecutionResult {
	res := action.ExecutionResult{FailedIndex: -1, Actions: actions}
	for i, a := range actions {
		if err := f.Execute(ctx, a); err != nil {
			res.FailedIndex = i
			res.Error = err
			return res
		}
		res.CompletedCount++
	}
	return res
}

var _ action.ExecutorWithResult = (*fakeExecutor)(nil)

// testBegin creates a BeginFunc suitable for tests. It captures the
// RunState so tests can inspect the outcome.
func testBegin(rs **RunState) BeginFunc {
	return func(ctx context.Context) *RunState {
		o := &outcome.OperationOutcome{}
		rec := outcome.NewRecorder(o, func(err error) {
			panic(fmt.Sprintf("recorder invariant violation: %v", err))
		})
		s := &RunState{
			Outcome: o,
			Rec:     rec,
			Logger:  slog.Default(),
		}
		*rs = s
		return s
	}
}

// helper constants for test step kinds and targets.
const (
	kindTest outcome.StepKind = "test.step"
	kindA    outcome.StepKind = "test.a"
	kindB    outcome.StepKind = "test.b"
	kindC    outcome.StepKind = "test.c"
)

var errTest = errors.New("test error")

// ---------- Forward execution ----------

func TestValidateSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Validate("check", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusCompleted, kindTest, "t1")
}

func TestValidateFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Validate("check", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindTest, "t1")
}

func TestDoSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("action", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusCompleted, kindTest, "t1")
}

func TestDoFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("action", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindTest, "t1")
}

func TestDoWithDetailsFn(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("action", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, DetailsFn(func(_ *Bindings) any {
			return map[string]string{"info": "details"}
		})),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTimelineLen(t, rs, 1)
	entry := rs.Outcome.Timeline[0]
	if entry.Details == nil {
		t.Fatal("expected details to be set")
	}
	m, ok := entry.Details.(map[string]string)
	if !ok {
		t.Fatalf("expected map[string]string, got %T", entry.Details)
	}
	if m["info"] != "details" {
		t.Fatalf("expected info=details, got %v", m["info"])
	}
}

func TestProduceSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[int]("value")
	plan := Build(
		Produce(key, kindTest, "t1", func(_ context.Context, _ *Bindings) (int, error) {
			return 42, nil
		}),
	)

	b, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := Get(b, key); v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusCompleted, kindTest, "t1")
}

func TestProduceFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[int]("value")
	plan := Build(
		Produce(key, kindTest, "t1", func(_ context.Context, _ *Bindings) (int, error) {
			return 0, errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindTest, "t1")
}

func TestTrySuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Try("best-effort", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusCompleted, kindTest, "t1")
}

func TestTryFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Try("best-effort", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: Try failure should not fail operation")
	}
	assertTimelineLen(t, rs, 1)
	assertStep(t, rs, 0, outcome.StepStatusWarned, kindTest, "t1")
}

func TestTryAfterPriorFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
		Try("best-effort", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 2)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindA, "t1")
	assertStep(t, rs, 1, outcome.StepStatusSkipped, kindB, "t2")
}

// ---------- Auto-skip ----------

func TestAutoSkipAfterDoFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
		Do("second", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			t.Fatal("should not be called")
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 2)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindA, "t1")
	assertStep(t, rs, 1, outcome.StepStatusSkipped, kindB, "t2")
}

func TestAutoSkipValidateFailsSkipsDoAndTry(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Validate("check", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
		Do("action", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			t.Fatal("should not be called")
			return nil
		}),
		Try("try", kindC, "t3", func(_ context.Context, _ *Bindings) error {
			t.Fatal("should not be called")
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 3)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindA, "t1")
	assertStep(t, rs, 1, outcome.StepStatusSkipped, kindB, "t2")
	assertStep(t, rs, 2, outcome.StepStatusSkipped, kindC, "t3")
}

func TestAutoSkipProduceFailsSkipsDo(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[int]("val")
	plan := Build(
		Produce(key, kindA, "t1", func(_ context.Context, _ *Bindings) (int, error) {
			return 0, errTest
		}),
		Do("action", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			t.Fatal("should not be called")
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertTimelineLen(t, rs, 2)
	assertStep(t, rs, 0, outcome.StepStatusFailed, kindA, "t1")
	assertStep(t, rs, 1, outcome.StepStatusSkipped, kindB, "t2")
}

// ---------- Bindings ----------

func TestProduceStoresBindingForLaterDo(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[string]("msg")
	var captured string
	plan := Build(
		Produce(key, kindA, "t1", func(_ context.Context, _ *Bindings) (string, error) {
			return "hello", nil
		}),
		Do("use", kindB, "t2", func(_ context.Context, b *Bindings) error {
			captured = Get(b, key)
			return nil
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured != "hello" {
		t.Fatalf("expected hello, got %q", captured)
	}
}

func TestGetMissingKeyPanics(t *testing.T) {
	b := newBindings()
	key := NewKey[int]("missing")
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T", r)
		}
		if msg != `operation.Get: key "missing" not bound` {
			t.Fatalf("unexpected panic message: %s", msg)
		}
	}()
	Get(b, key)
}

func TestMultipleProduceBindings(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	keyA := NewKey[int]("a")
	keyB := NewKey[string]("b")
	plan := Build(
		Produce(keyA, kindA, "t1", func(_ context.Context, _ *Bindings) (int, error) {
			return 1, nil
		}),
		Produce(keyB, kindB, "t2", func(_ context.Context, _ *Bindings) (string, error) {
			return "two", nil
		}),
	)

	b, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v := Get(b, keyA); v != 1 {
		t.Fatalf("expected 1, got %d", v)
	}
	if v := Get(b, keyB); v != "two" {
		t.Fatalf("expected two, got %q", v)
	}
}

func TestDetailsFnReadsEarlierBindings(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[int]("val")
	plan := Build(
		Produce(key, kindA, "t1", func(_ context.Context, _ *Bindings) (int, error) {
			return 99, nil
		}),
		Do("use", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return nil
		}, DetailsFn(func(b *Bindings) any {
			return map[string]int{"val": Get(b, key)}
		})),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTimelineLen(t, rs, 2)
	entry := rs.Outcome.Timeline[1]
	m, ok := entry.Details.(map[string]int)
	if !ok {
		t.Fatalf("expected map[string]int, got %T", entry.Details)
	}
	if m["val"] != 99 {
		t.Fatalf("expected val=99, got %d", m["val"])
	}
}

// ---------- Undo registration ----------

func TestDoWithUndoOnSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	// Set up: Do succeeds, then a subsequent Do fails to trigger rollback.
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	// The undo action should have been executed during rollback.
	if len(exec.executed) != 1 || exec.executed[0] != "undo-a" {
		t.Fatalf("expected undo-a executed, got %v", exec.executed)
	}
}

func TestDoWithUndoOnFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	// Undo should NOT have been accumulated (node failed).
	if len(exec.executed) != 0 {
		t.Fatalf("expected no undo execution, got %v", exec.executed)
	}
}

func TestProduceWithUndoFromOnSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[string]("val")
	plan := Build(
		Produce(key, kindA, "t1", func(_ context.Context, _ *Bindings) (string, error) {
			return "produced", nil
		}, UndoFrom(func(b *Bindings) []UndoEntry {
			v := Get(b, key)
			return []UndoEntry{{
				Action:   testAction("undo-" + v),
				Step:     outcome.NewStep(kindA, "t1", nil),
				Severity: SeverityError,
			}}
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	if len(exec.executed) != 1 || exec.executed[0] != "undo-produced" {
		t.Fatalf("expected undo-produced executed, got %v", exec.executed)
	}
}

func TestProduceWithUndoFromOnFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[string]("val")
	plan := Build(
		Produce(key, kindA, "t1", func(_ context.Context, _ *Bindings) (string, error) {
			return "", errTest
		}, UndoFrom(func(_ *Bindings) []UndoEntry {
			t.Fatal("UndoFrom should not be called on failure")
			return nil
		})),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	if len(exec.executed) != 0 {
		t.Fatalf("expected no undo execution, got %v", exec.executed)
	}
}

func TestValidateNeverAccumulatesUndo(t *testing.T) {
	// Validate nodes have no undo options. This test verifies that
	// even if a Validate succeeds before a failure, no rollback
	// actions are accumulated.
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Validate("check", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	if len(exec.executed) != 0 {
		t.Fatalf("expected no undo execution, got %v", exec.executed)
	}
}

func TestTryNeverAccumulatesUndo(t *testing.T) {
	// Try nodes have no undo. Even if they succeed, no undo entries
	// are accumulated.
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Try("try", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	if len(exec.executed) != 0 {
		t.Fatalf("expected no undo execution, got %v", exec.executed)
	}
}

// ---------- Rollback ----------

func TestRollbackSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)

	// Find the rollback entry in the timeline.
	var found bool
	for _, e := range rs.Outcome.Timeline {
		if e.Phase == outcome.PhaseRollback && e.Status == outcome.StepStatusCompleted {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a completed rollback step in timeline")
	}
}

func TestRollbackFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	exec.failOn("undo-a", errors.New("undo failed"))
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)

	var found bool
	for _, e := range rs.Outcome.Timeline {
		if e.Phase == outcome.PhaseRollback && e.Status == outcome.StepStatusFailed {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a failed rollback step in timeline")
	}
}

func TestRollbackReversedOrder(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("second", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-b"),
			Step:     outcome.NewStep(kindB, "t2", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindC, "t3", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)

	if len(exec.executed) != 2 {
		t.Fatalf("expected 2 undo executions, got %d", len(exec.executed))
	}
	if exec.executed[0] != "undo-b" || exec.executed[1] != "undo-a" {
		t.Fatalf("expected reversed order [undo-b, undo-a], got %v", exec.executed)
	}
}

func TestRollbackAllAttempted(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	exec.failOn("undo-b", errors.New("undo-b failed"))
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("second", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-b"),
			Step:     outcome.NewStep(kindB, "t2", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindC, "t3", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)

	// Both entries should have been attempted even though undo-b fails.
	if len(exec.executed) != 2 {
		t.Fatalf("expected 2 undo executions, got %d", len(exec.executed))
	}
	if exec.executed[0] != "undo-b" || exec.executed[1] != "undo-a" {
		t.Fatalf("expected [undo-b, undo-a], got %v", exec.executed)
	}
}

func TestRollbackSeverityErrorLogging(t *testing.T) {
	// This test verifies that SeverityError rollback failures result
	// in a failed rollback timeline entry. The actual log level is
	// tested implicitly through the slog infrastructure.
	var rs *RunState
	exec := newFakeExecutor()
	exec.failOn("undo-a", errors.New("undo failed"))
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertRollbackHasFailedStep(t, rs)
}

func TestRollbackSeverityWarningLogging(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	exec.failOn("undo-a", errors.New("undo failed"))
	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityWarning,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
	assertRollbackHasFailedStep(t, rs)
}

func TestResidualProbeCalledOnRollbackFailure(t *testing.T) {
	exec := newFakeExecutor()
	exec.failOn("undo-a", errors.New("undo failed"))
	probeCalled := false

	begin := func(_ context.Context) *RunState {
		o := &outcome.OperationOutcome{}
		rec := outcome.NewRecorder(o, func(err error) {
			panic(fmt.Sprintf("recorder invariant violation: %v", err))
		})
		return &RunState{
			Outcome: o,
			Rec:     rec,
			Logger:  slog.Default(),
			Probe: func(_ context.Context) ([]outcome.Artefact, error) {
				probeCalled = true
				return []outcome.Artefact{{Kind: outcome.ArtefactProgramPin, Path: "/test"}}, nil
			},
		}
	}

	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	Run(context.Background(), begin, exec, plan)
	if !probeCalled {
		t.Fatal("expected residual probe to be called")
	}
}

func TestResidualProbeNotCalledOnRollbackSuccess(t *testing.T) {
	exec := newFakeExecutor()
	probeCalled := false

	begin := func(_ context.Context) *RunState {
		o := &outcome.OperationOutcome{}
		rec := outcome.NewRecorder(o, func(err error) {
			panic(fmt.Sprintf("recorder invariant violation: %v", err))
		})
		return &RunState{
			Outcome: o,
			Rec:     rec,
			Logger:  slog.Default(),
			Probe: func(_ context.Context) ([]outcome.Artefact, error) {
				probeCalled = true
				return nil, nil
			},
		}
	}

	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	Run(context.Background(), begin, exec, plan)
	if probeCalled {
		t.Fatal("residual probe should not be called when rollback succeeds")
	}
}

func TestResidualProbeNotCalledWhenNil(t *testing.T) {
	exec := newFakeExecutor()
	exec.failOn("undo-a", errors.New("undo failed"))

	begin := func(_ context.Context) *RunState {
		o := &outcome.OperationOutcome{}
		rec := outcome.NewRecorder(o, func(err error) {
			panic(fmt.Sprintf("recorder invariant violation: %v", err))
		})
		return &RunState{
			Outcome: o,
			Rec:     rec,
			Logger:  slog.Default(),
			// Probe is nil
		}
	}

	plan := Build(
		Do("first", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}, WithUndo(UndoEntry{
			Action:   testAction("undo-a"),
			Step:     outcome.NewStep(kindA, "t1", nil),
			Severity: SeverityError,
		})),
		Do("fail", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	// Should not panic with nil probe.
	Run(context.Background(), begin, exec, plan)
}

func TestNoRollbackEntriesNoBeginRollback(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)

	// There should be no rollback entries in the timeline.
	for _, e := range rs.Outcome.Timeline {
		if e.Phase == outcome.PhaseRollback {
			t.Fatalf("unexpected rollback entry: %+v", e)
		}
	}
}

// ---------- Run / Run0 ----------

func TestRunReturnsBindingsOnSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	key := NewKey[int]("val")
	plan := Build(
		Produce(key, kindTest, "t1", func(_ context.Context, _ *Bindings) (int, error) {
			return 7, nil
		}),
	)

	b, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil bindings")
	}
	if v := Get(b, key); v != 7 {
		t.Fatalf("expected 7, got %d", v)
	}
}

func TestRunReturnsOperationErrorOnFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
}

func TestRun0ReturnsNilOnSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("action", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return nil
		}),
	)

	err := Run0(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun0ReturnsOperationErrorOnFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	err := Run0(context.Background(), testBegin(&rs), exec, plan)
	assertOperationError(t, err)
}

func TestOperationErrorUnwrapReturnsCause(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	opErr, ok := AsOperationError(err)
	if !ok {
		t.Fatal("expected *OperationError")
	}
	if opErr.Unwrap() == nil {
		t.Fatal("expected non-nil cause")
	}
}

func TestOutcomePrimaryErrorSetOnFailure(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	opErr, ok := AsOperationError(err)
	if !ok {
		t.Fatal("expected *OperationError")
	}
	if opErr.Outcome.PrimaryError == "" {
		t.Fatal("expected PrimaryError to be set")
	}
}

func TestOutcomeStatusIsFailureAfterFailStep(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Do("fail", kindTest, "t1", func(_ context.Context, _ *Bindings) error {
			return errTest
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	opErr, ok := AsOperationError(err)
	if !ok {
		t.Fatal("expected *OperationError")
	}
	if opErr.Outcome.Status != outcome.StatusFailure {
		t.Fatalf("expected failure status, got %q", opErr.Outcome.Status)
	}
}

// ---------- Edge cases ----------

func TestEmptyPlanSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build()

	b, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil bindings")
	}
	assertTimelineLen(t, rs, 0)
}

func TestAllTryNodesFailIsSuccess(t *testing.T) {
	var rs *RunState
	exec := newFakeExecutor()
	plan := Build(
		Try("try1", kindA, "t1", func(_ context.Context, _ *Bindings) error {
			return errors.New("warn 1")
		}),
		Try("try2", kindB, "t2", func(_ context.Context, _ *Bindings) error {
			return errors.New("warn 2")
		}),
	)

	_, err := Run(context.Background(), testBegin(&rs), exec, plan)
	if err != nil {
		t.Fatalf("unexpected error: Try failures should not fail the operation")
	}
	assertTimelineLen(t, rs, 2)
	assertStep(t, rs, 0, outcome.StepStatusWarned, kindA, "t1")
	assertStep(t, rs, 1, outcome.StepStatusWarned, kindB, "t2")
}

// ---------- Helpers ----------

func assertTimelineLen(t *testing.T, rs *RunState, n int) {
	t.Helper()
	if got := len(rs.Outcome.Timeline); got != n {
		t.Fatalf("expected %d timeline entries, got %d: %+v", n, got, rs.Outcome.Timeline)
	}
}

func assertStep(t *testing.T, rs *RunState, idx int, status outcome.StepStatus, kind outcome.StepKind, target string) {
	t.Helper()
	if idx >= len(rs.Outcome.Timeline) {
		t.Fatalf("timeline index %d out of range (len=%d)", idx, len(rs.Outcome.Timeline))
	}
	e := rs.Outcome.Timeline[idx]
	if e.Status != status {
		t.Fatalf("timeline[%d]: expected status %q, got %q", idx, status, e.Status)
	}
	if e.Kind != kind {
		t.Fatalf("timeline[%d]: expected kind %q, got %q", idx, kind, e.Kind)
	}
	if e.Target != target {
		t.Fatalf("timeline[%d]: expected target %q, got %q", idx, target, e.Target)
	}
}

func assertOperationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var opErr *OperationError
	if !errors.As(err, &opErr) {
		t.Fatalf("expected *OperationError, got %T: %v", err, err)
	}
}

func assertRollbackHasFailedStep(t *testing.T, rs *RunState) {
	t.Helper()
	for _, e := range rs.Outcome.Timeline {
		if e.Phase == outcome.PhaseRollback && e.Status == outcome.StepStatusFailed {
			return
		}
	}
	t.Fatal("expected a failed rollback step in timeline")
}
