//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
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
