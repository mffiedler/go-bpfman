package server_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/frobware/go-bpfman/server/pb"
)

// ----------------------------------------------------------------------------
// Map Sharing Tests
// ----------------------------------------------------------------------------

// TestMapSharing_MultiProgramLoad_FirstIsOwner verifies that:
//
//	Given a Load request with multiple programs (like from an OCI image),
//	When all programs are successfully loaded,
//	Then the first program has no MapOwnerID (it owns the maps),
//	And subsequent programs have MapOwnerID set to the first program's ID,
//	And all programs share the same MapPinPath.
func TestMapSharing_MultiProgramLoad_FirstIsOwner(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("multi.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "kprobe_counter", ProgramType: pb.BpfmanProgramType_KPROBE},
			{Name: "tracepoint_counter", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
			{Name: "xdp_stats", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "multi-prog-image",
		},
	}

	resp, err := fix.Server.Load(ctx, req)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, resp.Programs, 3, "expected 3 programs")

	// Get detailed info for each program
	ownerID := resp.Programs[0].KernelInfo.Id
	ownerResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: ownerID})
	require.NoError(t, err, "Get owner failed")

	// First program is the map owner - MapOwnerId should be 0 (not set)
	assert.Zero(t, ownerResp.Info.MapOwnerId, "first program should have no MapOwnerId (it owns the maps)")
	assert.NotEmpty(t, ownerResp.Info.MapPinPath, "first program should have MapPinPath set")
	ownerMapPinPath := ownerResp.Info.MapPinPath

	// Check subsequent programs
	for i := 1; i < len(resp.Programs); i++ {
		progID := resp.Programs[i].KernelInfo.Id
		progResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: progID})
		require.NoError(t, err, "Get program %d failed", i)

		// Subsequent programs should reference the owner
		assert.Equal(t, ownerID, progResp.Info.GetMapOwnerId(),
			"program %d should have MapOwnerId set to owner's ID", i)
		// All programs share the same maps directory
		assert.Equal(t, ownerMapPinPath, progResp.Info.MapPinPath,
			"program %d should share owner's MapPinPath", i)
	}
}

// TestMapSharing_SingleProgram_NoMapOwner verifies that:
//
//	Given a Load request with a single program,
//	When it is successfully loaded,
//	Then MapOwnerID is 0 (it owns its own maps).
func TestMapSharing_SingleProgram_NoMapOwner(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("single.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "single_prog", ProgramType: pb.BpfmanProgramType_KPROBE},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "single-program",
		},
	}

	resp, err := fix.Server.Load(ctx, req)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, resp.Programs, 1, "expected 1 program")

	progID := resp.Programs[0].KernelInfo.Id
	getResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: progID})
	require.NoError(t, err, "Get failed")

	// Single program owns its own maps
	assert.Zero(t, getResp.Info.MapOwnerId, "single program should have no MapOwnerID")
	assert.NotEmpty(t, getResp.Info.MapPinPath, "single program should have MapPinPath set")
}

// TestMapSharing_XDPAttach_UsesMapPinPath verifies that:
//
//	Given a loaded XDP program,
//	When it is attached to an interface,
//	Then the kernel receives the program's MapPinPath (not computed from kernel ID).
func TestMapSharing_XDPAttach_UsesMapPinPath(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("xdp.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_prog", ProgramType: pb.BpfmanProgramType_XDP},
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 1)

	progID := loadResp.Programs[0].KernelInfo.Id

	// Get the program's MapPinPath
	getResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: progID})
	require.NoError(t, err, "Get should succeed")
	expectedMapPinPath := getResp.Info.MapPinPath
	require.NotEmpty(t, expectedMapPinPath, "MapPinPath should be set")

	// Attach the program
	attachReq := &pb.AttachRequest{
		Id: progID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_XdpAttachInfo{
				XdpAttachInfo: &pb.XDPAttachInfo{
					Iface: "eth0",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the correct MapPinDir
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one XDP extension attach")
	assert.Equal(t, "attach-xdp-ext", extOps[0].Op)
	assert.Equal(t, expectedMapPinPath, extOps[0].MapPinDir,
		"XDP attach should use the program's MapPinPath")
}

// TestMapSharing_TCAttach_UsesMapPinPath verifies that:
//
//	Given a loaded TC program,
//	When it is attached to an interface,
//	Then the kernel receives the program's MapPinPath (not computed from kernel ID).
func TestMapSharing_TCAttach_UsesMapPinPath(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tc.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tc_prog", ProgramType: pb.BpfmanProgramType_TC},
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 1)

	progID := loadResp.Programs[0].KernelInfo.Id

	// Get the program's MapPinPath
	getResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: progID})
	require.NoError(t, err, "Get should succeed")
	expectedMapPinPath := getResp.Info.MapPinPath
	require.NotEmpty(t, expectedMapPinPath, "MapPinPath should be set")

	// Attach the program
	attachReq := &pb.AttachRequest{
		Id: progID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
					Iface:     "eth0",
					Priority:  50,
					Direction: "ingress",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the correct MapPinDir
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one TC extension attach")
	assert.Equal(t, "attach-tc-ext", extOps[0].Op)
	assert.Equal(t, expectedMapPinPath, extOps[0].MapPinDir,
		"TC attach should use the program's MapPinPath")
}

// TestMapSharing_MultiProgram_XDPAttach_UsesOwnerMapPinPath verifies that:
//
//	Given a multi-program load where the second program has MapOwnerID set,
//	When the second (XDP) program is attached,
//	Then the kernel receives the map owner's MapPinPath (shared maps directory).
func TestMapSharing_MultiProgram_XDPAttach_UsesOwnerMapPinPath(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load multiple programs - first is owner, second is XDP that shares maps
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("multi.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "kprobe_counter", ProgramType: pb.BpfmanProgramType_KPROBE},
			{Name: "xdp_stats", ProgramType: pb.BpfmanProgramType_XDP},
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 2)

	ownerID := loadResp.Programs[0].KernelInfo.Id
	xdpProgID := loadResp.Programs[1].KernelInfo.Id

	// Get the owner's MapPinPath
	ownerResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: ownerID})
	require.NoError(t, err, "Get owner should succeed")
	ownerMapPinPath := ownerResp.Info.MapPinPath
	require.NotEmpty(t, ownerMapPinPath, "owner should have MapPinPath")

	// Verify the XDP program has MapOwnerID set and same MapPinPath
	xdpResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: xdpProgID})
	require.NoError(t, err, "Get XDP program should succeed")
	assert.Equal(t, ownerID, xdpResp.Info.GetMapOwnerId(),
		"XDP program should reference the owner")
	assert.Equal(t, ownerMapPinPath, xdpResp.Info.MapPinPath,
		"XDP program should have same MapPinPath as owner")

	// Attach the XDP program
	attachReq := &pb.AttachRequest{
		Id: xdpProgID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_XdpAttachInfo{
				XdpAttachInfo: &pb.XDPAttachInfo{
					Iface: "eth0",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the owner's MapPinPath, not the XDP program's kernel ID
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one XDP extension attach")
	assert.Equal(t, "attach-xdp-ext", extOps[0].Op)
	assert.Equal(t, ownerMapPinPath, extOps[0].MapPinDir,
		"XDP attach should use the owner's MapPinPath, not compute from kernel ID")
}

// TestMapSharing_MultiProgram_TCAttach_UsesOwnerMapPinPath verifies that:
//
//	Given a multi-program load where the second program has MapOwnerID set,
//	When the second (TC) program is attached,
//	Then the kernel receives the map owner's MapPinPath (shared maps directory).
func TestMapSharing_MultiProgram_TCAttach_UsesOwnerMapPinPath(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load multiple programs - first is owner, second is TC that shares maps
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("multi.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "kprobe_counter", ProgramType: pb.BpfmanProgramType_KPROBE},
			{Name: "tc_stats", ProgramType: pb.BpfmanProgramType_TC},
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 2)

	ownerID := loadResp.Programs[0].KernelInfo.Id
	tcProgID := loadResp.Programs[1].KernelInfo.Id

	// Get the owner's MapPinPath
	ownerResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: ownerID})
	require.NoError(t, err, "Get owner should succeed")
	ownerMapPinPath := ownerResp.Info.MapPinPath
	require.NotEmpty(t, ownerMapPinPath, "owner should have MapPinPath")

	// Verify the TC program has MapOwnerID set and same MapPinPath
	tcResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: tcProgID})
	require.NoError(t, err, "Get TC program should succeed")
	assert.Equal(t, ownerID, tcResp.Info.GetMapOwnerId(),
		"TC program should reference the owner")
	assert.Equal(t, ownerMapPinPath, tcResp.Info.MapPinPath,
		"TC program should have same MapPinPath as owner")

	// Attach the TC program
	attachReq := &pb.AttachRequest{
		Id: tcProgID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcAttachInfo{
				TcAttachInfo: &pb.TCAttachInfo{
					Iface:     "eth0",
					Priority:  50,
					Direction: "ingress",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the owner's MapPinPath, not the TC program's kernel ID
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one TC extension attach")
	assert.Equal(t, "attach-tc-ext", extOps[0].Op)
	assert.Equal(t, ownerMapPinPath, extOps[0].MapPinDir,
		"TC attach should use the owner's MapPinPath, not compute from kernel ID")
}

// TestTCX_AttachUsesProgramPinPath verifies that:
//
//	Given a loaded TCX program,
//	When it is attached to an interface,
//	Then the kernel receives the program's PinPath (not derived from MapPinPath).
func TestTCX_AttachUsesProgramPinPath(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: fix.BytecodeFile("tcx.o")},
		},
		Info: []*pb.LoadInfo{
			{Name: "tcx_prog", ProgramType: pb.BpfmanProgramType_TCX},
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 1)

	progID := loadResp.Programs[0].KernelInfo.Id

	// The expected pin path follows the pattern: <fsRoot>/prog_<kernelID>
	// The fake kernel uses bpffsRoot + "/prog_" + id
	expectedPinPath := fmt.Sprintf("%s/prog_%d", fix.Dirs.FS(), progID)

	// Attach the program
	attachReq := &pb.AttachRequest{
		Id: progID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TcxAttachInfo{
				TcxAttachInfo: &pb.TCXAttachInfo{
					Iface:     "eth0",
					Direction: "ingress",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the correct programPinPath
	tcxOps := fix.Kernel.TCXAttachOps()
	require.Len(t, tcxOps, 1, "expected one TCX attach")
	assert.Equal(t, "attach-tcx", tcxOps[0].Op)
	assert.Equal(t, expectedPinPath, tcxOps[0].Name,
		"TCX attach should use prog.PinPath directly, not derive from MapPinPath")
}
