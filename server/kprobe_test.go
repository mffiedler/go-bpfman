package server_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// Kprobe/Kretprobe Lifecycle Tests
// =============================================================================
//
// These tests verify the kprobe lifecycle. Kprobe programs attach to kernel
// function entry points. The target function is specified at attach time.

// TestKprobe_AttachSucceeds verifies that:
//
//	Given a loaded kprobe program,
//	When I attach it with a function name,
//	Then a link is created.
func TestKprobe_AttachSucceeds(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a kprobe program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("kprobe.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "kprobe_prog",
				ProgramType: pb.BpfmanProgramType_KPROBE,
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "kprobe-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach kprobe with function name
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_KprobeAttachInfo{
				KprobeAttachInfo: &pb.KprobeAttachInfo{
					FnName: "do_sys_open",
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachKprobe should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestKprobe_AttachWithoutFnName_Fails verifies that:
//
//	Given a loaded kprobe program,
//	When I try to attach without a function name,
//	Then the operation fails.
func TestKprobe_AttachWithoutFnName_Fails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a kprobe program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("kprobe.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "kprobe_prog",
				ProgramType: pb.BpfmanProgramType_KPROBE,
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "kprobe-no-fnname-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt to attach without function name
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_KprobeAttachInfo{
				KprobeAttachInfo: &pb.KprobeAttachInfo{
					// FnName not set
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach should fail without FnName")
	assert.Contains(t, err.Error(), "fn_name", "error should mention fn_name")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// TestKprobe_FullLifecycle verifies the complete kprobe lifecycle.
func TestKprobe_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load kprobe program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("kprobe.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "kprobe_prog",
				ProgramType: pb.BpfmanProgramType_KPROBE,
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "kprobe-lifecycle-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Step 2: Attach
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_KprobeAttachInfo{
				KprobeAttachInfo: &pb.KprobeAttachInfo{
					FnName: "do_sys_open",
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")
	linkID := attachResp.LinkId

	// Verify state
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Step 3: Detach
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkID})
	require.NoError(t, err, "Detach should succeed")

	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after detach")

	// Step 4: Unload
	_, err = fix.Server.Unload(ctx, &pb.UnloadRequest{Id: programID})
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "should have 0 programs")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links")
}
