package manager_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager"
)

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

// TestDetach_NotFound_ReturnsPlainError verifies that a detach
// operation for a non-existent link returns a plain error because
// preflight failures bypass plan execution.
func TestDetach_NotFound_ReturnsPlainError(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	err := fix.Manager.Detach(ctx, bpfman.LinkID(999))
	require.Error(t, err)

	var notFound bpfman.ErrLinkNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrLinkNotFound, got %T", err)
}

// TestUnload_NotFound_ReturnsPlainError verifies that an unload
// operation for a non-existent program returns a plain error because
// preflight failures bypass plan execution.
func TestUnload_NotFound_ReturnsPlainError(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	err := fix.Manager.Unload(ctx, 999)
	require.Error(t, err)

	var notFound bpfman.ErrProgramNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrProgramNotFound, got %T", err)
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

	link, err := fix.Manager.Attach(ctx, nil, attachSpec)
	require.NoError(t, err)

	// Verify link was created
	assert.NotZero(t, link.Record.ID)
	assert.Equal(t, prog.Record.KernelID, link.Record.ProgramID)
}

// TestGC_Success_OutcomeTracksPhases verifies that GC completes
// successfully and reports correct statistics.
func TestGC_Success_OutcomeTracksPhases(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	result, err := fix.Manager.GC(ctx)
	require.NoError(t, err)

	// On an empty manager, GC should remove nothing.
	assert.Equal(t, 0, result.ProgramsRemoved)
	assert.Equal(t, 0, result.DispatchersRemoved)
	assert.Equal(t, 0, result.LinksRemoved)
	assert.Equal(t, 0, result.OrphanPinsRemoved)
	assert.Equal(t, 0, result.LiveOrphans)
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

// TestOutcome_ExecutionFailure_HasTimeline verifies that an operation
// that fails during plan execution produces a useful error.
func TestOutcome_ExecutionFailure_HasTimeline(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a program and attach it.
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), "tp_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTracepointAttachSpec(prog.Record.KernelID, "syscalls", "sys_enter_close")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, nil, attachSpec)
	require.NoError(t, err)

	// Inject a kernel failure on detach so the plan fails mid-execution.
	fix.Kernel.FailOnDetach(uint32(link.Record.ID), fmt.Errorf("injected failure"))

	err = fix.Manager.Detach(ctx, link.Record.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected failure")
}
