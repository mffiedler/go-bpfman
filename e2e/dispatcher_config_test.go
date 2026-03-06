//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/ns/netns"
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
	priorities := []int{500, 100, 800, 300, 900, 200, 700, 400, 600, 0}
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
	require.Equal(t, uint32(10), cfg.NumProgsEnabled,
		"all 10 slots should be occupied")

	switch h.dispType {
	case dispatcher.DispatcherTypeTCIngress:
		// TC: run_order follows priority ascending.
		// priorities = [500,100,800,300,900,200,700,400,600,0]
		// sorted indices by priority ascending:
		// prio 0->slot9, 100->slot1, 200->slot5, 300->slot3,
		// 400->slot7, 500->slot0, 600->slot8, 700->slot6,
		// 800->slot2, 900->slot4
		expected := [dispatcher.MaxPrograms]uint32{9, 1, 5, 3, 7, 0, 8, 6, 2, 4}
		assert.Equal(t, expected, cfg.RunOrder, "TC run_order should follow priority")

	case dispatcher.DispatcherTypeXDP:
		// XDP: all priority 0, run_order follows insertion
		// order (stable sort preserves append order).
		expected := [dispatcher.MaxPrograms]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
		assert.Equal(t, expected, cfg.RunOrder, "XDP run_order should follow insertion order")
	}
}

// TestDispatcher_AttachExceedsMaxPrograms verifies that attempting to
// attach an 11th program fails with a "no free slots" error.
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
// program from a dispatcher slot allows that slot to be reused by a
// subsequent attachment.
func TestDispatcher_SlotReusedAfterDetach(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testSlotReusedAfterDetach(t, h)
		})
	}
}

func testSlotReusedAfterDetach(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Fill all 10 slots.
	var links []bpfman.LinkRecord
	for i := 0; i < dispatcher.MaxPrograms; i++ {
		link := h.attach(t, progID, (i+1)*100)
		links = append(links, link)
	}

	// Detach slot 3 (4th attachment).
	err := h.env.Detach(context.Background(), links[3].ID)
	require.NoError(t, err, "detach slot 3")

	// Re-attach: should succeed and reuse the vacated slot.
	newLink := h.attach(t, progID, 350)

	t.Cleanup(func() {
		for i, link := range links {
			if i == 3 {
				continue // already detached
			}
			h.env.Detach(context.Background(), link.ID)
		}
		h.env.Detach(context.Background(), newLink.ID)
	})

	cfg := h.readConfig(t)
	require.Equal(t, uint32(10), cfg.NumProgsEnabled,
		"all 10 slots should be occupied again")

	switch h.dispType {
	case dispatcher.DispatcherTypeTCIngress:
		// The new program at priority 350 should appear between
		// slot 2 (priority 300) and slot 4 (priority 400) in
		// run_order.
		expected := [dispatcher.MaxPrograms]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
		assert.Equal(t, expected, cfg.RunOrder, "TC run_order after slot reuse")

	case dispatcher.DispatcherTypeXDP:
		// XDP: all priority 0. The reattached program goes into
		// slot 3 but is appended last in computeRuntimeConfig,
		// so stable sort places it after the other 9 slots.
		expected := [dispatcher.MaxPrograms]uint32{0, 1, 2, 4, 5, 6, 7, 8, 9, 3}
		assert.Equal(t, expected, cfg.RunOrder, "XDP run_order after slot reuse")
	}
}

// TestDispatcher_LifecycleAfterLastDetach verifies that removing the
// last attached program tears down the dispatcher entirely (pins
// removed), and that a fresh attachment creates a new dispatcher.
func TestDispatcher_LifecycleAfterLastDetach(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testLifecycleAfterLastDetach(t, h)
		})
	}
}

func testLifecycleAfterLastDetach(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Attach a single program.
	link := h.attach(t, progID, 100)
	h.verifyAttachPresent(t)

	cfg := h.readConfig(t)
	require.Equal(t, uint32(1), cfg.NumProgsEnabled,
		"should have 1 program")

	// Detach the only program: dispatcher should be torn down.
	err := h.env.Detach(context.Background(), link.ID)
	require.NoError(t, err)
	h.verifyAttachAbsent(t)

	// Reattach: a fresh dispatcher should be created.
	newLink := h.attach(t, progID, 200)
	t.Cleanup(func() {
		h.env.Detach(context.Background(), newLink.ID)
	})

	h.verifyAttachPresent(t)
	cfg = h.readConfig(t)
	require.Equal(t, uint32(1), cfg.NumProgsEnabled,
		"should have 1 program after reattach")
}

// TestDispatcher_DoubleBufferFlip verifies that each dispatcher
// mutation alternates the active buffer index. Starting from 0, the
// first attach should flip to 1, the second back to 0, and so on.
// This is a fundamental property of the double-buffer scheme.
func TestDispatcher_DoubleBufferFlip(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testDoubleBufferFlip(t, h)
		})
	}
}

func testDoubleBufferFlip(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Attach 5 programs, checking the active index after each.
	var links []bpfman.LinkRecord
	for i := 0; i < 5; i++ {
		link := h.attach(t, progID, (i+1)*100)
		links = append(links, link)

		active := h.readActiveIdx(t)
		// First attach creates the dispatcher (index starts
		// at 0, then the update flips to 1). Each subsequent
		// attach flips again.
		expected := uint32((i + 1) % 2)
		assert.Equal(t, expected, active,
			"active index after attach %d", i)
	}

	// Detach the first 3 and verify flips continue.
	for i := 0; i < 3; i++ {
		err := h.env.Detach(context.Background(), links[i].ID)
		require.NoError(t, err)

		active := h.readActiveIdx(t)
		expected := uint32((5 + i + 1) % 2)
		assert.Equal(t, expected, active,
			"active index after detach %d", i)
	}

	t.Cleanup(func() {
		for i := 3; i < 5; i++ {
			h.env.Detach(context.Background(), links[i].ID)
		}
	})
}

// TestDispatcher_ConfigRecomputedOnDetach verifies that filling all
// 10 slots then detaching the lowest-priority program correctly
// updates the runtime config to reflect 9 remaining programs.
func TestDispatcher_ConfigRecomputedOnDetach(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testConfigRecomputedOnDetach(t, h)
		})
	}
}

func testConfigRecomputedOnDetach(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

	// Fill all 10 slots.
	var links []bpfman.LinkRecord
	for i := 0; i < dispatcher.MaxPrograms; i++ {
		link := h.attach(t, progID, (i+1)*100)
		links = append(links, link)
	}

	cfg := h.readConfig(t)
	require.Equal(t, uint32(10), cfg.NumProgsEnabled,
		"should have 10 programs")

	// Detach the first program (lowest priority for TC, first
	// inserted for XDP).
	err := h.env.Detach(context.Background(), links[0].ID)
	require.NoError(t, err)

	t.Cleanup(func() {
		for i := 1; i < dispatcher.MaxPrograms; i++ {
			h.env.Detach(context.Background(), links[i].ID)
		}
	})

	cfg = h.readConfig(t)
	require.Equal(t, uint32(9), cfg.NumProgsEnabled,
		"should have 9 programs after detach")

	// Verify the run_order contains the remaining 9 slots.
	var runOrder []uint32
	for i := 0; i < 9; i++ {
		runOrder = append(runOrder, cfg.RunOrder[i])
	}

	// Slot 0 was detached; slots 1-9 should remain.
	expected := []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9}
	assert.Equal(t, expected, runOrder,
		"run_order should contain slots 1-9")
}

// TestDispatcher_MultipleInterfacesIndependent verifies that
// dispatcher state on one interface is independent of another.
// Detaching from interface B must not affect interface A's config.
func TestDispatcher_MultipleInterfacesIndependent(t *testing.T) {
	for _, h := range eachDispatcherType(t) {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			testMultipleInterfacesIndependent(t, h)
		})
	}
}

func testMultipleInterfacesIndependent(t *testing.T, h dispatcherTestHarness) {
	progID := h.loadProg(t)

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

	// Record A's config before any B mutations.
	cfgA := h.readConfig(t)
	require.Equal(t, uint32(3), cfgA.NumProgsEnabled,
		"interface A should have 3 programs")

	// Detach all programs from B.
	for _, l := range linksB {
		err := h.env.Detach(context.Background(), l.ID)
		require.NoError(t, err)
	}

	// A's config should be unchanged.
	cfgAAfter := h.readConfig(t)
	assert.Equal(t, cfgA.NumProgsEnabled, cfgAAfter.NumProgsEnabled,
		"A's program count should be unchanged")
	assert.Equal(t, cfgA.RunOrder, cfgAAfter.RunOrder,
		"A's run_order should be unchanged")
}
