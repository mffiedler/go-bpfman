package server_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// Fentry Lifecycle Tests
// =============================================================================
//
// These tests verify the fentry lifecycle. Fentry programs attach to kernel
// function entry points. The target function must be specified at load time
// via FentryLoadInfo.FnName.

// TestFentry_AttachSucceeds verifies that:
//
//	Given a loaded fentry program with FnName specified,
//	When I attach it,
//	Then a link is created.
func TestFentry_AttachSucceeds(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a fentry program with FnName specified
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("fentry.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "fentry_prog",
				ProgramType: pb.BpfmanProgramType_FENTRY,
				Info: &pb.ProgSpecificInfo{
					Info: &pb.ProgSpecificInfo_FentryLoadInfo{
						FentryLoadInfo: &pb.FentryLoadInfo{
							FnName: "tcp_connect",
						},
					},
				},
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "fentry-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach fentry
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_FentryAttachInfo{
				FentryAttachInfo: &pb.FentryAttachInfo{},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachFentry should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestFentry_LoadWithoutFnName_Fails verifies that:
//
//	Given a fentry program load request without FnName specified,
//	When I try to load it,
//	Then the operation fails because fentry requires attachFunc at load time.
func TestFentry_LoadWithoutFnName_Fails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Try to load a fentry program WITHOUT FnName (no ProgSpecificInfo)
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("fentry.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "fentry_prog",
				ProgramType: pb.BpfmanProgramType_FENTRY,
				// No Info field - FnName not specified
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "fentry-no-fnname-test",
		},
	}

	_, err := fix.Server.Load(ctx, loadReq)
	require.Error(t, err, "Load should fail without FnName for fentry")
	assert.Contains(t, err.Error(), "attachFunc", "error should mention attachFunc")

	// No programs should exist
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "no programs should exist")
}

// TestFentry_FullLifecycle verifies the complete fentry lifecycle:
//
//  1. Load fentry program with FnName
//  2. Attach to kernel function
//  3. Detach
//  4. Unload program
//  5. Verify clean state
func TestFentry_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load fentry program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("fentry.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "fentry_prog",
				ProgramType: pb.BpfmanProgramType_FENTRY,
				Info: &pb.ProgSpecificInfo{
					Info: &pb.ProgSpecificInfo_FentryLoadInfo{
						FentryLoadInfo: &pb.FentryLoadInfo{
							FnName: "tcp_connect",
						},
					},
				},
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "fentry-lifecycle-test",
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
			Info: &pb.AttachInfo_FentryAttachInfo{
				FentryAttachInfo: &pb.FentryAttachInfo{},
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
