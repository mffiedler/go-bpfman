package server_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// Fexit Lifecycle Tests
// =============================================================================
//
// These tests verify the fexit lifecycle. Fexit programs attach to kernel
// function exit points. The target function must be specified at load time
// via FexitLoadInfo.FnName.

// TestFexit_AttachSucceeds verifies that:
//
//	Given a loaded fexit program with FnName specified,
//	When I attach it,
//	Then a link is created.
func TestFexit_AttachSucceeds(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a fexit program with FnName specified
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("fexit.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "fexit_prog",
				ProgramType: pb.BpfmanProgramType_FEXIT,
				Info: &pb.ProgSpecificInfo{
					Info: &pb.ProgSpecificInfo_FexitLoadInfo{
						FexitLoadInfo: &pb.FexitLoadInfo{
							FnName: "tcp_close",
						},
					},
				},
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "fexit-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach fexit
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_FexitAttachInfo{
				FexitAttachInfo: &pb.FexitAttachInfo{},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachFexit should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestFexit_LoadWithoutFnName_Fails verifies that:
//
//	Given a fexit program load request without FnName specified,
//	When I try to load it,
//	Then the operation fails because fexit requires attachFunc at load time.
func TestFexit_LoadWithoutFnName_Fails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Try to load a fexit program WITHOUT FnName (no ProgSpecificInfo)
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("fexit.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "fexit_prog",
				ProgramType: pb.BpfmanProgramType_FEXIT,
				// No Info field - FnName not specified
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "fexit-no-fnname-test",
		},
	}

	_, err := fix.Server.Load(ctx, loadReq)
	require.Error(t, err, "Load should fail without FnName for fexit")
	assert.Contains(t, err.Error(), "attachFunc", "error should mention attachFunc")

	// No programs should exist
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "no programs should exist")
}

// TestFexit_FullLifecycle verifies the complete fexit lifecycle:
//
//  1. Load fexit program with FnName
//  2. Attach to kernel function
//  3. Detach
//  4. Unload program
//  5. Verify clean state
func TestFexit_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load fexit program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("fexit.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "fexit_prog",
				ProgramType: pb.BpfmanProgramType_FEXIT,
				Info: &pb.ProgSpecificInfo{
					Info: &pb.ProgSpecificInfo_FexitLoadInfo{
						FexitLoadInfo: &pb.FexitLoadInfo{
							FnName: "tcp_close",
						},
					},
				},
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "fexit-lifecycle-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id
	t.Logf("Step 1: Loaded program ID %d", programID)

	// Step 2: Attach
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_FexitAttachInfo{
				FexitAttachInfo: &pb.FexitAttachInfo{},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")
	linkID := attachResp.LinkId
	t.Logf("Step 2: Attached (link ID %d)", linkID)

	// Verify state
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Step 3: Detach
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkID})
	require.NoError(t, err, "Detach should succeed")
	t.Logf("Step 3: Detached link %d", linkID)

	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after detach")

	// Step 4: Unload
	_, err = fix.Server.Unload(ctx, &pb.UnloadRequest{Id: programID})
	require.NoError(t, err, "Unload should succeed")
	t.Logf("Step 4: Unloaded program %d", programID)

	// Step 5: Verify clean state
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "should have 0 programs")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links")
	t.Log("Step 5: Verified clean state - test passed")
}
