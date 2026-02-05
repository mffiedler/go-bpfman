package server_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// Uprobe/Uretprobe Lifecycle Tests
// =============================================================================
//
// These tests verify the uprobe lifecycle. Uprobe programs attach to user-space
// function entry points. The target binary is specified at attach time.

// TestUprobe_AttachSucceeds verifies that:
//
//	Given a loaded uprobe program,
//	When I attach it with a target,
//	Then a link is created.
func TestUprobe_AttachSucceeds(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a uprobe program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("uprobe.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "uprobe_prog",
				ProgramType: pb.BpfmanProgramType_UPROBE,
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "uprobe-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach uprobe with target
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_UprobeAttachInfo{
				UprobeAttachInfo: &pb.UprobeAttachInfo{
					Target: "/usr/lib/libc.so.6",
					FnName: stringPtr("malloc"),
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachUprobe should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestUprobe_AttachWithoutTarget_Fails verifies that:
//
//	Given a loaded uprobe program,
//	When I try to attach without a target,
//	Then the operation fails.
func TestUprobe_AttachWithoutTarget_Fails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a uprobe program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("uprobe.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "uprobe_prog",
				ProgramType: pb.BpfmanProgramType_UPROBE,
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "uprobe-no-target-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt to attach without target
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_UprobeAttachInfo{
				UprobeAttachInfo: &pb.UprobeAttachInfo{
					// Target not set
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach should fail without target")
	assert.Contains(t, err.Error(), "target", "error should mention target")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// TestUprobe_FullLifecycle verifies the complete uprobe lifecycle.
func TestUprobe_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load uprobe program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("uprobe.o")},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "uprobe_prog",
				ProgramType: pb.BpfmanProgramType_UPROBE,
			},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "uprobe-lifecycle-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Step 2: Attach
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_UprobeAttachInfo{
				UprobeAttachInfo: &pb.UprobeAttachInfo{
					Target: "/usr/lib/libc.so.6",
					FnName: stringPtr("malloc"),
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
