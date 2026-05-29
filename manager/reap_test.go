package manager_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

// TestReapDeadProgramRecords drives the reap against the real in-memory
// SQLite store so the program-delete cascade (link rows via ON DELETE
// CASCADE) and the dependents-first ordering (the map_owner_id ON
// DELETE RESTRICT foreign key) are exercised for real, not against
// plain data.
//
// It loads two shared-map programs -- the second's map_owner_id points
// at the first -- attaches each so they own real link rows, then makes
// the pair disappear from the kernel while their store records remain.
// The reap must delete both program records (dependent before owner, or
// the RESTRICT would reject the owner), cascade-delete their links, and
// leave a still-live program and its link untouched.
func TestReapDeadProgramRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	discoverer := newFakeDiscoverer()
	f := newTestFixtureWithDiscoverer(t, discoverer)

	sharedObj := f.BytecodeFile("shared.o")
	discoverer.SetPrograms(sharedObj, []platform.DiscoveredProgram{
		{Name: "owner", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
		{Name: "dependent", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
	})
	shared, err := f.LoadDirect(ctx,
		manager.LoadSource{FilePath: sharedObj}, nil,
		manager.LoadOpts{ShareMaps: true})
	require.NoError(t, err)
	require.Len(t, shared, 2)
	ownerID := shared[0].Record.ProgramID
	dependentID := shared[1].Record.ProgramID

	// Guard: without a real map_owner_id FK the ordering assertion
	// would be hollow (both rows independently deletable).
	require.NotNil(t, shared[1].Record.Handles.MapOwnerID,
		"dependent must record a map owner; otherwise the test does not exercise the RESTRICT ordering")
	require.Equal(t, ownerID, *shared[1].Record.Handles.MapOwnerID)

	// A standalone program that stays live in the kernel.
	liveObj := f.BytecodeFile("live.o")
	discoverer.SetPrograms(liveObj, []platform.DiscoveredProgram{
		{Name: "live", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
	})
	live, err := f.LoadDirect(ctx,
		manager.LoadSource{FilePath: liveObj}, nil, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, live, 1)
	liveID := live[0].Record.ProgramID

	// Attach each program so it owns a real link row -- the rows that
	// must cascade-delete when their program is reaped. Distinct
	// interfaces keep the attaches independent; the manager now
	// resolves the name, so register the extra ones (eth0 is seeded).
	f.Kernel.InjectInterface("eth1", 3)
	f.Kernel.InjectInterface("eth2", 4)
	attach := func(progID kernel.ProgramID, ifname string) {
		t.Helper()
		spec, err := bpfman.NewTCXAttachSpec(progID, ifname, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		_, err = f.Attach(ctx, spec)
		require.NoError(t, err)
	}
	attach(ownerID, "eth0")
	attach(dependentID, "eth1")
	attach(liveID, "eth2")

	for _, id := range []kernel.ProgramID{ownerID, dependentID, liveID} {
		links, err := f.Store.ListLinksByProgram(ctx, id)
		require.NoError(t, err)
		require.NotEmpty(t, links, "program %d should own a link before the reap", id)
	}

	// The shared-map generation dies in the kernel while its store
	// records remain (daemon restart / external unload).
	f.Kernel.RemoveKernelProgram(ownerID)
	f.Kernel.RemoveKernelProgram(dependentID)

	require.NoError(t, f.Manager.ReapDeadProgramRecordsForTest(ctx))

	// Dead program records are gone, dependent and owner both.
	_, err = f.Store.Get(ctx, dependentID)
	assert.ErrorIs(t, err, platform.ErrRecordNotFound, "dead dependent should be reaped")
	_, err = f.Store.Get(ctx, ownerID)
	assert.ErrorIs(t, err, platform.ErrRecordNotFound, "dead map owner should be reaped after its dependent")

	// Their link rows cascade-deleted with them.
	for _, id := range []kernel.ProgramID{ownerID, dependentID} {
		links, err := f.Store.ListLinksByProgram(ctx, id)
		require.NoError(t, err)
		assert.Empty(t, links, "dead program %d links should be cascade-deleted", id)
	}

	// The live program and its link survive untouched.
	_, err = f.Store.Get(ctx, liveID)
	assert.NoError(t, err, "live program must be preserved")
	liveLinks, err := f.Store.ListLinksByProgram(ctx, liveID)
	require.NoError(t, err)
	assert.NotEmpty(t, liveLinks, "live program's link must be preserved")
}

// TestReapKeepsDeadOwnerWithLiveDependent proves the reap does not
// over-prune: a dead map owner must stay while a live dependent still
// shares its maps. The map_owner_id ON DELETE RESTRICT FK would reject
// deleting the owner anyway, and pulling the maps out from under a live
// program would be wrong -- so the planner must leave it.
func TestReapKeepsDeadOwnerWithLiveDependent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	discoverer := newFakeDiscoverer()
	f := newTestFixtureWithDiscoverer(t, discoverer)

	obj := f.BytecodeFile("shared.o")
	discoverer.SetPrograms(obj, []platform.DiscoveredProgram{
		{Name: "owner", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
		{Name: "dependent", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
	})
	progs, err := f.LoadDirect(ctx,
		manager.LoadSource{FilePath: obj}, nil,
		manager.LoadOpts{ShareMaps: true})
	require.NoError(t, err)
	require.Len(t, progs, 2)
	ownerID := progs[0].Record.ProgramID
	dependentID := progs[1].Record.ProgramID
	require.NotNil(t, progs[1].Record.Handles.MapOwnerID)

	// Owner dies in the kernel; its dependent stays live.
	f.Kernel.RemoveKernelProgram(ownerID)

	require.NoError(t, f.Manager.ReapDeadProgramRecordsForTest(ctx))

	_, err = f.Store.Get(ctx, ownerID)
	assert.NoError(t, err, "dead owner with a live dependent must be preserved")
	_, err = f.Store.Get(ctx, dependentID)
	assert.NoError(t, err, "live dependent must be preserved")
}
