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

// readDispatcherConfig loads the pinned config and active maps, reads
// the active buffer index, and returns the current RuntimeConfig.
func readDispatcherConfig(t *testing.T, configMapPin, activeMapPin string) dispatcher.RuntimeConfig {
	t.Helper()

	activeMap, err := ebpf.LoadPinnedMap(activeMapPin, nil)
	require.NoError(t, err, "load pinned active map")
	defer activeMap.Close()

	var active uint32
	require.NoError(t, activeMap.Lookup(uint32(0), &active), "read active index")

	configMap, err := ebpf.LoadPinnedMap(configMapPin, nil)
	require.NoError(t, err, "load pinned config map")
	defer configMap.Close()

	var cfg dispatcher.RuntimeConfig
	require.NoError(t, configMap.Lookup(active, &cfg), "read active config")
	return cfg
}

// TestTC_DispatcherPriorityOrdering verifies that filling all 10
// dispatcher slots with different priorities produces a BPF runtime
// config whose run_order reflects priority ordering (lowest priority
// first).
func TestTC_DispatcherPriorityOrdering(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	// Attach all 10 slots with scrambled priorities so the
	// run_order must differ from the insertion order.
	priorities := []int{900, 0, 400, 100, 800, 200, 600, 300, 700, 500}
	var linkIDs []bpfman.LinkRecord
	for _, prio := range priorities {
		tcSpec, err := bpfman.NewTCAttachSpec(
			prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(prio)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err)
		linkIDs = append(linkIDs, link)
	}

	t.Cleanup(func() {
		for _, link := range linkIDs {
			env.Detach(context.Background(), link.ID)
		}
	})

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))
	activeMapPin := bpffs.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))

	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)

	assert.Equal(t, uint32(10), cfg.NumProgsEnabled,
		"should have 10 programs enabled")

	// Expected run_order: sort slots by priority ascending.
	// priority -> slot: 0->1, 100->3, 200->5, 300->7, 400->2,
	//                   500->9, 600->6, 700->8, 800->4, 900->0
	expectedOrder := []uint32{1, 3, 5, 7, 2, 9, 6, 8, 4, 0}
	for i, expected := range expectedOrder {
		assert.Equal(t, expected, cfg.RunOrder[i],
			"run_order[%d] should be slot %d (priority %d)",
			i, expected, priorities[expected])
	}
}

// TestTC_AttachExceedsMaxPrograms verifies that attempting to attach
// more than dispatcher.MaxPrograms extensions to a single TC
// dispatcher fails with a "no free dispatcher slots" error.
func TestTC_AttachExceedsMaxPrograms(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	// Fill all dispatcher slots.
	var links []bpfman.LinkRecord
	for i := range dispatcher.MaxPrograms {
		tcSpec, err := bpfman.NewTCAttachSpec(
			prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(i * 100)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "attach %d should succeed", i)
		links = append(links, link)
	}

	t.Cleanup(func() {
		for _, link := range links {
			env.Detach(context.Background(), link.ID)
		}
	})

	// The next attach must fail because all slots are occupied.
	tcSpec, err := bpfman.NewTCAttachSpec(
		prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
		bpfman.TCDirectionIngress,
	)
	require.NoError(t, err)
	tcSpec = tcSpec.WithPriority(dispatcher.MaxPrograms * 100)
	_, err = env.Attach(ctx, tcSpec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no free dispatcher slots")
}

// TestTC_SlotReusedAfterDetach verifies that detaching a program from
// the middle of a full dispatcher frees its slot, and that a
// subsequent attach reclaims that slot with the runtime config
// reflecting the new program's priority in the correct position.
func TestTC_SlotReusedAfterDetach(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	// Fill all 10 slots with ascending priorities: slot i gets
	// priority i*100.
	links := make([]bpfman.LinkRecord, dispatcher.MaxPrograms)
	for i := range dispatcher.MaxPrograms {
		tcSpec, err := bpfman.NewTCAttachSpec(
			prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(i * 100)
		links[i], err = env.Attach(ctx, tcSpec)
		require.NoError(t, err, "attach %d should succeed", i)
	}

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))
	activeMapPin := bpffs.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))

	// Sanity check: all 10 slots filled, trivial run_order.
	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
	require.Equal(t, uint32(dispatcher.MaxPrograms), cfg.NumProgsEnabled)

	// Detach the program in slot 3 (priority 300).
	const detachSlot = 3
	err = env.Detach(ctx, links[detachSlot].ID)
	require.NoError(t, err)

	cfg = readDispatcherConfig(t, configMapPin, activeMapPin)
	assert.Equal(t, uint32(dispatcher.MaxPrograms-1), cfg.NumProgsEnabled,
		"should have 9 programs after detach")

	// Attach a new program at priority 550. findFreeSlot should
	// assign it to the vacated slot 3.
	tcSpec, err := bpfman.NewTCAttachSpec(
		prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
		bpfman.TCDirectionIngress,
	)
	require.NoError(t, err)
	tcSpec = tcSpec.WithPriority(550)
	newLink, err := env.Attach(ctx, tcSpec)
	require.NoError(t, err)

	t.Cleanup(func() {
		for i, link := range links {
			if i == detachSlot {
				continue // already detached
			}
			env.Detach(context.Background(), link.ID)
		}
		env.Detach(context.Background(), newLink.ID)
	})

	// Verify 10 programs again, with the new program's priority
	// (550) slotting between priorities 500 (slot 5) and 600
	// (slot 6).
	//
	// Slot -> priority: 0:0, 1:100, 2:200, 3:550, 4:400, 5:500,
	//                   6:600, 7:700, 8:800, 9:900
	// Sorted by priority: 0,100,200,400,500,550,600,700,800,900
	// Expected run_order: [0,1,2,4,5,3,6,7,8,9]
	cfg = readDispatcherConfig(t, configMapPin, activeMapPin)
	assert.Equal(t, uint32(dispatcher.MaxPrograms), cfg.NumProgsEnabled,
		"should have 10 programs after reattach")

	expectedOrder := [dispatcher.MaxPrograms]uint32{0, 1, 2, 4, 5, 3, 6, 7, 8, 9}
	assert.Equal(t, expectedOrder, cfg.RunOrder,
		"run_order should reflect the new program at slot 3 with priority 550")
}

// TestTC_DispatcherLifecycleAfterLastDetach verifies that removing the
// last extension from a TC dispatcher causes the dispatcher to be
// fully torn down (TC filter removed, pins removed, store entry
// deleted) and that a subsequent attach creates a fresh dispatcher
// with a new program ID.
func TestTC_DispatcherLifecycleAfterLastDetach(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	dispType := dispatcher.DispatcherTypeTCIngress
	ifindex := uint32(iface.Ifindex)

	// Phase 1: attach a single program. A dispatcher should now exist.
	tcSpec, err := bpfman.NewTCAttachSpec(
		prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
		bpfman.TCDirectionIngress,
	)
	require.NoError(t, err)
	tcSpec = tcSpec.WithPriority(100)

	link, err := env.Attach(ctx, tcSpec)
	require.NoError(t, err)

	state1, err := env.GetDispatcher(ctx, dispType, nsid, ifindex)
	require.NoError(t, err, "dispatcher should exist after attach")

	// The TC filter should be present on the interface.
	filters := tcIngressFilters(t, iface.Name)
	require.NotEmpty(t, filters, "TC filter should be present after attach")

	// Phase 2: detach the only program. The dispatcher should be
	// fully cleaned up.
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	_, err = env.GetDispatcher(ctx, dispType, nsid, ifindex)
	require.ErrorIs(t, err, platform.ErrRecordNotFound,
		"dispatcher should be absent from store after last detach")

	// The TC filter should be gone.
	filters = tcIngressFilters(t, iface.Name)
	assert.Empty(t, filters, "TC filter should be removed after last detach")

	// The config and active map pins should be gone.
	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(dispType, nsid, ifindex)
	activeMapPin := bpffs.DispatcherActiveMapPath(dispType, nsid, ifindex)
	_, err = os.Stat(configMapPin)
	assert.True(t, os.IsNotExist(err), "config map pin should not exist: %s", configMapPin)
	_, err = os.Stat(activeMapPin)
	assert.True(t, os.IsNotExist(err), "active map pin should not exist: %s", activeMapPin)

	// Phase 3: attach again. A new dispatcher should be created
	// with a different program ID.
	tcSpec2, err := bpfman.NewTCAttachSpec(
		prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
		bpfman.TCDirectionIngress,
	)
	require.NoError(t, err)
	tcSpec2 = tcSpec2.WithPriority(200)

	link2, err := env.Attach(ctx, tcSpec2)
	require.NoError(t, err)
	t.Cleanup(func() { env.Detach(context.Background(), link2.ID) })

	state2, err := env.GetDispatcher(ctx, dispType, nsid, ifindex)
	require.NoError(t, err, "dispatcher should exist after second attach")
	assert.NotEqual(t, state1.ProgramID, state2.ProgramID,
		"second dispatcher should have a different program ID")

	// The TC filter should be back.
	filters = tcIngressFilters(t, iface.Name)
	assert.NotEmpty(t, filters, "TC filter should be present after second attach")
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

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)
	ifindex := uint32(iface.Ifindex)

	bpffs := env.Layout.BPFFS()

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
	ingressConfigPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, ifindex)
	ingressActivePin := bpffs.DispatcherActiveMapPath(
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

	egressConfigPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCEgress, nsid, ifindex)
	egressActivePin := bpffs.DispatcherActiveMapPath(
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

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	proceedOn := []int32{0, 3, 30} // TC_ACT_OK, TC_ACT_PIPE, DispatcherReturn

	type prog struct {
		kernelID   kernel.ProgramID
		mapPinPath string
	}

	loadProg := func() prog {
		programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
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

	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(veth.A.Ifindex))
	activeMapPin := bpffs.DispatcherActiveMapPath(
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

	// ── Peak 0: fill all 10 slots ──────────────────────────────
	initialPriorities := make([]int, dispatcher.MaxPrograms)
	for i := range initialPriorities {
		initialPriorities[i] = i * 100 // 0, 100, 200, ..., 900
	}
	fill(initialPriorities)
	verifyConfig("peak 0")
	verifyTraffic("peak 0")

	// ── Trough 1: drain first 6 ────────────────────────────────
	// Slots 0-5 freed; slots 6-9 survive (priorities 600-900).
	drain(0, 6)
	verifyConfig("trough 1")

	// ── Peak 1: refill 6 ──────────────────────────────────────
	// Priorities interleave with surviving 600, 700, 800, 900.
	fill([]int{950, 850, 750, 650, 550, 450})
	verifyConfig("peak 1")
	verifyTraffic("peak 1")

	// ── Trough 2: drain last 7 ────────────────────────────────
	// Slots 3-9 freed; slots 0-2 survive (priorities 950, 850,
	// 750 from wave 1).
	drain(3, 10)
	verifyConfig("trough 2")

	// ── Peak 2: refill 7 ──────────────────────────────────────
	// Priorities interleave with surviving 950, 850, 750.
	fill([]int{25, 125, 225, 325, 425, 525, 625})
	verifyConfig("peak 2")
	verifyTraffic("peak 2")

	// ── Trough 3: drain first 8 ───────────────────────────────
	// Slots 0-7 freed; slots 8-9 survive (priorities 525, 625
	// from wave 2).
	drain(0, 8)
	verifyConfig("trough 3")

	// ── Peak 3: refill 8 ──────────────────────────────────────
	// Priorities interleave with surviving 525, 625.
	fill([]int{62, 162, 262, 362, 462, 562, 662, 762})
	verifyConfig("peak 3")
	verifyTraffic("peak 3")
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

	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeXDP, nsid, uint32(iface.Ifindex))
	activeMapPin := bpffs.DispatcherActiveMapPath(
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

// TestTC_DispatcherConfigRecomputedOnDetach verifies that filling all
// 10 TC extension slots with ascending priorities, then detaching the
// lowest-priority program, correctly recomputes the runtime config
// with the remaining 9 in priority order.
func TestTC_DispatcherConfigRecomputedOnDetach(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	iface := NewTestInterface(t)
	ctx := context.Background()

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}
	programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
		{Type: bpfman.ProgramTypeTC, Name: "stats"},
	}, manager.LoadOpts{})
	require.NoError(t, err)

	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	// Attach all 10 slots with ascending priorities: slot 0 has
	// priority 0, slot 1 has priority 100, ..., slot 9 has 900.
	var linkIDs []bpfman.LinkRecord
	for i := 0; i < 10; i++ {
		tcSpec, err := bpfman.NewTCAttachSpec(
			prog.Status.Kernel.ID, iface.Name, iface.Ifindex,
			bpfman.TCDirectionIngress,
		)
		require.NoError(t, err)
		tcSpec = tcSpec.WithPriority(i * 100)
		link, err := env.Attach(ctx, tcSpec)
		require.NoError(t, err, "attach %d should succeed", i)
		linkIDs = append(linkIDs, link)
	}

	t.Cleanup(func() {
		for i := 1; i < 10; i++ {
			env.Detach(context.Background(), linkIDs[i].ID)
		}
	})

	nsid, err := netns.GetCurrentNsid()
	require.NoError(t, err)

	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))
	activeMapPin := bpffs.DispatcherActiveMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(iface.Ifindex))

	// Before detach: 10 programs, in ascending slot order since
	// priorities match insertion order.
	cfg := readDispatcherConfig(t, configMapPin, activeMapPin)
	require.Equal(t, uint32(10), cfg.NumProgsEnabled)
	for i := 0; i < 10; i++ {
		assert.Equal(t, uint32(i), cfg.RunOrder[i],
			"before detach: run_order[%d] should be slot %d", i, i)
	}

	// Detach the lowest-priority program (slot 0, priority 0)
	err = env.Detach(ctx, linkIDs[0].ID)
	require.NoError(t, err)

	// After detach: 9 programs remaining (slots 1-9), still in
	// ascending priority order.
	cfg = readDispatcherConfig(t, configMapPin, activeMapPin)
	assert.Equal(t, uint32(9), cfg.NumProgsEnabled,
		"should have 9 programs after detach")
	for i := 0; i < 9; i++ {
		assert.Equal(t, uint32(i+1), cfg.RunOrder[i],
			"after detach: run_order[%d] should be slot %d", i, i+1)
	}
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

	imageRef := platform.ImageRef{
		URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
		PullPolicy: bpfman.PullIfNotPresent,
	}

	// Load 5 separate instances so each gets independent maps.
	type loadedProg struct {
		kernelID   kernel.ProgramID
		mapPinPath string
	}

	var progs []loadedProg
	for i := 0; i < 5; i++ {
		programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
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

	bpffs := env.Layout.BPFFS()
	configMapPin := bpffs.DispatcherConfigMapPath(
		dispatcher.DispatcherTypeTCIngress, nsid, uint32(veth.A.Ifindex))
	activeMapPin := bpffs.DispatcherActiveMapPath(
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

			imageRef := platform.ImageRef{
				URL:        "quay.io/bpfman-bytecode/go-tc-counter:latest",
				PullPolicy: bpfman.PullIfNotPresent,
			}

			type loadedProg struct {
				kernelID   kernel.ProgramID
				mapPinPath string
			}

			var progs []loadedProg
			for i := 0; i < tt.n; i++ {
				programs, err := env.LoadImage(ctx, imageRef, []manager.ProgramSpec{
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
