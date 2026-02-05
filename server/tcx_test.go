package server_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// TCX Lifecycle Tests
// =============================================================================
//
// These tests verify the TCX lifecycle using the fake network interface
// resolver. TCX is the modern link-based TC attachment mechanism.

// TestTCX_FirstAttachCreatesLink verifies that:
//
//	Given a loaded TCX program,
//	When I attach it to an interface,
//	Then a link is created.
func TestTCX_FirstAttachCreatesLink(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tcx.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tcx_pass", ProgramType: pb.BpfmanProgramType_TCX},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tcx-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach to interface with ingress direction
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcxAttachInfo{
				TcxAttachInfo: &pb.TCXAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
					Priority:  50,
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachTCX should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestTCX_IngressAndEgressDirections verifies that:
//
//	Given a loaded TCX program,
//	When I attach it with both ingress and egress directions,
//	Then both attachments succeed.
func TestTCX_IngressAndEgressDirections(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tcx.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tcx_pass", ProgramType: pb.BpfmanProgramType_TCX},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tcx-direction-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach ingress
	ingressReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcxAttachInfo{
				TcxAttachInfo: &pb.TCXAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
				},
			},
		},
	}

	ingressResp, err := fix.Server.Attach(ctx, ingressReq)
	require.NoError(t, err, "Ingress attach should succeed")

	// Attach egress
	egressReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcxAttachInfo{
				TcxAttachInfo: &pb.TCXAttachInfo{
					Iface:     "eth0",
					Direction: "egress",
				},
			},
		},
	}

	egressResp, err := fix.Server.Attach(ctx, egressReq)
	require.NoError(t, err, "Egress attach should succeed")

	// Verify both links exist
	assert.Equal(t, 2, fix.Kernel.LinkCount(), "should have 2 links")
	assert.NotEqual(t, ingressResp.LinkId, egressResp.LinkId, "link IDs should differ")
}

// TestTCX_InvalidDirection verifies that:
//
//	Given a loaded TCX program,
//	When I try to attach with an invalid direction,
//	Then the operation fails.
func TestTCX_InvalidDirection(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tcx.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tcx_pass", ProgramType: pb.BpfmanProgramType_TCX},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tcx-invalid-direction-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt attach with invalid direction
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcxAttachInfo{
				TcxAttachInfo: &pb.TCXAttachInfo{
					Iface:     "eth0",
					Direction: "sideways",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach with invalid direction should fail")
	assert.Contains(t, err.Error(), "direction", "error should mention direction")
}

// TestTCX_AttachToNonExistentInterface verifies that:
//
//	Given a loaded TCX program,
//	When I try to attach it to a non-existent interface,
//	Then the operation fails with an appropriate error.
func TestTCX_AttachToNonExistentInterface(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tcx.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tcx_pass", ProgramType: pb.BpfmanProgramType_TCX},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tcx-nonexistent-iface-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt to attach to non-existent interface
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcxAttachInfo{
				TcxAttachInfo: &pb.TCXAttachInfo{
					Iface:     "nonexistent0",
					Direction: "ingress",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach to non-existent interface should fail")
	assert.Contains(t, err.Error(), "not found", "error should mention interface not found")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// TestTCX_FullLifecycle verifies the complete TCX lifecycle:
//
//  1. Load TCX program
//  2. Attach to ingress and egress on multiple interfaces
//  3. Detach all links
//  4. Unload program
//  5. Verify clean state
func TestTCX_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load TCX program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tcx.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tcx_pass", ProgramType: pb.BpfmanProgramType_TCX},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tcx-lifecycle-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id
	t.Logf("Step 1: Loaded program ID %d", programID)

	// Step 2: Attach to ingress and egress on multiple interfaces
	var linkIDs []uint32
	for _, iface := range []string{"lo", "eth0"} {
		for _, direction := range []string{"ingress", "egress"} {
			attachReq := &pb.AttachRequest{
				Id: programID,
				Attach: &pb.AttachInfo{
					Info: &pb.AttachInfo_TcxAttachInfo{
						TcxAttachInfo: &pb.TCXAttachInfo{
							Iface:     iface,
							Direction: direction,
						},
					},
				},
			}
			attachResp, err := fix.Server.Attach(ctx, attachReq)
			require.NoError(t, err, "Attach %s/%s should succeed", iface, direction)
			linkIDs = append(linkIDs, attachResp.LinkId)
			t.Logf("Step 2: Attached %s/%s (link ID %d)", iface, direction, attachResp.LinkId)
		}
	}

	// Verify state after attachments (4 links: 2 interfaces x 2 directions)
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 4, fix.Kernel.LinkCount(), "should have 4 links")

	// Step 3: Detach all links
	for i, linkID := range linkIDs {
		_, err := fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkID})
		require.NoError(t, err, "Detach link %d should succeed", linkID)
		t.Logf("Step 3: Detached link %d, remaining: %d", linkID, fix.Kernel.LinkCount())
		assert.Equal(t, 4-i-1, fix.Kernel.LinkCount(), "link count should decrement")
	}

	// Step 4: Unload program
	_, err = fix.Server.Unload(ctx, &pb.UnloadRequest{Id: programID})
	require.NoError(t, err, "Unload should succeed")
	t.Logf("Step 4: Unloaded program %d", programID)

	// Step 5: Verify clean state
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "should have 0 programs")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links")
	t.Log("Step 5: Verified clean state - test passed")
}
