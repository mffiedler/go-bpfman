//go:build e2e

package e2e

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
)

// dispatcherTestHarness encapsulates TC-vs-XDP differences behind a
// uniform interface so that tests exercising the shared dispatcher
// machinery can be parameterised across both dispatcher types.
type dispatcherTestHarness struct {
	name     string
	env      *TestEnv
	dispType dispatcher.DispatcherType
	iface    TestInterface

	// loadProg loads the BPF object file and returns the kernel
	// program ID. The program is automatically unloaded on test
	// cleanup.
	loadProg func(t *testing.T) kernel.ProgramID

	// makeAttachSpec creates an attach spec for the given program,
	// interface, and priority. For XDP the priority is ignored.
	makeAttachSpec func(t *testing.T, progID kernel.ProgramID, iface TestInterface, priority int) bpfman.AttachSpec

	// verifyAttachPresent asserts the dispatcher's kernel-level
	// attachment exists (TC: tc filter present; XDP: link pin exists).
	verifyAttachPresent func(t *testing.T)

	// verifyAttachAbsent asserts the dispatcher's kernel-level
	// attachment has been cleaned up.
	verifyAttachAbsent func(t *testing.T)
}

// attach creates an attachment on the harness's default interface.
func (h *dispatcherTestHarness) attach(t *testing.T, progID kernel.ProgramID, priority int) bpfman.LinkRecord {
	t.Helper()
	return h.attachTo(t, progID, h.iface, priority)
}

// attachTo creates an attachment on a specific interface.
func (h *dispatcherTestHarness) attachTo(t *testing.T, progID kernel.ProgramID, iface TestInterface, priority int) bpfman.LinkRecord {
	t.Helper()
	spec := h.makeAttachSpec(t, progID, iface, priority)
	link, err := h.env.Attach(context.Background(), spec)
	require.NoError(t, err)
	return link
}

// tryAttach attempts an attachment and returns the error (if any)
// instead of failing the test. Used by tests that expect failure.
func (h *dispatcherTestHarness) tryAttach(t *testing.T, progID kernel.ProgramID, priority int) (bpfman.LinkRecord, error) {
	t.Helper()
	spec := h.makeAttachSpec(t, progID, h.iface, priority)
	return h.env.Attach(context.Background(), spec)
}

// extensionCount returns the number of extension links attached to
// the dispatcher for the harness's default interface.
func (h *dispatcherTestHarness) extensionCount(t *testing.T) int {
	t.Helper()
	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	snap, err := h.env.GetDispatcherSnapshot(context.Background(), dispatcher.Key{
		Type: h.dispType, Nsid: nsid, Ifindex: uint32(h.iface.Ifindex),
	})
	require.NoError(t, err, "dispatcher should exist")
	return len(snap.Members)
}

// linkPosition returns the dispatcher position stored in the link
// details for the given link ID.
func (h *dispatcherTestHarness) linkPosition(t *testing.T, linkID kernel.LinkID) int32 {
	t.Helper()
	_, details, err := h.env.GetLink(context.Background(), linkID)
	require.NoError(t, err)
	switch d := details.(type) {
	case bpfman.XDPDetails:
		return d.Position
	case bpfman.TCDetails:
		return d.Position
	default:
		t.Fatalf("unexpected link details type %T", details)
		return -1
	}
}

// linkPriority returns the priority stored in the link details for the
// given link ID.
func (h *dispatcherTestHarness) linkPriority(t *testing.T, linkID kernel.LinkID) int32 {
	t.Helper()
	_, details, err := h.env.GetLink(context.Background(), linkID)
	require.NoError(t, err)
	switch d := details.(type) {
	case bpfman.XDPDetails:
		return d.Priority
	case bpfman.TCDetails:
		return d.Priority
	default:
		t.Fatalf("unexpected link details type %T", details)
		return -1
	}
}

// newTCIngressHarness creates a harness for TC ingress dispatcher
// tests. It creates its own TestEnv and TestInterface for isolation.
func newTCIngressHarness(t *testing.T) dispatcherTestHarness {
	t.Helper()
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)

	return dispatcherTestHarness{
		name:     "tc-ingress",
		env:      env,
		dispType: dispatcher.DispatcherTypeTCIngress,
		iface:    iface,

		loadProg: func(t *testing.T) kernel.ProgramID {
			t.Helper()
			programs, err := env.LoadFile(context.Background(),
				"testdata/tc_counter.bpf.o", []manager.ProgramSpec{
					{Type: bpfman.ProgramTypeTC, Name: "stats"},
				}, manager.LoadOpts{})
			require.NoError(t, err)
			require.Len(t, programs, 1)
			t.Cleanup(func() {
				env.Unload(context.Background(), programs[0].Status.Kernel.ID)
			})
			return programs[0].Status.Kernel.ID
		},

		makeAttachSpec: func(t *testing.T, progID kernel.ProgramID, iface TestInterface, priority int) bpfman.AttachSpec {
			t.Helper()
			tcSpec, err := bpfman.NewTCAttachSpec(
				progID, iface.Name, iface.Ifindex,
				bpfman.TCDirectionIngress,
			)
			require.NoError(t, err)
			return tcSpec.WithPriority(priority)
		},

		verifyAttachPresent: func(t *testing.T) {
			t.Helper()
			filters := tcIngressFilters(t, iface.Name)
			require.NotEmpty(t, filters, "TC filter should be present")
		},

		verifyAttachAbsent: func(t *testing.T) {
			t.Helper()
			filters := tcIngressFilters(t, iface.Name)
			assert.Empty(t, filters, "TC filter should be removed")
		},
	}
}

// newXDPHarness creates a harness for XDP dispatcher tests. It
// creates its own TestEnv and TestInterface for isolation.
func newXDPHarness(t *testing.T) dispatcherTestHarness {
	t.Helper()

	env := NewTestEnv(t)
	iface := NewTestInterface(t)

	return dispatcherTestHarness{
		name:     "xdp",
		env:      env,
		dispType: dispatcher.DispatcherTypeXDP,
		iface:    iface,

		loadProg: func(t *testing.T) kernel.ProgramID {
			t.Helper()
			programs, err := env.LoadFile(context.Background(),
				"testdata/xdp_counter.bpf.o", []manager.ProgramSpec{
					{Type: bpfman.ProgramTypeXDP, Name: "xdp_stats"},
				}, manager.LoadOpts{})
			require.NoError(t, err)
			require.Len(t, programs, 1)
			t.Cleanup(func() {
				env.Unload(context.Background(), programs[0].Status.Kernel.ID)
			})
			return programs[0].Status.Kernel.ID
		},

		makeAttachSpec: func(t *testing.T, progID kernel.ProgramID, iface TestInterface, priority int) bpfman.AttachSpec {
			t.Helper()
			xdpSpec, err := bpfman.NewXDPAttachSpec(progID, iface.Name, iface.Ifindex)
			require.NoError(t, err)
			return xdpSpec.WithPriority(priority)
		},

		verifyAttachPresent: func(t *testing.T) {
			t.Helper()
			nsid, err := netns.GetCurrentNsid()
			require.NoError(t, err)
			linkPin := env.Layout.BPFFS().DispatcherLinkPath(
				dispatcher.DispatcherTypeXDP, nsid, uint32(iface.Ifindex))
			_, err = os.Stat(linkPin)
			require.NoError(t, err, "XDP link pin should exist: %s", linkPin)
		},

		verifyAttachAbsent: func(t *testing.T) {
			t.Helper()
			nsid, err := netns.GetCurrentNsid()
			require.NoError(t, err)
			linkPin := env.Layout.BPFFS().DispatcherLinkPath(
				dispatcher.DispatcherTypeXDP, nsid, uint32(iface.Ifindex))
			_, err = os.Stat(linkPin)
			assert.True(t, os.IsNotExist(err),
				"XDP link pin should not exist: %s", linkPin)
		},
	}
}

// eachDispatcherType returns harnesses for every dispatcher type that
// shares the dispatcher code path (TC ingress and XDP).
func eachDispatcherType(t *testing.T) []dispatcherTestHarness {
	t.Helper()
	return []dispatcherTestHarness{
		newTCIngressHarness(t),
		newXDPHarness(t),
	}
}

// TestDispatcher_PriorityOrdering verifies that filling all 10
// dispatcher slots with scrambled priorities produces positions that
// reflect the correct sorted order. Extensions are sorted by priority
// ascending for both TC and XDP; when priorities collide, the
// secondary sort is by program name.
func TestDispatcher_PriorityOrdering(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testPriorityOrdering(t, h)
		})
	}
}

func testPriorityOrdering(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Attach all 10 slots with scrambled priorities so the
	// dispatcher must reorder them.
	priorities := []int{500, 100, 800, 300, 900, 200, 700, 400, 600, 50}
	var links []bpfman.LinkRecord
	for _, prio := range priorities {
		link := h.attach(t, progID, prio)
		links = append(links, link)
	}

	t.Cleanup(func() {
		for _, link := range links {
			h.env.Detach(context.Background(), link.ID)
		}
	})

	require.Equal(t, 10, h.extensionCount(t),
		"all 10 slots should be occupied")

	// Positions reflect priority ascending. Build the expected
	// position for each link by sorting priorities.
	sorted := make([]int, len(priorities))
	copy(sorted, priorities)
	sort.Ints(sorted)

	rankByPriority := make(map[int]int32, len(sorted))
	for i, p := range sorted {
		rankByPriority[p] = int32(i)
	}

	for i, link := range links {
		pos := h.linkPosition(t, link.ID)
		expected := rankByPriority[priorities[i]]
		assert.Equal(t, expected, pos,
			"link %d (priority %d): position should be %d, got %d",
			i, priorities[i], expected, pos)

		prio := h.linkPriority(t, link.ID)
		assert.Equal(t, int32(priorities[i]), prio,
			"link %d: stored priority should match requested priority %d, got %d",
			i, priorities[i], prio)
	}
}

// TestDispatcher_ZeroPriorityDefaultOrdering verifies that attaching
// a program with priority=0 stores 0 in the link details (not the
// default value 50) and that the dispatcher correctly orders the
// program as if its effective priority were 50.
func TestDispatcher_ZeroPriorityDefaultOrdering(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testZeroPriorityDefaultOrdering(t, h)
		})
	}
}

func testZeroPriorityDefaultOrdering(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Attach three programs: priority 25 (runs first), priority 0
	// (should behave as effective priority 50), and priority 75
	// (runs last).
	link25 := h.attach(t, progID, 25)
	link0 := h.attach(t, progID, 0)
	link75 := h.attach(t, progID, 75)

	t.Cleanup(func() {
		h.env.Detach(context.Background(), link25.ID)
		h.env.Detach(context.Background(), link0.ID)
		h.env.Detach(context.Background(), link75.ID)
	})

	// The stored priority should be exactly what was requested.
	assert.Equal(t, int32(25), h.linkPriority(t, link25.ID),
		"priority=25 should be stored as 25")
	assert.Equal(t, int32(0), h.linkPriority(t, link0.ID),
		"priority=0 should be stored as 0, not defaulted to 50")
	assert.Equal(t, int32(75), h.linkPriority(t, link75.ID),
		"priority=75 should be stored as 75")

	// The effective ordering should treat priority=0 as 50:
	// position 0: priority 25
	// position 1: priority 0 (effective 50)
	// position 2: priority 75
	assert.Equal(t, int32(0), h.linkPosition(t, link25.ID),
		"priority=25 should be at position 0")
	assert.Equal(t, int32(1), h.linkPosition(t, link0.ID),
		"priority=0 (effective 50) should be at position 1")
	assert.Equal(t, int32(2), h.linkPosition(t, link75.ID),
		"priority=75 should be at position 2")
}

// TestDispatcher_AttachExceedsMaxPrograms verifies that attempting to
// attach an 11th program fails with a "no free slots" error.
func TestDispatcher_AttachExceedsMaxPrograms(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testAttachExceedsMaxPrograms(t, h)
		})
	}
}

func testAttachExceedsMaxPrograms(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	var links []bpfman.LinkRecord
	for i := 0; i < dispatcher.MaxPrograms; i++ {
		link := h.attach(t, progID, (i+1)*100)
		links = append(links, link)
	}

	t.Cleanup(func() {
		for _, link := range links {
			h.env.Detach(context.Background(), link.ID)
		}
	})

	_, err := h.tryAttach(t, progID, 1100)
	require.Error(t, err, "11th attach should fail")
	assert.Contains(t, err.Error(), "no free dispatcher slots")
}

// TestDispatcher_SlotReusedAfterDetach verifies that detaching a
// program frees a slot that can be reused, and that the reattached
// program lands at the correct position in the sorted order.
func TestDispatcher_SlotReusedAfterDetach(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testSlotReusedAfterDetach(t, h)
		})
	}
}

func testSlotReusedAfterDetach(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Fill all 10 slots with ascending priorities.
	// links[i] has priority (i+1)*100: 100, 200, ..., 1000.
	var links []bpfman.LinkRecord
	for i := 0; i < dispatcher.MaxPrograms; i++ {
		link := h.attach(t, progID, (i+1)*100)
		links = append(links, link)
	}

	require.Equal(t, dispatcher.MaxPrograms, h.extensionCount(t),
		"all 10 slots should be occupied")

	// Detach the 4th attachment (priority 400).
	err := h.env.Detach(context.Background(), links[3].ID)
	require.NoError(t, err, "detach link at priority 400")

	assert.Equal(t, dispatcher.MaxPrograms-1, h.extensionCount(t),
		"should have 9 programs after detach")

	// Re-attach at priority 350. This should slot between
	// priorities 300 (position 2) and 500 (position 4), landing
	// at position 3.
	newLink := h.attach(t, progID, 350)

	t.Cleanup(func() {
		for i, link := range links {
			if i == 3 {
				continue
			}
			h.env.Detach(context.Background(), link.ID)
		}
		h.env.Detach(context.Background(), newLink.ID)
	})

	require.Equal(t, 10, h.extensionCount(t),
		"all 10 slots should be occupied again")

	newPos := h.linkPosition(t, newLink.ID)

	// Sorted: [100,200,300,350,500,600,700,800,900,1000]
	// The new link at priority 350 should have position 3.
	assert.Equal(t, int32(3), newPos,
		"reattached link (priority 350) should have position 3")
}

// TestDispatcher_LifecycleAfterLastDetach verifies that removing the
// last attached program tears down the dispatcher entirely (pins
// removed), and that a fresh attachment creates a new dispatcher.
func TestDispatcher_LifecycleAfterLastDetach(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testLifecycleAfterLastDetach(t, h)
		})
	}
}

func testLifecycleAfterLastDetach(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	ifindex := uint32(h.iface.Ifindex)

	dispKey := dispatcher.Key{Type: h.dispType, Nsid: nsid, Ifindex: ifindex}

	// Phase 1: attach a single program. A dispatcher should exist.
	link := h.attach(t, progID, 100)

	snap1, err := h.env.GetDispatcherSnapshot(context.Background(), dispKey)
	require.NoError(t, err, "dispatcher should exist after attach")
	h.verifyAttachPresent(t)

	require.Len(t, snap1.Members, 1, "should have 1 program")

	// Phase 2: detach the only program. The dispatcher should be
	// fully cleaned up.
	err = h.env.Detach(context.Background(), link.ID)
	require.NoError(t, err)

	_, err = h.env.GetDispatcherSnapshot(context.Background(), dispKey)
	require.ErrorIs(t, err, platform.ErrRecordNotFound,
		"dispatcher should be absent from store after last detach")

	h.verifyAttachAbsent(t)

	// Phase 3: attach again. A new dispatcher should be created
	// with a different program ID.
	newLink := h.attach(t, progID, 200)
	t.Cleanup(func() {
		h.env.Detach(context.Background(), newLink.ID)
	})

	snap2, err := h.env.GetDispatcherSnapshot(context.Background(), dispKey)
	require.NoError(t, err, "dispatcher should exist after second attach")
	assert.NotEqual(t, snap1.Runtime.ProgramID, snap2.Runtime.ProgramID,
		"second dispatcher should have a different program ID")

	h.verifyAttachPresent(t)
	require.Len(t, snap2.Members, 1, "should have 1 program after reattach")
}

// TestDispatcher_MultipleInterfacesIndependent verifies that
// dispatcher state on one interface is independent of another.
// Detaching from interface B must not affect interface A's extension count.
func TestDispatcher_MultipleInterfacesIndependent(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testMultipleInterfacesIndependent(t, h)
		})
	}
}

func testMultipleInterfacesIndependent(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	ifaceB := NewTestInterface(t)

	// Attach 3 programs to default interface (A).
	var linksA []bpfman.LinkRecord
	for i := 0; i < 3; i++ {
		link := h.attach(t, progID, (i+1)*100)
		linksA = append(linksA, link)
	}
	t.Cleanup(func() {
		for _, l := range linksA {
			h.env.Detach(context.Background(), l.ID)
		}
	})

	// Attach 2 programs to interface B.
	var linksB []bpfman.LinkRecord
	for i := 0; i < 2; i++ {
		link := h.attachTo(t, progID, ifaceB, (i+1)*100)
		linksB = append(linksB, link)
	}

	keyA := dispatcher.Key{Type: h.dispType, Nsid: nsid, Ifindex: uint32(h.iface.Ifindex)}
	keyB := dispatcher.Key{Type: h.dispType, Nsid: nsid, Ifindex: uint32(ifaceB.Ifindex)}

	// Verify A has 3 extensions.
	snapA, err := h.env.GetDispatcherSnapshot(context.Background(), keyA)
	require.NoError(t, err)
	require.Len(t, snapA.Members, 3, "interface A should have 3 programs")

	// Verify B has 2 extensions.
	snapB, err := h.env.GetDispatcherSnapshot(context.Background(), keyB)
	require.NoError(t, err)
	require.Len(t, snapB.Members, 2, "interface B should have 2 programs")

	// Detach all programs from B.
	for i, l := range linksB {
		err := h.env.Detach(context.Background(), l.ID)
		require.NoError(t, err, "ifaceB detach %d", i)
	}

	// B's dispatcher should be gone.
	_, err = h.env.GetDispatcherSnapshot(context.Background(), keyB)
	require.ErrorIs(t, err, platform.ErrRecordNotFound,
		"interface B dispatcher should be absent after detaching all links")

	// A's dispatcher should still exist with 3 extensions.
	snapAAfter, err := h.env.GetDispatcherSnapshot(context.Background(), keyA)
	require.NoError(t, err, "interface A dispatcher should still exist")

	assert.Len(t, snapAAfter.Members, len(snapA.Members),
		"A's program count should be unchanged")
}

// TestDispatcher_ExtensionLinksSurviveGC verifies that extension links
// are not deleted by garbage collection when their dispatcher is alive.
//
// Every dispatcher rebuild re-attaches all extensions, creating new
// kernel links with new IDs. The stored link IDs become stale, but the
// extensions are still active via the dispatcher. GC must not delete
// them.
//
// This test reproduces the daemon-mode bug where GC ran between gRPC
// RPCs and deleted extension links whose kernel IDs had been superseded
// by a rebuild, causing links to vanish from the store.
func TestDispatcher_ExtensionLinksSurviveGC(t *testing.T) {
	t.Parallel()
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testExtensionLinksSurviveGC(t, h)
		})
	}
}

func testExtensionLinksSurviveGC(t *testing.T, h dispatcherTestHarness) {
	ctx := context.Background()
	progID := h.loadProg(t)

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	ifindex := uint32(h.iface.Ifindex)

	// Attach first program. This creates the dispatcher.
	link1 := h.attach(t, progID, 100)
	t.Cleanup(func() { h.env.Detach(ctx, link1.ID) })

	require.Equal(t, 1, h.extensionCount(t))

	// Run GC. Link 1's kernel ID may already be stale from the
	// initial dispatcher build. It must survive.
	_, err = h.env.GC(ctx)
	require.NoError(t, err, "GC after first attach")

	// Verify the link still exists and is retrievable.
	_, _, err = h.env.GetLink(ctx, link1.ID)
	require.NoError(t, err, "link 1 should survive GC")
	require.Equal(t, 1, h.extensionCount(t),
		"extension count should be 1 after GC")

	// Attach second program. This triggers a dispatcher rebuild,
	// which re-attaches link 1 with a new kernel link ID.
	link2 := h.attach(t, progID, 200)
	t.Cleanup(func() { h.env.Detach(ctx, link2.ID) })

	require.Equal(t, 2, h.extensionCount(t))

	// Run GC again. Both links have been through at least one
	// rebuild. Their stored kernel IDs are stale, but their
	// dispatcher is alive. Both must survive.
	_, err = h.env.GC(ctx)
	require.NoError(t, err, "GC after second attach")

	_, _, err = h.env.GetLink(ctx, link1.ID)
	require.NoError(t, err, "link 1 should survive second GC")
	_, _, err = h.env.GetLink(ctx, link2.ID)
	require.NoError(t, err, "link 2 should survive second GC")
	require.Equal(t, 2, h.extensionCount(t),
		"extension count should be 2 after second GC")

	// Attach a third program and GC once more.
	link3 := h.attach(t, progID, 300)
	t.Cleanup(func() { h.env.Detach(ctx, link3.ID) })

	require.Equal(t, 3, h.extensionCount(t))

	_, err = h.env.GC(ctx)
	require.NoError(t, err, "GC after third attach")

	// All three links and the dispatcher must still exist.
	for i, linkID := range []kernel.LinkID{link1.ID, link2.ID, link3.ID} {
		_, _, err = h.env.GetLink(ctx, linkID)
		require.NoError(t, err, "link %d should survive third GC", i+1)
	}
	require.Equal(t, 3, h.extensionCount(t),
		"extension count should be 3 after third GC")

	// Verify the dispatcher itself is still present.
	_, err = h.env.GetDispatcherSnapshot(ctx, dispatcher.Key{
		Type: h.dispType, Nsid: nsid, Ifindex: ifindex,
	})
	require.NoError(t, err, "dispatcher should still exist after GC")

	// Verify positions are correctly ordered.
	assert.Equal(t, int32(0), h.linkPosition(t, link1.ID), "link 1 position")
	assert.Equal(t, int32(1), h.linkPosition(t, link2.ID), "link 2 position")
	assert.Equal(t, int32(2), h.linkPosition(t, link3.ID), "link 3 position")
}
