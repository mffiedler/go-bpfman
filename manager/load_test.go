package manager_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

func newTestImageRef() *platform.ImageRef {
	return &platform.ImageRef{
		URL:        "test.io/image:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
}

func TestLoad_AutoDiscover_SingleProgram(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "test_prog", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "test_prog", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}

func TestLoad_AutoDiscover_MultiplePrograms(t *testing.T) {
	t.Parallel()

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

	programs, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 3)
	assert.Equal(t, 3, f.Kernel.ProgramCount())

	// Verify programs are loaded in sorted order
	assert.Equal(t, "prog_a", programs[0].Record.Meta.Name)
	assert.Equal(t, "prog_b", programs[1].Record.Meta.Name)
	assert.Equal(t, "prog_c", programs[2].Record.Meta.Name)
}

// TestLoad_ExplicitPrograms_PreservesOrder asserts the contract that
// Manager.Load returns programs in the same order as the input
// ProgramSpec slice, independent of the order they appear in the
// object file. CLI and SDK consumers depend on this for stable
// jsonpath access (`{.programs[i]...}`) and for variable assignment
// in shell scripts (`let progs = [bpfman program load ...]`).
func TestLoad_ExplicitPrograms_PreservesOrder(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_b", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_c", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_d", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Request a non-alphabetical, non-source-order subset so the
	// assertion catches both "discoverer order leaked" and
	// "result was sorted".
	requested := []manager.ProgramSpec{
		{Name: "prog_c", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_a", Type: bpfman.ProgramTypeXDP},
		{Name: "prog_d", Type: bpfman.ProgramTypeXDP},
	}

	programs, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, requested, manager.LoadOpts{})

	require.NoError(t, err)
	require.Len(t, programs, len(requested))
	for i, want := range requested {
		assert.Equal(t, want.Name, programs[i].Record.Meta.Name,
			"programs[%d].Record.Meta.Name should match requested[%d].Name", i, i)
	}
}

func TestLoad_AutoDiscover_NoPrograms(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	// Don't set any programs - empty object file

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no programs found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_ExplicitPrograms_Valid(t *testing.T) {
	t.Parallel()

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
	programs, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, []manager.ProgramSpec{
		{Name: "prog_b", Type: bpfman.ProgramTypeXDP},
	}, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "prog_b", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}

func TestLoad_ExplicitPrograms_InvalidName(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "prog_a", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	// Request non-existent program
	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, []manager.ProgramSpec{
		{Name: "nonexistent", Type: bpfman.ProgramTypeXDP},
	}, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_Rollback_SecondProgramFails(t *testing.T) {
	t.Parallel()

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

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_b")

	// Verify rollback: prog_a should be unloaded from both kernel and database
	f.AssertCleanState()

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
	t.Parallel()

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

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prog_c")

	// Verify rollback: prog_a and prog_b should be unloaded from both kernel and database
	f.AssertCleanState()
}

func TestLoad_PullError(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	puller.SetPullError(fmt.Errorf("network error"))
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull image")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_DiscoverError(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	discoverer.SetDiscoverError(fmt.Errorf("corrupt ELF file"))
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discover programs")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_AutoDiscover_FentryFexit(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	puller := newFakeImagePuller()
	f := newTestFixtureWithOptions(t, discoverer, puller)
	objPath := f.BytecodeFile("object.o")
	puller.SetObjectPath(objPath)
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "trace_vfs_read", SectionName: "fentry/vfs_read", Type: bpfman.ProgramTypeFentry, AttachFunc: "vfs_read"},
		{Name: "trace_vfs_write", SectionName: "fexit/vfs_write", Type: bpfman.ProgramTypeFexit, AttachFunc: "vfs_write"},
	})

	programs, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 2)
	assert.Equal(t, 2, f.Kernel.ProgramCount())

	// Verify the programs were loaded (sorted by name)
	assert.Equal(t, "trace_vfs_read", programs[0].Record.Meta.Name)
	assert.Equal(t, "trace_vfs_write", programs[1].Record.Meta.Name)
}

func TestLoad_Rollback_FentryFexitSecondFails(t *testing.T) {
	t.Parallel()

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

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

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
	t.Parallel()

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

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_read")

	// Verify no programs remain in kernel or database
	f.AssertCleanState()

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
	t.Parallel()

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

	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, nil, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trace_vfs_write")

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
	t.Parallel()

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
	_, err := f.LoadDirect(context.Background(), manager.LoadSource{
		Image: newTestImageRef(),
	}, []manager.ProgramSpec{
		{Name: "prog_a", Type: bpfman.ProgramTypeXDP},
	}, manager.LoadOpts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom validation error")
	assert.Equal(t, 0, f.Kernel.ProgramCount())
}

func TestLoad_FileSource(t *testing.T) {
	t.Parallel()

	discoverer := newFakeDiscoverer()
	f := newTestFixtureWithDiscoverer(t, discoverer)
	objPath := f.BytecodeFile("object.o")
	discoverer.SetPrograms(objPath, []platform.DiscoveredProgram{
		{Name: "test_prog", SectionName: "xdp", Type: bpfman.ProgramTypeXDP},
	})

	programs, err := f.LoadDirect(context.Background(), manager.LoadSource{
		FilePath: objPath,
	}, nil, manager.LoadOpts{})

	require.NoError(t, err)
	assert.Len(t, programs, 1)
	assert.Equal(t, "test_prog", programs[0].Record.Meta.Name)
	assert.Equal(t, 1, f.Kernel.ProgramCount())
}
