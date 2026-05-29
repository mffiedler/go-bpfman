package manager_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/lock"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/platform"
)

func TestDeleteProgramsRecursiveTreatsBatchDependentsAsDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newSharedMapDeleteFixture(t, ctx)
	ownerID := f.ownerID
	dependentID := f.dependentID

	var results []manager.DeleteProgramResult
	err := lock.Run(ctx, f.fixture.Layout.LockPath(), func(ctx context.Context, writeLock lock.WriterScope) error {
		results = f.fixture.Manager.DeletePrograms(ctx, writeLock,
			[]kernel.ProgramID{ownerID, dependentID},
			manager.DeleteProgramsOpts{Recursive: true})
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.NoError(t, results[0].Err)
	assert.NoError(t, results[1].Err)

	assertProgramRecordDeleted(t, ctx, f.fixture, ownerID)
	assertProgramRecordDeleted(t, ctx, f.fixture, dependentID)
}

func TestDeleteProgramsAllRecursiveSucceedsWithSharedMapDependents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newSharedMapDeleteFixture(t, ctx)

	ids, err := f.fixture.Manager.ResolveDeleteProgramIDs(ctx, true, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []kernel.ProgramID{f.ownerID, f.dependentID}, ids)

	var results []manager.DeleteProgramResult
	err = lock.Run(ctx, f.fixture.Layout.LockPath(), func(ctx context.Context, writeLock lock.WriterScope) error {
		results = f.fixture.Manager.DeletePrograms(ctx, writeLock, ids,
			manager.DeleteProgramsOpts{Recursive: true})
		return nil
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, result := range results {
		assert.NoError(t, result.Err)
	}

	assertProgramRecordDeleted(t, ctx, f.fixture, f.ownerID)
	assertProgramRecordDeleted(t, ctx, f.fixture, f.dependentID)
}

type sharedMapDeleteFixture struct {
	fixture     *testFixture
	ownerID     kernel.ProgramID
	dependentID kernel.ProgramID
}

func newSharedMapDeleteFixture(t *testing.T, ctx context.Context) sharedMapDeleteFixture {
	t.Helper()

	discoverer := newFakeDiscoverer()
	f := newTestFixtureWithDiscoverer(t, discoverer)

	obj := f.BytecodeFile("shared-delete.o")
	discoverer.SetPrograms(obj, []platform.DiscoveredProgram{
		{Name: "owner", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
		{Name: "dependent", SectionName: "tcx", Type: bpfman.ProgramTypeTCX},
	})

	progs, err := f.LoadDirect(ctx,
		manager.LoadSource{FilePath: obj}, nil,
		manager.LoadOpts{ShareMaps: true})
	require.NoError(t, err)
	require.Len(t, progs, 2)
	require.NotNil(t, progs[1].Record.Handles.MapOwnerID)
	require.Equal(t, progs[0].Record.ProgramID, *progs[1].Record.Handles.MapOwnerID)

	return sharedMapDeleteFixture{
		fixture:     f,
		ownerID:     progs[0].Record.ProgramID,
		dependentID: progs[1].Record.ProgramID,
	}
}

func assertProgramRecordDeleted(t *testing.T, ctx context.Context, f *testFixture, id kernel.ProgramID) {
	t.Helper()
	_, err := f.Store.Get(ctx, id)
	assert.ErrorIs(t, err, platform.ErrRecordNotFound)
}
