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
// XDP Dispatcher Lifecycle Tests
// =============================================================================
//
// These tests verify the XDP dispatcher lifecycle, similar to the integration
// test in integration-tests/test-dispatcher-cleanup.sh but using a fake kernel
// and network interface resolver.

// TestXDPDispatcher_FirstAttachCreatesDispatcher verifies that:
//
//	Given a loaded XDP program,
//	When I attach it to an interface for the first time,
//	Then a dispatcher is created,
//	And the extension count is 1.
func TestXDPDispatcher_FirstAttachCreatesDispatcher(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-dispatcher-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach to interface (using fake "lo" interface)
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_XdpAttachInfo{
				XdpAttachInfo: &pb.XDPAttachInfo{
					Iface: "lo",
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "AttachXDP should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestXDPDispatcher_MultipleAttachesCreateMultipleLinks verifies that:
//
//	Given a loaded XDP program,
//	When I attach it multiple times to the same interface,
//	Then multiple links are created.
func TestXDPDispatcher_MultipleAttachesCreateMultipleLinks(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-multi-attach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach multiple times
	var linkIDs []uint32
	for i := 0; i < 3; i++ {
		attachReq := &pb.AttachRequest{
			Id: programID,
			Attach: &pb.AttachInfo{
				Info: &pb.AttachInfo_XdpAttachInfo{
					XdpAttachInfo: &pb.XDPAttachInfo{
						Iface: "lo",
					},
				},
			},
		}
		attachResp, err := fix.Server.Attach(ctx, attachReq)
		require.NoError(t, err, "AttachXDP %d should succeed", i+1)
		linkIDs = append(linkIDs, attachResp.LinkId)
	}

	// Verify we have 3 links
	assert.Equal(t, 3, fix.Kernel.LinkCount(), "should have 3 links in kernel")
	assert.Len(t, linkIDs, 3, "should have collected 3 link IDs")
}

// TestXDPDispatcher_DetachDecrementsLinkCount verifies that:
//
//	Given a program with multiple XDP attachments,
//	When I detach one link,
//	Then the link count decrements,
//	And remaining links are still valid.
func TestXDPDispatcher_DetachDecrementsLinkCount(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load and attach twice
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-detach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_XdpAttachInfo{
				XdpAttachInfo: &pb.XDPAttachInfo{
					Iface: "lo",
				},
			},
		},
	}

	attach1, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "First attach should succeed")

	attach2, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Second attach should succeed")

	// Verify we have 2 links
	assert.Equal(t, 2, fix.Kernel.LinkCount(), "should have 2 links")

	// Detach first link
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: attach1.LinkId})
	require.NoError(t, err, "Detach first link should succeed")

	// Should have 1 link remaining
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link after first detach")

	// Detach second link
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: attach2.LinkId})
	require.NoError(t, err, "Detach second link should succeed")

	// Should have no links
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after second detach")
}

// TestXDPDispatcher_FullLifecycle verifies the complete dispatcher lifecycle:
//
//  1. Load XDP program
//  2. Attach multiple times
//  3. Detach all links one by one
//  4. Unload program
//  5. Verify clean state
//
// This mirrors the integration test in test-dispatcher-cleanup.sh.
func TestXDPDispatcher_FullLifecycle(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load XDP program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-lifecycle-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id
	t.Logf("Step 1: Loaded program ID %d", programID)

	// Step 2: Attach multiple times (simulate filling dispatcher slots)
	numAttachments := 5
	var linkIDs []uint32
	for i := 0; i < numAttachments; i++ {
		attachReq := &pb.AttachRequest{
			Id: programID,
			Attach: &pb.AttachInfo{
				Info: &pb.AttachInfo_XdpAttachInfo{
					XdpAttachInfo: &pb.XDPAttachInfo{
						Iface: "lo",
					},
				},
			},
		}
		attachResp, err := fix.Server.Attach(ctx, attachReq)
		require.NoError(t, err, "Attach %d should succeed", i+1)
		linkIDs = append(linkIDs, attachResp.LinkId)
		t.Logf("Step 2: Attached link %d (kernel ID %d)", i+1, attachResp.LinkId)
	}

	// Verify state after attachments
	// 2 programs: 1 user XDP program + 1 XDP dispatcher program
	assert.Equal(t, 2, fix.Kernel.ProgramCount(), "should have 2 programs (user + dispatcher)")
	assert.Equal(t, numAttachments, fix.Kernel.LinkCount(), "should have %d links", numAttachments)

	// Step 3: Detach all links one by one
	for i, linkID := range linkIDs {
		_, err := fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkID})
		require.NoError(t, err, "Detach link %d should succeed", linkID)
		expectedLinks := numAttachments - i - 1
		assert.Equal(t, expectedLinks, fix.Kernel.LinkCount(),
			"should have %d links after detaching link %d", expectedLinks, i+1)
		t.Logf("Step 3: Detached link %d, remaining links: %d", linkID, expectedLinks)
	}

	// Step 4: Verify no links remain
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after all detaches")

	// Step 5: Unload program
	_, err = fix.Server.Unload(ctx, &pb.UnloadRequest{Id: programID})
	require.NoError(t, err, "Unload should succeed")
	t.Logf("Step 4: Unloaded program %d", programID)

	// Step 6: Verify clean state
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "should have 0 programs")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links")

	// Verify database is clean
	listResp, err := fix.Server.List(ctx, &pb.ListRequest{})
	require.NoError(t, err, "List should succeed")
	assert.Empty(t, listResp.Results, "should have 0 programs in database")

	t.Log("Step 5: Verified clean state - test passed")
}

// TestXDP_ExtensionPositionsAreSequential verifies that multiple XDP
// extensions on the same interface get sequential positions derived
// from CountDispatcherLinks rather than cached state.
func TestXDP_ExtensionPositionsAreSequential(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadResp, err := fix.Server.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-position-test",
		},
	})
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	var linkIDs []uint32
	for i := 0; i < 3; i++ {
		attachResp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
			Id: programID,
			Attach: &pb.AttachInfo{
				Info: &pb.AttachInfo_XdpAttachInfo{
					XdpAttachInfo: &pb.XDPAttachInfo{
						Iface: "lo",
					},
				},
			},
		})
		require.NoError(t, err, "attach %d should succeed", i)
		linkIDs = append(linkIDs, attachResp.LinkId)
	}

	for i, linkID := range linkIDs {
		record, err := fix.Store.GetLink(ctx, bpfman.LinkID(linkID))
		require.NoError(t, err)
		xdpDetails, ok := record.Details.(bpfman.XDPDetails)
		require.True(t, ok, "expected XDPDetails, got %T", record.Details)
		assert.Equal(t, int32(i), xdpDetails.Position,
			"link %d should have position %d", linkID, i)
	}
}

// TestXDP_DispatcherStateInStore verifies that the store tracks
// dispatcher state and cleans it up when the last extension is
// detached.
func TestXDP_DispatcherStateInStore(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadResp, err := fix.Server.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-disp-state-test",
		},
	})
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attach two extensions
	var linkIDs []uint32
	for i := 0; i < 2; i++ {
		resp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
			Id: programID,
			Attach: &pb.AttachInfo{
				Info: &pb.AttachInfo_XdpAttachInfo{
					XdpAttachInfo: &pb.XDPAttachInfo{
						Iface: "lo",
					},
				},
			},
		})
		require.NoError(t, err)
		linkIDs = append(linkIDs, resp.LinkId)
	}

	// Verify dispatcher state
	dispatchers, err := fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	require.Len(t, dispatchers, 1)
	assert.Equal(t, dispatcher.DispatcherTypeXDP, dispatchers[0].Type)
	assert.Equal(t, uint32(1), dispatchers[0].Ifindex) // lo = ifindex 1

	count, err := fix.Store.CountDispatcherLinks(ctx, dispatchers[0].KernelID)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Detach first — dispatcher should still exist
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkIDs[0]})
	require.NoError(t, err)

	dispatchers, err = fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	assert.Len(t, dispatchers, 1, "dispatcher should still exist with 1 extension")

	// Detach second — dispatcher should be cleaned up
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkIDs[1]})
	require.NoError(t, err)

	dispatchers, err = fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	assert.Empty(t, dispatchers, "dispatcher should be removed after last extension detached")
}

// TestXDP_PinPathConventions verifies that dispatcher cleanup removes
// pins at convention-derived paths, including the link pin (XDP
// dispatchers use BPF links, unlike TC which uses netlink).
func TestXDP_PinPathConventions(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	loadResp, err := fix.Server.Load(ctx, &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-pin-path-test",
		},
	})
	require.NoError(t, err)
	programID := loadResp.Programs[0].KernelInfo.Id

	attachResp, err := fix.Server.Attach(ctx, &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_XdpAttachInfo{
				XdpAttachInfo: &pb.XDPAttachInfo{
					Iface: "lo",
				},
			},
		},
	})
	require.NoError(t, err)

	// Capture dispatcher state before cleanup
	dispatchers, err := fix.Store.ListDispatchers(ctx)
	require.NoError(t, err)
	require.Len(t, dispatchers, 1)
	disp := dispatchers[0]

	// Detach (triggers full dispatcher cleanup)
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: attachResp.LinkId})
	require.NoError(t, err)

	removedPins := fix.Kernel.RemovedPins()
	bpffsRoot := fix.Dirs.FS()

	revisionDir := dispatcher.DispatcherRevisionDir(bpffsRoot, disp.Type, disp.Nsid, disp.Ifindex, disp.Revision)
	expectedProgPin := dispatcher.DispatcherProgPath(revisionDir)
	expectedLinkPin := dispatcher.DispatcherLinkPath(bpffsRoot, disp.Type, disp.Nsid, disp.Ifindex)

	// XDP dispatchers should have link pin, prog pin, and revision dir removed
	assert.Contains(t, removedPins, expectedLinkPin,
		"should remove link pin at %s", expectedLinkPin)
	assert.Contains(t, removedPins, expectedProgPin,
		"should remove prog pin at %s", expectedProgPin)
	assert.Contains(t, removedPins, revisionDir,
		"should remove revision dir at %s", revisionDir)

	typeDir := dispatcher.TypeDir(bpffsRoot, dispatcher.DispatcherTypeXDP)
	assert.True(t, strings.HasPrefix(revisionDir, typeDir),
		"revision dir %s should be under %s", revisionDir, typeDir)
}

// TestXDP_AttachToNonExistentInterface verifies that:
//
//	Given a loaded XDP program,
//	When I try to attach it to a non-existent interface,
//	Then the operation fails with an appropriate error.
func TestXDP_AttachToNonExistentInterface(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_pass", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-nonexistent-iface-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	programID := loadResp.Programs[0].KernelInfo.Id

	// Attempt to attach to non-existent interface
	attachReq := &pb.AttachRequest{
		Id: programID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_XdpAttachInfo{
				XdpAttachInfo: &pb.XDPAttachInfo{
					Iface: "nonexistent0",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach to non-existent interface should fail")
	assert.Contains(t, err.Error(), "not found", "error should mention interface not found")

	// Program should still be loaded
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "program should still be loaded")
	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}
