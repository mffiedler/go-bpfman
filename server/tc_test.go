package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// TC Dispatcher Lifecycle Tests
// =============================================================================
//
// These tests verify the TC dispatcher lifecycle using the fake network
// interface resolver.

// TestTC_FirstAttachCreatesLink verifies that:
//
//	Given a loaded TC program,
//	When I attach it to an interface,
//	Then a link is created.
func TestTC_FirstAttachCreatesLink(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach to interface with ingress direction
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
					Priority:  50,
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachTC should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestTC_IngressAndEgressDirections verifies that:
//
//	Given a loaded TC program,
//	When I attach it with both ingress and egress directions,
//	Then both attachments succeed.
func TestTC_IngressAndEgressDirections(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-direction-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach ingress
	ingressReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
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
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
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

// TestTC_InvalidDirection verifies that:
//
//	Given a loaded TC program,
//	When I try to attach with an invalid direction,
//	Then the operation fails.
func TestTC_InvalidDirection(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-invalid-direction-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt attach with invalid direction
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
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

// TestTC_AttachToNonExistentInterface verifies that:
//
//	Given a loaded TC program,
//	When I try to attach it to a non-existent interface,
//	Then the operation fails with an appropriate error.
func TestTC_AttachToNonExistentInterface(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-nonexistent-iface-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt to attach to non-existent interface
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
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

// TestTC_FullLifecycle verifies the complete TC lifecycle:
//
//  1. Load TC program
//  2. Attach to ingress and egress
//  3. Detach all links
//  4. Unload program
//  5. Verify clean state
func TestTC_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load TC program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-lifecycle-test",
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
					Info: &pb.AttachInfo_TcAttachInfo{
						TcAttachInfo: &pb.TCAttachInfo{
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
	// 5 programs: 1 user TC program + 4 TC dispatchers (2 interfaces x 2 directions)
	assert.Equal(t, 5, fix.Kernel.ProgramCount(), "should have 5 programs (user + 4 dispatchers)")
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

// TestTC_ExtensionPositionsAreSequential verifies that multiple TC
// extensions on the same interface/direction get sequential positions
// derived from CountDispatcherLinks rather than cached state.
func TestTC_ExtensionPositionsAreSequential(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-position-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach three times to the same interface/direction
	var linkIDs []uint32
	for i := 0; i < 3; i++ {
		attachResp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
			Id: programID,
			Attach: &pb.AttachInfo{
				Info: &pb.AttachInfo_TcAttachInfo{
					TcAttachInfo: &pb.TCAttachInfo{
						Iface:     "eth0",
						Direction: "ingress",
					},
				},
			},
		})
		require.NoError(t, err, "attach %d should succeed", i)
		linkIDs = append(linkIDs, attachResp.LinkId)
	}

	// Verify positions are 0, 1, 2 via store
	for i, linkID := range linkIDs {
		record, err := fix.Store.GetLink(ctx, bpfman.LinkID(linkID))
		require.NoError(t, err, "GetLink for link %d", linkID)
		tcDetails, ok := record.Details.(bpfman.TCDetails)
		require.True(t, ok, "expected TCDetails, got %T", record.Details)
		assert.Equal(t, int32(i), tcDetails.Position,
			"link %d should have position %d", linkID, i)
	}
}

// TestTC_DispatcherStateInStore verifies that the store correctly
// tracks dispatcher state after attachment and cleans it up after the
// last extension is detached.
func TestTC_DispatcherStateInStore(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadResp, err := fix.Server.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-disp-state-test",
		},
	})
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach one extension
	attachResp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
				},
			},
		},
	})
	require.NoError(t, err)

	// Verify dispatcher exists in store
	dispatchers, err := fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	require.Len(t, dispatchers, 1, "should have 1 dispatcher")
	assert.Equal(t, dispatcher.DispatcherTypeTCIngress, dispatchers[0].Type)
	assert.Equal(t, uint32(2), dispatchers[0].Ifindex) // eth0 = ifindex 2

	// Verify CountDispatcherLinks returns 1
	count, err := fix.Store.CountDispatcherLinks(ctx, dispatchers[0].KernelID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "should have 1 extension link")

	// Detach the extension
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: attachResp.LinkId})
	require.NoError(t, err)

	// Dispatcher should be cleaned up
	dispatchers, err = fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	assert.Empty(t, dispatchers, "dispatcher should be removed after last extension detached")
}

// TestTC_FilterHandleRoundTrip verifies that the TC filter handle
// assigned at attach time is correctly looked up via
// FindTCFilterHandle at detach time and passed to DetachTCFilter.
func TestTC_FilterHandleRoundTrip(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadResp, err := fix.Server.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-handle-test",
		},
	})
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach a single ingress extension
	attachResp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
				},
			},
		},
	})
	require.NoError(t, err)

	// Verify a TC filter was registered in fakeKernel
	assert.Equal(t, 1, fix.Kernel.TCFilterCount(), "should have 1 TC filter tracked")

	// Detach the extension (triggers dispatcher cleanup)
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: attachResp.LinkId})
	require.NoError(t, err)

	// Verify DetachTCFilter was called
	tcDetaches := fix.Kernel.TCDetaches()
	require.Len(t, tcDetaches, 1, "should have 1 TC filter detach")
	assert.Equal(t, 2, tcDetaches[0].ifindex, "should detach from eth0 (ifindex 2)")
	assert.Equal(t, uint32(0xFFFFFFF2), tcDetaches[0].parent, "should use HANDLE_MIN_INGRESS")
	assert.Equal(t, uint16(50), tcDetaches[0].priority, "should use priority 50")

	// TC filter should be removed
	assert.Equal(t, 0, fix.Kernel.TCFilterCount(), "TC filter should be removed")
}

// TestTC_PinPathConventions verifies that dispatcher cleanup removes
// pins at paths matching the convention defined in dispatcher/paths.go.
func TestTC_PinPathConventions(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadResp, err := fix.Server.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_pass", ProgramType: pb.BpfmanProgramType_TC},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "tc-pin-path-test",
		},
	})
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach a single extension
	attachResp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
				},
			},
		},
	})
	require.NoError(t, err)

	// Get dispatcher state before detaching
	dispatchers, err := fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	require.Len(t, dispatchers, 1)
	disp := dispatchers[0]

	// Detach (triggers full dispatcher cleanup)
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: attachResp.LinkId})
	require.NoError(t, err)

	// Verify RemovePin was called with convention-derived paths
	removedPins := fix.Kernel.RemovedPins()

	bpffsRoot := fix.Root.BPFFS().FS()
	revisionDir := dispatcher.DispatcherRevisionDir(bpffsRoot, disp.Type, disp.Nsid, disp.Ifindex, disp.Revision)
	expectedProgPin := dispatcher.DispatcherProgPath(revisionDir)

	// TC dispatchers do not have a link pin (they use netlink); only
	// the prog pin and revision directory should be removed.
	assert.Contains(t, removedPins, expectedProgPin,
		"should remove prog pin at %s", expectedProgPin)
	assert.Contains(t, removedPins, revisionDir,
		"should remove revision dir at %s", revisionDir)

	// Verify the revision dir is under the correct type directory
	typeDir := dispatcher.TypeDir(bpffsRoot, dispatcher.DispatcherTypeTCIngress)
	assert.True(t, strings.HasPrefix(revisionDir, typeDir),
		"revision dir %s should be under %s", revisionDir, typeDir)
}
