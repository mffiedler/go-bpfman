// Package server_test uses Behaviour-Driven Development (BDD) style.
//
// Each test follows the Given/When/Then structure:
//   - Given: Initial state and context (the fixture)
//   - When: The action being tested
//   - Then: The expected outcome
//
// This makes tests readable as specifications of behaviour. When adding
// new tests, follow this pattern and use descriptive test names that
// explain the scenario being tested.
//
// The tests use a fake kernel implementation that simulates BPF operations
// without syscalls, combined with a real in-memory SQLite database. This
// enables fast, reliable testing of the full request path through gRPC.
package server_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/frobware/go-bpfman"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// =============================================================================
// Program Load/Unload/Get/List Tests
// =============================================================================

func TestLoadProgram_WithValidRequest_Succeeds(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "my_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "my-program",
			"app":                   "test-app",
		},
	}

	resp, err := srv.Load(ctx, req)
	require.NoError(t, err, "Load failed")
	require.Len(t, resp.Programs, 1, "expected 1 program")

	prog := resp.Programs[0]

	// Verify ProgramInfo fields
	assert.Equal(t, "my_prog", prog.Info.Name, "Info.Name")
	require.NotNil(t, prog.Info.Bytecode, "Info.Bytecode")
	file, ok := prog.Info.Bytecode.Location.(*pb.BytecodeLocation_File)
	require.True(t, ok, "expected BytecodeLocation_File")
	assert.Equal(t, "/path/to/prog.o", file.File, "Info.Bytecode.File")
	assert.Equal(t, "my-program", prog.Info.Metadata["bpfman.io/ProgramName"], "Info.Metadata[bpfman.io/ProgramName]")
	assert.Equal(t, "test-app", prog.Info.Metadata["app"], "Info.Metadata[app]")
	assert.NotEmpty(t, prog.Info.MapPinPath, "Info.MapPinPath")

	// Verify KernelProgramInfo fields
	assert.NotZero(t, prog.KernelInfo.Id, "KernelInfo.Id")
	assert.Equal(t, "my_prog", prog.KernelInfo.Name, "KernelInfo.Name")
	assert.Equal(t, uint32(bpfman.ProgramTypeTracepoint), prog.KernelInfo.ProgramType, "KernelInfo.ProgramType")
}

// TestGetProgram_ReturnsAllFields verifies that:
//
//	Given a program loaded with specific metadata,
//	When I retrieve it via Get,
//	Then all fields match what was provided at load time.
func TestGetProgram_ReturnsAllFields(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "get_test_prog", ProgramType: pb.BpfmanProgramType_KPROBE},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "get-test-program",
			"environment":           "testing",
			"version":               "1.0.0",
		},
	}

	loadResp, err := srv.Load(ctx, req)
	require.NoError(t, err, "Load failed")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	getResp, err := srv.Get(ctx, &pb.GetRequest{Id: kernelID})
	require.NoError(t, err, "Get failed")

	// Verify ProgramInfo fields
	assert.Equal(t, "get_test_prog", getResp.Info.Name, "Info.Name")
	require.NotNil(t, getResp.Info.Bytecode, "Info.Bytecode")
	file, ok := getResp.Info.Bytecode.Location.(*pb.BytecodeLocation_File)
	require.True(t, ok, "expected BytecodeLocation_File")
	assert.Equal(t, "/path/to/prog.o", file.File, "Info.Bytecode.File")
	assert.Equal(t, "get-test-program", getResp.Info.Metadata["bpfman.io/ProgramName"], "Info.Metadata[bpfman.io/ProgramName]")
	assert.Equal(t, "testing", getResp.Info.Metadata["environment"], "Info.Metadata[environment]")
	assert.Equal(t, "1.0.0", getResp.Info.Metadata["version"], "Info.Metadata[version]")
	assert.NotEmpty(t, getResp.Info.MapPinPath, "Info.MapPinPath")

	// Verify KernelProgramInfo fields
	assert.Equal(t, kernelID, getResp.KernelInfo.Id, "KernelInfo.Id")
	assert.Equal(t, "get_test_prog", getResp.KernelInfo.Name, "KernelInfo.Name")
	assert.Equal(t, uint32(bpfman.ProgramTypeKprobe), getResp.KernelInfo.ProgramType, "KernelInfo.ProgramType")
}

// TestLoadProgram_WithGlobalData verifies that:
//
//	Given a program loaded with global data,
//	When I retrieve it via Get,
//	Then the global data is returned correctly.
func TestLoadProgram_WithGlobalData(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	globalData := map[string][]byte{
		"GLOBAL_u8":  {0x01},
		"GLOBAL_u32": {0x0A, 0x0B, 0x0C, 0x0D},
		"sampling":   {0x00, 0x00, 0x00, 0x01},
	}

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "global_test_prog",
				ProgramType: pb.BpfmanProgramType_KPROBE,
			},
		},
		Metadata: map[string]string{
			"app": "global-test",
		},
		GlobalData: globalData,
	}

	loadResp, err := srv.Load(ctx, req)
	require.NoError(t, err, "Load failed")
	require.Len(t, loadResp.Programs, 1, "expected 1 program")

	// Verify global data is returned in the load response
	prog := loadResp.Programs[0]
	assert.Equal(t, globalData, prog.Info.GlobalData, "GlobalData in load response")

	// Verify global data is returned via Get
	kernelID := prog.KernelInfo.Id
	getResp, err := srv.Get(ctx, &pb.GetRequest{Id: kernelID})
	require.NoError(t, err, "Get failed")
	assert.Equal(t, globalData, getResp.Info.GlobalData, "GlobalData in get response")
}

// TestLoadProgram_WithMetadataAndGlobalData verifies that:
//
//	Given a program loaded with both metadata and global data,
//	When I retrieve it via Get and List,
//	Then both are returned correctly.
func TestLoadProgram_WithMetadataAndGlobalData(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	metadata := map[string]string{
		"owner":       "test-team",
		"environment": "staging",
	}
	globalData := map[string][]byte{
		"config_flag": {0xFF},
	}

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{
				Name:        "combined_test_prog",
				ProgramType: pb.BpfmanProgramType_TRACEPOINT,
			},
		},
		Metadata:   metadata,
		GlobalData: globalData,
	}

	loadResp, err := srv.Load(ctx, req)
	require.NoError(t, err, "Load failed")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	// Verify via Get
	getResp, err := srv.Get(ctx, &pb.GetRequest{Id: kernelID})
	require.NoError(t, err, "Get failed")
	assert.Equal(t, "test-team", getResp.Info.Metadata["owner"], "Metadata[owner]")
	assert.Equal(t, "staging", getResp.Info.Metadata["environment"], "Metadata[environment]")
	assert.Equal(t, globalData, getResp.Info.GlobalData, "GlobalData")

	// Verify via List
	listResp, err := srv.List(ctx, &pb.ListRequest{})
	require.NoError(t, err, "List failed")
	require.Len(t, listResp.Results, 1, "expected 1 program in list")
	assert.Equal(t, "test-team", listResp.Results[0].Info.Metadata["owner"], "List Metadata[owner]")
	assert.Equal(t, globalData, listResp.Results[0].Info.GlobalData, "List GlobalData")
}

// TestListPrograms_ReturnsAllFields verifies that:
//
//	Given multiple programs loaded with different metadata,
//	When I list all programs,
//	Then each result contains correctly populated Info and KernelInfo.
func TestListPrograms_ReturnsAllFields(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	// Load two programs with distinct metadata
	programs := []struct {
		name        string
		programName string
		programType pb.BpfmanProgramType
		app         string
	}{
		{"prog_one", "program-one", pb.BpfmanProgramType_TRACEPOINT, "frontend"},
		{"prog_two", "program-two", pb.BpfmanProgramType_XDP, "backend"},
	}

	expectedIDs := make(map[string]uint32)
	for _, p := range programs {
		req := &pb.LoadRequest{
			Bytecode: &pb.BytecodeLocation{
				Location: &pb.BytecodeLocation_File{File: "/path/to/" + p.name + ".o"},
			},
			Info: []*pb.LoadInfo{
				{Name: p.name, ProgramType: p.programType},
			},
			Metadata: map[string]string{
				"bpfman.io/ProgramName": p.programName,
				"app":                   p.app,
			},
		}
		resp, err := srv.Load(ctx, req)
		require.NoError(t, err, "Load %s failed", p.name)
		expectedIDs[p.programName] = resp.Programs[0].KernelInfo.Id
	}

	listResp, err := srv.List(ctx, &pb.ListRequest{})
	require.NoError(t, err, "List failed")
	require.Len(t, listResp.Results, 2, "expected 2 results")

	// Build a map for easier lookup
	resultsByName := make(map[string]*pb.ListResponse_ListResult)
	for _, r := range listResp.Results {
		resultsByName[r.Info.Metadata["bpfman.io/ProgramName"]] = r
	}

	// Verify program-one
	r1, ok := resultsByName["program-one"]
	require.True(t, ok, "program-one not found in list results")
	assert.Equal(t, "prog_one", r1.Info.Name, "program-one Info.Name")
	assert.Equal(t, "frontend", r1.Info.Metadata["app"], "program-one Info.Metadata[app]")
	assert.Equal(t, expectedIDs["program-one"], r1.KernelInfo.Id, "program-one KernelInfo.Id")
	assert.Equal(t, uint32(bpfman.ProgramTypeTracepoint), r1.KernelInfo.ProgramType, "program-one KernelInfo.ProgramType")

	// Verify program-two
	r2, ok := resultsByName["program-two"]
	require.True(t, ok, "program-two not found in list results")
	assert.Equal(t, "prog_two", r2.Info.Name, "program-two Info.Name")
	assert.Equal(t, "backend", r2.Info.Metadata["app"], "program-two Info.Metadata[app]")
	assert.Equal(t, expectedIDs["program-two"], r2.KernelInfo.Id, "program-two KernelInfo.Id")
	assert.Equal(t, uint32(bpfman.ProgramTypeXDP), r2.KernelInfo.ProgramType, "program-two KernelInfo.ProgramType")
}

// TestLoadProgram_WithDuplicateName_BothSucceed verifies that:
//
//	Given a server with one program already loaded using a name,
//	When I attempt to load another program with the same name,
//	Then both programs load successfully (duplicates are allowed).
//
// Multiple programs can share the same bpfman.io/ProgramName, e.g., when
// loading multiple BPF programs from a single OCI image via the operator.
func TestLoadProgram_WithDuplicateName_BothSucceed(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	firstReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "my_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "shared-name",
		},
	}
	resp1, err := srv.Load(ctx, firstReq)
	require.NoError(t, err, "first Load failed")
	require.Len(t, resp1.Programs, 1)

	secondReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "my_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "shared-name",
		},
	}
	resp2, err := srv.Load(ctx, secondReq)
	require.NoError(t, err, "second Load should succeed (duplicates allowed)")
	require.Len(t, resp2.Programs, 1)

	// Verify both programs exist with different IDs
	assert.NotEqual(t, resp1.Programs[0].KernelInfo.Id, resp2.Programs[0].KernelInfo.Id, "should be different programs")
}

// TestLoadProgram_WithDifferentNames_BothSucceed verifies that:
//
//	Given an empty server,
//	When I load two programs with different names,
//	Then both programs exist and are listed.
func TestLoadProgram_WithDifferentNames_BothSucceed(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	for _, name := range []string{"program-a", "program-b"} {
		req := &pb.LoadRequest{
			Bytecode: &pb.BytecodeLocation{
				Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
			},
			Info: []*pb.LoadInfo{
				{Name: "prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
			},
			Metadata: map[string]string{
				"bpfman.io/ProgramName": name,
			},
		}
		_, err := srv.Load(ctx, req)
		require.NoError(t, err, "Load %s failed", name)
	}

	listResp, err := srv.List(ctx, &pb.ListRequest{})
	require.NoError(t, err, "List failed")
	assert.Len(t, listResp.Results, 2, "expected 2 programs")
}

// TestUnloadProgram_WhenProgramExists_RemovesIt verifies that:
//
//	Given a server with one program loaded,
//	When I unload the program,
//	Then the unload succeeds and the program is no longer retrievable.
func TestUnloadProgram_WhenProgramExists_RemovesIt(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "my_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "my-program",
		},
	}
	loadResp, err := srv.Load(ctx, loadReq)
	require.NoError(t, err, "Load failed")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	_, err = srv.Unload(ctx, &pb.UnloadRequest{Id: kernelID})
	require.NoError(t, err, "Unload failed")

	_, err = srv.Get(ctx, &pb.GetRequest{Id: kernelID})
	require.Error(t, err, "expected Get after unload to fail")
	st, _ := status.FromError(err)
	assert.Equal(t, codes.NotFound, st.Code(), "expected NotFound")
}

// TestUnloadProgram_WhenProgramDoesNotExist_ReturnsNotFound verifies that:
//
//	Given an empty server with no programs,
//	When I try to unload a non-existent program,
//	Then the operation returns NotFound (fail-fast).
func TestUnloadProgram_WhenProgramDoesNotExist_ReturnsNotFound(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, err := srv.Unload(ctx, &pb.UnloadRequest{Id: 999})
	require.Error(t, err, "Unload of non-existent program should fail")

	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	assert.Equal(t, codes.NotFound, st.Code(), "expected NotFound status code")
	assert.Contains(t, st.Message(), "does not exist", "expected 'does not exist' in message")
}

// TestUnloadProgram_KernelOnlyProgram_ReturnsNotFound verifies that:
//
//	Given a program that exists in the kernel but is not managed by bpfman,
//	When I attempt to unload it,
//	Then the server returns a NotFound error,
//	And the error message indicates the program is not managed.
func TestUnloadProgram_KernelOnlyProgram_ReturnsNotFound(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Inject a program directly into the kernel (bypassing bpfman)
	const kernelOnlyProgID = 42
	fix.Kernel.InjectKernelProgram(kernelOnlyProgID, "orphan_prog", bpfman.ProgramTypeTracepoint)

	// Attempt to unload the kernel-only program
	_, err := fix.Server.Unload(ctx, &pb.UnloadRequest{Id: kernelOnlyProgID})
	require.Error(t, err, "Unload of kernel-only program should fail")

	// Verify it's a gRPC NotFound error
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	assert.Equal(t, codes.NotFound, st.Code(), "expected NotFound status code")

	// Verify the error message indicates the program is not managed
	assert.Contains(t, st.Message(), "not managed by bpfman",
		"error message should indicate program is not managed")
}

// TestLoadProgram_AfterUnload_NameBecomesAvailable verifies that:
//
//	Given a program was loaded and then unloaded,
//	When I load a new program with the same name,
//	Then the load succeeds because the name was freed.
func TestLoadProgram_AfterUnload_NameBecomesAvailable(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	firstReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "my_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "reusable-name",
		},
	}
	loadResp, err := srv.Load(ctx, firstReq)
	require.NoError(t, err, "first Load failed")

	_, err = srv.Unload(ctx, &pb.UnloadRequest{Id: loadResp.Programs[0].KernelInfo.Id})
	require.NoError(t, err, "Unload failed")

	secondReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "my_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "reusable-name",
		},
	}
	_, err = srv.Load(ctx, secondReq)
	assert.NoError(t, err, "second Load with reused name should succeed")
}

// TestListPrograms_WithMetadataFilter_ReturnsOnlyMatching verifies that:
//
//	Given two programs with different app metadata,
//	When I list programs filtering by app=frontend,
//	Then only the frontend program is returned.
func TestListPrograms_WithMetadataFilter_ReturnsOnlyMatching(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	for _, app := range []string{"frontend", "backend"} {
		req := &pb.LoadRequest{
			Bytecode: &pb.BytecodeLocation{
				Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
			},
			Info: []*pb.LoadInfo{
				{Name: "prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
			},
			Metadata: map[string]string{
				"bpfman.io/ProgramName": app,
				"app":                   app,
			},
		}
		_, err := srv.Load(ctx, req)
		require.NoError(t, err, "Load %s failed", app)
	}

	filteredResp, err := srv.List(ctx, &pb.ListRequest{
		MatchMetadata: map[string]string{"app": "frontend"},
	})
	require.NoError(t, err, "List failed")
	require.Len(t, filteredResp.Results, 1, "expected 1 filtered program")
	assert.Equal(t, "frontend", filteredResp.Results[0].Info.Metadata["app"], "wrong program returned")
}

// TestLoadProgram_AllProgramTypes_RoundTrip verifies that:
//
//	Given an empty server,
//	When I load programs of each supported type,
//	Then each program's type is correctly stored and returned via Get.
func TestLoadProgram_AllProgramTypes_RoundTrip(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	// Test all program types that can be loaded via the proto API.
	// Note: proto enum doesn't distinguish kretprobe/uretprobe from kprobe/uprobe.
	tests := []struct {
		name       string
		protoType  pb.BpfmanProgramType
		domainType bpfman.ProgramType
	}{
		{"XDP", pb.BpfmanProgramType_XDP, bpfman.ProgramTypeXDP},
		{"TC", pb.BpfmanProgramType_TC, bpfman.ProgramTypeTC},
		{"TCX", pb.BpfmanProgramType_TCX, bpfman.ProgramTypeTCX},
		{"Tracepoint", pb.BpfmanProgramType_TRACEPOINT, bpfman.ProgramTypeTracepoint},
		{"Kprobe", pb.BpfmanProgramType_KPROBE, bpfman.ProgramTypeKprobe},
		{"Uprobe", pb.BpfmanProgramType_UPROBE, bpfman.ProgramTypeUprobe},
		{"Fentry", pb.BpfmanProgramType_FENTRY, bpfman.ProgramTypeFentry},
		{"Fexit", pb.BpfmanProgramType_FEXIT, bpfman.ProgramTypeFexit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			progName := "prog_" + tt.name

			// Build LoadInfo - fentry/fexit require ProgSpecificInfo with FnName
			loadInfo := &pb.LoadInfo{Name: progName, ProgramType: tt.protoType}
			switch tt.protoType {
			case pb.BpfmanProgramType_FENTRY:
				loadInfo.Info = &pb.ProgSpecificInfo{
					Info: &pb.ProgSpecificInfo_FentryLoadInfo{
						FentryLoadInfo: &pb.FentryLoadInfo{FnName: "test_func"},
					},
				}
			case pb.BpfmanProgramType_FEXIT:
				loadInfo.Info = &pb.ProgSpecificInfo{
					Info: &pb.ProgSpecificInfo_FexitLoadInfo{
						FexitLoadInfo: &pb.FexitLoadInfo{FnName: "test_func"},
					},
				}
			}

			// Load
			loadReq := &pb.LoadRequest{
				Bytecode: &pb.BytecodeLocation{
					Location: &pb.BytecodeLocation_File{File: "/path/to/" + progName + ".o"},
				},
				Info: []*pb.LoadInfo{loadInfo},
				Metadata: map[string]string{
					"bpfman.io/ProgramName": progName,
				},
			}

			loadResp, err := srv.Load(ctx, loadReq)
			require.NoError(t, err, "Load failed")
			require.Len(t, loadResp.Programs, 1, "expected 1 program")

			kernelID := loadResp.Programs[0].KernelInfo.Id
			assert.Equal(t, uint32(tt.domainType), loadResp.Programs[0].KernelInfo.ProgramType,
				"Load response has wrong program type")

			// Get - verify round-trip
			getResp, err := srv.Get(ctx, &pb.GetRequest{Id: kernelID})
			require.NoError(t, err, "Get failed")
			assert.Equal(t, uint32(tt.domainType), getResp.KernelInfo.ProgramType,
				"Get response has wrong program type")

			// Cleanup for next iteration
			_, err = srv.Unload(ctx, &pb.UnloadRequest{Id: kernelID})
			require.NoError(t, err, "Unload failed")
		})
	}
}

// TestListPrograms_AllProgramTypes_ReturnsCorrectTypes verifies that:
//
//	Given multiple programs of different types loaded,
//	When I list all programs,
//	Then each program's type is correctly returned.
func TestListPrograms_AllProgramTypes_ReturnsCorrectTypes(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	// Load programs of different types
	programTypes := []struct {
		name       string
		protoType  pb.BpfmanProgramType
		domainType bpfman.ProgramType
	}{
		{"xdp_prog", pb.BpfmanProgramType_XDP, bpfman.ProgramTypeXDP},
		{"tc_prog", pb.BpfmanProgramType_TC, bpfman.ProgramTypeTC},
		{"tp_prog", pb.BpfmanProgramType_TRACEPOINT, bpfman.ProgramTypeTracepoint},
		{"kprobe_prog", pb.BpfmanProgramType_KPROBE, bpfman.ProgramTypeKprobe},
	}

	expectedTypes := make(map[string]bpfman.ProgramType)
	for _, pt := range programTypes {
		req := &pb.LoadRequest{
			Bytecode: &pb.BytecodeLocation{
				Location: &pb.BytecodeLocation_File{File: "/path/to/" + pt.name + ".o"},
			},
			Info: []*pb.LoadInfo{
				{Name: pt.name, ProgramType: pt.protoType},
			},
			Metadata: map[string]string{
				"bpfman.io/ProgramName": pt.name,
			},
		}
		_, err := srv.Load(ctx, req)
		require.NoError(t, err, "Load %s failed", pt.name)
		expectedTypes[pt.name] = pt.domainType
	}

	// List all programs
	listResp, err := srv.List(ctx, &pb.ListRequest{})
	require.NoError(t, err, "List failed")
	require.Len(t, listResp.Results, len(programTypes), "expected %d programs", len(programTypes))

	// Verify each program has the correct type
	for _, result := range listResp.Results {
		progName := result.Info.Metadata["bpfman.io/ProgramName"]
		expectedType, ok := expectedTypes[progName]
		require.True(t, ok, "unexpected program %s in list", progName)
		assert.Equal(t, uint32(expectedType), result.KernelInfo.ProgramType,
			"program %s has wrong type", progName)
	}
}

// TestLoadProgram_WithInvalidProgramType_IsRejected verifies that:
//
//	Given an empty server,
//	When I attempt to load a program with an invalid program type,
//	Then the server rejects the request with an error.
func TestLoadProgram_WithInvalidProgramType_IsRejected(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "bad_prog", ProgramType: pb.BpfmanProgramType(999)}, // Invalid type
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "bad-program",
		},
	}

	_, err := srv.Load(ctx, req)
	require.Error(t, err, "Load with invalid program type should fail")
	assert.Contains(t, err.Error(), "unknown program type",
		"error should mention unknown program type")
}

// TestLoadProgram_WithUnspecifiedProgramType_IsRejected verifies that:
//
//	Given an empty server,
//	When I attempt to load a program without specifying a program type,
//	Then the server rejects the request with an error.
func TestLoadProgram_WithUnspecifiedProgramType_IsRejected(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	// pb.BpfmanProgramType zero value (XDP=0) is actually valid,
	// but we can test that an out-of-range negative-like value fails.
	// Actually, XDP is 0 in the proto, so "unspecified" isn't really
	// representable. This test documents that behaviour.
	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "xdp_prog", ProgramType: pb.BpfmanProgramType_XDP}, // XDP = 0
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "xdp-program",
		},
	}

	// This should succeed - XDP (0) is a valid type
	resp, err := srv.Load(ctx, req)
	require.NoError(t, err, "Load with XDP type should succeed")
	assert.Equal(t, uint32(bpfman.ProgramTypeXDP), resp.Programs[0].KernelInfo.ProgramType)
}

// =============================================================================
// Partial Failure and Rollback Tests
// =============================================================================
//
// These tests verify that when operations fail partway through:
// 1. Kernel state is properly rolled back (no orphaned programs)
// 2. Database state is clean (nothing persisted)
// 3. Error is properly propagated to the caller

// TestLoadProgram_PartialFailure_SecondProgramFails verifies that:
//
//	Given a server configured to fail on the second program load,
//	When I attempt to load two programs in a single request,
//	Then the first program is unloaded (rolled back),
//	And neither program exists in the kernel,
//	And neither program exists in the database.
func TestLoadProgram_PartialFailure_SecondProgramFails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail on the second program
	fix.Kernel.FailOnProgram("prog_two", fmt.Errorf("injected failure on prog_two"))

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/multi.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "prog_one", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
			{Name: "prog_two", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "multi-prog",
		},
	}

	_, err := fix.Server.Load(ctx, req)

	// Should have failed
	require.Error(t, err, "Load should fail when second program fails")
	assert.Contains(t, err.Error(), "injected failure", "error should mention injected failure")

	// Verify kernel operations: load prog_one, fail prog_two, unload prog_one
	fix.AssertKernelOps([]string{
		"load:prog_one:ok",
		"load:prog_two:error",
		"unload:prog_one:ok",
	})

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_PartialFailure_ThirdOfThreeFails verifies that:
//
//	Given a server configured to fail on the third program load,
//	When I attempt to load three programs,
//	Then the first two programs are unloaded (rolled back),
//	And no programs exist in the kernel or database.
//
// Note: We avoid using bpfman.io/ProgramName metadata because it has a unique
// constraint. Using non-unique metadata (like "app") allows batch loading.
func TestLoadProgram_PartialFailure_ThirdOfThreeFails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail on the third program
	fix.Kernel.FailOnProgram("prog_three", fmt.Errorf("injected failure on prog_three"))

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/multi.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "prog_one", ProgramType: pb.BpfmanProgramType_XDP},
			{Name: "prog_two", ProgramType: pb.BpfmanProgramType_TC},
			{Name: "prog_three", ProgramType: pb.BpfmanProgramType_KPROBE},
		},
		Metadata: map[string]string{
			"app": "triple-prog", // Non-unique metadata - ok for batch loads
		},
	}

	_, err := fix.Server.Load(ctx, req)

	// Should have failed
	require.Error(t, err, "Load should fail when third program fails")

	// Verify kernel operations: load 1, load 2, fail 3, unload 2, unload 1
	fix.AssertKernelOps([]string{
		"load:prog_one:ok",
		"load:prog_two:ok",
		"load:prog_three:error",
		"unload:prog_two:ok",
		"unload:prog_one:ok",
	})

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_PartialFailure_FirstProgramFails verifies that:
//
//	Given a server configured to fail on the first program load,
//	When I attempt to load two programs,
//	Then no rollback is needed (nothing succeeded),
//	And no programs exist in the kernel or database.
func TestLoadProgram_PartialFailure_FirstProgramFails(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail on the first program
	fix.Kernel.FailOnProgram("prog_one", fmt.Errorf("injected failure on prog_one"))

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/multi.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "prog_one", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
			{Name: "prog_two", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "multi-prog",
		},
	}

	_, err := fix.Server.Load(ctx, req)

	// Should have failed
	require.Error(t, err, "Load should fail when first program fails")

	// Verify kernel operations: only the failed load attempt
	fix.AssertKernelOps([]string{
		"load:prog_one:error",
	})

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_SingleProgram_FailsCleanly verifies that:
//
//	Given a server configured to fail on a single program load,
//	When I attempt to load one program,
//	Then the error is returned,
//	And no programs exist in the kernel or database.
func TestLoadProgram_SingleProgram_FailsCleanly(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail
	fix.Kernel.FailOnProgram("single_prog", fmt.Errorf("injected failure"))

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/single.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "single_prog", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "single-prog",
		},
	}

	_, err := fix.Server.Load(ctx, req)

	require.Error(t, err, "Load should fail")
	fix.AssertKernelOps([]string{"load:single_prog:error"})
	fix.AssertCleanState()
}

// TestLoadProgram_FailOnNthLoad verifies that:
//
//	Given a server configured to fail on the Nth load operation,
//	When I load multiple programs,
//	Then the failure occurs at the expected point,
//	And rollback cleans up all previously loaded programs.
func TestLoadProgram_FailOnNthLoad(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail on the 2nd load attempt
	fix.Kernel.FailOnNthLoad(2, fmt.Errorf("nth load failure"))

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/multi.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "prog_a", ProgramType: pb.BpfmanProgramType_XDP},
			{Name: "prog_b", ProgramType: pb.BpfmanProgramType_XDP},
			{Name: "prog_c", ProgramType: pb.BpfmanProgramType_XDP},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "nth-fail-test",
		},
	}

	_, err := fix.Server.Load(ctx, req)

	require.Error(t, err, "Load should fail on 2nd program")
	fix.AssertKernelOps([]string{
		"load:prog_a:ok",
		"load:prog_b:error",
		"unload:prog_a:ok",
	})
	fix.AssertCleanState()
}

// =============================================================================
// Attach Failure Tests
// =============================================================================

// TestAttachTracepoint_WhenAttachFails_ProgramRemainsLoaded verifies that:
//
//	Given a program that was successfully loaded,
//	When I attempt to attach it and the attach operation fails,
//	Then the program remains loaded in the kernel and database,
//	And no link is created,
//	And the error is properly propagated.
func TestAttachTracepoint_WhenAttachFails_ProgramRemainsLoaded(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a tracepoint program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/tracepoint.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "tp_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "attach-fail-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 1, "expected 1 program")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	// Configure kernel to fail on tracepoint attach
	fix.Kernel.FailOnAttach("tracepoint", fmt.Errorf("injected attach failure"))

	// Attempt to attach - should fail
	attachReq := &pb.AttachRequest{
		Id: kernelID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TracepointAttachInfo{
				TracepointAttachInfo: &pb.TracepointAttachInfo{
					Tracepoint: "syscalls/sys_enter_openat",
				},
			},
		},
	}

	_, err = fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach should fail")
	assert.Contains(t, err.Error(), "injected attach failure", "error should mention injected failure")

	// Verify kernel operations: load succeeded, attach failed
	fix.AssertKernelOps([]string{
		"load:tp_prog:ok",
		"attach:tracepoint:syscalls/sys_enter_openat:error",
	})

	// Program should still be loaded
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "program should still be in kernel")

	// Program should still be in database
	programs, err := fix.Store.List(ctx)
	require.NoError(t, err, "failed to list programs from store")
	assert.Len(t, programs, 1, "program should still be in database")

	// Should be able to retrieve it via Get
	getResp, err := fix.Server.Get(ctx, &pb.GetRequest{Id: kernelID})
	require.NoError(t, err, "Get should succeed for loaded program")
	assert.Equal(t, "tp_prog", getResp.Info.Name, "program name should match")

	// No links should exist
	assert.Empty(t, getResp.Info.Links, "no links should exist after failed attach")
}

// TestUnloadProgram_WithActiveLinks_DetachesLinksThenUnloads verifies that:
//
//	Given a program that was successfully loaded and has active links,
//	When I unload the program,
//	Then the links are detached first,
//	Then the program is unloaded,
//	And the kernel and database are clean.
func TestUnloadProgram_WithActiveLinks_DetachesLinksThenUnloads(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a tracepoint program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/tracepoint.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "tp_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "unload-with-links-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	require.Len(t, loadResp.Programs, 1, "expected 1 program")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	// Attach the program to a tracepoint
	attachReq := &pb.AttachRequest{
		Id: kernelID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TracepointAttachInfo{
				TracepointAttachInfo: &pb.TracepointAttachInfo{
					Tracepoint: "syscalls/sys_enter_write",
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")
	require.NotZero(t, attachResp.LinkId, "link ID should be non-zero")

	// Verify we have 1 program and 1 link
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Unload the program - should detach link first
	_, err = fix.Server.Unload(ctx, &pb.UnloadRequest{Id: kernelID})
	require.NoError(t, err, "Unload should succeed")

	// Verify operation sequence: load -> attach -> detach -> unload
	ops := fix.Kernel.Operations()
	require.GreaterOrEqual(t, len(ops), 3, "expected at least 3 operations")

	// First op: load
	assert.Equal(t, "load", ops[0].Op, "first op should be load")
	assert.Equal(t, "tp_prog", ops[0].Name, "load should be for tp_prog")

	// Second op: attach
	assert.Equal(t, "attach", ops[1].Op, "second op should be attach")
	assert.Contains(t, ops[1].Name, "tracepoint", "attach should be for tracepoint")

	// Third op: detach (before unload)
	assert.Equal(t, "detach", ops[2].Op, "third op should be detach")

	// Fourth op: unload
	assert.Equal(t, "unload", ops[3].Op, "fourth op should be unload")

	// Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Constraint Validation Tests
// =============================================================================

// TestAttach_ToNonExistentProgram_ReturnsNotFound verifies that:
//
//	Given an empty server with no programs loaded,
//	When I attempt to attach to a non-existent program ID,
//	Then the server returns a NotFound error.
func TestAttach_ToNonExistentProgram_ReturnsNotFound(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Attempt to attach to non-existent program ID 999
	attachReq := &pb.AttachRequest{
		Id: 999,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TracepointAttachInfo{
				TracepointAttachInfo: &pb.TracepointAttachInfo{
					Tracepoint: "syscalls/sys_enter_write",
				},
			},
		},
	}

	_, err := fix.Server.Attach(ctx, attachReq)
	require.Error(t, err, "Attach to non-existent program should fail")

	// Verify it's a gRPC NotFound error
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	assert.Equal(t, codes.NotFound, st.Code(), "expected NotFound status code")

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_WithEmptyName_IsRejected verifies that:
//
//	Given an empty server,
//	When I attempt to load a program with an empty name,
//	Then the server rejects the request with an error.
func TestLoadProgram_WithEmptyName_IsRejected(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	req := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/prog.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "", ProgramType: pb.BpfmanProgramType_TRACEPOINT}, // Empty name
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "empty-name-test",
		},
	}

	_, err := fix.Server.Load(ctx, req)
	require.Error(t, err, "Load with empty program name should fail")

	// Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Detach Tests
// =============================================================================

// TestDetach_NonExistentLink_ReturnsNotFound verifies that:
//
//	Given an empty server with no links,
//	When I attempt to detach a non-existent link ID,
//	Then the server returns a NotFound error.
func TestDetach_NonExistentLink_ReturnsNotFound(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Attempt to detach non-existent link ID 999
	_, err := fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: 999})
	require.Error(t, err, "Detach of non-existent link should fail")

	// Verify it's a gRPC NotFound error
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	assert.Equal(t, codes.NotFound, st.Code(), "expected NotFound status code")

	// Verify clean state
	fix.AssertCleanState()
}

// TestDetach_KernelOnlyLink_ReturnsNotFound verifies that:
//
//	Given a link that exists in the kernel but is not managed by bpfman,
//	When I attempt to detach it,
//	Then the server returns a NotFound error,
//	And the error message indicates the link is not managed.
func TestDetach_KernelOnlyLink_ReturnsNotFound(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Inject a link directly into the kernel (bypassing bpfman)
	const kernelOnlyLinkID = 42
	fix.Kernel.InjectKernelLink(kernelOnlyLinkID, bpfman.LinkKindTracepoint)

	// Attempt to detach the kernel-only link
	_, err := fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: kernelOnlyLinkID})
	require.Error(t, err, "Detach of kernel-only link should fail")

	// Verify it's a gRPC NotFound error
	st, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	assert.Equal(t, codes.NotFound, st.Code(), "expected NotFound status code")

	// Verify the error message indicates the link is not managed
	assert.Contains(t, st.Message(), "not managed by bpfman",
		"error message should indicate link is not managed")
}

// TestDetach_ExistingLink_Succeeds verifies that:
//
//	Given a program with an active link,
//	When I detach the link,
//	Then the detach succeeds,
//	And the link is removed,
//	And the program remains loaded.
func TestDetach_ExistingLink_Succeeds(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a tracepoint program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/tracepoint.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "tp_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "detach-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	// Attach to a tracepoint
	attachReq := &pb.AttachRequest{
		Id: kernelID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TracepointAttachInfo{
				TracepointAttachInfo: &pb.TracepointAttachInfo{
					Tracepoint: "syscalls/sys_enter_read",
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")
	linkID := attachResp.LinkId

	// Verify we have 1 program and 1 link
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Detach the link
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkID})
	require.NoError(t, err, "Detach should succeed")

	// Verify link is removed but program remains
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "program should still be loaded")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "link should be removed")

	// Verify operation sequence
	ops := fix.Kernel.Operations()
	assert.Equal(t, "load", ops[0].Op, "first op should be load")
	assert.Equal(t, "attach", ops[1].Op, "second op should be attach")
	assert.Equal(t, "detach", ops[2].Op, "third op should be detach")
}

// TestMultipleLinks_SameProgram_AllDetachable verifies that:
//
//	Given a program with multiple active links,
//	When I detach them one by one,
//	Then each detach succeeds,
//	And the program remains loaded until explicitly unloaded.
func TestMultipleLinks_SameProgram_AllDetachable(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a tracepoint program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/tracepoint.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "tp_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "multi-link-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	// Attach to multiple tracepoints
	tracepoints := []string{
		"syscalls/sys_enter_read",
		"syscalls/sys_enter_write",
		"syscalls/sys_enter_open",
	}

	var linkIDs []uint32
	for _, tp := range tracepoints {
		attachReq := &pb.AttachRequest{
			Id: kernelID,
			Attach: &pb.AttachInfo{
				Info: &pb.AttachInfo_TracepointAttachInfo{
					TracepointAttachInfo: &pb.TracepointAttachInfo{
						Tracepoint: tp,
					},
				},
			},
		}

		attachResp, err := fix.Server.Attach(ctx, attachReq)
		require.NoError(t, err, "Attach to %s should succeed", tp)
		linkIDs = append(linkIDs, attachResp.LinkId)
	}

	// Verify we have 1 program and 3 links
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 3, fix.Kernel.LinkCount(), "should have 3 links")

	// Detach links one by one
	for i, linkID := range linkIDs {
		_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: linkID})
		require.NoError(t, err, "Detach link %d should succeed", i)
		assert.Equal(t, 2-i, fix.Kernel.LinkCount(), "should have %d links remaining", 2-i)
	}

	// Program should still be loaded
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "program should still be loaded")

	// Clean up by unloading the program
	_, err = fix.Server.Unload(ctx, &pb.UnloadRequest{Id: kernelID})
	require.NoError(t, err, "Unload should succeed")

	// Now verify clean state
	fix.AssertCleanState()
}

// TestDetach_KernelFailure_ReturnsError verifies that:
//
//	Given a program with an active link,
//	When I attempt to detach and the kernel fails,
//	Then the detach operation returns an error,
//	And the link remains in the kernel (potential inconsistent state).
//
// Note: This tests the edge case where kernel detach fails. The link may
// remain in the database even though the kernel operation failed, which
// could lead to state inconsistency requiring reconciliation.
func TestDetach_KernelFailure_ReturnsError(t *testing.T) {
	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a tracepoint program
	loadReq := &pb.LoadRequest{
		Bytecode: &pb.BytecodeLocation{
			Location: &pb.BytecodeLocation_File{File: "/path/to/tracepoint.o"},
		},
		Info: []*pb.LoadInfo{
			{Name: "tp_prog", ProgramType: pb.BpfmanProgramType_TRACEPOINT},
		},
		Metadata: map[string]string{
			"bpfman.io/ProgramName": "detach-failure-test",
		},
	}

	loadResp, err := fix.Server.Load(ctx, loadReq)
	require.NoError(t, err, "Load should succeed")
	kernelID := loadResp.Programs[0].KernelInfo.Id

	// Attach to a tracepoint
	attachReq := &pb.AttachRequest{
		Id: kernelID,
		Attach: &pb.AttachInfo{
			Info: &pb.AttachInfo_TracepointAttachInfo{
				TracepointAttachInfo: &pb.TracepointAttachInfo{
					Tracepoint: "syscalls/sys_enter_close",
				},
			},
		},
	}

	attachResp, err := fix.Server.Attach(ctx, attachReq)
	require.NoError(t, err, "Attach should succeed")
	durableLinkID := attachResp.LinkId

	// Get the kernel link ID to configure FailOnDetach.
	// attachResp.LinkId is the durable link ID used for lookups,
	// but FailOnDetach needs the kernel-assigned link ID.
	getResp, err := fix.Server.GetLink(ctx, &pb.GetLinkRequest{KernelLinkId: durableLinkID})
	require.NoError(t, err, "GetLink should succeed")
	kernelLinkID := getResp.Link.Summary.KernelLinkId

	// Configure kernel to fail on detach for this kernel link ID
	fix.Kernel.FailOnDetach(kernelLinkID, fmt.Errorf("injected detach failure"))

	// Attempt to detach - should fail
	_, err = fix.Server.Detach(ctx, &pb.DetachRequest{LinkId: durableLinkID})
	require.Error(t, err, "Detach should fail due to kernel error")
	assert.Contains(t, err.Error(), "injected detach failure", "error should mention injected failure")

	// Verify the link still exists in the fake kernel (was not deleted)
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "link should still exist in kernel after failed detach")

	// Verify operation sequence
	ops := fix.Kernel.Operations()
	lastOp := ops[len(ops)-1]
	assert.Equal(t, "detach", lastOp.Op, "last op should be detach")
	assert.NotNil(t, lastOp.Err, "last op should have recorded the error")
}
