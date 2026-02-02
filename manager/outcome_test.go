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
	assert.Empty(t, result.Outcome.Error)

	// Verify completed steps
	assert.Len(t, result.Outcome.Completed, 2, "expected 2 completed steps: kernel.load_program and store.save_program")

	// First step should be kernel load
	assert.Equal(t, outcome.StepKindKernelLoad, result.Outcome.Completed[0].Kind)
	assert.Equal(t, "test_prog", result.Outcome.Completed[0].Target)

	// Second step should be store save
	assert.Equal(t, outcome.StepKindStoreSaveProgram, result.Outcome.Completed[1].Kind)

	// No failed or skipped steps
	assert.Nil(t, result.Outcome.Failed)
	assert.Empty(t, result.Outcome.Skipped)
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
	unloadResult, err := fix.Manager.Unload(ctx, loadResult.Program.Kernel.ID)
	require.NoError(t, err)

	// Verify outcome indicates success
	assert.Equal(t, outcome.StatusSuccess, unloadResult.Outcome.Status)
	assert.Empty(t, unloadResult.Outcome.Error)

	// Verify completed steps include unload operations
	assert.NotEmpty(t, unloadResult.Outcome.Completed)

	// No failed or skipped steps on success
	assert.Nil(t, unloadResult.Outcome.Failed)
	assert.Empty(t, unloadResult.Outcome.Skipped)
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
	assert.NotEmpty(t, result.Outcome.Error)

	// Verify failed step is recorded
	require.NotNil(t, result.Outcome.Failed)
	assert.Equal(t, outcome.StepKindPreflight, result.Outcome.Failed.Kind)
	assert.NotEmpty(t, result.Outcome.Failed.Error)

	// System state should be clean (no residue on preflight failure)
	assert.Equal(t, "clean", result.Outcome.SystemState())
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
	assert.NotEmpty(t, result.Outcome.Error)

	// Verify failed step is recorded
	require.NotNil(t, result.Outcome.Failed)
	assert.Equal(t, outcome.StepKindPreflight, result.Outcome.Failed.Kind)

	// System state should be clean (no residue on preflight failure)
	assert.Equal(t, "clean", result.Outcome.SystemState())
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
	attachSpec, err := bpfman.NewTracepointAttachSpec(loadResult.Program.Kernel.ID, "sched", "sched_switch")
	require.NoError(t, err)

	attachResult, err := fix.Manager.AttachTracepoint(ctx, attachSpec, bpfman.AttachOpts{})
	require.NoError(t, err)

	// Verify outcome indicates success
	assert.Equal(t, outcome.StatusSuccess, attachResult.Outcome.Status)
	assert.Empty(t, attachResult.Outcome.Error)

	// Verify completed steps
	assert.Len(t, attachResult.Outcome.Completed, 2)
	assert.Equal(t, outcome.StepKindAttachTracepoint, attachResult.Outcome.Completed[0].Kind)
	assert.Equal(t, outcome.StepKindStoreSaveLink, attachResult.Outcome.Completed[1].Kind)
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
	assert.Empty(t, result.Outcome.Error)

	// Verify Phase 1 steps (store GC)
	require.GreaterOrEqual(t, len(result.Outcome.Completed), 3, "expected at least 3 store GC steps")

	// Check for store GC step kinds
	stepKinds := make(map[outcome.StepKind]bool)
	for _, step := range result.Outcome.Completed {
		stepKinds[step.Kind] = true
	}
	assert.True(t, stepKinds[outcome.StepKindStoreGCPrograms], "missing store.gc_programs step")
	assert.True(t, stepKinds[outcome.StepKindStoreGCLinks], "missing store.gc_links step")
	assert.True(t, stepKinds[outcome.StepKindStoreGCDispatchers], "missing store.gc_dispatchers step")
}

// TestOutcome_SystemStateReflectsActualState verifies that the outcome's
// SystemState() method correctly reflects the actual system state after
// an operation.
func TestOutcome_SystemStateReflectsActualState(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load and then unload a program
	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	loadResult, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	unloadResult, err := fix.Manager.Unload(ctx, loadResult.Program.Kernel.ID)
	require.NoError(t, err)

	// SystemState should be clean
	assert.Equal(t, "clean", unloadResult.Outcome.SystemState())

	// Verify actual state matches reported state
	fix.AssertCleanState()
}

// TestOutcome_NeedsManualCleanupReturnsFalseOnSuccess verifies that
// NeedsManualCleanup() returns false for successful operations.
func TestOutcome_NeedsManualCleanupReturnsFalseOnSuccess(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec("/fake/path.o", "test_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)

	result, err := fix.Manager.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	assert.False(t, result.Outcome.NeedsManualCleanup())
}

// TestOutcome_Started verifies that the Started() method correctly
// reports whether any steps were attempted.
func TestOutcome_Started(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// An operation that fails in preflight should still show as started
	result, err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err)

	assert.True(t, result.Outcome.Started(), "preflight failure should count as started")
}
