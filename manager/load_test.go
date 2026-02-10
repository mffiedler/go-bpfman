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
	"github.com/frobware/go-bpfman/outcome"
	"github.com/frobware/go-bpfman/platform"
)

// extractLoadOutcome extracts the outcome from a Load error.
func extractLoadOutcome(t *testing.T, err error) outcome.OperationOutcome {
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

// timelineHasRollback returns true if there are any rollback phase entries.
func timelineHasRollback(timeline []outcome.TimelineEntry) bool {
	for _, e := range timeline {
		if e.Phase == outcome.PhaseRollback {
			return true
		}
	}
	return false
}

func TestLoad_AutoDiscover_SingleProgram(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "test_prog", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "test_prog", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}

func TestLoad_AutoDiscover_MultiplePrograms(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 3)
	assert.Equal(t, 3, f.Kernel.ProgramCount())

	// Verify programs are loaded in sorted order
	assert.Equal(t, "prog_a", programs[0].Record.Meta.Name)
	assert.Equal(t, "prog_b", programs[1].Record.Meta.Name)
	assert.Equal(t, "prog_c", programs[2].Record.Meta.Name)
}

func TestLoad_AutoDiscover_NoPrograms(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	// Don't set any programs - empty object file

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no programs found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_ExplicitPrograms_Valid(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Request only prog_b
	programs, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, []manager.ProgramSpec{
		{Name: "prog_b", Type: bpfman.ProgramTypeXDP},
	}, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "prog_b", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}

func TestLoad_ExplicitPrograms_InvalidName(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Request non-existent program
	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, []manager.ProgramSpec{
		{Name: "nonexistent", Type: bpfman.ProgramTypeXDP},
	}, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_Rollback_SecondProgramFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Make second program fail to load
	f.Kernel.FailOnProgram("prog_b", fmt.Errorf("injected load failure"))

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_b")

	// Verify rollback: prog_a should be unloaded from both kernel and database
	f.AssertCleanState()

	// The error is from the failing program's per-program plan.
	// kernel-load failed, so db-check, fs-publish, store-save are skipped.
	o := extractLoadOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "prog_b", failed.Target)
	assert.NotEmpty(t, failed.Error)

	// No completed steps in the failing program's plan.
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 0)

	// Skipped: db-check, fs-publish, store-save
	skipped := timelineSkipped(o.Timeline)
	assert.Len(t, skipped, 3)

	// No rollback in the failing program's plan (kernel-load
	// failed so no undo entries were registered).
	assert.False(t, timelineHasRollback(o.Timeline))

	// Verify the operation sequence
	ops := f.Kernel.Operations()
	// Should see: load prog_a (ok), load prog_b (error), unload prog_a (cleanup)
	assert.GreaterOrEqual(t, len(ops), 3)

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

func TestLoad_Rollback_ThirdProgramFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Make third program fail to load
	f.Kernel.FailOnProgram("prog_c", fmt.Errorf("injected load failure"))

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_c")

	// The error is from prog_c's per-program plan.
	o := extractLoadOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "prog_c", failed.Target)

	// No completed steps in the failing program's plan.
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 0)

	// Skipped: db-check, fs-publish, store-save
	skipped := timelineSkipped(o.Timeline)
	assert.Len(t, skipped, 3)

	// No rollback in the failing program's plan.
	assert.False(t, timelineHasRollback(o.Timeline))

	// Verify rollback: prog_a and prog_b should be unloaded from both kernel and database
	f.AssertCleanState()
}

func TestLoad_PullError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	puller.SetPullError(fmt.Errorf("network error"))
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull image")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_DiscoverError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetDiscoverError(fmt.Errorf("corrupt ELF file"))
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover programs")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_AutoDiscover_FentryFexit(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	programs, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 2)
	assert.Equal(t, 2, f.Kernel.ProgramCount())

	// Verify the programs were loaded (sorted by name)
	assert.Equal(t, "trace_vfs_read", programs[0].Record.Meta.Name)
	assert.Equal(t, "trace_vfs_write", programs[1].Record.Meta.Name)
}

func TestLoad_Rollback_FentryFexitSecondFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	// Make second program (fexit) fail to load
	f.Kernel.FailOnProgram("trace_vfs_write", fmt.Errorf("injected fexit load failure"))

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

	// The error is from trace_vfs_write's per-program plan.
	o := extractLoadOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "trace_vfs_write", failed.Target)

	// No completed steps in the failing program's plan.
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 0)

	// Skipped: db-check, fs-publish, store-save
	skipped := timelineSkipped(o.Timeline)
	assert.Len(t, skipped, 3)

	// No rollback in the failing program's plan.
	assert.False(t, timelineHasRollback(o.Timeline))

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

func TestLoad_Rollback_FentryFexitFirstFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	// Make first program (fentry) fail to load - no cleanup needed
	f.Kernel.FailOnProgram("trace_vfs_read", fmt.Errorf("injected fentry load failure"))

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_read")

	// Verify no programs remain in kernel or database
	f.AssertCleanState()

	// The error is from trace_vfs_read's per-program plan.
	o := extractLoadOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, "trace_vfs_read", failed.Target)

	// No completed steps (kernel-load failed immediately).
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 0)

	// Skipped: db-check, fs-publish, store-save
	skipped := timelineSkipped(o.Timeline)
	assert.Len(t, skipped, 3)

	// No rollback needed (nothing successfully loaded)
	assert.False(t, timelineHasRollback(o.Timeline))

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

func TestLoad_Rollback_MixedTypesThirdFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "my_xdp", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	// Make third program (fexit) fail to load
	f.Kernel.FailOnProgram("trace_vfs_write", fmt.Errorf("injected fexit load failure"))

	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

	// The error is from trace_vfs_write's per-program plan.
	o := extractLoadOutcome(t, err)
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.PrimaryError)
	failed := timelineFindFailed(o.Timeline)
	require.NotNil(t, failed)
	assert.Equal(t, outcome.StepKindKernelLoad, failed.Kind)
	assert.Equal(t, "trace_vfs_write", failed.Target)

	// No completed steps in the failing program's plan.
	completed := timelineCompletedPrimary(o.Timeline)
	assert.Len(t, completed, 0)

	// Skipped: db-check, fs-publish, store-save
	skipped := timelineSkipped(o.Timeline)
	assert.Len(t, skipped, 3)

	// No rollback in the failing program's plan.
	assert.False(t, timelineHasRollback(o.Timeline))

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

func TestLoad_ValidationError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})
	discoverer.SetValidateError(fmt.Errorf("custom validation error"))

	// Request explicit programs to trigger validation
	_, err := f.Manager.Load(context.Background(), manager.LoadSource{
		Image: &platform.ImageRef{URL: "test.io/image:latest"},
	}, []manager.ProgramSpec{
		{Name: "prog_a", Type: bpfman.ProgramTypeXDP},
	}, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom validation error")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_FileSource(t *testing.T) {
	discoverer := newFakeDiscoverer()
	f := newTestFixtureWithDiscoverer(t, discoverer)
	objPath := f.BytecodeFile("object.o")
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "test_prog", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.Manager.Load(context.Background(), manager.LoadSource{
		FilePath: objPath,
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "test_prog", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}
