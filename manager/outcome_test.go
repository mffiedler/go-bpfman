package manager_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/outcome"
)

// outcomeTestCompletedPrimary returns completed entries in the primary phase.
func outcomeTestCompletedPrimary(timeline []outcome.TimelineEntry) []outcome.TimelineEntry {
	var result []outcome.TimelineEntry
	for _, e := range timeline {
		if e.Phase == outcome.PhasePrimary && e.Status == outcome.StepStatusCompleted {
			result = append(result, e)
		}
	}
	return result
}

// outcomeTestFindFailed returns the first failed entry from the timeline, or nil if none.
func outcomeTestFindFailed(timeline []outcome.TimelineEntry) *outcome.TimelineEntry {
	for i := range timeline {
		if timeline[i].Status == outcome.StepStatusFailed {
			return &timeline[i]
		}
	}
	return nil
}

// outcomeTestSkipped returns skipped entries from the timeline.
func outcomeTestSkipped(timeline []outcome.TimelineEntry) []outcome.TimelineEntry {
	var result []outcome.TimelineEntry
	for _, e := range timeline {
		if e.Status == outcome.StepStatusSkipped {
			result = append(result, e)
		}
	}
	return result
}

// extractManagerError extracts the ManagerError from an error using errors.As.
func extractManagerError(t *testing.T, err error) *manager.ManagerError {
	t.Helper()
	var me *manager.ManagerError
	require.True(t, errors.As(err, &me), "expected error to be *manager.ManagerError, got %T", err)
	return me
}

// TestLoad_Success verifies that a successful load operation completes
// without error and returns the program.
func TestLoad_Success(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("path.o"), "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Verify program was loaded
	assert.NotZero(t, prog.Record.KernelID)
	assert.Equal(t, "test_prog", prog.Record.Meta.Name)
}

// TestUnload_Success verifies that a successful unload operation
// completes without error.
func TestUnload_Success(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// First load a program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("path.o"), "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Now unload it
	err = fix.Manager.Unload(ctx, prog.Record.KernelID)
	require.NoError(t, err)

	// Verify state is clean
	fix.AssertCleanState()
}

// TestDetach_NotFound_OutcomeRecordsFailure verifies that a detach
// operation for a non-existent link records the failure in the outcome.
func TestDetach_NotFound_OutcomeRecordsFailure(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err)

	// Extract outcome from error
	me := extractManagerError(t, err)
	o := me.Outcome

	// Verify outcome indicates failure
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)

	// Verify failed step is recorded
	failed := outcomeTestFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)
	assert.NotEmpty(t, failed.Error)

	// System state should be clean (no residue on preflight failure)
	assert.Equal(t, "clean", o.SystemState)
}

// TestUnload_NotFound_OutcomeRecordsFailure verifies that an unload
// operation for a non-existent program records the failure in the outcome.
func TestUnload_NotFound_OutcomeRecordsFailure(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	err := fix.Manager.Unload(ctx, 999)
	require.Error(t, err)

	// Extract outcome from error
	me := extractManagerError(t, err)
	o := me.Outcome

	// Verify outcome indicates failure
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)

	// Verify failed step is recorded
	failed := outcomeTestFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)

	// System state should be clean (no residue on preflight failure)
	assert.Equal(t, "clean", o.SystemState)
}

// TestAttachTracepoint_Success verifies that a successful attach operation
// completes without error and returns the link.
func TestAttachTracepoint_Success(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a program first
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("path.o"), "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach it
	attachSpec, err := bpfman.NewTracepointAttachSpec(prog.Record.KernelID, "sched", "sched_switch")
	require.NoError(t, err)

	link, err := fix.Manager.Attach(ctx, nil, attachSpec, bpfman.AttachOpts{})
	require.NoError(t, err)

	// Verify link was created
	assert.NotZero(t, link.Record.ID)
	assert.Equal(t, prog.Record.KernelID, link.Record.ProgramID)
}

// TestGC_Success_OutcomeTracksPhases verifies that GC records both
// store GC and rule engine phases.
func TestGC_Success_OutcomeTracksPhases(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	result, err := fix.Manager.GC(ctx)
	require.NoError(t, err)

	// Verify outcome indicates success
	assert.Equal(t, outcome.StatusSuccess, result.Outcome.Status)
	assert.Empty(t, result.Outcome.PrimaryError)

	// Verify Phase 1 steps (store GC)
	completed := outcomeTestCompletedPrimary(result.Outcome.Timeline)
	require.GreaterOrEqual(t, len(completed), 3, "expected at least 3 store GC steps")

	// Check for store GC step kinds
	stepKinds := make(map[outcome.StepKind]bool)
	for _, step := range completed {
		stepKinds[step.Kind] = true
	}
	assert.True(t, stepKinds[outcome.StepKindStoreGCPrograms], "missing store.gc_programs step")
	assert.True(t, stepKinds[outcome.StepKindStoreGCLinks], "missing store.gc_links step")
	assert.True(t, stepKinds[outcome.StepKindStoreGCDispatchers], "missing store.gc_dispatchers step")
}

// TestOutcome_SystemStateReflectsActualState verifies that after an unload
// the system state is clean.
func TestOutcome_SystemStateReflectsActualState(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load and then unload a program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("path.o"), "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	err = fix.Manager.Unload(ctx, prog.Record.KernelID)
	require.NoError(t, err)

	// Verify actual state is clean
	fix.AssertCleanState()
}

// TestOutcome_Started verifies that an operation that fails in preflight
// produces an error with timeline entries.
func TestOutcome_Started(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// An operation that fails in preflight should have timeline entries
	err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err)

	// Extract outcome from error
	me := extractManagerError(t, err)

	// Timeline should have at least one entry (the failed step)
	assert.NotEmpty(t, me.Outcome.Timeline, "preflight failure should have timeline entry")
}
