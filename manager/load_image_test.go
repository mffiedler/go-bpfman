package manager_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/interpreter"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/outcome"
)

func TestLoadImage_AutoDiscover_SingleProgram(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "test_prog", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, result.Programs, 1)
	assert.Equal(t, "test_prog", result.Programs[0].Kernel.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())

	// Verify outcome structure on success
	o := result.Outcome
	assert.Equal(t, outcome.StatusSuccess, o.Status)
	assert.Empty(t, o.Error)
	assert.Nil(t, o.Failed)
	assert.Nil(t, o.Rollback)
	assert.Empty(t, o.Skipped)
	// Should have: image.pull, image.discover, kernel.load, store.save
	assert.Len(t, o.Completed, 4)

	// Verify step kinds
	kinds := make([]outcome.StepKind, len(o.Completed))
	for i, s := range o.Completed {
		kinds[i] = s.Kind
	}
	assert.Contains(t, kinds, outcome.StepKindPullImage)
	assert.Contains(t, kinds, outcome.StepKindDiscoverPrograms)
	assert.Contains(t, kinds, outcome.StepKindKernelLoad)
	assert.Contains(t, kinds, outcome.StepKindStoreSaveProgram)

	// SystemState should be clean on success
	assert.Equal(t, "clean", o.SystemState())
}

func TestLoadImage_AutoDiscover_MultiplePrograms(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, result.Programs, 3)
	assert.Equal(t, 3, f.Kernel.ProgramCount())

	// Verify programs are loaded in sorted order
	assert.Equal(t, "prog_a", result.Programs[0].Kernel.Name)
	assert.Equal(t, "prog_b", result.Programs[1].Kernel.Name)
	assert.Equal(t, "prog_c", result.Programs[2].Kernel.Name)
}

func TestLoadImage_AutoDiscover_NoPrograms(t *testing.T) {
	discoverer := newFakeDiscoverer()
	// Don't set any programs - empty object file

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	_, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no programs found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoadImage_ExplicitPrograms_Valid(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Request only prog_b
	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, []manager.ImageProgramSpec{
		{ProgramName: "prog_b", ProgramType: bpfman.ProgramTypeXDP},
	}, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, result.Programs, 1)
	assert.Equal(t, "prog_b", result.Programs[0].Kernel.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}

func TestLoadImage_ExplicitPrograms_InvalidName(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Request non-existent program
	_, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
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
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Make second program fail to load
	f.Kernel.FailOnProgram("prog_b", fmt.Errorf("injected load failure"))

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_b")

	// Verify rollback: prog_a should be unloaded from both kernel and database
	f.AssertCleanState()

	// Verify outcome structure on failure
	o := result.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.Error)
	require.NotNil(t, o.Failed)
	assert.Equal(t, outcome.StepKindKernelLoad, o.Failed.Kind)
	assert.Equal(t, "prog_b", o.Failed.Target)
	assert.NotEmpty(t, o.Failed.Error)

	// Should have completed: image.pull, image.discover, kernel.load(prog_a), store.save(prog_a)
	assert.Len(t, o.Completed, 4)

	// No programs skipped (only 2 programs, first succeeded, second failed)
	assert.Empty(t, o.Skipped)

	// Verify cleanup was recorded
	require.NotNil(t, o.Rollback)
	assert.Equal(t, outcome.StatusSuccess, o.Rollback.Status)
	assert.Len(t, o.Rollback.Completed, 1)
	assert.Equal(t, outcome.StepKindKernelUnload, o.Rollback.Completed[0].Kind)
	assert.Equal(t, "prog_a", o.Rollback.Completed[0].Target)

	// SystemState should be clean after successful cleanup
	assert.Equal(t, "clean", o.SystemState())

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
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Make third program fail to load
	f.Kernel.FailOnProgram("prog_c", fmt.Errorf("injected load failure"))

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_c")

	// Verify outcome structure
	o := result.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.Error)
	require.NotNil(t, o.Failed)
	assert.Equal(t, outcome.StepKindKernelLoad, o.Failed.Kind)
	assert.Equal(t, "prog_c", o.Failed.Target)

	// Should have completed: image.pull, image.discover, kernel.load(prog_a), store.save(prog_a),
	// kernel.load(prog_b), store.save(prog_b)
	assert.Len(t, o.Completed, 6)

	// Verify cleanup was recorded - prog_a and prog_b should be rolled back
	require.NotNil(t, o.Rollback)
	assert.Equal(t, outcome.StatusSuccess, o.Rollback.Status)
	assert.Len(t, o.Rollback.Completed, 2, "should have 2 cleanup steps for prog_a and prog_b")

	// SystemState should be clean after successful rollback
	assert.Equal(t, "clean", o.SystemState())
	assert.False(t, o.NeedsManualCleanup())

	// Verify rollback: prog_a and prog_b should be unloaded from both kernel and database
	f.AssertCleanState()
}

func TestLoadImage_PullError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")
	puller.SetPullError(fmt.Errorf("network error"))

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull image")
	assert.Equal(t, 0, f.Kernel.ProgramCount())

	// Verify outcome structure - early failure, no completed steps
	o := result.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.Error)
	require.NotNil(t, o.Failed)
	assert.Equal(t, outcome.StepKindPullImage, o.Failed.Kind)
	assert.Equal(t, "test.io/image:latest", o.Failed.Target)
	assert.Empty(t, o.Completed)
	assert.Empty(t, o.Skipped)
	assert.Nil(t, o.Rollback)
}

func TestLoadImage_DiscoverError(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetDiscoverError(fmt.Errorf("corrupt ELF file"))

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	_, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover programs")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoadImage_AutoDiscover_FentryFexit(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.NoError(t, err)
	assert.Len(t, result.Programs, 2)
	assert.Equal(t, 2, f.Kernel.ProgramCount())

	// Verify the programs were loaded (sorted by name)
	assert.Equal(t, "trace_vfs_read", result.Programs[0].Kernel.Name)
	assert.Equal(t, "trace_vfs_write", result.Programs[1].Kernel.Name)
}

func TestLoadImage_Rollback_FentryFexitSecondFails(t *testing.T) {
	discoverer := newFakeDiscoverer()
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Make second program (fexit) fail to load
	f.Kernel.FailOnProgram("trace_vfs_write", fmt.Errorf("injected fexit load failure"))

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

	// Verify outcome structure
	o := result.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.Error)
	require.NotNil(t, o.Failed)
	assert.Equal(t, outcome.StepKindKernelLoad, o.Failed.Kind)
	assert.Equal(t, "trace_vfs_write", o.Failed.Target)

	// Should have completed: image.pull, image.discover, kernel.load(fentry), store.save(fentry)
	assert.Len(t, o.Completed, 4)

	// Verify cleanup was recorded - fentry should be rolled back
	require.NotNil(t, o.Rollback)
	assert.Equal(t, outcome.StatusSuccess, o.Rollback.Status)
	assert.Len(t, o.Rollback.Completed, 1)
	assert.Equal(t, outcome.StepKindKernelUnload, o.Rollback.Completed[0].Kind)
	assert.Equal(t, "trace_vfs_read", o.Rollback.Completed[0].Target)

	// SystemState should be clean after successful rollback
	assert.Equal(t, "clean", o.SystemState())

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
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Make first program (fentry) fail to load - no rollback needed
	f.Kernel.FailOnProgram("trace_vfs_read", fmt.Errorf("injected fentry load failure"))

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_read")

	// Verify no programs remain in kernel or database
	f.AssertCleanState()

	// Verify outcome structure
	o := result.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	require.NotNil(t, o.Failed)
	assert.Equal(t, "trace_vfs_read", o.Failed.Target)

	// Should have completed: image.pull, image.discover (no kernel.load succeeded)
	assert.Len(t, o.Completed, 2)

	// Second program should be skipped
	assert.Len(t, o.Skipped, 1)
	assert.Equal(t, outcome.StepKindKernelLoad, o.Skipped[0].Kind)
	assert.Equal(t, "trace_vfs_write", o.Skipped[0].Target)

	// No cleanup needed (nothing successfully loaded)
	assert.Nil(t, o.Rollback)

	// SystemState should be clean (no residue)
	assert.Equal(t, "clean", o.SystemState())

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
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "my_xdp", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Make third program (fexit) fail to load
	f.Kernel.FailOnProgram("trace_vfs_write", fmt.Errorf("injected fexit load failure"))

	result, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, nil, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

	// Verify outcome structure
	o := result.Outcome
	assert.Equal(t, outcome.StatusFailure, o.Status)
	assert.NotEmpty(t, o.Error)
	require.NotNil(t, o.Failed)
	assert.Equal(t, outcome.StepKindKernelLoad, o.Failed.Kind)
	assert.Equal(t, "trace_vfs_write", o.Failed.Target)

	// Should have completed: image.pull, image.discover, kernel.load(xdp), store.save(xdp),
	// kernel.load(fentry), store.save(fentry)
	assert.Len(t, o.Completed, 6)

	// Verify cleanup was recorded - xdp and fentry should be rolled back
	require.NotNil(t, o.Rollback)
	assert.Equal(t, outcome.StatusSuccess, o.Rollback.Status)
	assert.Len(t, o.Rollback.Completed, 2, "should have 2 cleanup steps for xdp and fentry")

	// SystemState should be clean after successful rollback
	assert.Equal(t, "clean", o.SystemState())
	assert.False(t, o.NeedsManualCleanup())

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
	discoverer.SetPrograms("/fake/object.o", []interpreter.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})
	discoverer.SetValidateError(fmt.Errorf("custom validation error"))

	f := newTestFixtureWithDiscoverer(t, discoverer)
	puller := newFakeImagePuller("/fake/object.o")

	// Request explicit programs to trigger validation
	_, err := f.Manager.LoadImage(context.Background(), puller, interpreter.ImageRef{
		URL: "test.io/image:latest",
	}, []manager.ImageProgramSpec{
		{ProgramName: "prog_a", ProgramType: bpfman.ProgramTypeXDP},
	}, manager.LoadImageOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom validation error")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}
