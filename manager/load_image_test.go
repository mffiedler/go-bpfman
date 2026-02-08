package manager_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/outcome"
)

// extractLoadImageOutcome extracts the outcome from a LoadImage error.
func extractLoadImageOutcome(t *testing.T, err error) outcome.OperationOutcome {
	t.Helper()
	var me *manager.ManagerError
	require.True(t, errors.As(err, &me), "expected *manager.ManagerError, got %T", err)
	return me.Outcome
}

// timelineFindFailed returns the first failed entry from the timeline, or nil if none.
func timelineFindFailed(timeline []outcome.TimelineEntry) *outcome.TimelineEntry {
	for i := range timeline {
		if timeline[i].Status == outcome.StepStatusFailed {
			return &timeline[i]
		}
	}
	return nil
}

// timelineCompletedPrimary returns completed entries in the primary phase.
func timelineCompletedPrimary(timeline []outcome.TimelineEntry) []outcome.TimelineEntry {
	var result []outcome.TimelineEntry
	for _, e := range timeline {
		if e.Phase == outcome.PhasePrimary && e.Status == outcome.StepStatusCompleted {
			result = append(result, e)
		}
	}
	return result
}

// timelineSkipped returns skipped entries from the timeline.
func timelineSkipped(timeline []outcome.TimelineEntry) []outcome.TimelineEntry {
	var result []outcome.TimelineEntry
	for _, e := range timeline {
		if e.Status == outcome.StepStatusSkipped {
			result = append(result, e)
		}
	}
	return result
}

// timelineRollbackCompleted returns completed entries in the rollback phase.
func timelineRollbackCompleted(timeline []outcome.TimelineEntry) []outcome.TimelineEntry {
	var result []outcome.TimelineEntry
	for _, e := range timeline {
		if e.Phase == outcome.PhaseRollback && e.Status == outcome.StepStatusCompleted {
			result = append(result, e)
		}
	}
	return result
}

// timelineHasRollback returns true if there are any rollback phase entries.
func timelineHasRollback(timeline []outcome.TimelineEntry) bool {
	for _, e := range timeline {
		if e.Phase == outcome.PhaseRollback {
			return true
		}
	}
	return false
}

// timelineKinds returns the kinds of all entries matching the filter.
func timelineKinds(entries []outcome.TimelineEntry) []outcome.StepKind {
	kinds := make([]outcome.StepKind, len(entries))
	for i, e := range entries {
		kinds[i] = e.Kind
	}
	return kinds
}

func TestLoadImage_AutoDiscover_SingleProgram(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "test_prog", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "test_prog", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
	// Outcome is not accessible on success - absence of error implies success
}

func TestLoadImage_AutoDiscover_MultiplePrograms(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 3)
	assert.Equal(t, 3, f.Kernel.ProgramCount())

	// Verify programs are loaded in sorted order
	assert.Equal(t, "prog_a", programs[0].Record.Meta.Name)
	assert.Equal(t, "prog_b", programs[1].Record.Meta.Name)
	assert.Equal(t, "prog_c", programs[2].Record.Meta.Name)
}

func TestLoadImage_AutoDiscover_NoPrograms(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	// Don't set any programs - empty object file

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no programs found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoadImage_ExplicitPrograms_Valid(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Request only prog_b
	programs, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, []manager.ImageProgramSpec{
		{ProgramName: "prog_b", ProgramType: bpfman.ProgramTypeXDP},
	}, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "prog_b", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}

func TestLoadImage_ExplicitPrograms_InvalidName(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Request non-existent program
	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, []manager.ImageProgramSpec{
		{ProgramName: "nonexistent", ProgramType: bpfman.ProgramTypeXDP},
	}, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoadImage_Rollback_SecondProgramFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Make second program fail to load
	f.Kernel.FailOnProgram("prog_b", fmt.Errorf("injected load failure"))

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_b")

	// Verify rollback: prog_a should be unloaded from both kernel and database
	f.AssertCleanState()

	// Verify outcome structure on failure
	o := extractLoadImageOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "prog_b", failed.Target)
	assert.NotEmpty(t, failed.Error)

	// Should have completed: image.pull, image.discover, kernel.load(prog_a),
	// fs.publish(prog_a), store.save(prog_a)
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 5)

	// No programs skipped (only 2 programs, first succeeded, second failed)
	assert.Empty(t, timelineSkipped(o.Timeline))

	// Verify cleanup was recorded
	rollbackCompleted := timelineRollbackCompleted(o.Timeline)
	assert.True(t, timelineHasRollback(o.Timeline))
	assert.Len(t, rollbackCompleted, 1)
	assert.Equal(t, outcome.StepKindKernelUnload, rollbackCompleted[0].Kind)
	assert.Equal(t, "prog_a", rollbackCompleted[0].Target)

	// SystemState should be clean after successful cleanup
	assert.Equal(t, "clean", o.SystemState)

	// Verify the operation sequence
	ops := f.Kernel.Operations()
	// Should see: load prog_a (ok), load prog_b (error), unload prog_a (rollback)
	assert.GreaterOrEqual(t, len(ops), 3)

	// Find the operations
	var loadA, loadB, unloadA bool
	for _, op := range ops {
		if op.Op == "load" && op.Name == "prog_a" && op.Err == nil {
			loadA = true
		}
		if op.Op == "load" && op.Name == "prog_b" && op.Err != nil {
			loadB = true
		}
		if op.Op == "unload" && op.Name == "prog_a" {
			unloadA = true
		}
	}
	assert.True(t, loadA, "expected prog_a to be loaded")
	assert.True(t, loadB, "expected prog_b load to fail")
	assert.True(t, unloadA, "expected prog_a to be unloaded during rollback")
}

func TestLoadImage_Rollback_ThirdProgramFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Make third program fail to load
	f.Kernel.FailOnProgram("prog_c", fmt.Errorf("injected load failure"))

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_c")

	// Verify outcome structure
	o := extractLoadImageOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "prog_c", failed.Target)

	// Should have completed: image.pull, image.discover,
	// kernel.load(prog_a), fs.publish(prog_a), store.save(prog_a),
	// kernel.load(prog_b), fs.publish(prog_b), store.save(prog_b)
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 8)

	// Verify cleanup was recorded - prog_a and prog_b should be rolled back
	rollbackCompleted := timelineRollbackCompleted(o.Timeline)
	assert.True(t, timelineHasRollback(o.Timeline))
	assert.Len(t, rollbackCompleted, 2, "should have 2 cleanup steps for prog_a and prog_b")

	// SystemState should be clean after successful rollback
	assert.Equal(t, "clean", o.SystemState)
	assert.False(t, o.ManualCleanupRequired)

	// Verify rollback: prog_a and prog_b should be unloaded from both kernel and database
	f.AssertCleanState()
}

func TestLoadImage_PullError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	puller.SetPullError(fmt.Errorf("network error"))
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull image")
	assert.Equal(t, 0, f.Kernel.ProgramCount())

	// Verify outcome structure - early failure, no completed steps
	o := extractLoadImageOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindPullImage, failed.Kind)
	assert.Equal(t, "test.io/image:latest", failed.Target)
	assert.Empty(t, timelineCompletedPrimary(o.Timeline))
	assert.Empty(t, timelineSkipped(o.Timeline))
	assert.False(t, timelineHasRollback(o.Timeline))
}

func TestLoadImage_DiscoverError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetDiscoverError(fmt.Errorf("corrupt ELF file"))
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover programs")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoadImage_AutoDiscover_FentryFexit(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	programs, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 2)
	assert.Equal(t, 2, f.Kernel.ProgramCount())

	// Verify the programs were loaded (sorted by name)
	assert.Equal(t, "trace_vfs_read", programs[0].Record.Meta.Name)
	assert.Equal(t, "trace_vfs_write", programs[1].Record.Meta.Name)
}

func TestLoadImage_Rollback_FentryFexitSecondFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	// Make second program (fexit) fail to load
	f.Kernel.FailOnProgram("trace_vfs_write", fmt.Errorf("injected fexit load failure"))

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

	// Verify outcome structure
	o := extractLoadImageOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "trace_vfs_write", failed.Target)

	// Should have completed: image.pull, image.discover, kernel.load(fentry),
	// fs.publish(fentry), store.save(fentry)
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 5)

	// Verify cleanup was recorded - fentry should be rolled back
	rollbackCompleted := timelineRollbackCompleted(o.Timeline)
	assert.True(t, timelineHasRollback(o.Timeline))
	assert.Len(t, rollbackCompleted, 1)
	assert.Equal(t, outcome.StepKindKernelUnload, rollbackCompleted[0].Kind)
	assert.Equal(t, "trace_vfs_read", rollbackCompleted[0].Target)

	// SystemState should be clean after successful rollback
	assert.Equal(t, "clean", o.SystemState)

	// Verify rollback: fentry program should be unloaded from both kernel and database
	f.AssertCleanState()

	// Verify the operation sequence
	ops := f.Kernel.Operations()
	var loadFentry, loadFexit, unloadFentry bool
	for _, op := range ops {
		if op.Op == "load" && op.Name == "trace_vfs_read" && op.Err == nil {
			loadFentry = true
		}
		if op.Op == "load" && op.Name == "trace_vfs_write" && op.Err != nil {
			loadFexit = true
		}
		if op.Op == "unload" && op.Name == "trace_vfs_read" {
			unloadFentry = true
		}
	}
	assert.True(t, loadFentry, "expected fentry program to be loaded")
	assert.True(t, loadFexit, "expected fexit program load to fail")
	assert.True(t, unloadFentry, "expected fentry program to be unloaded during rollback")
}

func TestLoadImage_Rollback_FentryFexitFirstFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	// Make first program (fentry) fail to load - no rollback needed
	f.Kernel.FailOnProgram("trace_vfs_read", fmt.Errorf("injected fentry load failure"))

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_read")

	// Verify no programs remain in kernel or database
	f.AssertCleanState()

	// Verify outcome structure
	o := extractLoadImageOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, "trace_vfs_read", failed.Target)

	// Should have completed: image.pull, image.discover (no kernel.load succeeded)
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 2)

	// Second program should be skipped
	skipped := timelineSkipped(o.Timeline)
	assert.Len(t, skipped, 1)
	assert.Equal(t, outcome.StepKindKernelLoad, skipped[0].Kind)
	assert.Equal(t, "trace_vfs_write", skipped[0].Target)

	// No cleanup needed (nothing successfully loaded)
	assert.False(t, timelineHasRollback(o.Timeline))

	// SystemState should be clean (no residue)
	assert.Equal(t, "clean", o.SystemState)

	// Verify second program was never attempted
	ops := f.Kernel.Operations()
	var loadFentry, loadFexit bool
	for _, op := range ops {
		if op.Op == "load" && op.Name == "trace_vfs_read" {
			loadFentry = true
		}
		if op.Op == "load" && op.Name == "trace_vfs_write" {
			loadFexit = true
		}
	}
	assert.True(t, loadFentry, "expected fentry program load to be attempted")
	assert.False(t, loadFexit, "expected fexit program load to NOT be attempted after first failure")
}

func TestLoadImage_Rollback_MixedTypesThirdFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "my_xdp", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	// Make third program (fexit) fail to load
	f.Kernel.FailOnProgram("trace_vfs_write", fmt.Errorf("injected fexit load failure"))

	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

	// Verify outcome structure
	o := extractLoadImageOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "trace_vfs_write", failed.Target)

	// Should have completed: image.pull, image.discover,
	// kernel.load(xdp), fs.publish(xdp), store.save(xdp),
	// kernel.load(fentry), fs.publish(fentry), store.save(fentry)
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 8)

	// Verify cleanup was recorded - xdp and fentry should be rolled back
	rollbackCompleted := timelineRollbackCompleted(o.Timeline)
	assert.True(t, timelineHasRollback(o.Timeline))
	assert.Len(t, rollbackCompleted, 2, "should have 2 cleanup steps for xdp and fentry")

	// SystemState should be clean after successful rollback
	assert.Equal(t, "clean", o.SystemState)
	assert.False(t, o.ManualCleanupRequired)

	// Verify rollback: both xdp and fentry should be unloaded from both kernel and database
	f.AssertCleanState()

	// Verify the operation sequence
	ops := f.Kernel.Operations()
	var loadXDP, loadFentry, loadFexit, unloadXDP, unloadFentry bool
	for _, op := range ops {
		if op.Op == "load" && op.Name == "my_xdp" && op.Err == nil {
			loadXDP = true
		}
		if op.Op == "load" && op.Name == "trace_vfs_read" && op.Err == nil {
			loadFentry = true
		}
		if op.Op == "load" && op.Name == "trace_vfs_write" && op.Err != nil {
			loadFexit = true
		}
		if op.Op == "unload" && op.Name == "my_xdp" {
			unloadXDP = true
		}
		if op.Op == "unload" && op.Name == "trace_vfs_read" {
			unloadFentry = true
		}
	}
	assert.True(t, loadXDP, "expected xdp program to be loaded")
	assert.True(t, loadFentry, "expected fentry program to be loaded")
	assert.True(t, loadFexit, "expected fexit program load to fail")
	assert.True(t, unloadXDP, "expected xdp program to be unloaded during rollback")
	assert.True(t, unloadFentry, "expected fentry program to be unloaded during rollback")
}

func TestLoadImage_ValidationError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})
	discoverer.SetValidateError(fmt.Errorf("custom validation error"))

	// Request explicit programs to trigger validation
	_, err := f.Manager.LoadImage(context.Background(), interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, []manager.ImageProgramSpec{
		{ProgramName: "prog_a", ProgramType: bpfman.ProgramTypeXDP},
	}, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom validation error")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}
