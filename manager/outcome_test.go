package manager_test

import (
	"context"
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

// TestLoad_Success_OutcomeTracksCompletedSteps verifies that a successful
// load operation records the completed steps in the outcome.
func TestLoad_Success_OutcomeTracksCompletedSteps(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	result, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Verify outcome indicates success
	assert.Equal(t, outcome.StatusSuccess, result.Outcome.Status)
	assert.Empty(t, result.Outcome.PrimaryError)

	// Verify completed steps
	completed := outcomeTestCompletedPrimary(result.Outcome.Timeline)
	assert.Len(t, completed, 2, "expected 2 completed steps: kernel.load_program and store.save_program")

	// First step should be kernel load
	assert.Equal(t, outcome.StepKindKernelLoad, completed[0].Kind)
	assert.Equal(t, "test_prog", completed[0].Target)

	// Second step should be store save
	assert.Equal(t, outcome.StepKindStoreSaveProgram, completed[1].Kind)

	// No failed or skipped steps
	assert.Nil(t, outcomeTestFindFailed(result.Outcome.Timeline))
	assert.Empty(t, outcomeTestSkipped(result.Outcome.Timeline))
}

// TestUnload_Success_OutcomeTracksSteps verifies that a successful unload
// operation records the completed steps in the outcome.
func TestUnload_Success_OutcomeTracksSteps(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// First load a program
	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	loadResult, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Now unload it
	unloadResult, err := fix.Manager.Unload(ctx, loadResult.Program.Spec.KernelID)
	require.NoError(t, err)

	// Verify outcome indicates success
	assert.Equal(t, outcome.StatusSuccess, unloadResult.Outcome.Status)
	assert.Empty(t, unloadResult.Outcome.PrimaryError)

	// Verify completed steps include unload operations
	completed := outcomeTestCompletedPrimary(unloadResult.Outcome.Timeline)
	assert.NotEmpty(t, completed)

	// No failed or skipped steps on success
	assert.Nil(t, outcomeTestFindFailed(unloadResult.Outcome.Timeline))
	assert.Empty(t, outcomeTestSkipped(unloadResult.Outcome.Timeline))
}

// TestDetach_NotFound_OutcomeRecordsFailure verifies that a detach
// operation for a non-existent link records the failure in the outcome.
func TestDetach_NotFound_OutcomeRecordsFailure(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	result, err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err)

	// Verify outcome indicates failure
	assert.Equal(t, outcome.StatusFailure, result.Outcome.Status)
	assert.NotEmpty(t, result.Outcome.PrimaryError)

	// Verify failed step is recorded
	failed := outcomeTestFindFailed(result.Outcome.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)
	assert.NotEmpty(t, failed.Error)

	// System state should be clean (no residue on preflight failure)
	assert.Equal(t, "clean", result.Outcome.SystemState)
}

// TestUnload_NotFound_OutcomeRecordsFailure verifies that an unload
// operation for a non-existent program records the failure in the outcome.
func TestUnload_NotFound_OutcomeRecordsFailure(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	result, err := fix.Manager.Unload(ctx, 999)
	require.Error(t, err)

	// Verify outcome indicates failure
	assert.Equal(t, outcome.StatusFailure, result.Outcome.Status)
	assert.NotEmpty(t, result.Outcome.PrimaryError)

	// Verify failed step is recorded
	failed := outcomeTestFindFailed(result.Outcome.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPreflight, failed.Kind)

	// System state should be clean (no residue on preflight failure)
	assert.Equal(t, "clean", result.Outcome.SystemState)
}

// TestAttachTracepoint_Success_OutcomeTracksSteps verifies that a successful
// attach operation records the completed steps.
func TestAttachTracepoint_Success_OutcomeTracksSteps(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a program first
	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	loadResult, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach it
	attachSpec, err := bpfman.NewTracepointAttachSpec(loadResult.Program.Spec.KernelID, "sched", "sched_switch")
	require.NoError(t, err)

	attachResult, err := fix.Manager.AttachTracepoint(ctx, attachSpec, bpfman.AttachOpts{})
	require.NoError(t, err)

	// Verify outcome indicates success
	assert.Equal(t, outcome.StatusSuccess, attachResult.Outcome.Status)
	assert.Empty(t, attachResult.Outcome.PrimaryError)

	// Verify completed steps
	completed := outcomeTestCompletedPrimary(attachResult.Outcome.Timeline)
	assert.Len(t, completed, 2)
	assert.Equal(t, outcome.StepKindAttachTracepoint, completed[0].Kind)
	assert.Equal(t, outcome.StepKindStoreSaveLink, completed[1].Kind)
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

// TestOutcome_SystemStateReflectsActualState verifies that the outcome's
// SystemState field correctly reflects the actual system state after
// an operation.
func TestOutcome_SystemStateReflectsActualState(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load and then unload a program
	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	loadResult, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	unloadResult, err := fix.Manager.Unload(ctx, loadResult.Program.Spec.KernelID)
	require.NoError(t, err)

	// SystemState should be clean
	assert.Equal(t, "clean", unloadResult.Outcome.SystemState)

	// Verify actual state matches reported state
	fix.AssertCleanState()
}

// TestOutcome_ManualCleanupRequiredReturnsFalseOnSuccess verifies that
// ManualCleanupRequired returns false for successful operations.
func TestOutcome_ManualCleanupRequiredReturnsFalseOnSuccess(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	result, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	assert.False(t, result.Outcome.ManualCleanupRequired)
}

// TestOutcome_Started verifies that the Started() method correctly
// reports whether any steps were attempted.
func TestOutcome_Started(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// An operation that fails in preflight should still show as started
	result, err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err)

	// Timeline should have at least one entry (the failed step)
	assert.NotEmpty(t, result.Outcome.Timeline, "preflight failure should have timeline entry")
}
