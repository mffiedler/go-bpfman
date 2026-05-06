package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
	"github.com/frobware/go-bpfman/platform/store/sqlite"
)

// testXDPProgram returns a ProgramRecord for XDP testing.
func testXDPProgram(name string) bpfman.ProgramRecord {
	return bpfman.ProgramRecord{
		Load: bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeXDP, "/test/path/"+name+".o"),
		Handles: bpfman.ProgramHandles{
			PinPath: bpfman.ProgPinPath("/sys/fs/bpf/" + name),
		},
		Meta: bpfman.ProgramMeta{
			Name: name,
		},
		CreatedAt: time.Now(),
	}
}

// testTCProgram returns a ProgramRecord for TC testing.
func testTCProgram(name string) bpfman.ProgramRecord {
	return bpfman.ProgramRecord{
		Load: bpfman.TestLoadSpecWithPath(bpfman.ProgramTypeTC, "/test/path/"+name+".o"),
		Handles: bpfman.ProgramHandles{
			PinPath: bpfman.ProgPinPath("/sys/fs/bpf/" + name),
		},
		Meta: bpfman.ProgramMeta{
			Name: name,
		},
		CreatedAt: time.Now(),
	}
}

const (
	testNsid    = uint64(4026531840)
	testIfindex = uint32(2)
)

func xdpKey() dispatcher.Key {
	return dispatcher.Key{
		Type:    dispatcher.DispatcherTypeXDP,
		Nsid:    testNsid,
		Ifindex: testIfindex,
	}
}

func tcIngressKey() dispatcher.Key {
	return dispatcher.Key{
		Type:    dispatcher.DispatcherTypeTCIngress,
		Nsid:    testNsid,
		Ifindex: testIfindex,
	}
}

// setupXDPSnapshot creates managed programs and returns a snapshot
// ready for ReplaceDispatcherSnapshot.
func setupXDPSnapshot(t *testing.T, ctx context.Context, store platform.Store) platform.DispatcherSnapshot {
	t.Helper()

	// Create two managed programs that will be extensions.
	prog1ID := kernel.ProgramID(1001)
	prog2ID := kernel.ProgramID(1002)
	require.NoError(t, store.Save(ctx, prog1ID, testXDPProgram("xdp_prog1")))
	require.NoError(t, store.Save(ctx, prog2ID, testXDPProgram("xdp_prog2")))

	dispProgramID := kernel.ProgramID(500)
	linkID := kernel.LinkID(501)

	return platform.DispatcherSnapshot{
		Key:      xdpKey(),
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID: dispProgramID,
			LinkID:    &linkID,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:   prog1ID,
				ProgramName: "xdp_prog1",
				ProgPinPath: "/sys/fs/bpf/xdp_prog1",
				LinkID:      kernel.LinkID(0x80000001),
				LinkPinPath: "/sys/fs/bpf/dispatch/r1/link0",
				Position:    0,
				Priority:    50,
				ProceedOn:   0x04, // XDP_PASS
				Ifname:      "eth0",
			},
			{
				ProgramID:   prog2ID,
				ProgramName: "xdp_prog2",
				ProgPinPath: "/sys/fs/bpf/xdp_prog2",
				LinkID:      kernel.LinkID(0x80000002),
				LinkPinPath: "/sys/fs/bpf/dispatch/r1/link1",
				Position:    1,
				Priority:    100,
				ProceedOn:   0x06, // XDP_PASS | XDP_DROP
				Ifname:      "eth0",
			},
		},
	}
}

func TestSnapshotStore_ReplaceAndGet_XDP(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	snap := setupXDPSnapshot(t, ctx, store)

	// Replace (first time = create).
	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap))

	// Get the snapshot back.
	got, err := store.GetDispatcherSnapshot(ctx, xdpKey())
	require.NoError(t, err)

	assert.Equal(t, snap.Key, got.Key)
	assert.Equal(t, snap.Revision, got.Revision)
	assert.Equal(t, snap.Runtime.ProgramID, got.Runtime.ProgramID)
	require.NotNil(t, got.Runtime.LinkID)
	assert.Equal(t, *snap.Runtime.LinkID, *got.Runtime.LinkID)
	require.Len(t, got.Members, 2)

	// Members should be ordered by priority.
	assert.Equal(t, "xdp_prog1", got.Members[0].ProgramName)
	assert.Equal(t, 0, got.Members[0].Position)
	assert.Equal(t, 50, got.Members[0].Priority)
	assert.Equal(t, uint32(0x04), got.Members[0].ProceedOn)
	assert.Equal(t, "eth0", got.Members[0].Ifname)
	assert.Equal(t, kernel.LinkID(0x80000001), got.Members[0].LinkID)

	assert.Equal(t, "xdp_prog2", got.Members[1].ProgramName)
	assert.Equal(t, 1, got.Members[1].Position)
	assert.Equal(t, 100, got.Members[1].Priority)
}

func TestSnapshotStore_ReplaceRemovesOldMembership(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	snap := setupXDPSnapshot(t, ctx, store)

	// Initial replace with two members.
	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap))

	got, err := store.GetDispatcherSnapshot(ctx, xdpKey())
	require.NoError(t, err)
	require.Len(t, got.Members, 2)

	// Replace again with only one member (simulating detach of prog2).
	// New dispatcher program ID from rebuild.
	newDispProgramID := kernel.ProgramID(600)
	newLinkID := kernel.LinkID(601)
	snap2 := platform.DispatcherSnapshot{
		Key:      xdpKey(),
		Revision: 2,
		Runtime: platform.DispatcherRuntime{
			ProgramID: newDispProgramID,
			LinkID:    &newLinkID,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:   kernel.ProgramID(1001),
				ProgramName: "xdp_prog1",
				ProgPinPath: "/sys/fs/bpf/xdp_prog1",
				LinkID:      kernel.LinkID(0x80000003),
				LinkPinPath: "/sys/fs/bpf/dispatch/r2/link0",
				Position:    0,
				Priority:    50,
				ProceedOn:   0x04,
				Ifname:      "eth0",
			},
		},
	}

	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap2))

	got2, err := store.GetDispatcherSnapshot(ctx, xdpKey())
	require.NoError(t, err)
	assert.Equal(t, uint32(2), got2.Revision)
	assert.Equal(t, newDispProgramID, got2.Runtime.ProgramID)
	require.Len(t, got2.Members, 1)
	assert.Equal(t, "xdp_prog1", got2.Members[0].ProgramName)
	assert.Equal(t, kernel.LinkID(0x80000003), got2.Members[0].LinkID)

	// Old link records should be gone (the links table cascade
	// removed old detail rows).
	_, err = store.GetLink(ctx, kernel.LinkID(0x80000001))
	require.Error(t, err, "old link should be deleted")
	_, err = store.GetLink(ctx, kernel.LinkID(0x80000002))
	require.Error(t, err, "old link should be deleted")
}

func TestSnapshotStore_DeleteSnapshot(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	snap := setupXDPSnapshot(t, ctx, store)

	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap))

	// Delete the snapshot.
	require.NoError(t, store.DeleteDispatcherSnapshot(ctx, xdpKey()))

	// Dispatcher should be gone.
	_, err = store.GetDispatcherSnapshot(ctx, xdpKey())
	require.Error(t, err)
	assert.True(t, errors.Is(err, platform.ErrRecordNotFound))

	// Extension links should be gone.
	_, err = store.GetLink(ctx, kernel.LinkID(0x80000001))
	require.Error(t, err)
	_, err = store.GetLink(ctx, kernel.LinkID(0x80000002))
	require.Error(t, err)
}

func TestSnapshotStore_DeleteNonExistent(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	err = store.DeleteDispatcherSnapshot(ctx, xdpKey())
	require.Error(t, err)
	assert.True(t, errors.Is(err, platform.ErrRecordNotFound))
}

func TestSnapshotStore_GetNonExistent(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	_, err = store.GetDispatcherSnapshot(ctx, xdpKey())
	require.Error(t, err)
	assert.True(t, errors.Is(err, platform.ErrRecordNotFound))
}

func TestSnapshotStore_TransactionRollback(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	snap := setupXDPSnapshot(t, ctx, store)

	// First, create the snapshot so we have known state.
	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap))

	// Attempt a replacement inside a transaction that rolls back.
	deliberateErr := errors.New("deliberate rollback")
	err = store.RunInTransaction(ctx, func(txStore platform.Store) error {
		snap2 := snap
		snap2.Revision = 99
		snap2.Members = nil // empty members
		if err := txStore.ReplaceDispatcherSnapshot(ctx, snap2); err != nil {
			return err
		}
		return deliberateErr
	})
	require.ErrorIs(t, err, deliberateErr)

	// Original snapshot should be intact.
	got, err := store.GetDispatcherSnapshot(ctx, xdpKey())
	require.NoError(t, err)
	assert.Equal(t, uint32(1), got.Revision)
	require.Len(t, got.Members, 2)
}

func TestSnapshotStore_ListSummaries(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create programs for XDP.
	prog1ID := kernel.ProgramID(1001)
	prog2ID := kernel.ProgramID(1002)
	require.NoError(t, store.Save(ctx, prog1ID, testXDPProgram("xdp_prog1")))
	require.NoError(t, store.Save(ctx, prog2ID, testXDPProgram("xdp_prog2")))

	// Create programs for TC.
	prog3ID := kernel.ProgramID(2001)
	require.NoError(t, store.Save(ctx, prog3ID, testTCProgram("tc_prog1")))

	// XDP dispatcher with 2 members.
	xdpLinkID := kernel.LinkID(501)
	xdpSnap := platform.DispatcherSnapshot{
		Key:      xdpKey(),
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID: kernel.ProgramID(500),
			LinkID:    &xdpLinkID,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:   prog1ID,
				ProgramName: "xdp_prog1",
				ProgPinPath: "/sys/fs/bpf/xdp_prog1",
				LinkID:      kernel.LinkID(0x80000001),
				LinkPinPath: "/sys/fs/bpf/dispatch/xdp/link0",
				Position:    0,
				Priority:    50,
				ProceedOn:   0x04,
				Ifname:      "eth0",
			},
			{
				ProgramID:   prog2ID,
				ProgramName: "xdp_prog2",
				ProgPinPath: "/sys/fs/bpf/xdp_prog2",
				LinkID:      kernel.LinkID(0x80000002),
				LinkPinPath: "/sys/fs/bpf/dispatch/xdp/link1",
				Position:    1,
				Priority:    100,
				ProceedOn:   0x04,
				Ifname:      "eth0",
			},
		},
	}
	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, xdpSnap))

	// TC ingress dispatcher with 1 member.
	tcPriority := uint16(50)
	tcSnap := platform.DispatcherSnapshot{
		Key:      tcIngressKey(),
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID:      kernel.ProgramID(700),
			FilterPriority: &tcPriority,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:   prog3ID,
				ProgramName: "tc_prog1",
				ProgPinPath: "/sys/fs/bpf/tc_prog1",
				LinkID:      kernel.LinkID(0x80000010),
				LinkPinPath: "/sys/fs/bpf/dispatch/tc/link0",
				Position:    0,
				Priority:    50,
				ProceedOn:   0x04,
				Ifname:      "eth0",
			},
		},
	}
	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, tcSnap))

	// List summaries.
	summaries, err := store.ListDispatcherSummaries(ctx)
	require.NoError(t, err)
	require.Len(t, summaries, 2)

	// Build a map by dispatcher type for order-independent assertion.
	byType := map[string]platform.DispatcherSummary{}
	for _, s := range summaries {
		byType[s.Key.Type.String()] = s
	}

	xdpSummary := byType["xdp"]
	assert.Equal(t, 2, xdpSummary.MemberCount)
	assert.Equal(t, kernel.ProgramID(500), xdpSummary.Runtime.ProgramID)
	require.NotNil(t, xdpSummary.Runtime.LinkID)
	assert.Equal(t, kernel.LinkID(501), *xdpSummary.Runtime.LinkID)

	tcSummary := byType["tc-ingress"]
	assert.Equal(t, 1, tcSummary.MemberCount)
	assert.Equal(t, kernel.ProgramID(700), tcSummary.Runtime.ProgramID)
	assert.Nil(t, tcSummary.Runtime.LinkID)
	require.NotNil(t, tcSummary.Runtime.FilterPriority)
	assert.Equal(t, uint16(50), *tcSummary.Runtime.FilterPriority)
}

func TestSnapshotStore_TC_NullLinkID(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Create a TC program.
	progID := kernel.ProgramID(2001)
	require.NoError(t, store.Save(ctx, progID, testTCProgram("tc_prog")))

	tcPriority := uint16(50)
	snap := platform.DispatcherSnapshot{
		Key:      tcIngressKey(),
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID:      kernel.ProgramID(700),
			FilterPriority: &tcPriority,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:   progID,
				ProgramName: "tc_prog",
				ProgPinPath: "/sys/fs/bpf/tc_prog",
				LinkID:      kernel.LinkID(0x80000010),
				LinkPinPath: "/sys/fs/bpf/dispatch/tc/link0",
				Position:    0,
				Priority:    50,
				ProceedOn:   0x04,
				Ifname:      "eth0",
			},
		},
	}

	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap))

	// Verify via snapshot API.
	got, err := store.GetDispatcherSnapshot(ctx, tcIngressKey())
	require.NoError(t, err)
	assert.Nil(t, got.Runtime.LinkID, "TC dispatchers should have nil LinkID")
	require.NotNil(t, got.Runtime.FilterPriority)
	assert.Equal(t, uint16(50), *got.Runtime.FilterPriority)
	require.Len(t, got.Members, 1)

}

func TestSnapshotStore_EmptyMembers(t *testing.T) {
	t.Parallel()

	store, err := sqlite.NewInMemory(context.Background(), testLogger())
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()

	// Replace with zero members (dispatcher exists but no extensions).
	linkID := kernel.LinkID(501)
	snap := platform.DispatcherSnapshot{
		Key:      xdpKey(),
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID: kernel.ProgramID(500),
			LinkID:    &linkID,
		},
		Members: nil,
	}

	require.NoError(t, store.ReplaceDispatcherSnapshot(ctx, snap))

	got, err := store.GetDispatcherSnapshot(ctx, xdpKey())
	require.NoError(t, err)
	assert.Equal(t, uint32(1), got.Revision)
	assert.Empty(t, got.Members)
}
