//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
)

func TestMain(m *testing.M) {
	// Fail fast on prerequisites
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "e2e tests require root privileges")
		os.Exit(1)
	}

	// Clean up stale test directories from crashed runs
	if err := cleanupStaleTestDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to clean stale test dirs: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// TestTracepoint_LoadAttachDetachUnload tests the full lifecycle of a tracepoint program.
func TestTracepoint_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTracepoint(t, "syscalls", "sys_enter_kill")

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-tracepoint-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeTracepoint,
			Name: "tracepoint_kill_recorder",
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeTracepoint, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("tracepoint"), prog.Status.Kernel.ProgramType)

	// Register cleanup for the program
	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	// Kernel-reported name should match
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	// Verify bpfman-managed metadata has full name and pin path
	require.Equal(t, "tracepoint_kill_recorder", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")
	// Kernel-reported name is truncated (16 chars max), verify it's a prefix of the full name
	kernelName := prog.Status.Kernel.Name
	require.True(t, strings.HasPrefix("tracepoint_kill_recorder", kernelName),
		"kernel name %q should be prefix of full name", kernelName)

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, listedProgs[0].Status.Kernel.ProgramType)
	// Kernel name should match
	require.Equal(t, kernelName, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	// Metadata has full name
	require.Equal(t, "tracepoint_kill_recorder", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client
	tpSpec, err := bpfman.NewTracepointAttachSpec(prog.Status.Kernel.ID, "syscalls", "sys_enter_kill")
	require.NoError(t, err)
	link, err := env.Attach(ctx, tpSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindTracepoint, link.Kind)

	// Register cleanup for the link
	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	// Note: link.ID from attach is the kernel link ID. We verify type/details instead.
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	// Verify tracepoint-specific details
	tpDetails, ok := gotLinkDetails.(bpfman.TracepointDetails)
	require.True(t, ok, "expected TracepointDetails, got %T", gotLinkDetails)
	require.Equal(t, "syscalls", tpDetails.Group)
	require.Equal(t, "sys_enter_kill", tpDetails.Name)

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestKprobe_LoadAttachDetachUnload tests the full lifecycle of a kprobe program.
func TestKprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelFunction(t, "try_to_wake_up")

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-kprobe-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeKprobe,
			Name: "kprobe_counter",
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeKprobe, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("kprobe"), prog.Status.Kernel.ProgramType)

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "kprobe_counter", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "kprobe_counter", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client
	kpSpec, err := bpfman.NewKprobeAttachSpec(prog.Status.Kernel.ID, "try_to_wake_up")
	require.NoError(t, err)
	link, err := env.Attach(ctx, kpSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindKprobe, link.Kind)

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	kprobeDetails, ok := gotLinkDetails.(bpfman.KprobeDetails)
	require.True(t, ok, "expected KprobeDetails, got %T", gotLinkDetails)
	require.Equal(t, "try_to_wake_up", kprobeDetails.FnName)
	require.Equal(t, uint64(0), kprobeDetails.Offset, "offset should match what was passed")
	require.False(t, kprobeDetails.Retprobe)

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestKretprobe_LoadAttachDetachUnload tests the full lifecycle of a kretprobe program.
func TestKretprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelFunction(t, "try_to_wake_up")

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	// Note: kretprobe uses the same image as kprobe but loads the kretprobe program
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-kprobe-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeKretprobe,
			Name: "kprobe_counter", // Same program as kprobe, loaded as kretprobe
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeKretprobe, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("kprobe"), prog.Status.Kernel.ProgramType) // kernel sees kprobe for both kprobe and kretprobe

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "kprobe_counter", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "kprobe_counter", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client (kretprobe uses AttachKprobe API)
	kpSpec, err := bpfman.NewKprobeAttachSpec(prog.Status.Kernel.ID, "try_to_wake_up")
	require.NoError(t, err)
	link, err := env.Attach(ctx, kpSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	// Note: AttachKprobe returns LinkKindKprobe (the API doesn't know the program type),
	// but GetLink will return the authoritative LinkKindKretprobe from the server.
	require.NotZero(t, link.ID, "kernel should assign link ID")

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return authoritative link info from server
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, bpfman.LinkKindKretprobe, gotLinkSummary.Kind, "server should report kretprobe link kind")
	kprobeDetails, ok := gotLinkDetails.(bpfman.KprobeDetails)
	require.True(t, ok, "expected KprobeDetails, got %T", gotLinkDetails)
	require.Equal(t, "try_to_wake_up", kprobeDetails.FnName)
	require.Equal(t, uint64(0), kprobeDetails.Offset, "offset should match what was passed")
	require.True(t, kprobeDetails.Retprobe, "kretprobe should have Retprobe=true")

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, bpfman.LinkKindKretprobe, listedLinks[0].Kind, "ListLinks should report kretprobe")

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestUprobe_LoadAttachDetachUnload tests the full lifecycle of a uprobe program.
func TestUprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	target, fnName := uprobeTarget()
	if target == "" {
		t.Skip("libc not found at standard paths")
	}

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-uprobe-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeUprobe,
			Name: "uprobe_counter",
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeUprobe, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("kprobe"), prog.Status.Kernel.ProgramType) // kernel sees kprobe for uprobes

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "uprobe_counter", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "uprobe_counter", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client to malloc in libc
	upSpec, err := bpfman.NewUprobeAttachSpec(prog.Status.Kernel.ID, target)
	require.NoError(t, err)
	upSpec = upSpec.WithFnName(fnName)
	link, err := env.Attach(ctx, upSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindUprobe, link.Kind)

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	uprobeDetails, ok := gotLinkDetails.(bpfman.UprobeDetails)
	require.True(t, ok, "expected UprobeDetails, got %T", gotLinkDetails)
	require.Equal(t, target, uprobeDetails.Target)
	require.Equal(t, fnName, uprobeDetails.FnName)
	require.Equal(t, uint64(0), uprobeDetails.Offset, "offset should match what was passed")
	require.False(t, uprobeDetails.Retprobe)

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestUretprobe_LoadAttachDetachUnload tests the full lifecycle of a uretprobe program.
func TestUretprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	target, fnName := uprobeTarget()
	if target == "" {
		t.Skip("libc not found at standard paths")
	}

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-uprobe-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeUretprobe,
			Name: "uprobe_counter", // Same program as uprobe, loaded as uretprobe
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeUretprobe, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("kprobe"), prog.Status.Kernel.ProgramType) // kernel sees kprobe for uretprobes

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "uprobe_counter", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "uprobe_counter", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client to malloc in libc (uretprobe uses AttachUprobe API)
	upSpec, err := bpfman.NewUprobeAttachSpec(prog.Status.Kernel.ID, target)
	require.NoError(t, err)
	upSpec = upSpec.WithFnName(fnName)
	link, err := env.Attach(ctx, upSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	// Note: AttachUprobe returns LinkKindUprobe (the API doesn't know the program type),
	// but GetLink will return the authoritative LinkKindUretprobe from the server.
	require.NotZero(t, link.ID, "kernel should assign link ID")

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return authoritative link info from server
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, bpfman.LinkKindUretprobe, gotLinkSummary.Kind, "server should report uretprobe link kind")
	uprobeDetails, ok := gotLinkDetails.(bpfman.UprobeDetails)
	require.True(t, ok, "expected UprobeDetails, got %T", gotLinkDetails)
	require.Equal(t, target, uprobeDetails.Target)
	require.Equal(t, fnName, uprobeDetails.FnName)
	require.Equal(t, uint64(0), uprobeDetails.Offset, "offset should match what was passed")
	require.True(t, uprobeDetails.Retprobe, "uretprobe should have Retprobe=true")

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, bpfman.LinkKindUretprobe, listedLinks[0].Kind, "ListLinks should report uretprobe")

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestFentry_LoadAttachDetachUnload tests the full lifecycle of a fentry program.
func TestFentry_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKernelFunction(t, "do_unlinkat")

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// For fentry/fexit, we load from a local bytecode file
	// The attach function is specified at load time
	bytecodeFile := findBytecodeFile("fentry.bpf.o")
	if bytecodeFile == "" {
		t.Skip("fentry.bpf.o bytecode file not found")
	}

	// When: load from file via client
	programs, err := env.LoadFile(ctx, bytecodeFile, []manager.ProgramSpec{
		{Name: "test_fentry", Type: bpfman.ProgramTypeFentry, AttachFunc: "do_unlinkat"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)
	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeFentry, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("tracing"), prog.Status.Kernel.ProgramType)

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "test_fentry", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "test_fentry", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client (fentry doesn't need additional params - target is in program)
	feSpec, err := bpfman.NewFentryAttachSpec(prog.Status.Kernel.ID)
	require.NoError(t, err)
	link, err := env.Attach(ctx, feSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindFentry, link.Kind)

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	fentryDetails, ok := gotLinkDetails.(bpfman.FentryDetails)
	require.True(t, ok, "expected FentryDetails, got %T", gotLinkDetails)
	require.Equal(t, "do_unlinkat", fentryDetails.FnName)

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestFexit_LoadAttachDetachUnload tests the full lifecycle of a fexit program.
func TestFexit_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKernelFunction(t, "do_unlinkat")

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// For fentry/fexit, we load from a local bytecode file
	bytecodeFile := findBytecodeFile("fentry.bpf.o")
	if bytecodeFile == "" {
		t.Skip("fentry.bpf.o bytecode file not found")
	}

	// When: load from file via client
	programs, err := env.LoadFile(ctx, bytecodeFile, []manager.ProgramSpec{
		{Name: "test_fexit", Type: bpfman.ProgramTypeFexit, AttachFunc: "do_unlinkat"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)
	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeFexit, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("tracing"), prog.Status.Kernel.ProgramType)

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "test_fexit", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "test_fexit", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client
	fxSpec, err := bpfman.NewFexitAttachSpec(prog.Status.Kernel.ID)
	require.NoError(t, err)
	link, err := env.Attach(ctx, fxSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindFexit, link.Kind)

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	fexitDetails, ok := gotLinkDetails.(bpfman.FexitDetails)
	require.True(t, ok, "expected FexitDetails, got %T", gotLinkDetails)
	require.Equal(t, "do_unlinkat", fexitDetails.FnName)

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestTC_LoadAttachDetachUnload tests the full lifecycle of a TC program.
// TC programs use dispatchers for multi-program support.
func TestTC_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t, "tc")
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-tc-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeTC,
			Name: "stats",
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeTC, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("schedcls"), prog.Status.Kernel.ProgramType)

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "stats", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "stats", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client to test interface
	// TC uses dispatchers and supports both ingress and egress
	tcSpec, err := bpfman.NewTCAttachSpec(prog.Status.Kernel.ID, iface.Name, iface.Ifindex, bpfman.TCDirectionIngress)
	require.NoError(t, err)
	tcSpec = tcSpec.WithPriority(50)
	link, err := env.Attach(ctx, tcSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindTC, link.Kind)

	// Verify tc filter is visible to tc(8) tooling.
	// The dispatcher is attached as a legacy netlink BPF filter with pref 50.
	filterCount := tcFilterCount(t, iface.Name, "ingress")
	require.GreaterOrEqual(t, filterCount, 1, "tc filter should be visible after attach")

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	// Note: TC uses dispatchers, so ProgramID is the dispatcher's program ID,
	// not the extension program ID used in attach. We verify the type/details instead.
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	tcDetails, ok := gotLinkDetails.(bpfman.TCDetails)
	require.True(t, ok, "expected TCDetails, got %T", gotLinkDetails)
	require.Equal(t, iface.Name, tcDetails.Interface)
	require.Equal(t, uint32(iface.Ifindex), tcDetails.Ifindex)
	require.Equal(t, bpfman.TCDirectionIngress, tcDetails.Direction)
	require.Equal(t, int32(50), tcDetails.Priority)
	require.NotZero(t, tcDetails.DispatcherID, "TC should use dispatcher")
	require.NotZero(t, tcDetails.Revision, "dispatcher should have revision")

	// Verify TC ingress filters exist on the interface via netlink
	filters := tcIngressFilters(t, iface.Name)
	require.NotEmpty(t, filters, "expected at least one TC ingress filter after attach")
	foundPriority := false
	for _, f := range filters {
		if f.Attrs().Priority == 50 {
			foundPriority = true
			break
		}
	}
	require.True(t, foundPriority, "expected a TC filter with priority 50")

	// Round-trip: ListLinks should include our link
	// Note: TC uses dispatchers, so ProgramID is the dispatcher's program ID.
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Verify tc filter has been removed by the detach
	filterCountAfter := tcFilterCount(t, iface.Name, "ingress")
	require.Equal(t, 0, filterCountAfter, "tc filter should be removed after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")

	// Verify TC ingress filters are removed after detach/unload
	filtersAfter := tcIngressFilters(t, iface.Name)
	require.Empty(t, filtersAfter, "expected no TC ingress filters after detach/unload")
}

// TestTCX_LoadAttachDetachUnload tests the full lifecycle of a TCX program.
// TCX requires kernel 6.6+ and uses native multi-program support.
func TestTCX_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelVersion(t, 6, 6)

	env := NewTestEnv(t)
	iface := NewTestInterface(t, "tcx")
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/go-tc-counter:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeTCX,
			Name: "stats",
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeTCX, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("schedcls"), prog.Status.Kernel.ProgramType) // kernel sees schedcls for both tc and tcx

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "stats", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "stats", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client to test interface
	tcxSpec, err := bpfman.NewTCXAttachSpec(prog.Status.Kernel.ID, iface.Name, iface.Ifindex, bpfman.TCDirectionIngress)
	require.NoError(t, err)
	tcxSpec = tcxSpec.WithPriority(50)
	link, err := env.Attach(ctx, tcxSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindTCX, link.Kind)

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	tcxDetails, ok := gotLinkDetails.(bpfman.TCXDetails)
	require.True(t, ok, "expected TCXDetails, got %T", gotLinkDetails)
	require.Equal(t, iface.Name, tcxDetails.Interface)
	require.Equal(t, uint32(iface.Ifindex), tcxDetails.Ifindex)
	require.Equal(t, bpfman.TCDirectionIngress, tcxDetails.Direction)
	require.Equal(t, int32(50), tcxDetails.Priority)
	// TCX uses native kernel multi-prog support, not dispatchers

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestXDP_LoadAttachDetachUnload tests the full lifecycle of an XDP program.
// XDP programs use dispatchers for multi-program support.
func TestXDP_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t, "xdp")
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from OCI image via client
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/xdp_pass:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeXDP,
			Name: "pass",
		},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeXDP, prog.Record.Load.ProgramType())
	require.Equal(t, kernel.ProgramType("xdp"), prog.Status.Kernel.ProgramType)

	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Round-trip: Get should return matching program info
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)
	require.NotNil(t, gotProg.Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, gotProg.Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.ProgramType, gotProg.Status.Kernel.ProgramType)
	require.Equal(t, prog.Status.Kernel.Name, gotProg.Status.Kernel.Name)
	require.NotEmpty(t, gotProg.Status.Kernel.Tag, "kernel should assign tag")
	require.False(t, gotProg.Status.Kernel.LoadedAt.IsZero(), "kernel should track LoadedAt")
	require.Equal(t, "pass", gotProg.Record.Meta.Name)
	require.NotEmpty(t, gotProg.Record.Handles.PinPath, "program should have pin path")

	// Round-trip: List should include our program
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)
	require.NotNil(t, listedProgs[0].Status.Kernel)
	require.Equal(t, prog.Status.Kernel.ID, listedProgs[0].Status.Kernel.ID)
	require.Equal(t, prog.Status.Kernel.Name, listedProgs[0].Status.Kernel.Name)
	require.NotEmpty(t, listedProgs[0].Status.Kernel.Tag)
	require.False(t, listedProgs[0].Status.Kernel.LoadedAt.IsZero())
	require.Equal(t, "pass", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client to test interface
	xdpSpec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, iface.Name, iface.Ifindex)
	require.NoError(t, err)
	link, err := env.Attach(ctx, xdpSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindXDP, link.Kind)

	t.Cleanup(func() {
		env.Detach(context.Background(), link.ID)
	})

	// Round-trip: GetLink should return matching link info
	// Note: XDP uses dispatchers, so ProgramID is the dispatcher's program ID,
	// not the extension program ID used in attach. We verify the type/details instead.
	gotLinkSummary, gotLinkDetails, err := env.GetLink(ctx, link.ID)
	require.NoError(t, err)
	require.NotZero(t, gotLinkSummary.ID, "should have kernel link ID")
	require.Equal(t, link.Kind, gotLinkSummary.Kind)
	xdpDetails, ok := gotLinkDetails.(bpfman.XDPDetails)
	require.True(t, ok, "expected XDPDetails, got %T", gotLinkDetails)
	require.Equal(t, iface.Name, xdpDetails.Interface)
	require.Equal(t, uint32(iface.Ifindex), xdpDetails.Ifindex)
	require.NotZero(t, xdpDetails.DispatcherID, "XDP should use dispatcher")
	require.NotZero(t, xdpDetails.Revision, "dispatcher should have revision")

	// Round-trip: ListLinks should include our link
	// Note: XDP uses dispatchers, so ProgramID is the dispatcher's program ID.
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestLoadWithMetadataAndGlobalData verifies that user-supplied metadata and
// global data are stored and returned correctly through the full stack.
func TestLoadWithMetadataAndGlobalData(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// Define user metadata and global data
	userMetadata := map[string]string{
		"owner":                 "test-team",
		"environment":           "e2e-testing",
		"bpfman.io/application": "metadata-test",
	}
	globalData := map[string][]byte{
		"config_u8":  {0x42},
		"config_u32": {0xDE, 0xAD, 0xBE, 0xEF},
	}

	// When: load from OCI image with metadata and global data
	imageRef := platform.ImageRef{URL:"quay.io/bpfman-bytecode/xdp_pass:latest", PullPolicy: bpfman.PullIfNotPresent}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeXDP,
			Name: "pass",
		},
	}, manager.LoadOpts{
		UserMetadata: userMetadata,
		GlobalData:   globalData,
	})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() {
		env.Unload(context.Background(), prog.Status.Kernel.ID)
	})

	// Then: Get should return the user metadata and global data
	gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Verify user metadata is returned
	require.Equal(t, "test-team", gotProg.Record.Meta.Metadata["owner"],
		"Get should return user metadata 'owner'")
	require.Equal(t, "e2e-testing", gotProg.Record.Meta.Metadata["environment"],
		"Get should return user metadata 'environment'")
	require.Equal(t, "metadata-test", gotProg.Record.Meta.Metadata["bpfman.io/application"],
		"Get should return user metadata 'bpfman.io/application'")

	// Verify global data is returned
	require.Equal(t, []byte{0x42}, gotProg.Record.Load.GlobalData()["config_u8"],
		"Get should return global data 'config_u8'")
	require.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, gotProg.Record.Load.GlobalData()["config_u32"],
		"Get should return global data 'config_u32'")

	// Then: List should also return the user metadata and global data
	listedProgs, err := env.List(ctx)
	require.NoError(t, err)
	require.Len(t, listedProgs, 1)

	// Verify user metadata via List
	require.Equal(t, "test-team", listedProgs[0].Record.Meta.Metadata["owner"],
		"List should return user metadata 'owner'")
	require.Equal(t, "e2e-testing", listedProgs[0].Record.Meta.Metadata["environment"],
		"List should return user metadata 'environment'")

	// Verify global data via List
	require.Equal(t, []byte{0x42}, listedProgs[0].Record.Load.GlobalData()["config_u8"],
		"List should return global data 'config_u8'")
	require.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, listedProgs[0].Record.Load.GlobalData()["config_u32"],
		"List should return global data 'config_u32'")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state
	env.AssertCleanState()
}

// uprobeTarget returns the path and function name for uprobe tests.
// Uses libc malloc - works on standard Linux and NixOS.
func uprobeTarget() (target, fnName string) {
	// Patterns to find libc. Order matters - check direct paths first,
	// then paths with one subdirectory level (for arch-specific dirs like x86_64-linux-gnu).
	// Note: filepath.Glob's * doesn't match /, so /lib/*/libc.so.* handles subdirs.
	patterns := []string{
		"/lib/libc.so.*",
		"/lib64/libc.so.*",
		"/lib/*/libc.so.*",
		"/usr/lib/libc.so.*",
		"/usr/lib64/libc.so.*",
		"/usr/lib/*/libc.so.*",
		"/nix/store/*glibc*/lib/libc.so.*",
	}

	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			// Skip the linker script (libc.so), use the real library (libc.so.6)
			if filepath.Ext(path) != ".so" {
				return path, "malloc"
			}
		}
	}

	return "", ""
}

// findBytecodeFile looks for a bytecode file in the integration-tests/bytecode directory.
func findBytecodeFile(name string) string {
	// Try relative to current directory
	candidates := []string{
		filepath.Join("integration-tests", "bytecode", name),
		filepath.Join("..", "integration-tests", "bytecode", name),
	}

	// Also try from the e2e directory
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, "integration-tests", "bytecode", name),
			filepath.Join(filepath.Dir(wd), "integration-tests", "bytecode", name),
		)
	}

	for _, path := range candidates {
		if absPath, err := filepath.Abs(path); err == nil {
			if _, err := os.Stat(absPath); err == nil {
				return absPath
			}
		}
	}

	return ""
}
