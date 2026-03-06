//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/ns/netns"
	"github.com/frobware/go-bpfman/platform"
)

// readActiveIndex loads the pinned active map and returns the current
// active buffer index (0 or 1).
func readActiveIndex(t *testing.T, activeMapPin string) uint32 {
	t.Helper()
	activeMap, err := ebpf.LoadPinnedMap(activeMapPin, nil)
	require.NoError(t, err, "load pinned active map")
	defer activeMap.Close()

	var active uint32
	require.NoError(t, activeMap.Lookup(uint32(0), &active), "read active index")
	return active
}

// readDispatcherConfig loads the pinned config and active maps, reads
// the active buffer index, and returns the current RuntimeConfig.
func readDispatcherConfig(t *testing.T, configMapPin, activeMapPin string) dispatcher.RuntimeConfig {
	t.Helper()

	active := readActiveIndex(t, activeMapPin)

	configMap, err := ebpf.LoadPinnedMap(configMapPin, nil)
	require.NoError(t, err, "load pinned config map")
	defer configMap.Close()

	var cfg dispatcher.RuntimeConfig
	require.NoError(t, configMap.Lookup(active, &cfg), "read active config")
	return cfg
}

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

// configMapPin returns the pinned config map path for the harness's
// default interface.
func (h *dispatcherTestHarness) configMapPin(t *testing.T) string {
	t.Helper()
	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	return h.env.Layout.BPFFS().DispatcherConfigMapPath(
		h.dispType, nsid, uint32(h.iface.Ifindex))
}

// activeMapPin returns the pinned active map path for the harness's
// default interface.
func (h *dispatcherTestHarness) activeMapPin(t *testing.T) string {
	t.Helper()
	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	return h.env.Layout.BPFFS().DispatcherActiveMapPath(
		h.dispType, nsid, uint32(h.iface.Ifindex))
}

// readConfig reads the dispatcher runtime config for the harness's
// default interface.
func (h *dispatcherTestHarness) readConfig(t *testing.T) dispatcher.RuntimeConfig {
	t.Helper()
	return readDispatcherConfig(t, h.configMapPin(t), h.activeMapPin(t))
}

// readActiveIdx returns the current active buffer index for the
// harness's default interface.
func (h *dispatcherTestHarness) readActiveIdx(t *testing.T) uint32 {
	t.Helper()
	return readActiveIndex(t, h.activeMapPin(t))
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
			return xdpSpec
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
// dispatcher slots with different priorities produces a BPF runtime
// config whose run_order reflects priority ordering (lowest priority
// first). For XDP, which has no priority support, all attachments
// store priority 0 and the run_order follows insertion order.
func TestDispatcher_PriorityOrdering(t *testing.T) {
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
	// run_order must differ from the insertion order (TC) or
	// confirm insertion-order stability (XDP).
	priorities := []int{900, 0, 400, 100, 800, 200, 600, 300, 700, 500}
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

	cfg := h.readConfig(t)

	assert.Equal(t, uint32(10), cfg.NumProgsEnabled,
		"should have 10 programs enabled")

	if h.dispType == dispatcher.DispatcherTypeTCIngress {
		// TC: priorities produce a specific reordering.
		// priority -> slot: 0->1, 100->3, 200->5, 300->7, 400->2,
		//                   500->9, 600->6, 700->8, 800->4, 900->0
		expectedOrder := []uint32{1, 3, 5, 7, 2, 9, 6, 8, 4, 0}
		for i, expected := range expectedOrder {
			assert.Equal(t, expected, cfg.RunOrder[i],
				"run_order[%d] should be slot %d (priority %d)",
				i, expected, priorities[expected])
		}
	} else {
		// XDP: no priority support, all priority 0. Run_order
		// follows insertion order because (priority, name) are
		// identical across all slots.
		for i := range 10 {
			assert.Equal(t, uint32(i), cfg.RunOrder[i],
				"run_order[%d] should be slot %d (insertion order)", i, i)
		}
	}
}

// TestDispatcher_AttachExceedsMaxPrograms verifies that attempting to
// attach more than dispatcher.MaxPrograms extensions to a single
// dispatcher fails with a "no free dispatcher slots" error.
func TestDispatcher_AttachExceedsMaxPrograms(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testAttachExceedsMaxPrograms(t, h)
		})
	}
}

func testAttachExceedsMaxPrograms(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Fill all dispatcher slots.
	var links []bpfman.LinkRecord
	for i := range dispatcher.MaxPrograms {
		link := h.attach(t, progID, i*100)
		links = append(links, link)
	}

	t.Cleanup(func() {
		for _, link := range links {
			h.env.Detach(context.Background(), link.ID)
		}
	})

	// The next attach must fail because all slots are occupied.
	_, err := h.tryAttach(t, progID, dispatcher.MaxPrograms*100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no free dispatcher slots")
}

// TestDispatcher_SlotReusedAfterDetach verifies that detaching a
// program from the middle of a full dispatcher frees its slot, and
// that a subsequent attach reclaims that slot with the runtime config
// reflecting the correct run_order.
func TestDispatcher_SlotReusedAfterDetach(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testSlotReusedAfterDetach(t, h)
		})
	}
}

func testSlotReusedAfterDetach(t *testing.T, h dispatcherTestHarness) {
	ctx := context.Background()
	progID := h.loadProg(t)

	// Fill all 10 slots with ascending priorities: slot i gets
	// priority i*100. For XDP priority is ignored (always 0).
	links := make([]bpfman.LinkRecord, dispatcher.MaxPrograms)
	for i := range dispatcher.MaxPrograms {
		links[i] = h.attach(t, progID, i*100)
	}

	// Sanity check: all 10 slots filled.
	cfg := h.readConfig(t)
	require.Equal(t, uint32(dispatcher.MaxPrograms), cfg.NumProgsEnabled)

	// Detach the program in slot 3 (priority 300 for TC).
	const detachSlot = 3
	err := h.env.Detach(ctx, links[detachSlot].ID)
	require.NoError(t, err)

	cfg = h.readConfig(t)
	assert.Equal(t, uint32(dispatcher.MaxPrograms-1), cfg.NumProgsEnabled,
		"should have 9 programs after detach")

	// Attach a new program at priority 550 (TC) or 0 (XDP).
	// findFreeSlot should assign it to the vacated slot 3.
	newLink := h.attach(t, progID, 550)

	t.Cleanup(func() {
		for i, link := range links {
			if i == detachSlot {
				continue // already detached
			}
			h.env.Detach(ctx, link.ID)
		}
		h.env.Detach(ctx, newLink.ID)
	})

	cfg = h.readConfig(t)
	assert.Equal(t, uint32(dispatcher.MaxPrograms), cfg.NumProgsEnabled,
		"should have 10 programs after reattach")

	if h.dispType == dispatcher.DispatcherTypeTCIngress {
		// Slot -> priority: 0:0, 1:100, 2:200, 3:550, 4:400, 5:500,
		//                   6:600, 7:700, 8:800, 9:900
		// Sorted by priority: 0,100,200,400,500,550,600,700,800,900
		// Expected run_order: [0,1,2,4,5,3,6,7,8,9]
		expectedOrder := [dispatcher.MaxPrograms]uint32{0, 1, 2, 4, 5, 3, 6, 7, 8, 9}
		assert.Equal(t, expectedOrder, cfg.RunOrder,
			"run_order should reflect the new program at slot 3 with priority 550")
	} else {
		// XDP: all priority 0, same program name. The new
		// program in slot 3 was appended after the existing
		// slots [0,1,2,4,5,6,7,8,9] in computeRuntimeConfig.
		// Stable sort preserves that input order.
		expectedOrder := [dispatcher.MaxPrograms]uint32{0, 1, 2, 4, 5, 6, 7, 8, 9, 3}
		assert.Equal(t, expectedOrder, cfg.RunOrder,
			"run_order should reflect stable sort with new slot appended last")
	}
}

// TestDispatcher_LifecycleAfterLastDetach verifies that removing the
// last extension from a dispatcher causes it to be fully torn down
// (attachment removed, pins removed, store entry deleted) and that a
// subsequent attach creates a fresh dispatcher with a new program ID.
func TestDispatcher_LifecycleAfterLastDetach(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testLifecycleAfterLastDetach(t, h)
		})
	}
}

func testLifecycleAfterLastDetach(t *testing.T, h dispatcherTestHarness) {
	ctx := context.Background()
	progID := h.loadProg(t)

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	ifindex := uint32(h.iface.Ifindex)

	// Phase 1: attach a single program. A dispatcher should exist.
	link, err := h.env.Attach(ctx, h.makeAttachSpec(t, progID, h.iface, 100))
	require.NoError(t, err)

	state1, err := h.env.GetDispatcher(ctx, h.dispType, nsid, ifindex)
	require.NoError(t, err, "dispatcher should exist after attach")

	h.verifyAttachPresent(t)

	// Phase 2: detach the only program. The dispatcher should be
	// fully cleaned up.
	err = h.env.Detach(ctx, link.ID)
	require.NoError(t, err)

	_, err = h.env.GetDispatcher(ctx, h.dispType, nsid, ifindex)
	require.ErrorIs(t, err, platform.ErrRecordNotFound,
		"dispatcher should be absent from store after last detach")

	h.verifyAttachAbsent(t)

	// The config and active map pins should be gone.
	bpffs := h.env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(h.dispType, nsid, ifindex)
	activeMapPin := bpffs.DispatcherActiveMapPath(h.dispType, nsid, ifindex)
	_, err = os.Stat(configMapPin)
	assert.True(t, os.IsNotExist(err), "config map pin should not exist: %s", configMapPin)
	_, err = os.Stat(activeMapPin)
	assert.True(t, os.IsNotExist(err), "active map pin should not exist: %s", activeMapPin)

	// Phase 3: attach again. A new dispatcher should be created
	// with a different program ID.
	link2, err := h.env.Attach(ctx, h.makeAttachSpec(t, progID, h.iface, 200))
	require.NoError(t, err)
	t.Cleanup(func() { h.env.Detach(ctx, link2.ID) })

	state2, err := h.env.GetDispatcher(ctx, h.dispType, nsid, ifindex)
	require.NoError(t, err, "dispatcher should exist after second attach")
	assert.NotEqual(t, state1.ProgramID, state2.ProgramID,
		"second dispatcher should have a different program ID")

	h.verifyAttachPresent(t)
}

// TestDispatcher_DoubleBufferFlip verifies that each dispatcher
// config mutation (attach or detach) writes to the inactive buffer
// and then flips the active index. The initial state after dispatcher
// creation is active=0. Each UpdateDispatcherConfig call toggles the
// index: attach 1 -> active=1, attach 2 -> active=0, etc.
func TestDispatcher_DoubleBufferFlip(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testDoubleBufferFlip(t, h)
		})
	}
}

func testDoubleBufferFlip(t *testing.T, h dispatcherTestHarness) {
	ctx := context.Background()
	progID := h.loadProg(t)

	// Attach 1: dispatcher created (active=0), then config
	// updated (writes buffer 1, flips active to 1).
	link1 := h.attach(t, progID, 100)
	t.Cleanup(func() { h.env.Detach(ctx, link1.ID) })
	assert.Equal(t, uint32(1), h.readActiveIdx(t),
		"after attach 1: active should be 1")

	// Attach 2: writes buffer 0, flips active to 0.
	link2 := h.attach(t, progID, 200)
	t.Cleanup(func() { h.env.Detach(ctx, link2.ID) })
	assert.Equal(t, uint32(0), h.readActiveIdx(t),
		"after attach 2: active should be 0")

	// Attach 3: writes buffer 1, flips active to 1.
	link3 := h.attach(t, progID, 300)
	assert.Equal(t, uint32(1), h.readActiveIdx(t),
		"after attach 3: active should be 1")

	// Detach 3: recomputes config with 2 remaining programs,
	// writes buffer 0, flips active to 0.
	require.NoError(t, h.env.Detach(ctx, link3.ID))
	assert.Equal(t, uint32(0), h.readActiveIdx(t),
		"after detach 3: active should be 0")

	// Detach 2: writes buffer 1, flips active to 1.
	require.NoError(t, h.env.Detach(ctx, link2.ID))
	assert.Equal(t, uint32(1), h.readActiveIdx(t),
		"after detach 2: active should be 1")
}

// TestDispatcher_ConfigRecomputedOnDetach verifies that filling all
// 10 dispatcher slots then detaching them one at a time correctly
// updates the BPF runtime config at each step.
func TestDispatcher_ConfigRecomputedOnDetach(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testConfigRecomputedOnDetach(t, h)
		})
	}
}

func testConfigRecomputedOnDetach(t *testing.T, h dispatcherTestHarness) {
	ctx := context.Background()
	progID := h.loadProg(t)

	// Attach all 10 slots with ascending priorities: slot 0 has
	// priority 0, slot 1 has priority 100, ..., slot 9 has 900.
	// For XDP the priorities are ignored (all 0).
	var links []bpfman.LinkRecord
	for i := range 10 {
		link := h.attach(t, progID, i*100)
		links = append(links, link)
	}

	t.Cleanup(func() {
		for i := 1; i < 10; i++ {
			h.env.Detach(ctx, links[i].ID)
		}
	})

	// Before detach: 10 programs. For both TC (ascending
	// priorities) and XDP (all priority 0, insertion order),
	// run_order is [0, 1, 2, ..., 9].
	cfg := h.readConfig(t)
	require.Equal(t, uint32(10), cfg.NumProgsEnabled)
	for i := range 10 {
		assert.Equal(t, uint32(i), cfg.RunOrder[i],
			"before detach: run_order[%d] should be slot %d", i, i)
	}

	// Detach the first program (slot 0).
	err := h.env.Detach(ctx, links[0].ID)
	require.NoError(t, err)

	// After detach: 9 programs remaining (slots 1-9), still in
	// ascending order for both TC (priorities 100-900) and XDP
	// (all priority 0, insertion order among remaining slots).
	cfg = h.readConfig(t)
	assert.Equal(t, uint32(9), cfg.NumProgsEnabled,
		"should have 9 programs after detach")
	for i := range 9 {
		assert.Equal(t, uint32(i+1), cfg.RunOrder[i],
			"after detach: run_order[%d] should be slot %d", i, i+1)
	}
}

// TestDispatcher_MultipleInterfacesIndependent verifies that
// dispatchers on different interfaces are fully independent.
// Attaching the same program to two interfaces, then detaching from
// one, must leave the other's dispatcher config unchanged.
func TestDispatcher_MultipleInterfacesIndependent(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testMultipleInterfacesIndependent(t, h)
		})
	}
}

func testMultipleInterfacesIndependent(t *testing.T, h dispatcherTestHarness) {
	ctx := context.Background()
	ifaceB := NewTestInterface(t)
	progID := h.loadProg(t)

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffs := h.env.Layout.BPFFS()

	// Attach 3 programs to interface A (harness default).
	var linksA []bpfman.LinkRecord
	for i := range 3 {
		link := h.attach(t, progID, i*100)
		linksA = append(linksA, link)
	}
	t.Cleanup(func() {
		for _, l := range linksA {
			h.env.Detach(ctx, l.ID)
		}
	})

	// Attach 2 programs to interface B.
	var linksB []bpfman.LinkRecord
	for i := range 2 {
		link := h.attachTo(t, progID, ifaceB, i*100)
		linksB = append(linksB, link)
	}

	// Record interface A's config.
	configPinA := bpffs.DispatcherConfigMapPath(h.dispType, nsid, uint32(h.iface.Ifindex))
	activePinA := bpffs.DispatcherActiveMapPath(h.dispType, nsid, uint32(h.iface.Ifindex))
	cfgA := readDispatcherConfig(t, configPinA, activePinA)
	require.Equal(t, uint32(3), cfgA.NumProgsEnabled, "ifaceA should have 3 programs")

	// Verify interface B's config.
	configPinB := bpffs.DispatcherConfigMapPath(h.dispType, nsid, uint32(ifaceB.Ifindex))
	activePinB := bpffs.DispatcherActiveMapPath(h.dispType, nsid, uint32(ifaceB.Ifindex))
	cfgB := readDispatcherConfig(t, configPinB, activePinB)
	require.Equal(t, uint32(2), cfgB.NumProgsEnabled, "ifaceB should have 2 programs")

	// Detach all programs from interface B.
	for i, l := range linksB {
		err := h.env.Detach(ctx, l.ID)
		require.NoError(t, err, "ifaceB detach %d", i)
	}

	// Interface B's dispatcher should be gone.
	_, err = h.env.GetDispatcher(ctx, h.dispType, nsid, uint32(ifaceB.Ifindex))
	require.ErrorIs(t, err, platform.ErrRecordNotFound,
		"ifaceB dispatcher should be absent after detaching all links")

	// Interface A's dispatcher should be unaffected.
	_, err = h.env.GetDispatcher(ctx, h.dispType, nsid, uint32(h.iface.Ifindex))
	require.NoError(t, err, "ifaceA dispatcher should still exist")

	cfgAAfter := readDispatcherConfig(t, configPinA, activePinA)
	assert.Equal(t, cfgA.NumProgsEnabled, cfgAAfter.NumProgsEnabled,
		"ifaceA program count should be unchanged")
	assert.Equal(t, cfgA.RunOrder, cfgAAfter.RunOrder,
		"ifaceA run_order should be unchanged")
	assert.Equal(t, cfgA.ChainCallActions, cfgAAfter.ChainCallActions,
		"ifaceA chain_call_actions should be unchanged")
}

// TestXDP_DispatcherConfigAfterDetach verifies that filling all 10
// XDP extension slots then detaching them one at a time correctly
// updates the BPF runtime config at each step.
func TestXDP_DispatcherConfigAfterDetach(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/xdp_pass:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeXDP, Name: "pass"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	// Fill all 10 slots
	xdpSpec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, iface.Name, iface.Ifindex)
	require.NoError(t, err)

	var linkIDs []bpfman.LinkRecord
	for i := 0; i < 10; i++ {
		link, err := env.Attach(ctx, xdpSpec)
		require.NoError(t, err, "attach %d should succeed", i)
		linkIDs = append(linkIDs, link)
	}

	// Keep the last link alive for cleanup
	t.Cleanup(func() {
		env.Detach(context.Background(), linkIDs[9].ID)
	})

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffsLayout := env.Layout.BPFFS()
	configMapPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeXDP, nsid, uint32(iface.Ifindex))
	activeMapPin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeXDP, nsid, uint32(iface.Ifindex))

	// Verify 10 programs enabled before detach
	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
	require.Equal(t, uint32(10), cfg.NumProgsEnabled,
		"should have 10 programs before detach")

	// Detach first 9 links one at a time, verifying count decreases
	for i := 0; i < 9; i++ {
		err = env.Detach(ctx, linkIDs[i].ID)
		require.NoError(t, err, "detach %d should succeed", i)

		cfg = readDispatcherConfig(t, configMapPin, activeMapPin)
		assert.Equal(t, uint32(9-i), cfg.NumProgsEnabled,
			"should have %d programs after detaching %d", 9-i, i+1)
	}
}

// TestTC_IngressEgressIndependence verifies that ingress and egress
// dispatchers on the same interface are fully independent. Attaching
// programs to both directions, then detaching all egress links, must
// leave the ingress dispatcher config unchanged. This exercises the
// (nsid, ifindex, direction) keying in the dispatcher store.
func TestTC_IngressEgressIndependence(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	programs, err := env.LoadFile(ctx, "testdata/tc_counter.bpf.o", []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	ifindex := uint32(iface.Ifindex)

	bpffsLayout := env.Layout.BPFFS()

	// Attach 3 programs to ingress.
	var ingressLinks []bpfman.LinkRecord
	for i := range 3 {
		tcSpec, err := bpfman.NewTCAttachSpec(
			prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(i * 100)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "ingress attach %d", i)
		ingressLinks = append(ingressLinks, link)
	}
	t.Cleanup(func() {
		for _, l := range ingressLinks {
			env.Detach(context.Background(), l.ID)
		}
	})

	// Record ingress config before any egress activity.
	ingressConfigPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, ifindex)
	ingressActivePin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, ifindex)
	ingressCfgBefore := readDispatcherConfig(t, ingressConfigPin, ingressActivePin)
	require.Equal(t, uint32(3), ingressCfgBefore.NumProgsEnabled,
		"ingress should have 3 programs")

	// Attach 2 programs to egress.
	var egressLinks []bpfman.LinkRecord
	for i := range 2 {
		tcSpec, err := bpfman.NewTCAttachSpec(
			prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
			bpfman.TCDirectionEgress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(i * 100)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "egress attach %d", i)
		egressLinks = append(egressLinks, link)
	}

	// Verify both dispatchers exist.
	_, err = env.GetDispatcher(ctx, dispatcher.DispatcherTypeTCIngress, nsid, ifindex)
	require.NoError(t, err, "ingress dispatcher should exist")
	_, err = env.GetDispatcher(ctx, dispatcher.DispatcherTypeTCEgress, nsid, ifindex)
	require.NoError(t, err, "egress dispatcher should exist")

	egressConfigPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCEgress, nsid, ifindex)
	egressActivePin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCEgress, nsid, ifindex)
	egressCfg := readDispatcherConfig(t, egressConfigPin, egressActivePin)
	require.Equal(t, uint32(2), egressCfg.NumProgsEnabled,
		"egress should have 2 programs")

	// Detach all egress links.
	for i, l := range egressLinks {
		err := env.Detach(ctx, l.ID)
		require.NoError(t, err, "egress detach %d", i)
	}

	// Egress dispatcher should be gone.
	_, err = env.GetDispatcher(ctx, dispatcher.DispatcherTypeTCEgress, nsid, ifindex)
	require.ErrorIs(t, err, platform.ErrRecordNotFound,
		"egress dispatcher should be absent after detaching all egress links")

	// Ingress dispatcher should be unaffected.
	_, err = env.GetDispatcher(ctx, dispatcher.DispatcherTypeTCIngress, nsid, ifindex)
	require.NoError(t, err, "ingress dispatcher should still exist")

	ingressCfgAfter := readDispatcherConfig(t, ingressConfigPin, ingressActivePin)
	assert.Equal(t, ingressCfgBefore.NumProgsEnabled, ingressCfgAfter.NumProgsEnabled,
		"ingress program count should be unchanged")
	assert.Equal(t, ingressCfgBefore.RunOrder, ingressCfgAfter.RunOrder,
		"ingress run_order should be unchanged")
	assert.Equal(t, ingressCfgBefore.ChainCallActions, ingressCfgAfter.ChainCallActions,
		"ingress chain_call_actions should be unchanged")
}

// TestTC_DispatcherPriorityTieBreakByName verifies that when two
// programs share the same priority, the dispatcher orders them
// alphabetically by program name. "beta" is attached first (slot 0),
// "alpha" second (slot 1), both at priority 100. The run_order must
// place alpha's slot before beta's, proving the secondary sort.
func TestTC_DispatcherPriorityTieBreakByName(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	// Load "beta" and "alpha" as separate programs so they have
	// distinct Meta.Name values in the dispatcher slot records.
	progsBeta, err := env.LoadFile(ctx, "testdata/tc_pass.bpf.o", []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "beta"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, progsBeta, 1)
	beta := progsBeta[0]
	t.Cleanup(func() { env.Unload(context.Background(), beta.Status.Kernel.ID) })

	progsAlpha, err := env.LoadFile(ctx, "testdata/tc_pass.bpf.o", []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "alpha"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, progsAlpha, 1)
	alpha := progsAlpha[0]
	t.Cleanup(func() { env.Unload(context.Background(), alpha.Status.Kernel.ID) })

	// Attach beta first (gets slot 0), then alpha (gets slot 1).
	// Both at the same priority so the tie-break decides ordering.
	tcBeta, err := bpfman.NewTCAttachSpec(
		beta.Status.Kernel.ID, iface.Name, iface.Ifindex,
		bpfman.TCDirectionIngress,
	)
	require.NoError(t, err)
	tcBeta = tcBeta.WithPriority(100)
	linkBeta, err := env.Attach(ctx, tcBeta)
	require.NoError(t, err)
	t.Cleanup(func() { env.Detach(context.Background(), linkBeta.ID) })

	tcAlpha, err := bpfman.NewTCAttachSpec(
		alpha.Status.Kernel.ID, iface.Name, iface.Ifindex,
		bpfman.TCDirectionIngress,
	)
	require.NoError(t, err)
	tcAlpha = tcAlpha.WithPriority(100)
	linkAlpha, err := env.Attach(ctx, tcAlpha)
	require.NoError(t, err)
	t.Cleanup(func() { env.Detach(context.Background(), linkAlpha.ID) })

	// Read the dispatcher runtime config.
	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffsLayout := env.Layout.BPFFS()
	configMapPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))
	activeMapPin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))

	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
	require.Equal(t, uint32(2), cfg.NumProgsEnabled, "should have 2 programs")

	// beta is in slot 0, alpha in slot 1. Alphabetical ordering
	// means alpha runs first: run_order = [1, 0].
	assert.Equal(t, uint32(1), cfg.RunOrder[0],
		"run_order[0] should be slot 1 (alpha)")
	assert.Equal(t, uint32(0), cfg.RunOrder[1],
		"run_order[1] should be slot 0 (beta)")
}

// TestTC_DispatcherSineWave exercises repeated fill-drain-refill
// cycles where the drain boundary shifts each oscillation, verifying
// that slot reuse, runtime config recomputation, and traffic delivery
// remain correct throughout.
//
// The test performs three oscillations. Each trough drains one more
// slot than the previous (6, 7, 8) and the drain region alternates
// between the low and high ends of the slot space:
//
//	Peak 0:   fill all 10 slots                     [0-9 occupied]
//	Trough 1: drain first 6    (slots 0-5 freed)    [6-9 survive]
//	Peak 1:   refill 6         (slots 0-5 reused)   [0-9 occupied]
//	Trough 2: drain last 7     (slots 3-9 freed)    [0-2 survive]
//	Peak 2:   refill 7         (slots 3-9 reused)   [0-9 occupied]
//	Trough 3: drain first 8    (slots 0-7 freed)    [8-9 survive]
//	Peak 3:   refill 8         (slots 0-7 reused)   [0-9 occupied]
//
// The shifting drain boundary ensures that every physical slot
// position is both vacated and reused at least once. Each refill
// wave uses unique priorities that interleave with the surviving
// programs, producing non-trivial run_order permutations that differ
// at every peak.
//
// At each peak the test asserts:
//   - NumProgsEnabled equals dispatcher.MaxPrograms (10).
//   - RunOrder matches the priority-sorted slot positions.
//   - Every program (including newly attached ones) receives new
//     traffic: packet counts are recorded before a ping burst and
//     verified to have increased afterward.
//
// At each trough the test asserts:
//   - NumProgsEnabled equals the surviving program count.
//   - The first N entries of RunOrder match the expected ordering.
//
// Programs are loaded as separate instances (one LoadImage per
// program) so that each has an independent tc_stats_map for traffic
// verification. Proceed-on is set to TC_ACT_OK|TC_ACT_PIPE|
// DispatcherReturn on every attachment so the full chain executes.
func TestTC_DispatcherSineWave(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := NewTestVethPair(t)
	ctx := context.Background()

	objFile := "testdata/tc_counter.bpf.o"
	proceedOn := []int32{0, 3, 30} // TC_ACT_OK, TC_ACT_PIPE, DispatcherReturn

	type prog struct {
		kernelID   kernel.ProgramID
		mapPinPath string
	}

	loadProg := func() prog {
		programs, err := env.LoadFile(ctx, objFile, []manager.ProgramSpec{
			{Type: bpfman.ProgramTypeTC, Name: "stats"},
		}, manager.LoadOpts{})
		require.NoError(t, err)
		require.Len(t, programs, 1)
		p := programs[0]
		t.Cleanup(func() { env.Unload(context.Background(), p.Status.Kernel.ID) })
		return prog{p.Status.Kernel.ID, p.Record.Handles.MapPinPath}
	}

	attachProg := func(p prog, priority int) bpfman.LinkRecord {
		tcSpec, err := bpfman.NewTCAttachSpec(
			p.kernelID, veth.A.Name, veth.A.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(priority).WithProceedOn(proceedOn)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "attach at priority %d", priority)
		return link
	}

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffsLayout := env.Layout.BPFFS()
	configMapPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(veth.A.Ifindex))
	activeMapPin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(veth.A.Ifindex))

	// -- Slot tracking --------------------------------------------------

	type slotEntry struct {
		prog     prog
		link     bpfman.LinkRecord
		priority int
	}

	var slots [dispatcher.MaxPrograms]*slotEntry

	t.Cleanup(func() {
		for _, s := range slots {
			if s != nil {
				env.Detach(context.Background(), s.link.ID)
			}
		}
	})

	// fill loads and attaches len(priorities) new programs into
	// the first available free slots.
	fill := func(priorities []int) {
		t.Helper()
		j := 0
		for i := range dispatcher.MaxPrograms {
			if slots[i] != nil || j >= len(priorities) {
				continue
			}
			p := loadProg()
			link := attachProg(p, priorities[j])
			slots[i] = &slotEntry{p, link, priorities[j]}
			j++
		}
		require.Equal(t, len(priorities), j, "fill: not enough free slots")
	}

	// drain detaches programs in slots [lo, hi).
	drain := func(lo, hi int) {
		t.Helper()
		for i := lo; i < hi; i++ {
			require.NotNilf(t, slots[i], "drain: slot %d already empty", i)
			err := env.Detach(ctx, slots[i].link.ID)
			require.NoError(t, err, "drain: detach slot %d", i)
			slots[i] = nil
		}
	}

	// occupiedCount returns the number of non-nil slots.
	occupiedCount := func() uint32 {
		var n uint32
		for _, s := range slots {
			if s != nil {
				n++
			}
		}
		return n
	}

	// expectedRunOrder computes the expected RunOrder by sorting
	// occupied slots by priority ascending.
	expectedRunOrder := func() [dispatcher.MaxPrograms]uint32 {
		type entry struct {
			slot     int
			priority int
		}
		var occupied []entry
		for i, s := range slots {
			if s != nil {
				occupied = append(occupied, entry{i, s.priority})
			}
		}
		sort.Slice(occupied, func(i, j int) bool {
			return occupied[i].priority < occupied[j].priority
		})
		var order [dispatcher.MaxPrograms]uint32
		for i, e := range occupied {
			order[i] = uint32(e.slot)
		}
		return order
	}

	// verifyConfig reads the dispatcher config and asserts it
	// matches the expected state derived from the slot array.
	verifyConfig := func(phase string) {
		t.Helper()
		cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
		count := occupiedCount()
		assert.Equal(t, count, cfg.NumProgsEnabled,
			"%s: NumProgsEnabled", phase)
		expected := expectedRunOrder()
		if count == uint32(dispatcher.MaxPrograms) {
			assert.Equal(t, expected, cfg.RunOrder,
				"%s: RunOrder", phase)
		} else {
			assert.Equal(t, expected[:count], cfg.RunOrder[:count],
				"%s: RunOrder[:%d]", phase, count)
		}
	}

	// verifyTraffic sends traffic and asserts every active
	// program's packet count increased.
	verifyTraffic := func(phase string) {
		t.Helper()
		var active []prog
		for _, s := range slots {
			if s != nil {
				active = append(active, s.prog)
			}
		}
		before := make([]uint64, len(active))
		for i, p := range active {
			before[i] = readStatsMap(t, filepath.Join(p.mapPinPath, "tc_stats_map"))
		}
		veth.Ping(t, 20)
		for i, p := range active {
			after := readStatsMap(t, filepath.Join(p.mapPinPath, "tc_stats_map"))
			assert.Greater(t, after, before[i],
				"%s: program %d (kernel_id=%d) should have received new traffic",
				phase, i, p.kernelID)
		}
	}

	// -- Peak 0: fill all 10 slots --------------------------------------
	initialPriorities := make([]int, dispatcher.MaxPrograms)
	for i := range initialPriorities {
		initialPriorities[i] = i * 100 // 0, 100, 200, ..., 900
	}
	fill(initialPriorities)
	verifyConfig("peak 0")
	verifyTraffic("peak 0")

	// -- Trough 1: drain first 6 ----------------------------------------
	// Slots 0-5 freed; slots 6-9 survive (priorities 600-900).
	drain(0, 6)
	verifyConfig("trough 1")

	// -- Peak 1: refill 6 -----------------------------------------------
	// Priorities interleave with surviving 600, 700, 800, 900.
	fill([]int{950, 850, 750, 650, 550, 450})
	verifyConfig("peak 1")
	verifyTraffic("peak 1")

	// -- Trough 2: drain last 7 -----------------------------------------
	// Slots 3-9 freed; slots 0-2 survive (priorities 950, 850,
	// 750 from wave 1).
	drain(3, 10)
	verifyConfig("trough 2")

	// -- Peak 2: refill 7 -----------------------------------------------
	// Priorities interleave with surviving 950, 850, 750.
	fill([]int{25, 125, 225, 325, 425, 525, 625})
	verifyConfig("peak 2")
	verifyTraffic("peak 2")

	// -- Trough 3: drain first 8 ----------------------------------------
	// Slots 0-7 freed; slots 8-9 survive (priorities 525, 625
	// from wave 2).
	drain(0, 8)
	verifyConfig("trough 3")

	// -- Peak 3: refill 8 -----------------------------------------------
	// Priorities interleave with surviving 525, 625.
	fill([]int{62, 162, 262, 362, 462, 562, 662, 762})
	verifyConfig("peak 3")
	verifyTraffic("peak 3")
}

// tcStatsEntry matches the BPF struct used by go-tc-counter.
type tcStatsEntry struct {
	Packets uint64
	Bytes   uint64
}

// readStatsMap loads a pinned tc_stats_map (PerCPUArray) and returns
// the total packet count summed across all CPUs. The go-tc-counter
// program stores a tcStatsEntry per CPU at key 0.
func readStatsMap(t *testing.T, mapPinPath string) uint64 {
	t.Helper()

	m, err := ebpf.LoadPinnedMap(mapPinPath, nil)
	require.NoError(t, err, "load pinned tc_stats_map at %s", mapPinPath)
	defer m.Close()

	var perCPU []tcStatsEntry
	err = m.Lookup(uint32(0), &perCPU)
	require.NoError(t, err, "lookup key 0 in tc_stats_map")

	var total uint64
	for _, entry := range perCPU {
		total += entry.Packets
	}
	return total
}

// TestTC_DispatcherChainExecution verifies that all programs in a TC
// dispatch chain actually execute when real traffic flows through the
// interface. Five separate programs are loaded and attached at
// different priorities; after sending traffic through a veth pair,
// each program's independent counter map must show a non-zero packet
// count.
func TestTC_DispatcherChainExecution(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := NewTestVethPair(t)
	ctx := context.Background()

	objFile := "testdata/tc_counter.bpf.o"

	// Load 5 separate instances so each gets independent maps.
	type loadedProg struct {
		kernelID   kernel.ProgramID
		mapPinPath string
	}

	var progs []loadedProg
	for i := 0; i < 5; i++ {
		programs, err := env.LoadFile(ctx, objFile, []manager.ProgramSpec{
			{Type: bpfman.ProgramTypeTC, Name: "stats"},
		}, manager.LoadOpts{})
		require.NoError(t, err, "load %d", i)
		require.Len(t, programs, 1)

		prog := programs[0]
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

		progs = append(progs, loadedProg{
			kernelID:   prog.Status.Kernel.ID,
			mapPinPath: prog.Record.Handles.MapPinPath,
		})
	}

	// Attach each at a different priority with proceed-on
	// OK|Pipe|DispatcherReturn so the chain continues through all
	// programs.
	priorities := []int{500, 100, 300, 200, 400}
	proceedOn := []int32{0, 3, 30} // TC_ACT_OK, TC_ACT_PIPE, bpfman dispatcher return

	var linkIDs []bpfman.LinkRecord
	for i, prio := range priorities {
		tcSpec, err := bpfman.NewTCAttachSpec(
			progs[i].kernelID, veth.A.Name, veth.A.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(prio).WithProceedOn(proceedOn)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "attach %d at priority %d", i, prio)
		linkIDs = append(linkIDs, link)
	}

	t.Cleanup(func() {
		for _, link := range linkIDs {
			env.Detach(context.Background(), link.ID)
		}
	})

	// Verify the runtime config has all 5 programs in priority order.
	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffsLayout := env.Layout.BPFFS()
	configMapPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(veth.A.Ifindex))
	activeMapPin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(veth.A.Ifindex))

	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
	assert.Equal(t, uint32(5), cfg.NumProgsEnabled,
		"should have 5 programs enabled")

	// Expected run_order by priority ascending:
	// priority 100 -> slot 1, 200 -> slot 3, 300 -> slot 2,
	// 400 -> slot 4, 500 -> slot 0
	expectedOrder := []uint32{1, 3, 2, 4, 0}
	for i, expected := range expectedOrder {
		assert.Equal(t, expected, cfg.RunOrder[i],
			"run_order[%d] should be slot %d", i, expected)
	}

	// Send traffic through the veth pair.
	veth.Ping(t, 20)

	// Read each program's counter map and verify non-zero counts.
	for i, prog := range progs {
		statsPath := filepath.Join(prog.mapPinPath, "tc_stats_map")
		packets := readStatsMap(t, statsPath)
		t.Logf("program %d (kernel_id=%d, priority=%d): %d packets",
			i, prog.kernelID, priorities[i], packets)
		assert.Greater(t, packets, uint64(0),
			"program %d (priority %d) should have counted packets", i, priorities[i])
	}
}

// TestTC_DispatcherChainProceedOn verifies that the TC dispatcher
// chain-break logic works correctly. When a program's proceed-on
// configuration excludes the action returned by that program
// (TC_ACT_OK), the dispatcher must stop the chain: programs after
// the break point must see exactly zero packets.
func TestTC_DispatcherChainProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	// proceed-on that includes TC_ACT_OK (0): chain continues.
	proceedOnContinue := []int32{0, 3, 30} // OK, Pipe, DispatcherReturn

	// proceed-on that excludes TC_ACT_OK: chain stops here.
	// go-tc-counter always returns TC_ACT_OK, so requiring only
	// TC_ACT_SHOT (2) causes the dispatcher to halt the chain.
	proceedOnStop := []int32{2} // TC_ACT_SHOT only

	tests := []struct {
		name    string
		n       int
		breakAt int // execution position where chain stops; -1 = all proceed
	}{
		{"single program", 1, -1},
		{"3 programs, all proceed", 3, -1},
		{"3 programs, break after first", 3, 0},
		{"3 programs, break after second", 3, 1},
		{"3 programs, break after third", 3, 2},
		{"5 programs, all proceed", 5, -1},
		{"5 programs, break after first", 5, 0},
		{"5 programs, break after third", 5, 2},
		{"5 programs, break after fifth", 5, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewTestEnv(t)
			veth := NewTestVethPair(t)
			ctx := context.Background()

			objFile := "testdata/tc_counter.bpf.o"

			type loadedProg struct {
				kernelID   kernel.ProgramID
				mapPinPath string
			}

			var progs []loadedProg
			for i := 0; i < tt.n; i++ {
				programs, err := env.LoadFile(ctx, objFile, []manager.ProgramSpec{
					{Type: bpfman.ProgramTypeTC, Name: "stats"},
				}, manager.LoadOpts{})
				require.NoError(t, err, "load %d", i)
				require.Len(t, programs, 1)

				prog := programs[0]
				t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

				progs = append(progs, loadedProg{
					kernelID:   prog.Status.Kernel.ID,
					mapPinPath: prog.Record.Handles.MapPinPath,
				})
			}

			// Attach each program at ascending priorities so
			// attachment order equals execution order.
			var linkIDs []bpfman.LinkRecord
			for i := 0; i < tt.n; i++ {
				tcSpec, err := bpfman.NewTCAttachSpec(
					progs[i].kernelID, veth.A.Name, veth.A.Ifindex,
					bpfman.TCDirectionIngress,
				)
				require.NoError(t, err)

				po := proceedOnContinue
				if tt.breakAt >= 0 && i == tt.breakAt {
					po = proceedOnStop
				}

				tcSpec = tcSpec.WithPriority((i + 1) * 100).WithProceedOn(po)
				link, err := env.Attach(ctx, tcSpec)
				require.NoError(t, err, "attach %d at priority %d", i, (i+1)*100)
				linkIDs = append(linkIDs, link)
			}

			t.Cleanup(func() {
				for _, link := range linkIDs {
					env.Detach(context.Background(), link.ID)
				}
			})

			// Send traffic through the veth pair.
			veth.Ping(t, 20)

			// Verify packet counts for each program.
			for i, prog := range progs {
				statsPath := filepath.Join(prog.mapPinPath, "tc_stats_map")
				packets := readStatsMap(t, statsPath)
				t.Logf("program %d (kernel_id=%d): %d packets", i, prog.kernelID, packets)

				if tt.breakAt == -1 || i <= tt.breakAt {
					assert.Greater(t, packets, uint64(0),
						"program %d should have counted packets (at or before break point)", i)
				} else {
					assert.Equal(t, uint64(0), packets,
						"program %d should have zero packets (after break point at position %d)", i, tt.breakAt)
				}
			}
		})
	}
}

// TestTC_EgressTrafficCounting verifies that TC programs attached in
// the egress direction see real traffic. Three programs are loaded
// separately (so each has an independent stats map), attached to
// egress on a veth interface, and then traffic is generated by
// pinging from the peer namespace to the root namespace. The ICMP
// replies leaving through A's egress must be counted by all three
// programs.
func TestTC_EgressTrafficCounting(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := NewTestVethPair(t)
	ctx := context.Background()

	objFile := "testdata/tc_counter.bpf.o"

	type loadedProg struct {
		kernelID   kernel.ProgramID
		mapPinPath string
	}

	// Load 3 separate instances so each gets independent maps.
	var progs []loadedProg
	for i := range 3 {
		programs, err := env.LoadFile(ctx, objFile, []manager.ProgramSpec{
			{Type: bpfman.ProgramTypeTC, Name: "stats"},
		}, manager.LoadOpts{})
		require.NoError(t, err, "load %d", i)
		require.Len(t, programs, 1)

		prog := programs[0]
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

		progs = append(progs, loadedProg{
			kernelID:   prog.Status.Kernel.ID,
			mapPinPath: prog.Record.Handles.MapPinPath,
		})
	}

	// Attach each at a different priority on egress with
	// proceed-on OK|Pipe|DispatcherReturn so the full chain
	// executes.
	proceedOn := []int32{0, 3, 30}
	var linkIDs []bpfman.LinkRecord
	for i, prio := range []int{100, 200, 300} {
		tcSpec, err := bpfman.NewTCAttachSpec(
			progs[i].kernelID, veth.A.Name, veth.A.Ifindex,
			bpfman.TCDirectionEgress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(prio).WithProceedOn(proceedOn)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "attach %d at priority %d", i, prio)
		linkIDs = append(linkIDs, link)
	}

	t.Cleanup(func() {
		for _, link := range linkIDs {
			env.Detach(context.Background(), link.ID)
		}
	})

	// Verify the egress dispatcher has all 3 programs.
	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffsLayout := env.Layout.BPFFS()
	configMapPin := bpffsLayout.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCEgress, nsid, uint32(veth.A.Ifindex))
	activeMapPin := bpffsLayout.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCEgress, nsid, uint32(veth.A.Ifindex))

	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
	require.Equal(t, uint32(3), cfg.NumProgsEnabled,
		"egress dispatcher should have 3 programs")

	// Send traffic: ping from B to A. The ICMP replies leave
	// through A's egress, where the programs are attached.
	veth.Ping(t, 20)

	// Each program's stats map must show non-zero packet counts.
	for i, prog := range progs {
		statsPath := filepath.Join(prog.mapPinPath, "tc_stats_map")
		packets := readStatsMap(t, statsPath)
		t.Logf("egress program %d (kernel_id=%d): %d packets",
			i, prog.kernelID, packets)
		assert.Greater(t, packets, uint64(0),
			"egress program %d should have counted packets", i)
	}
}
