package manager_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

type failDispatcherSnapshotStore struct {
	platform.Store
	failOnCall int
	calls      int
	err        error
}

func (s *failDispatcherSnapshotStore) RunInTransaction(
	ctx context.Context,
	name string,
	fn func(platform.Store) error,
) error {
	return s.Store.RunInTransaction(ctx, name, func(tx platform.Store) error {
		return fn(&failDispatcherSnapshotTx{Store: tx, parent: s})
	})
}

type failDispatcherSnapshotTx struct {
	platform.Store
	parent *failDispatcherSnapshotStore
}

func (tx *failDispatcherSnapshotTx) ReplaceDispatcherSnapshot(
	ctx context.Context,
	snap platform.DispatcherSnapshotSpec,
) (platform.DispatcherSnapshot, error) {
	tx.parent.calls++
	if tx.parent.calls == tx.parent.failOnCall {
		return platform.DispatcherSnapshot{}, tx.parent.err
	}
	return tx.Store.ReplaceDispatcherSnapshot(ctx, snap)
}

// =============================================================================
// Fentry Lifecycle Tests
// =============================================================================

// TestFentry_AttachSucceeds verifies that:
//
//	Given a loaded fentry program with FnName specified,
//	When I attach it,
//	Then a link is created.
func TestFentry_AttachSucceeds(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a fentry program with FnName specified
	spec, err := bpfman.NewAttachLoadSpec(fix.BytecodeFile("fentry.o"), "fentry_prog", bpfman.ProgramTypeFentry, "tcp_connect")
	require.NoError(t, err, "failed to create load spec")

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach fentry
	attachSpec, err := bpfman.NewFentryAttachSpec(prog.Record.ProgramID)
	require.NoError(t, err, "failed to create attach spec")
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachFentry should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestFentry_LoadWithoutFnName_Fails verifies that:
//
//	Given a fentry program load request without FnName specified,
//	When I try to create the spec,
//	Then spec creation fails because fentry requires attachFunc.
func TestFentry_LoadWithoutFnName_Fails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)

	// Try to create a fentry spec WITHOUT FnName - should fail at spec creation
	_, err := bpfman.NewLoadSpec("/path/to/fentry.o", "fentry_prog", bpfman.ProgramTypeFentry)
	require.Error(t, err, "spec creation should fail without FnName for fentry")
	assert.Contains(t, err.Error(), "attachFunc", "error should mention attachFunc")

	// No programs should exist
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "no programs should exist")
}

// TestFentry_FullLifecycle verifies the complete fentry lifecycle.
func TestFentry_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load fentry program
	spec, err := bpfman.NewAttachLoadSpec(fix.BytecodeFile("fentry.o"), "fentry_prog", bpfman.ProgramTypeFentry, "tcp_connect")
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach
	attachSpec, err := bpfman.NewFentryAttachSpec(prog.Record.ProgramID)
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify state
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Step 3: Detach
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err, "Detach should succeed")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after detach")

	// Step 4: Unload
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Fexit Lifecycle Tests
// =============================================================================

// TestFexit_AttachSucceeds verifies that:
//
//	Given a loaded fexit program with FnName specified,
//	When I attach it,
//	Then a link is created.
func TestFexit_AttachSucceeds(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a fexit program with FnName specified
	spec, err := bpfman.NewAttachLoadSpec(fix.BytecodeFile("fexit.o"), "fexit_prog", bpfman.ProgramTypeFexit, "tcp_close")
	require.NoError(t, err, "failed to create load spec")

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach fexit
	attachSpec, err := bpfman.NewFexitAttachSpec(prog.Record.ProgramID)
	require.NoError(t, err, "failed to create attach spec")
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachFexit should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestFexit_LoadWithoutFnName_Fails verifies that:
//
//	Given a fexit program load request without FnName specified,
//	When I try to create the spec,
//	Then spec creation fails because fexit requires attachFunc.
func TestFexit_LoadWithoutFnName_Fails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)

	// Try to create a fexit spec WITHOUT FnName - should fail at spec creation
	_, err := bpfman.NewLoadSpec("/path/to/fexit.o", "fexit_prog", bpfman.ProgramTypeFexit)
	require.Error(t, err, "spec creation should fail without FnName for fexit")
	assert.Contains(t, err.Error(), "attachFunc", "error should mention attachFunc")

	// No programs should exist
	assert.Equal(t, 0, fix.Kernel.ProgramCount(), "no programs should exist")
}

// TestFexit_FullLifecycle verifies the complete fexit lifecycle.
func TestFexit_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load fexit program
	spec, err := bpfman.NewAttachLoadSpec(fix.BytecodeFile("fexit.o"), "fexit_prog", bpfman.ProgramTypeFexit, "tcp_close")
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach
	attachSpec, err := bpfman.NewFexitAttachSpec(prog.Record.ProgramID)
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify state
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Step 3: Detach
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err, "Detach should succeed")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after detach")

	// Step 4: Unload
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Kprobe Lifecycle Tests
// =============================================================================

// TestKprobe_AttachSucceeds verifies that:
//
//	Given a loaded kprobe program,
//	When I attach it with a function name,
//	Then a link is created.
func TestKprobe_AttachSucceeds(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a kprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("kprobe.o"), "kprobe_prog", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach kprobe with function name
	attachSpec, err := bpfman.NewKprobeAttachSpec(prog.Record.ProgramID, "do_sys_open")
	require.NoError(t, err, "failed to create attach spec")
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachKprobe should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestKprobe_AttachWithOffset verifies that:
//
//	Given a loaded kprobe program,
//	When I attach it with a function name and offset,
//	Then a link is created.
func TestKprobe_AttachWithOffset(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a kprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("kprobe.o"), "kprobe_prog", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach kprobe with function name and offset
	attachSpec, err := bpfman.NewKprobeAttachSpec(prog.Record.ProgramID, "do_sys_open")
	require.NoError(t, err, "failed to create attach spec")
	attachSpec = attachSpec.WithOffset(0x10)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachKprobe should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestKprobe_AttachWithoutFnName_Fails verifies that:
//
//	Given a loaded kprobe program,
//	When I try to attach without a function name,
//	Then the operation fails.
func TestKprobe_AttachWithoutFnName_Fails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a kprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("kprobe.o"), "kprobe_prog", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)
	_, err = fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attempt to attach without function name - should fail at spec creation
	_, err = bpfman.NewKprobeAttachSpec(0, "")
	require.Error(t, err, "creating attach spec without fn_name should fail")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// TestKprobe_FullLifecycle verifies the complete kprobe lifecycle.
func TestKprobe_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load kprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("kprobe.o"), "kprobe_prog", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach
	attachSpec, err := bpfman.NewKprobeAttachSpec(prog.Record.ProgramID, "do_sys_open")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify state
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Step 3: Detach
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err, "Detach should succeed")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after detach")

	// Step 4: Unload
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Uprobe Lifecycle Tests
// =============================================================================

// TestUprobe_AttachSucceeds verifies that:
//
//	Given a loaded uprobe program,
//	When I attach it with a target,
//	Then a link is created.
func TestUprobe_AttachSucceeds(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a uprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("uprobe.o"), "uprobe_prog", bpfman.ProgramTypeUprobe)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach uprobe with target using real lock
	attachSpec, err := bpfman.NewUprobeAttachSpec(prog.Record.ProgramID, "/usr/lib/libc.so.6")
	require.NoError(t, err, "failed to create attach spec")
	attachSpec = attachSpec.WithFnName("malloc")

	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachUprobe should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

func TestUprobe_ContainerAttachStoresKernelIDAndPin(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("uprobe.o"), "uprobe_prog", bpfman.ProgramTypeUprobe)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	attachSpec, err := bpfman.NewUprobeAttachSpec(prog.Record.ProgramID, "/usr/lib/libc.so.6")
	require.NoError(t, err, "failed to create attach spec")
	attachSpec = attachSpec.WithFnName("malloc").WithContainerPid(1234)

	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachUprobe container should succeed")
	require.NotZero(t, link.Record.ID, "bpfman link ID should be non-zero")
	assert.NotNil(t, link.Record.KernelLinkID, "container uprobe should capture a kernel link ID")
	assert.NotNil(t, link.Record.PinPath, "container uprobe should record its bpffs link pin")
	assert.True(t, link.Status.KernelSeen, "container uprobe should report the captured kernel link")
	assert.True(t, link.Status.PinPresent, "container uprobe should create a bpffs link pin")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "container uprobe should create a fake enumerable kernel link")

	record, err := fix.Store.GetLink(ctx, link.Record.ID)
	require.NoError(t, err, "stored link should round-trip")
	assert.NotNil(t, record.KernelLinkID, "stored container uprobe should capture a kernel link ID")
	assert.NotNil(t, record.PinPath, "stored container uprobe should record its bpffs link pin")

	details, ok := record.Details.(bpfman.UprobeDetails)
	require.True(t, ok, "expected UprobeDetails")
	assert.Equal(t, int32(1234), details.ContainerPid)
	assert.Equal(t, "/usr/lib/libc.so.6", details.Target)
	assert.Equal(t, "malloc", details.FnName)
}

// TestUprobe_AttachWithoutTarget_Fails verifies that:
//
//	Given a loaded uprobe program,
//	When I try to attach without a target,
//	Then the operation fails.
func TestUprobe_AttachWithoutTarget_Fails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a uprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("uprobe.o"), "uprobe_prog", bpfman.ProgramTypeUprobe)
	require.NoError(t, err)
	_, err = fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attempt to attach without target - should fail at spec creation
	_, err = bpfman.NewUprobeAttachSpec(0, "")
	require.Error(t, err, "creating attach spec without target should fail")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// TestUprobe_FullLifecycle verifies the complete uprobe lifecycle.
func TestUprobe_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load uprobe program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("uprobe.o"), "uprobe_prog", bpfman.ProgramTypeUprobe)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach with lock
	attachSpec, err := bpfman.NewUprobeAttachSpec(prog.Record.ProgramID, "/usr/lib/libc.so.6")
	require.NoError(t, err)
	attachSpec = attachSpec.WithFnName("malloc")

	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify state
	assert.Equal(t, 1, fix.Kernel.ProgramCount(), "should have 1 program")
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link")

	// Step 3: Detach
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err, "Detach should succeed")
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after detach")

	// Step 4: Unload
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// XDP Lifecycle Tests
// =============================================================================

// TestXDP_FirstAttachCreatesLink verifies that:
//
//	Given a loaded XDP program,
//	When I attach it to an interface,
//	Then a link is created.
func TestXDP_FirstAttachCreatesLink(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach to interface (programID, ifname, ifindex)
	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
	require.NoError(t, err, "failed to create attach spec")
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachXDP should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestXDP_MultipleAttachesCreateMultipleLinks verifies that:
//
//	Given a loaded XDP program,
//	When I attach it multiple times to the same interface,
//	Then multiple links are created.
func TestXDP_MultipleAttachesCreateMultipleLinks(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach multiple times
	var linkIDs []bpfman.LinkID
	for i := range 3 {
		attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "AttachXDP %d should succeed", i+1)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	// Verify we have 3 links
	assert.Equal(t, 3, fix.Kernel.LinkCount(), "should have 3 links in kernel")
	assert.Len(t, linkIDs, 3, "should have collected 3 link IDs")
}

// TestXDP_FullLifecycle verifies the complete XDP lifecycle.
func TestXDP_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load XDP program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach multiple times
	numAttachments := 3
	var linkIDs []bpfman.LinkID
	for i := range numAttachments {
		attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "Attach %d should succeed", i+1)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	// Step 3: Detach all links one by one
	for i, linkID := range linkIDs {
		err := fix.Detach(ctx, linkID)
		require.NoError(t, err, "Detach link %d should succeed", linkID)
		expectedLinks := numAttachments - i - 1
		assert.Equal(t, expectedLinks, fix.Kernel.LinkCount(),
			"should have %d links after detaching link %d", expectedLinks, i+1)
	}

	// Step 4: Unload program
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// TC Lifecycle Tests
// =============================================================================

// TestTC_FirstAttachCreatesLink verifies that:
//
//	Given a loaded TC program,
//	When I attach it to an interface,
//	Then a link is created.
func TestTC_FirstAttachCreatesLink(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach to interface with ingress direction (programID, ifname, ifindex, direction)
	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err, "failed to create attach spec")
	attachSpec = attachSpec.WithPriority(50)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachTC should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestTC_IngressAndEgressDirections verifies that:
//
//	Given a loaded TC program,
//	When I attach it with both ingress and egress directions,
//	Then both attachments succeed.
func TestTC_IngressAndEgressDirections(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach ingress
	ingressSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	ingressSpec = ingressSpec.WithPriority(50)
	ingressLink, err := fix.Attach(ctx, ingressSpec)
	require.NoError(t, err, "Ingress attach should succeed")

	// Attach egress
	egressSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionEgress)
	require.NoError(t, err)
	egressSpec = egressSpec.WithPriority(50)
	egressLink, err := fix.Attach(ctx, egressSpec)
	require.NoError(t, err, "Egress attach should succeed")

	// Verify both links exist
	assert.Equal(t, 2, fix.Kernel.LinkCount(), "should have 2 links")
	assert.NotEqual(t, ingressLink.Record.ID, egressLink.Record.ID, "link IDs should differ")
}

// TestTC_FullLifecycle verifies the complete TC lifecycle.
func TestTC_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load TC program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach to ingress and egress on multiple interfaces
	var linkIDs []bpfman.LinkID
	interfaces := []struct {
		ifindex int
		name    string
	}{
		{1, "lo"},
		{2, "eth0"},
	}
	directions := []bpfman.TCDirection{bpfman.TCDirectionIngress, bpfman.TCDirectionEgress}

	for _, iface := range interfaces {
		for _, dir := range directions {
			attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, iface.name, dir)
			require.NoError(t, err)
			attachSpec = attachSpec.WithPriority(50)
			link, err := fix.Attach(ctx, attachSpec)
			require.NoError(t, err, "Attach %s/%s should succeed", iface.name, dir)
			linkIDs = append(linkIDs, link.Record.ID)
		}
	}

	// Verify 4 links (2 interfaces x 2 directions)
	assert.Equal(t, 4, fix.Kernel.LinkCount(), "should have 4 links")

	// Step 3: Detach all links
	for i, linkID := range linkIDs {
		err := fix.Detach(ctx, linkID)
		require.NoError(t, err, "Detach link %d should succeed", linkID)
		assert.Equal(t, 4-i-1, fix.Kernel.LinkCount(), "link count should decrement")
	}

	// Step 4: Unload program
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// TCX Lifecycle Tests
// =============================================================================

// TestTCX_FirstAttachCreatesLink verifies that:
//
//	Given a loaded TCX program,
//	When I attach it to an interface,
//	Then a link is created.
func TestTCX_FirstAttachCreatesLink(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tcx.o"), "tcx_pass", bpfman.ProgramTypeTCX)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach to interface with ingress direction (programID, ifname, ifindex, direction)
	attachSpec, err := bpfman.NewTCXAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err, "failed to create attach spec")
	attachSpec = attachSpec.WithPriority(50)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "AttachTCX should succeed")
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify link exists in fake kernel
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link in kernel")
}

// TestTCX_IngressAndEgressDirections verifies that:
//
//	Given a loaded TCX program,
//	When I attach it with both ingress and egress directions,
//	Then both attachments succeed.
func TestTCX_IngressAndEgressDirections(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tcx.o"), "tcx_pass", bpfman.ProgramTypeTCX)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach ingress
	ingressSpec, err := bpfman.NewTCXAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	ingressSpec = ingressSpec.WithPriority(50)
	ingressLink, err := fix.Attach(ctx, ingressSpec)
	require.NoError(t, err, "Ingress attach should succeed")

	// Attach egress
	egressSpec, err := bpfman.NewTCXAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionEgress)
	require.NoError(t, err)
	egressSpec = egressSpec.WithPriority(50)
	egressLink, err := fix.Attach(ctx, egressSpec)
	require.NoError(t, err, "Egress attach should succeed")

	// Verify both links exist
	assert.Equal(t, 2, fix.Kernel.LinkCount(), "should have 2 links")
	assert.NotEqual(t, ingressLink.Record.ID, egressLink.Record.ID, "link IDs should differ")
}

// TestTCX_FullLifecycle verifies the complete TCX lifecycle.
func TestTCX_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load TCX program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tcx.o"), "tcx_pass", bpfman.ProgramTypeTCX)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Step 2: Attach to ingress and egress on multiple interfaces
	var linkIDs []bpfman.LinkID
	interfaces := []struct {
		ifindex int
		name    string
	}{
		{1, "lo"},
		{2, "eth0"},
	}
	directions := []bpfman.TCDirection{bpfman.TCDirectionIngress, bpfman.TCDirectionEgress}

	for _, iface := range interfaces {
		for _, dir := range directions {
			attachSpec, err := bpfman.NewTCXAttachSpec(prog.Record.ProgramID, iface.name, dir)
			require.NoError(t, err)
			attachSpec = attachSpec.WithPriority(50)
			link, err := fix.Attach(ctx, attachSpec)
			require.NoError(t, err, "Attach %s/%s should succeed", iface.name, dir)
			linkIDs = append(linkIDs, link.Record.ID)
		}
	}

	// Verify 4 links (2 interfaces x 2 directions)
	assert.Equal(t, 4, fix.Kernel.LinkCount(), "should have 4 links")

	// Step 3: Detach all links
	for i, linkID := range linkIDs {
		err := fix.Detach(ctx, linkID)
		require.NoError(t, err, "Detach link %d should succeed", linkID)
		assert.Equal(t, 4-i-1, fix.Kernel.LinkCount(), "link count should decrement")
	}

	// Step 4: Unload program
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Link Listing Tests
// =============================================================================

// TestListLinks_ReturnsAllLinks verifies that:
//
//	Given multiple attached links,
//	When I list links,
//	Then all links are returned.
func TestListLinks_ReturnsAllLinks(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a tracepoint program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tracepoint.o"), "tp_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Attach multiple times to different tracepoints
	tracepoints := []struct{ group, name string }{
		{"syscalls", "sys_enter_open"},
		{"syscalls", "sys_enter_close"},
		{"syscalls", "sys_enter_read"},
	}

	var linkIDs []bpfman.LinkID
	for _, tp := range tracepoints {
		attachSpec, err := bpfman.NewTracepointAttachSpecFromString(prog.Record.ProgramID, tp.group+"/"+tp.name)
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "Attach to %s/%s should succeed", tp.group, tp.name)
		linkIDs = append(linkIDs, link.Record.ID)
	}
	require.Len(t, linkIDs, len(tracepoints), "should have collected link IDs for all tracepoints")

	// List all links by program
	links, err := fix.Manager.ListLinksByProgram(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "ListLinksByProgram should succeed")
	assert.Len(t, links, 3, "should have 3 links")
}

// TestListLinks_EmptyWhenNoLinks verifies that:
//
//	Given no attached links,
//	When I list links,
//	Then an empty list is returned.
func TestListLinks_EmptyWhenNoLinks(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a program but don't attach
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), "prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// List links for this program
	links, err := fix.Manager.ListLinksByProgram(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "ListLinksByProgram should succeed")
	assert.Empty(t, links, "should have 0 links")
}

// TestListLinks_EmptyReturnsNonNilSlice pins the contract that
// ListLinks returns a non-nil slice when no links exist. The shell
// binds this result through ValueFromStruct -> json.Marshal, where
// a nil slice would serialise as `null` rather than `[]` and break
// downstream jq expressions like `.links[]`. The "no links"
// representation in Go-space is an empty slice, not nil.
func TestListLinks_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	links, err := fix.Manager.ListLinks(ctx)
	require.NoError(t, err)
	require.NotNil(t, links, "ListLinks must return an empty slice, not nil, when no links exist")
	assert.Empty(t, links)
}

// TestListPrograms_EmptyReturnsNonNilSlice pins the same contract
// for ListPrograms.Programs. See TestListLinks_EmptyReturnsNonNilSlice
// for the reasoning.
func TestListPrograms_EmptyReturnsNonNilSlice(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	result, err := fix.Manager.ListPrograms(ctx)
	require.NoError(t, err)
	require.NotNil(t, result.Programs, "ListPrograms must return Programs as an empty slice, not nil, when no programs exist")
	assert.Empty(t, result.Programs)
}

// =============================================================================
// Validation Tests
// =============================================================================

// TestLoadProgram_WithEmptyName_IsRejected verifies that:
//
//	Given an empty manager,
//	When I attempt to load a program with an empty name,
//	Then the operation fails.
func TestLoadProgram_WithEmptyName_IsRejected(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)

	// Try to create a spec with empty name
	_, err := bpfman.NewLoadSpec("/path/to/prog.o", "", bpfman.ProgramTypeTracepoint)
	require.Error(t, err, "creating spec with empty name should fail")

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_WithInvalidProgramType_IsRejected verifies that:
//
//	Given an empty manager,
//	When I attempt to load a program with an invalid program type,
//	Then the operation fails.
func TestLoadProgram_WithInvalidProgramType_IsRejected(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)

	// Try to create a spec with zero-value type (invalid)
	_, err := bpfman.NewLoadSpec("/path/to/prog.o", "bad_prog", bpfman.ProgramType{})
	require.Error(t, err, "creating spec with invalid program type should fail")

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_PartialFailure_FirstProgramFails verifies that:
//
//	Given a manager configured to fail on the first program load,
//	When I attempt to load a program,
//	Then the failure occurs with failure outcome and no state is left behind.
func TestLoadProgram_PartialFailure_FirstProgramFails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail on the first program
	fix.Kernel.FailOnProgram("first_prog", errors.New("injected failure on first"))

	// Load first program - should fail
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), "first_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	_, err = fix.Load(ctx, spec, manager.LoadOpts{})
	require.Error(t, err, "First Load should fail")
	assert.Contains(t, err.Error(), "injected failure", "error should mention injected failure")

	// Verify clean state
	fix.AssertCleanState()
}

// TestLoadProgram_PartialFailure_ThirdOfThreeFails verifies that:
//
//	Given multiple sequential program loads where the third fails,
//	When I attempt to load three programs,
//	Then the first two succeed with success outcomes, the third fails with failure outcome.
func TestLoadProgram_PartialFailure_ThirdOfThreeFails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Configure kernel to fail on the third program
	fix.Kernel.FailOnProgram("third_prog", errors.New("injected failure on third"))

	// Load first two programs - should succeed
	for i, name := range []string{"first_prog", "second_prog"} {
		spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), name, bpfman.ProgramTypeTracepoint)
		require.NoError(t, err)
		_, err = fix.Load(ctx, spec, manager.LoadOpts{})
		require.NoError(t, err, "Load %d should succeed", i+1)
		// Outcome is not accessible on success - absence of error implies success
	}

	// Load third program - should fail
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), "third_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	_, err = fix.Load(ctx, spec, manager.LoadOpts{})
	require.Error(t, err, "Third Load should fail")
	assert.Contains(t, err.Error(), "injected failure", "error should mention injected failure")

	// First two should still exist
	listResult, err := fix.Manager.ListPrograms(ctx)
	require.NoError(t, err)
	assert.Len(t, listResult.Programs, 2, "should have 2 programs from first two loads")
}

// =============================================================================
// Map Sharing Tests
// =============================================================================

// TestMapSharing_MultiProgramLoad_FirstIsOwner verifies that:
//
//	Given a multi-program load scenario where second program uses WithMapOwnerID,
//	When all programs are successfully loaded,
//	Then the first program has no MapOwnerID (it owns the maps),
//	And subsequent programs have MapOwnerID set to the first program's ID.
func TestMapSharing_MultiProgramLoad_FirstIsOwner(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load first program - becomes map owner
	spec1, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "kprobe_counter", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)

	prog1, err := fix.Load(ctx, spec1, manager.LoadOpts{
		UserMetadata: map[string]string{"bpfman.io/ProgramName": "multi-prog-image"},
	})
	require.NoError(t, err, "First program load should succeed")
	ownerID := prog1.Record.ProgramID

	// Load second program with MapOwnerID pointing to first
	spec2, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "tracepoint_counter", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	spec2 = spec2.WithMapOwnerID(ownerID)

	prog2, err := fix.Load(ctx, spec2, manager.LoadOpts{
		UserMetadata: map[string]string{"bpfman.io/ProgramName": "multi-prog-image"},
	})
	require.NoError(t, err, "Second program load should succeed")

	// Load third program with MapOwnerID pointing to first
	spec3, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "xdp_stats", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	spec3 = spec3.WithMapOwnerID(ownerID)

	prog3, err := fix.Load(ctx, spec3, manager.LoadOpts{
		UserMetadata: map[string]string{"bpfman.io/ProgramName": "multi-prog-image"},
	})
	require.NoError(t, err, "Third program load should succeed")

	// Verify we have 3 programs
	assert.Equal(t, 3, fix.Kernel.ProgramCount(), "should have 3 programs")

	// Verify map sharing through pin directories
	// First program owns maps - uses its own ID in pin dir
	assert.Contains(t, prog1.Record.Handles.MapsDir, fmt.Sprintf("/%d", ownerID),
		"first program should have its own maps directory")

	// Second and third programs share maps with owner
	assert.Contains(t, prog2.Record.Handles.MapsDir, fmt.Sprintf("/%d", ownerID),
		"second program should share owner's maps directory")
	assert.Contains(t, prog3.Record.Handles.MapsDir, fmt.Sprintf("/%d", ownerID),
		"third program should share owner's maps directory")

	// All should have same pin dir
	assert.Equal(t, prog1.Record.Handles.MapsDir, prog2.Record.Handles.MapsDir,
		"second program should have same PinDir as owner")
	assert.Equal(t, prog1.Record.Handles.MapsDir, prog3.Record.Handles.MapsDir,
		"third program should have same PinDir as owner")
}

// TestMapSharing_SingleProgram_NoMapOwner verifies that:
//
//	Given a single program load,
//	When it is successfully loaded,
//	Then it owns its own maps (no MapOwnerID).
func TestMapSharing_SingleProgram_NoMapOwner(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("single.o"), "single_prog", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Single program owns its own maps - pin dir contains its own ID
	assert.Contains(t, prog.Record.Handles.MapsDir, fmt.Sprintf("/%d", prog.Record.ProgramID),
		"single program should have its own maps directory")
}

// TestPinBasedExtension_XDPAttach_UsesProgPinPath verifies that:
//
//	Given a loaded XDP program,
//	When it is attached to an interface,
//	Then the kernel receives the program's PinPath for pin-based extension loading.
func TestPinBasedExtension_XDPAttach_UsesProgPinPath(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_prog", bpfman.ProgramTypeXDP)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	expectedProgPinPath := prog.Record.Handles.PinPath.String()
	require.NotEmpty(t, expectedProgPinPath, "PinPath should be set")

	// Attach the program
	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "eth0")
	require.NoError(t, err)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the correct ProgPinPath
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one XDP extension attach")
	assert.Equal(t, "attach-xdp-ext", extOps[0].Op)
	assert.Equal(t, expectedProgPinPath, extOps[0].ProgPinPath,
		"XDP attach should use the program's PinPath")
}

// TestPinBasedExtension_TCAttach_UsesProgPinPath verifies that:
//
//	Given a loaded TC program,
//	When it is attached to an interface,
//	Then the kernel receives the program's PinPath for pin-based extension loading.
func TestPinBasedExtension_TCAttach_UsesProgPinPath(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_prog", bpfman.ProgramTypeTC)
	require.NoError(t, err)

	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	expectedProgPinPath := prog.Record.Handles.PinPath.String()
	require.NotEmpty(t, expectedProgPinPath, "PinPath should be set")

	// Attach the program
	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the correct ProgPinPath
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one TC extension attach")
	assert.Equal(t, "attach-tc-ext", extOps[0].Op)
	assert.Equal(t, expectedProgPinPath, extOps[0].ProgPinPath,
		"TC attach should use the program's PinPath")
}

// TestPinBasedExtension_MultiProgram_XDPAttach_UsesOwnPinPath verifies that:
//
//	Given a multi-program load where the second program has MapOwnerID set,
//	When the second (XDP) program is attached,
//	Then the kernel receives the XDP program's own PinPath (not the owner's).
func TestPinBasedExtension_MultiProgram_XDPAttach_UsesOwnPinPath(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load first program - becomes map owner
	spec1, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "kprobe_counter", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)
	prog1, err := fix.Load(ctx, spec1, manager.LoadOpts{})
	require.NoError(t, err)
	ownerID := prog1.Record.ProgramID
	ownerMapPinPath := prog1.Record.Handles.MapsDir

	// Load XDP program with MapOwnerID pointing to first
	spec2, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "xdp_stats", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	spec2 = spec2.WithMapOwnerID(ownerID)
	prog2, err := fix.Load(ctx, spec2, manager.LoadOpts{})
	require.NoError(t, err)

	// Verify XDP program has same MapPinPath as owner
	assert.Equal(t, ownerMapPinPath, prog2.Record.Handles.MapsDir,
		"XDP program should have same MapPinPath as owner")

	// Attach the XDP program
	attachSpec, err := bpfman.NewXDPAttachSpec(prog2.Record.ProgramID, "eth0")
	require.NoError(t, err)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the XDP program's own PinPath
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one XDP extension attach")
	assert.Equal(t, prog2.Record.Handles.PinPath.String(), extOps[0].ProgPinPath,
		"XDP attach should use the program's own PinPath")
}

// TestPinBasedExtension_MultiProgram_TCAttach_UsesOwnPinPath verifies that:
//
//	Given a multi-program load where the second program has MapOwnerID set,
//	When the second (TC) program is attached,
//	Then the kernel receives the TC program's own PinPath (not the owner's).
func TestPinBasedExtension_MultiProgram_TCAttach_UsesOwnPinPath(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load first program - becomes map owner
	spec1, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "kprobe_counter", bpfman.ProgramTypeKprobe)
	require.NoError(t, err)
	prog1, err := fix.Load(ctx, spec1, manager.LoadOpts{})
	require.NoError(t, err)
	ownerID := prog1.Record.ProgramID
	ownerMapPinPath := prog1.Record.Handles.MapsDir

	// Load TC program with MapOwnerID pointing to first
	spec2, err := bpfman.NewLoadSpec(fix.BytecodeFile("multi.o"), "tc_stats", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	spec2 = spec2.WithMapOwnerID(ownerID)
	prog2, err := fix.Load(ctx, spec2, manager.LoadOpts{})
	require.NoError(t, err)

	// Verify TC program has same MapPinPath as owner
	assert.Equal(t, ownerMapPinPath, prog2.Record.Handles.MapsDir,
		"TC program should have same MapPinPath as owner")

	// Attach the TC program
	attachSpec, err := bpfman.NewTCAttachSpec(prog2.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the TC program's own PinPath
	extOps := fix.Kernel.ExtensionAttachOps()
	require.Len(t, extOps, 1, "expected one TC extension attach")
	assert.Equal(t, prog2.Record.Handles.PinPath.String(), extOps[0].ProgPinPath,
		"TC attach should use the program's own PinPath")
}

// =============================================================================
// Dispatcher State Tests
// =============================================================================

// TestXDP_DispatcherStateInStore verifies that the store tracks
// dispatcher state and cleans it up when the last extension is detached.
func TestXDP_DispatcherStateInStore(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach two extensions
	var linkIDs []bpfman.LinkID
	for range 2 {
		attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	// Verify dispatcher state
	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	snap, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	assert.Equal(t, 2, len(snap.Members))

	// Detach first - dispatcher should still exist
	err = fix.Detach(ctx, linkIDs[0])
	require.NoError(t, err)

	summaries, err = fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	assert.Len(t, summaries, 1, "dispatcher should still exist with 1 extension")

	// Detach second - dispatcher should be cleaned up
	err = fix.Detach(ctx, linkIDs[1])
	require.NoError(t, err)

	summaries, err = fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	assert.Empty(t, summaries, "dispatcher should be removed after last extension detached")
}

// TestTC_DispatcherStateInStore verifies that the store correctly
// tracks dispatcher state after attachment and cleans it up after the
// last extension is detached.
func TestTC_DispatcherStateInStore(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach one extension
	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	// Verify dispatcher exists in store
	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1, "should have 1 dispatcher")
	assert.Equal(t, uint32(2), summaries[0].Key.Ifindex) // eth0 = ifindex 2

	// Verify snapshot has 1 member
	snap, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	assert.Equal(t, 1, len(snap.Members), "should have 1 extension link")

	// Detach the extension
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err)

	// Dispatcher should be cleaned up
	summaries, err = fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	assert.Empty(t, summaries, "dispatcher should be removed after last extension detached")
}

func TestXDP_DispatcherRebuildRollbackRestoresOuterLink(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("injected snapshot persist failure")
	fix := newTestFixtureWithStore(t, func(store platform.Store) platform.Store {
		return &failDispatcherSnapshotStore{
			Store:      store,
			failOnCall: 2,
			err:        persistErr,
		}
	})
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
	require.NoError(t, err)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	snap, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	require.Len(t, snap.Members, 1)

	linkPath := fix.Layout.BPFFS().DispatcherLinkPath(snap.Key.Type, snap.Key.Nsid, snap.Key.Ifindex)
	oldTarget, ok := fix.Kernel.XDPDispatcherTarget(linkPath)
	require.True(t, ok, "dispatcher link should exist")
	assert.Equal(t,
		fix.Layout.BPFFS().DispatcherProgPath(snap.Key.Type, snap.Key.Nsid, snap.Key.Ifindex, snap.Revision),
		oldTarget,
	)

	_, err = fix.Attach(ctx, attachSpec)
	require.ErrorIs(t, err, persistErr)

	restoredTarget, ok := fix.Kernel.XDPDispatcherTarget(linkPath)
	require.True(t, ok, "rollback should keep the dispatcher link")
	assert.Equal(t, oldTarget, restoredTarget,
		"snapshot failure must retarget the outer link to the old dispatcher")

	after, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	assert.Equal(t, snap.Revision, after.Revision)
	assert.Len(t, after.Members, 1)
}

func TestTC_DispatcherRebuildRollbackRestoresOldFilter(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("injected snapshot persist failure")
	fix := newTestFixtureWithStore(t, func(store platform.Store) platform.Store {
		return &failDispatcherSnapshotStore{
			Store:      store,
			failOnCall: 2,
			err:        persistErr,
		}
	})
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	snap, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	require.Len(t, snap.Members, 1)

	_, err = fix.Attach(ctx, attachSpec)
	require.ErrorIs(t, err, persistErr)

	handles := fix.Kernel.TCFilterHandles()
	require.Len(t, handles, 1, "rollback should leave exactly one TC filter")
	created := tcFilterCreateHandles(fix.Kernel.Operations())
	require.GreaterOrEqual(t, len(created), 3,
		"rollback should create a replacement filter for the old dispatcher")
	assert.NotEqual(t, created[1], handles[0],
		"snapshot failure must not leave the failed new filter installed")

	after, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	assert.Equal(t, snap.Revision, after.Revision)
	assert.Len(t, after.Members, 1)
}

func tcFilterCreateHandles(ops []kernelOp) []uint32 {
	var handles []uint32
	for _, op := range ops {
		if op.Op == "create-tc-filter" {
			handles = append(handles, op.ID)
		}
	}
	return handles
}

func TestXDP_DetachRebuildRollbackAllowsRetry(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("injected snapshot persist failure")
	fix := newTestFixtureWithStore(t, func(store platform.Store) platform.Store {
		return &failDispatcherSnapshotStore{
			Store:      store,
			failOnCall: 3,
			err:        persistErr,
		}
	})
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
	require.NoError(t, err)
	first, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	err = fix.Detach(ctx, first.Record.ID)
	require.ErrorIs(t, err, persistErr)

	err = fix.Detach(ctx, first.Record.ID)
	require.NoError(t, err, "retry should not trip over orphaned detach-rebuild pins")

	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	snap, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	assert.Len(t, snap.Members, 1)
}

func TestTC_DetachRebuildRollbackAllowsRetry(t *testing.T) {
	t.Parallel()

	persistErr := errors.New("injected snapshot persist failure")
	fix := newTestFixtureWithStore(t, func(store platform.Store) platform.Store {
		return &failDispatcherSnapshotStore{
			Store:      store,
			failOnCall: 3,
			err:        persistErr,
		}
	})
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	first, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	err = fix.Detach(ctx, first.Record.ID)
	require.ErrorIs(t, err, persistErr)

	err = fix.Detach(ctx, first.Record.ID)
	require.NoError(t, err, "retry should not trip over orphaned detach-rebuild pins")

	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	snap, err := fix.Store.GetDispatcherSnapshot(ctx, summaries[0].Key)
	require.NoError(t, err)
	assert.Len(t, snap.Members, 1)
	assert.Equal(t, 1, fix.Kernel.TCFilterCount())
}

// =============================================================================
// Extension Position Tests
// =============================================================================

// TestXDP_ExtensionPositionsAreSequential verifies that multiple XDP
// extensions on the same interface get sequential positions.
func TestXDP_ExtensionPositionsAreSequential(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	var linkIDs []bpfman.LinkID
	for i := range 3 {
		attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "attach %d should succeed", i)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	// Verify all positions are assigned and the last-attached
	// (newest) program is at position 0 — matching Rust bpfman
	// behaviour where new programs sort before existing ones at
	// the same priority.
	positions := make(map[int32]bool)
	for _, linkID := range linkIDs {
		record, err := fix.Store.GetLink(ctx, linkID)
		require.NoError(t, err)
		xdpDetails, ok := record.Details.(bpfman.XDPDetails)
		require.True(t, ok, "expected XDPDetails, got %T", record.Details)
		positions[xdpDetails.Position] = true
	}
	assert.Len(t, positions, 3, "should have 3 unique positions")
	for i := range int32(3) {
		assert.True(t, positions[i], "position %d should be assigned", i)
	}

	// The last-attached program should be at position 0.
	lastRecord, err := fix.Store.GetLink(ctx, linkIDs[2])
	require.NoError(t, err)
	lastXDP, ok := lastRecord.Details.(bpfman.XDPDetails)
	require.True(t, ok)
	assert.Equal(t, int32(0), lastXDP.Position,
		"last-attached link should be at position 0")
}

// TestTC_ExtensionPositionsAreSequential verifies that multiple TC
// extensions on the same interface/direction get sequential positions.
func TestTC_ExtensionPositionsAreSequential(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach three times to the same interface/direction
	var linkIDs []bpfman.LinkID
	for i := range 3 {
		attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
		require.NoError(t, err)
		attachSpec = attachSpec.WithPriority(50)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "attach %d should succeed", i)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	// Verify all three positions are assigned and unique.
	positions := make(map[int32]bool)
	for _, linkID := range linkIDs {
		record, err := fix.Store.GetLink(ctx, linkID)
		require.NoError(t, err)
		tcDetails, ok := record.Details.(bpfman.TCDetails)
		require.True(t, ok, "expected TCDetails, got %T", record.Details)
		positions[tcDetails.Position] = true
	}
	assert.Len(t, positions, 3, "should have 3 unique positions")
	for i := range int32(3) {
		assert.True(t, positions[i], "position %d should be assigned", i)
	}

	// The last-attached program should be at position 0.
	lastRecord, err := fix.Store.GetLink(ctx, linkIDs[2])
	require.NoError(t, err)
	lastTC, ok := lastRecord.Details.(bpfman.TCDetails)
	require.True(t, ok)
	assert.Equal(t, int32(0), lastTC.Position,
		"last-attached link should be at position 0")
}

// =============================================================================
// Pin Path Convention Tests
// =============================================================================

// TestXDP_PinPathConventions verifies that dispatcher cleanup removes
// pins at convention-derived paths.
func TestXDP_PinPathConventions(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	// Capture dispatcher state before cleanup
	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	// Detach (triggers full dispatcher cleanup)
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err)

	// Verify pins were removed
	removedPins := fix.Kernel.RemovedPins()
	assert.NotEmpty(t, removedPins, "should have removed some pins during cleanup")
}

// TestTC_PinPathConventions verifies that dispatcher cleanup removes
// pins at paths matching the convention.
func TestTC_PinPathConventions(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	// Get dispatcher state before detaching
	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	// Detach (triggers full dispatcher cleanup)
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err)

	// Verify pins were removed
	removedPins := fix.Kernel.RemovedPins()
	assert.NotEmpty(t, removedPins, "should have removed some pins during cleanup")
}

// =============================================================================
// TC Filter Handle Tests
// =============================================================================

// TestTC_FilterHandleRoundTrip verifies that the TC filter handle
// assigned at attach time is correctly looked up at detach time.
func TestTC_FilterHandleRoundTrip(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach a single ingress extension
	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	// Verify a TC filter was registered in fakeKernel
	assert.Equal(t, 1, fix.Kernel.TCFilterCount(), "should have 1 TC filter tracked")

	// Detach the extension (triggers dispatcher cleanup)
	err = fix.Detach(ctx, link.Record.ID)
	require.NoError(t, err)

	// Verify DetachTCFilter was called
	tcDetaches := fix.Kernel.TCDetaches()
	require.Len(t, tcDetaches, 1, "should have 1 TC filter detach")

	// TC filter should be removed
	assert.Equal(t, 0, fix.Kernel.TCFilterCount(), "TC filter should be removed")
}

func TestTC_DispatcherRebuildDetachesOldFilterHandle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec1, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog1, err := fix.Load(ctx, spec1, manager.LoadOpts{})
	require.NoError(t, err)

	attach1, err := bpfman.NewTCAttachSpec(prog1.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attach1 = attach1.WithPriority(50)
	_, err = fix.Attach(ctx, attach1)
	require.NoError(t, err)

	firstHandles := fix.Kernel.TCFilterHandles()
	require.Len(t, firstHandles, 1)
	oldHandle := firstHandles[0]

	spec2, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog2, err := fix.Load(ctx, spec2, manager.LoadOpts{})
	require.NoError(t, err)

	attach2, err := bpfman.NewTCAttachSpec(prog2.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attach2 = attach2.WithPriority(60)
	_, err = fix.Attach(ctx, attach2)
	require.NoError(t, err)

	events := fix.Kernel.TCDetachEvents()
	require.Len(t, events, 1)
	assert.Equal(t, oldHandle, events[0].handle, "rebuild must detach the old filter handle")

	currentHandles := fix.Kernel.TCFilterHandles()
	require.Len(t, currentHandles, 1)
	assert.NotEqual(t, oldHandle, currentHandles[0], "new dispatcher filter should remain live")
}

func TestTC_DispatcherRebuildDetachesOldFilterInNetns(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()
	netnsPath := "/proc/self/ns/net"

	spec1, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog1, err := fix.Load(ctx, spec1, manager.LoadOpts{})
	require.NoError(t, err)

	attach1, err := bpfman.NewTCAttachSpec(prog1.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attach1 = attach1.WithNetns(netnsPath).WithPriority(50)
	_, err = fix.Attach(ctx, attach1)
	require.NoError(t, err)

	firstHandles := fix.Kernel.TCFilterHandles()
	require.Len(t, firstHandles, 1)
	oldHandle := firstHandles[0]

	spec2, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog2, err := fix.Load(ctx, spec2, manager.LoadOpts{})
	require.NoError(t, err)

	attach2, err := bpfman.NewTCAttachSpec(prog2.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attach2 = attach2.WithNetns(netnsPath).WithPriority(60)
	_, err = fix.Attach(ctx, attach2)
	require.NoError(t, err)

	events := fix.Kernel.TCDetachEvents()
	require.Len(t, events, 1)
	assert.Equal(t, oldHandle, events[0].handle, "netns rebuild must detach the old filter handle")

	currentHandles := fix.Kernel.TCFilterHandles()
	require.Len(t, currentHandles, 1)
	assert.NotEqual(t, oldHandle, currentHandles[0], "new netns dispatcher filter should remain live")
}

// =============================================================================
// Direction Validation Tests
// =============================================================================

// TestTC_InvalidDirection verifies that:
//
//	Given a loaded TC program,
//	When I try to attach with an invalid direction,
//	Then the operation fails.
func TestTC_InvalidDirection(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attempt to create attach spec with invalid direction
	_, err = bpfman.NewTCAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirection{})
	require.Error(t, err, "creating attach spec with invalid direction should fail")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// TestTCX_InvalidDirection verifies that:
//
//	Given a loaded TCX program,
//	When I try to attach with an invalid direction,
//	Then the operation fails.
func TestTCX_InvalidDirection(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tcx.o"), "tcx_pass", bpfman.ProgramTypeTCX)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attempt to create attach spec with invalid direction
	_, err = bpfman.NewTCXAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirection{})
	require.Error(t, err, "creating attach spec with invalid direction should fail")

	// No links should exist
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "no links should exist")
}

// =============================================================================
// TCX Pin Path Tests
// =============================================================================

// TestTCX_AttachUsesProgramPinPath verifies that:
//
//	Given a loaded TCX program,
//	When it is attached to an interface,
//	Then the kernel receives the program's PinPath.
func TestTCX_AttachUsesProgramPinPath(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tcx.o"), "tcx_prog", bpfman.ProgramTypeTCX)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// The expected pin path follows bpffs conventions
	expectedPinPath := fix.Layout.BPFFS().ProgPinPath(prog.Record.ProgramID).String()

	// Attach the program
	attachSpec, err := bpfman.NewTCXAttachSpec(prog.Record.ProgramID, "eth0", bpfman.TCDirectionIngress)
	require.NoError(t, err)
	attachSpec = attachSpec.WithPriority(50)
	_, err = fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "Attach should succeed")

	// Verify the kernel received the correct programPinPath
	tcxOps := fix.Kernel.TCXAttachOps()
	require.Len(t, tcxOps, 1, "expected one TCX attach")
	assert.Equal(t, "attach-tcx", tcxOps[0].Op)
	assert.Equal(t, expectedPinPath, tcxOps[0].Name,
		"TCX attach should use prog.PinPath directly")
}

// =============================================================================
// GetLink Details Test
// =============================================================================

// TestGetLink_ReturnsLinkDetails verifies that:
//
//	Given an attached link,
//	When I get link details,
//	Then the correct details are returned.
func TestGetLink_ReturnsLinkDetails(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load and attach a tracepoint program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tracepoint.o"), "tp_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTracepointAttachSpecFromString(prog.Record.ProgramID, "syscalls/sys_enter_open")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	// Get link details
	record, err := fix.Manager.GetLink(ctx, link.Record.ID)
	require.NoError(t, err, "GetLink should succeed")
	assert.Equal(t, bpfman.LinkKindTracepoint, record.Kind, "link kind should be tracepoint")

	// Verify tracepoint details
	tpDetails, ok := record.Details.(bpfman.TracepointDetails)
	require.True(t, ok, "expected TracepointDetails, got %T", record.Details)
	assert.Equal(t, "syscalls", tpDetails.Group)
	assert.Equal(t, "sys_enter_open", tpDetails.Name)
}

// =============================================================================
// Unspecified Program Type Test
// =============================================================================

// TestLoadProgram_WithUnspecifiedProgramType_IsRejected verifies that:
//
//	Given an empty manager,
//	When I attempt to load a program with unspecified type,
//	Then the operation fails.
func TestLoadProgram_WithUnspecifiedProgramType_IsRejected(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)

	// Try to create a spec with zero-value type (unspecified)
	_, err := bpfman.NewLoadSpec("/path/to/prog.o", "prog", bpfman.ProgramType{})
	require.Error(t, err, "creating spec with unspecified program type should fail")

	// Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// XDP Dispatcher Tests (same functionality as XDP tests, different naming)
// =============================================================================

// TestXDPDispatcher_FirstAttachCreatesDispatcher verifies that:
//
//	Given a loaded XDP program,
//	When I attach it to an interface for the first time,
//	Then a dispatcher is created.
func TestXDPDispatcher_FirstAttachCreatesDispatcher(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)
	require.NotZero(t, link.Record.ID, "link ID should be non-zero")

	// Verify dispatcher was created
	summaries, err := fix.Store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	assert.Len(t, summaries, 1, "should have 1 dispatcher")
}

// TestXDPDispatcher_MultipleAttachesCreateMultipleLinks verifies that:
//
//	Given a loaded XDP program,
//	When I attach it multiple times to the same interface,
//	Then multiple links are created.
func TestXDPDispatcher_MultipleAttachesCreateMultipleLinks(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	var linkIDs []bpfman.LinkID
	for i := range 3 {
		attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "AttachXDP %d should succeed", i+1)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	assert.Equal(t, 3, fix.Kernel.LinkCount(), "should have 3 links in kernel")
	assert.Len(t, linkIDs, 3, "should have collected 3 link IDs")
}

// TestXDPDispatcher_DetachDecrementsLinkCount verifies that:
//
//	Given a program with multiple XDP attachments,
//	When I detach one link,
//	Then the link count decrements.
func TestXDPDispatcher_DetachDecrementsLinkCount(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Attach twice
	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
	require.NoError(t, err)
	link1, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)
	link2, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err)

	assert.Equal(t, 2, fix.Kernel.LinkCount(), "should have 2 links")

	// Detach first link
	err = fix.Detach(ctx, link1.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, fix.Kernel.LinkCount(), "should have 1 link after first detach")

	// Detach second link
	err = fix.Detach(ctx, link2.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, fix.Kernel.LinkCount(), "should have 0 links after second detach")
}

// TestXDPDispatcher_FullLifecycle verifies the complete dispatcher lifecycle.
func TestXDPDispatcher_FullLifecycle(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Step 1: Load XDP program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	// Step 2: Attach multiple times
	numAttachments := 5
	var linkIDs []bpfman.LinkID
	for i := range numAttachments {
		attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "lo")
		require.NoError(t, err)
		link, err := fix.Attach(ctx, attachSpec)
		require.NoError(t, err, "Attach %d should succeed", i+1)
		linkIDs = append(linkIDs, link.Record.ID)
	}

	// Verify state after attachments (2 programs: user + dispatcher)
	assert.Equal(t, 2, fix.Kernel.ProgramCount(), "should have 2 programs (user + dispatcher)")
	assert.Equal(t, numAttachments, fix.Kernel.LinkCount(), "should have %d links", numAttachments)

	// Step 3: Detach all links one by one
	for i, linkID := range linkIDs {
		err := fix.Detach(ctx, linkID)
		require.NoError(t, err, "Detach link %d should succeed", linkID)
		expectedLinks := numAttachments - i - 1
		assert.Equal(t, expectedLinks, fix.Kernel.LinkCount(),
			"should have %d links after detaching link %d", expectedLinks, i+1)
	}

	// Step 4: Unload program
	err = fix.Unload(ctx, prog.Record.ProgramID)
	require.NoError(t, err, "Unload should succeed")

	// Step 5: Verify clean state
	fix.AssertCleanState()
}

// =============================================================================
// Non-Existent Interface Tests
// =============================================================================

// TestXDP_AttachToNonExistentInterface verifies that:
//
//	Given a loaded XDP program,
//	When I try to attach it to a non-existent interface,
//	Then the operation fails with failure outcome and appropriate error.
func TestXDP_AttachToNonExistentInterface(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load an XDP program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("xdp.o"), "xdp_pass", bpfman.ProgramTypeXDP)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Configure the resolver to fail for this interface name.
	fix.Kernel.FailOnIfname("nonexistent0", fmt.Errorf("interface not found: %w", platform.ErrInterfaceNotFound))

	// Attempt to attach to non-existent interface
	attachSpec, err := bpfman.NewXDPAttachSpec(prog.Record.ProgramID, "nonexistent0")
	require.NoError(t, err, "spec creation should succeed")
	_, err = fix.Attach(ctx, attachSpec)
	require.Error(t, err, "AttachXDP to non-existent interface should fail")
	assert.ErrorIs(t, err, platform.ErrInterfaceNotFound, "error should identify an unresolved interface")
}

// TestTC_AttachToNonExistentInterface verifies that:
//
//	Given a loaded TC program,
//	When I try to attach it to a non-existent interface,
//	Then the operation fails with failure outcome and appropriate error.
func TestTC_AttachToNonExistentInterface(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TC program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tc.o"), "tc_pass", bpfman.ProgramTypeTC)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Configure kernel to fail for this interface name
	fix.Kernel.FailOnIfname("nonexistent0", fmt.Errorf("interface not found: %w", platform.ErrInterfaceNotFound))

	// Attempt to attach to non-existent interface
	attachSpec, err := bpfman.NewTCAttachSpec(prog.Record.ProgramID, "nonexistent0", bpfman.TCDirectionIngress)
	require.NoError(t, err, "spec creation should succeed")
	attachSpec = attachSpec.WithPriority(50)
	_, err = fix.Attach(ctx, attachSpec)
	require.Error(t, err, "AttachTC to non-existent interface should fail")
	assert.ErrorIs(t, err, platform.ErrInterfaceNotFound, "error should identify an unresolved interface")
}

// TestTCX_AttachToNonExistentInterface verifies that:
//
//	Given a loaded TCX program,
//	When I try to attach it to a non-existent interface,
//	Then the operation fails with failure outcome and appropriate error.
func TestTCX_AttachToNonExistentInterface(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load a TCX program
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tcx.o"), "tcx_pass", bpfman.ProgramTypeTCX)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err, "Load should succeed")

	// Configure the resolver to fail for this interface name.
	fix.Kernel.FailOnIfname("nonexistent0", fmt.Errorf("interface not found: %w", platform.ErrInterfaceNotFound))

	// Attempt to attach to non-existent interface
	attachSpec, err := bpfman.NewTCXAttachSpec(prog.Record.ProgramID, "nonexistent0", bpfman.TCDirectionIngress)
	require.NoError(t, err, "spec creation should succeed")
	attachSpec = attachSpec.WithPriority(50)
	_, err = fix.Attach(ctx, attachSpec)
	require.Error(t, err, "AttachTCX to non-existent interface should fail")
	assert.ErrorIs(t, err, platform.ErrInterfaceNotFound, "error should identify an unresolved interface")
}

// =============================================================================
// Tests with Server-Compatible Naming (for parity)
// =============================================================================

// TestAttach_ToNonExistentProgram_ReturnsNotFound verifies that:
//
//	Given an empty manager with no programs,
//	When I attempt to attach a non-existent program,
//	Then the manager returns ErrProgramNotFound as a plain error.
//
// Preflight failures (getProgram, prepare) return plain errors,
// consistent with Load and Unload preflight behaviour.
func TestAttach_ToNonExistentProgram_ReturnsNotFound(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Try to attach a program that doesn't exist
	attachSpec, err := bpfman.NewTracepointAttachSpecFromString(99999, "syscalls/sys_enter_open")
	require.NoError(t, err, "spec creation should succeed")
	_, err = fix.Attach(ctx, attachSpec)
	require.Error(t, err, "Attach to non-existent program should fail")

	var notFound bpfman.ErrProgramNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrProgramNotFound, got %T: %v", err, err)
	assert.Equal(t, kernel.ProgramID(99999), notFound.ID)
}

// TestAttach_ToNonExistentProgram_WinsOverTracepointPreflight verifies that:
//
//	Given a missing program ID and an unknown tracepoint,
//	When I attempt to attach,
//	Then the manager returns ErrProgramNotFound before any tracepoint validation error.
func TestAttach_ToNonExistentProgram_WinsOverTracepointPreflight(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	fix.Kernel.tracepoints = []string{"sched/sched_switch"}

	attachSpec, err := bpfman.NewTracepointAttachSpecFromString(99999, "syscalls/sched_switch")
	require.NoError(t, err, "spec creation should succeed")
	_, err = fix.Attach(ctx, attachSpec)
	require.Error(t, err, "Attach to non-existent program should fail")

	var notFound bpfman.ErrProgramNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrProgramNotFound, got %T: %v", err, err)
	assert.Equal(t, kernel.ProgramID(99999), notFound.ID)

	var tpErr bpfman.ErrTracepointNotFound
	assert.False(t, errors.As(err, &tpErr), "did not expect ErrTracepointNotFound, got %T: %v", err, err)
}

// TestGetLink_NonExistentLink_ReturnsNotFound verifies that:
//
//	Given an empty manager with no links,
//	When I attempt to get a non-existent link,
//	Then the manager returns ErrLinkNotFound.
func TestGetLink_NonExistentLink_ReturnsNotFound(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Try to get a link that doesn't exist
	_, err := fix.Manager.GetLink(ctx, bpfman.LinkID(99999))
	require.Error(t, err, "GetLink for non-existent link should fail")

	var notFound bpfman.ErrLinkNotFound
	assert.True(t, errors.As(err, &notFound), "expected ErrLinkNotFound, got %T: %v", err, err)
	assert.Equal(t, bpfman.LinkID(99999), notFound.LinkID)
}

// TestListPrograms_WithMetadataFilter_ReturnsOnlyMatching verifies that:
//
//	Given multiple programs with different metadata,
//	When I list programs filtering by metadata,
//	Then only matching programs are returned.
func TestListPrograms_WithMetadataFilter_ReturnsOnlyMatching(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// Load programs with different metadata
	for _, name := range []string{"prog1", "prog2", "prog3"} {
		spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), name, bpfman.ProgramTypeTracepoint)
		require.NoError(t, err)
		_, err = fix.Load(ctx, spec, manager.LoadOpts{
			UserMetadata: map[string]string{
				"bpfman.io/ProgramName": name,
				"app":                   "test-app",
			},
		})
		require.NoError(t, err)
	}

	// Load a program with different metadata
	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("prog.o"), "other_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	_, err = fix.Load(ctx, spec, manager.LoadOpts{
		UserMetadata: map[string]string{
			"bpfman.io/ProgramName": "other_prog",
			"app":                   "different-app",
		},
	})
	require.NoError(t, err)

	// List all programs
	result, err := fix.Manager.ListPrograms(ctx)
	require.NoError(t, err)
	assert.Len(t, result.Programs, 4, "should have 4 programs total")

	// Count programs with "app=test-app" metadata
	count := 0
	for _, p := range result.Programs {
		if p.Record.Meta.Metadata["app"] == "test-app" {
			count++
		}
	}
	assert.Equal(t, 3, count, "should have 3 programs with app=test-app")
}

// TestTracepointAttach_PreflightRejectsUnknown verifies that attaching
// to a tracepoint that is not present in the kernel's tracepoint list
// is rejected with bpfman.ErrTracepointNotFound before any kernel work
// is attempted, and that the error carries nearest-match suggestions.
func TestTracepointAttach_PreflightRejectsUnknown(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	fix.Kernel.tracepoints = []string{
		"sched/sched_switch",
		"syscalls/sys_enter_kill",
	}

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tracepoint.o"), "tp_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTracepointAttachSpecFromString(prog.Record.ProgramID, "syscalls/sched_switch")
	require.NoError(t, err)
	_, err = fix.Attach(ctx, attachSpec)
	require.Error(t, err)

	var tpErr bpfman.ErrTracepointNotFound
	require.ErrorAs(t, err, &tpErr, "expected ErrTracepointNotFound, got %T: %v", err, err)
	assert.Equal(t, "syscalls", tpErr.Group)
	assert.Equal(t, "sched_switch", tpErr.Name)
	assert.Contains(t, tpErr.Suggestions, "sched/sched_switch",
		"expected sched/sched_switch among suggestions, got %v", tpErr.Suggestions)
	assert.Contains(t, err.Error(), "did you mean")
}

// TestTracepointAttach_PreflightAllowsKnown verifies that an attach
// whose target is in the kernel's tracepoint list proceeds normally.
func TestTracepointAttach_PreflightAllowsKnown(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	fix.Kernel.tracepoints = []string{"syscalls/sys_enter_kill"}

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tracepoint.o"), "tp_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTracepointAttachSpecFromString(prog.Record.ProgramID, "syscalls/sys_enter_kill")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "attach to known tracepoint should succeed")
	assert.NotZero(t, link.Record.ID)
}

// TestTracepointAttach_PreflightSkippedWhenListEmpty verifies that the
// pre-flight check treats an empty tracepoint list as "cannot validate"
// and lets the attach proceed (the fakeKernel default preserves the
// existing behaviour of attach tests that don't stage a list).
func TestTracepointAttach_PreflightSkippedWhenListEmpty(t *testing.T) {
	t.Parallel()

	fix := newTestFixture(t)
	ctx := context.Background()

	// tracepoints left nil on purpose.

	spec, err := bpfman.NewLoadSpec(fix.BytecodeFile("tracepoint.o"), "tp_prog", bpfman.ProgramTypeTracepoint)
	require.NoError(t, err)
	prog, err := fix.Load(ctx, spec, manager.LoadOpts{})
	require.NoError(t, err)

	attachSpec, err := bpfman.NewTracepointAttachSpecFromString(prog.Record.ProgramID, "made_up/tracepoint")
	require.NoError(t, err)
	link, err := fix.Attach(ctx, attachSpec)
	require.NoError(t, err, "attach should succeed when tracepoint list is empty")
	assert.NotZero(t, link.Record.ID)
}
