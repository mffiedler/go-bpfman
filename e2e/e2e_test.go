//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/e2e/testnet"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager"
)

// TestTracepoint_LoadAttachDetachUnload tests the full lifecycle of a tracepoint program.
func TestTracepoint_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTracepoint(t, "syscalls", "sys_enter_kill")

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/tracepoint_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeTracepoint,
			Name: "tracepoint_kill_recorder",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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
	tpSpec, err := bpfman.NewTracepointAttachSpecFromString(prog.Status.Kernel.ID, "syscalls/sys_enter_kill")
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

	// Behavioural validation: drive a known number of kills from
	// the workload subprocess (filtered in-kernel by expected_pid)
	// and assert the counter equals events * weight exactly. A
	// still-firing program after detach, a misrouted event, or a
	// missed weight global all surface as wrong arithmetic.
	const events = 5
	workload.Kill(events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "tp_count"))
	t.Logf("tracepoint: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"tracepoint counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "tp_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Kill(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgTracepoint_LoadAttachDetachUnload proves that the
// variadic `--programs` form of `program load file` produces three
// independent tracepoint programs from a single object, with
// per-program global data and metadata correctly routed, and that
// detaching one link from the same-hook chain stops only that
// program -- the others keep firing. Counter values are weighted
// products of (events * per-program weight), so a still-firing
// detached program produces a wrong number rather than a missed
// signal.
func TestMultiProgTracepoint_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTracepoint(t, "syscalls", "sys_enter_kill")

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	metadata := map[string]string{
		"test":    t.Name(),
		"surface": "multi-tracepoint",
	}
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTracepoint, Name: "tp_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tracepoint_counter.bpf.o", specs, manager.LoadOpts{
		UserMetadata: metadata,
		GlobalData:   globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))

	for i, prog := range programs {
		name := "tp_" + plans[i].suffix
		require.NotNil(t, prog.Status.Kernel, "kernel info present for %s", name)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for %s", name)
		require.Equal(t, bpfman.ProgramTypeTracepoint, prog.Record.Load.ProgramType())
		require.Equal(t, name, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

		// Round-trip metadata + global data per program.
		gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
		require.NoError(t, err)
		require.Equal(t, t.Name(), gotProg.Record.Meta.Metadata["test"], "%s metadata.test", name)
		require.Equal(t, "multi-tracepoint", gotProg.Record.Meta.Metadata["surface"], "%s metadata.surface", name)
		require.Equal(t, globals["expected_pid"], gotProg.Record.Load.GlobalData()["expected_pid"], "%s global expected_pid", name)
		for _, p := range plans {
			gname := "weight_" + p.suffix
			require.Equal(t, globals[gname], gotProg.Record.Load.GlobalData()[gname], "%s global %s", name, gname)
		}
	}

	// Each program owns a distinct counter map.
	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "tp_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewTracepointAttachSpecFromString(prog.Status.Kernel.ID, "syscalls/sys_enter_kill")
		require.NoError(t, err)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err)
		require.Equal(t, bpfman.LinkKindTracepoint, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c, drain. Wave 2: a, b -> detach b, drain. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "tp_a_count")
	mapIDB := mapIDByName(t, programs[1], "tp_b_count")
	mapIDC := mapIDByName(t, programs[2], "tp_c_count")
	workload.Kill(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach tp_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Kill(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Kill(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach tp_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Kill(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Kill(eventsPerWave)

	expectEvents := []uint64{
		3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		1*uint64(eventsPerWave) + uint64(qc.EventsCounted),                     // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "tp_"+plans[i].suffix+"_count"))
		t.Logf("tp_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"tp_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach tp_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestMultiProgMixed_LoadAttachDetachUnload proves that the
// variadic `--programs` form supports heterogeneous program types
// in one object (tracepoint + kprobe + kretprobe), with each type
// going down its own attach path, and that detaching one link in
// the cross-type chain stops that program firing while the
// still-attached programs of any type continue.
func TestMultiProgMixed_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTracepoint(t, "syscalls", "sys_enter_unlinkat")
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		name         string
		progType     bpfman.ProgramType
		linkKind     bpfman.LinkKind
		mapName      string
		weightGlobal string
		weight       uint64
		expectEvents uint64
		newAttach    func(progID kernel.ProgramID) (bpfman.AttachSpec, error)
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{
			name: "mixed_tp", progType: bpfman.ProgramTypeTracepoint, linkKind: bpfman.LinkKindTracepoint,
			mapName: "mtp_count", weightGlobal: "weight_tp", weight: weights[0], expectEvents: 3 * eventsPerWave,
			newAttach: func(id kernel.ProgramID) (bpfman.AttachSpec, error) {
				return bpfman.NewTracepointAttachSpecFromString(id, "syscalls/sys_enter_unlinkat")
			},
		},
		{
			name: "mixed_kp", progType: bpfman.ProgramTypeKprobe, linkKind: bpfman.LinkKindKprobe,
			mapName: "mkp_count", weightGlobal: "weight_kp", weight: weights[1], expectEvents: 2 * eventsPerWave,
			newAttach: func(id kernel.ProgramID) (bpfman.AttachSpec, error) {
				return bpfman.NewKprobeAttachSpec(id, "do_unlinkat")
			},
		},
		{
			name: "mixed_krp", progType: bpfman.ProgramTypeKretprobe, linkKind: bpfman.LinkKindKretprobe,
			mapName: "mkrp_count", weightGlobal: "weight_krp", weight: weights[2], expectEvents: 1 * eventsPerWave,
			newAttach: func(id kernel.ProgramID) (bpfman.AttachSpec, error) {
				return bpfman.NewKprobeAttachSpec(id, "do_unlinkat")
			},
		},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	metadata := map[string]string{
		"test":    t.Name(),
		"surface": "multi-mixed",
	}
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: p.progType, Name: p.name}
		globals[p.weightGlobal] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_mixed_counter.bpf.o", specs, manager.LoadOpts{
		UserMetadata: metadata,
		GlobalData:   globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))

	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for %s", plans[i].name)
		require.NotZero(t, prog.Status.Kernel.ID)
		require.Equal(t, plans[i].progType, prog.Record.Load.ProgramType(), "program %s", plans[i].name)
		require.Equal(t, plans[i].name, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

		gotProg, err := env.Get(ctx, prog.Status.Kernel.ID)
		require.NoError(t, err)
		require.Equal(t, t.Name(), gotProg.Record.Meta.Metadata["test"], "%s metadata.test", plans[i].name)
		require.Equal(t, "multi-mixed", gotProg.Record.Meta.Metadata["surface"], "%s metadata.surface", plans[i].name)
		require.Equal(t, globals["expected_pid"], gotProg.Record.Load.GlobalData()["expected_pid"], "%s global expected_pid", plans[i].name)
		for _, p := range plans {
			require.Equal(t, globals[p.weightGlobal], gotProg.Record.Load.GlobalData()[p.weightGlobal], "%s global %s", plans[i].name, p.weightGlobal)
		}
	}

	// Each program owns a distinct counter map.
	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, plans[i].mapName)
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := plans[i].newAttach(prog.Status.Kernel.ID)
		require.NoError(t, err)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].name)
		require.Equal(t, plans[i].linkKind, link.Kind, "link kind for %s", plans[i].name)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: tp, kp, krp -> detach krp, drain. Wave 2: tp, kp -> detach kp, drain. Wave 3: tp.
	mapIDTp := mapIDByName(t, programs[0], plans[0].mapName) // control: always attached
	mapIDKp := mapIDByName(t, programs[1], plans[1].mapName)
	mapIDKrp := mapIDByName(t, programs[2], plans[2].mapName)
	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach %s", plans[2].name)
	qkrp := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDKrp, DetachedWeight: plans[2].weight,
		ControlMap: mapIDTp, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence krp: probes=%d, eventsCounted=%d, latency=%s", qkrp.Probes, qkrp.EventsCounted, qkrp.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach %s", plans[1].name)
	qkp := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDKp, DetachedWeight: plans[1].weight,
		ControlMap: mapIDTp, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence kp: probes=%d, eventsCounted=%d, latency=%s", qkp.Probes, qkp.EventsCounted, qkp.Latency)

	workload.Unlink(eventsPerWave)

	expectEvents := []uint64{
		3*uint64(eventsPerWave) + uint64(qkrp.Probes) + uint64(qkp.Probes),        // tp (always attached)
		2*uint64(eventsPerWave) + uint64(qkrp.Probes) + uint64(qkp.EventsCounted), // kp
		1*uint64(eventsPerWave) + uint64(qkrp.EventsCounted),                      // krp
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, plans[i].mapName))
		t.Logf("%s: events=%d weight=%d want=%d got=%d", plans[i].name, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].name, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach %s", plans[0].name)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestMultiProgKprobe_LoadAttachDetachUnload proves that detaching
// one kprobe from a same-hook multi-program chain stops that
// program firing while the still-attached programs continue --
// the property the perf-link order-of-operations fix in commit
// 1459c0b was added to guarantee. Without the fix the kernel
// keeps a detached perf-link program running, and staggered
// exact-equality counters surface that as a wrong product rather
// than a missed signal.
func TestMultiProgKprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	// Three kprobe programs all attach to do_unlinkat. Detach is
	// staggered across three workload waves so each program ends
	// up with a distinct event count: a sees waves 1+2+3, b sees
	// 1+2, c sees only 1.
	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeKprobe, Name: "mkp_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_kprobe_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mkp_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mkp_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeKprobe, prog.Record.Load.ProgramType())
		require.Equal(t, "mkp_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	// Each program owns a distinct counter map.
	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mkp_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewKprobeAttachSpec(prog.Status.Kernel.ID, "do_unlinkat")
		require.NoError(t, err)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mkp_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindKprobe, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c, drain. Wave 2: a, b -> detach b, drain. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "mkp_a_count")
	mapIDB := mapIDByName(t, programs[1], "mkp_b_count")
	mapIDC := mapIDByName(t, programs[2], "mkp_c_count")
	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mkp_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mkp_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Unlink(eventsPerWave)

	expectEvents := []uint64{
		3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		1*uint64(eventsPerWave) + uint64(qc.EventsCounted),                     // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mkp_"+plans[i].suffix+"_count"))
		t.Logf("mkp_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mkp_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mkp_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestKprobe_LoadAttachDetachUnload tests the full lifecycle of a kprobe program.
func TestKprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/kprobe_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeKprobe,
			Name: "kprobe_counter",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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
	kpSpec, err := bpfman.NewKprobeAttachSpec(prog.Status.Kernel.ID, "do_unlinkat")
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
	require.Equal(t, "do_unlinkat", kprobeDetails.FnName)
	require.Equal(t, uint64(0), kprobeDetails.Offset, "offset should match what was passed")
	require.False(t, kprobeDetails.Retprobe)

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// Behavioural validation: drive a known number of unlinks from
	// the workload subprocess and assert events * weight exactly.
	const events = 5
	workload.Unlink(events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "kp_count"))
	t.Logf("kprobe: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"kprobe counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "kp_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Unlink(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgKretprobe_LoadAttachDetachUnload proves that
// detaching one kretprobe from a same-hook multi-program chain
// stops that program firing while the still-attached programs
// continue. Same property as TestMultiProgKprobe but exercising
// the kretprobe attach path; reuses the kprobe object loaded with
// Type: Kretprobe.
func TestMultiProgKretprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeKretprobe, Name: "mkp_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_kprobe_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mkp_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mkp_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeKretprobe, prog.Record.Load.ProgramType())
		require.Equal(t, "mkp_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mkp_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewKprobeAttachSpec(prog.Status.Kernel.ID, "do_unlinkat")
		require.NoError(t, err)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mkp_%s", plans[i].suffix)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c, drain. Wave 2: a, b -> detach b, drain. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "mkp_a_count")
	mapIDB := mapIDByName(t, programs[1], "mkp_b_count")
	mapIDC := mapIDByName(t, programs[2], "mkp_c_count")
	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mkp_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mkp_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Unlink(eventsPerWave)

	// Expected event tallies, counting probes fired during the
	// quiescence waits. a is always attached so it counts every
	// probe. b is attached during qc's drain so it counts those
	// probes; b is detached during qb's drain but counts the events
	// that fired before detach took effect (qb.EventsCounted). c is
	// detached during qc's drain so it counts only its own pre-
	// effective probes (qc.EventsCounted) and is silent through qb.
	expectEvents := []uint64{
		3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		1*uint64(eventsPerWave) + uint64(qc.EventsCounted),                     // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mkp_"+plans[i].suffix+"_count"))
		t.Logf("mkp_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mkp_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mkp_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestKretprobe_LoadAttachDetachUnload tests the full lifecycle of a kretprobe program.
func TestKretprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file (same program as kprobe, loaded as kretprobe)
	programs, err := env.LoadFile(ctx, "testdata/bpf/kprobe_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeKretprobe,
			Name: "kprobe_counter",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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
	kpSpec, err := bpfman.NewKprobeAttachSpec(prog.Status.Kernel.ID, "do_unlinkat")
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
	require.Equal(t, "do_unlinkat", kprobeDetails.FnName)
	require.Equal(t, uint64(0), kprobeDetails.Offset, "offset should match what was passed")
	require.True(t, kprobeDetails.Retprobe, "kretprobe should have Retprobe=true")

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, bpfman.LinkKindKretprobe, listedLinks[0].Kind, "ListLinks should report kretprobe")

	// Behavioural validation: drive a known number of unlinks; each
	// do_unlinkat call returns once, so events * weight matches
	// exactly.
	const events = 5
	workload.Unlink(events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "kp_count"))
	t.Logf("kretprobe: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"kretprobe counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "kp_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Unlink(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgUprobe_LoadAttachDetachUnload proves that detaching
// one uprobe from a same-hook multi-program chain stops that
// program firing while the still-attached programs continue. Same
// property as TestMultiProgKprobe but exercising the uprobe attach
// path against e2e_uprobe_call_malloc inside the e2e.test binary.
func TestMultiProgUprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	target, fnName := uprobeTarget()

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeUprobe, Name: "mup_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_uprobe_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mup_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mup_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeUprobe, prog.Record.Load.ProgramType())
		require.Equal(t, "mup_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mup_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewUprobeAttachSpec(prog.Status.Kernel.ID, target)
		require.NoError(t, err)
		spec = spec.WithFnName(fnName)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mup_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindUprobe, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c. Wave 2: a, b -> detach b. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "mup_a_count")
	mapIDB := mapIDByName(t, programs[1], "mup_b_count")
	mapIDC := mapIDByName(t, programs[2], "mup_c_count")
	workload.Uprobe(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mup_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Uprobe(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Uprobe(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mup_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Uprobe(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Uprobe(eventsPerWave)

	expectEvents := []uint64{
		3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		1*uint64(eventsPerWave) + uint64(qc.EventsCounted),                     // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mup_"+plans[i].suffix+"_count"))
		t.Logf("mup_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mup_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mup_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestUprobe_LoadAttachDetachUnload tests the full lifecycle of a uprobe program.
func TestUprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	target, fnName := uprobeTarget()

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/uprobe_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeUprobe,
			Name: "uprobe_counter",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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

	// When: attach via client to e2e_uprobe_call_malloc in the e2e.test binary itself
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

	// Behavioural validation: drive a known number of uprobe fires
	// from the workload subprocess. The driver calls
	// e2e_uprobe_call_malloc in its own PID; the BPF program filters
	// on expected_pid so parallel tests' uprobe traffic does not
	// reach this map.
	const events = 5
	workload.Uprobe(events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "up_count"))
	t.Logf("uprobe: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"uprobe counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "up_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Uprobe(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgUretprobe_LoadAttachDetachUnload proves that
// detaching one uretprobe from a same-hook multi-program chain
// stops that program firing while the still-attached programs
// continue. Same property as TestMultiProgUprobe but exercising
// the uretprobe attach path; reuses the uprobe object loaded
// with Type: Uretprobe.
func TestMultiProgUretprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	target, fnName := uprobeTarget()

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeUretprobe, Name: "mup_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_uprobe_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mup_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mup_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeUretprobe, prog.Record.Load.ProgramType())
		require.Equal(t, "mup_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mup_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewUprobeAttachSpec(prog.Status.Kernel.ID, target)
		require.NoError(t, err)
		spec = spec.WithFnName(fnName)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mup_%s", plans[i].suffix)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c, drain. Wave 2: a, b -> detach b, drain. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "mup_a_count")
	mapIDB := mapIDByName(t, programs[1], "mup_b_count")
	mapIDC := mapIDByName(t, programs[2], "mup_c_count")
	workload.Uprobe(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mup_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Uprobe(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Uprobe(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mup_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Uprobe(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Uprobe(eventsPerWave)

	expectEvents := []uint64{
		3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		1*uint64(eventsPerWave) + uint64(qc.EventsCounted),                     // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mup_"+plans[i].suffix+"_count"))
		t.Logf("mup_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mup_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mup_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestUretprobe_LoadAttachDetachUnload tests the full lifecycle of a uretprobe program.
func TestUretprobe_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	target, fnName := uprobeTarget()

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file (same program as uprobe, loaded as uretprobe)
	programs, err := env.LoadFile(ctx, "testdata/bpf/uprobe_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeUretprobe,
			Name: "uprobe_counter",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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

	// When: attach via client to e2e_uprobe_call_malloc in the e2e.test binary (uretprobe uses AttachUprobe API)
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

	// Behavioural validation: drive a known number of uprobe fires
	// from the workload subprocess. Each call to
	// e2e_uprobe_call_malloc returns once, so the uretprobe count
	// matches events * weight exactly.
	const events = 5
	workload.Uprobe(events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "up_count"))
	t.Logf("uretprobe: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"uretprobe counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "up_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Uprobe(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgFentry_LoadAttachDetachUnload proves that the
// kernel's fentry trampoline correctly multiplexes three fentry
// programs attached to the same target (do_unlinkat) and that
// detaching one removes only that program from the trampoline
// chain. fentry uses BPF tracing trampolines rather than perf
// links, so this exercises a different attach surface than the
// kprobe / uprobe siblings.
func TestMultiProgFentry_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{
			Type:       bpfman.ProgramTypeFentry,
			Name:       "mfe_" + p.suffix,
			AttachFunc: "do_unlinkat",
		}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_fentry_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mfe_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mfe_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeFentry, prog.Record.Load.ProgramType())
		require.Equal(t, "mfe_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mfe_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewFentryAttachSpec(prog.Status.Kernel.ID)
		require.NoError(t, err)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mfe_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindFentry, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c, drain. Wave 2: a, b -> detach b, drain. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "mfe_a_count")
	mapIDB := mapIDByName(t, programs[1], "mfe_b_count")
	mapIDC := mapIDByName(t, programs[2], "mfe_c_count")

	// Wait for attach to be effective on c (the last-attached); by
	// the time c counts a probe, the trampoline image contains all
	// three. Fentry/fexit on a shared target function rebuilds the
	// trampoline on every attach and is racy under suite contention.
	qa := waitProgramActive(t, AttachActiveProbe{
		AttachedMap: mapIDC, AttachedWeight: plans[2].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-attach activation c: probes=%d, lostProbes=%d, latency=%s", qa.Probes, qa.LostProbes, qa.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mfe_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mfe_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Unlink(eventsPerWave)

	// Each program counts:
	//   - qa.Probes from the activation barrier (1 of which actually counted on each attached prog)
	//   - 3*eventsPerWave for waves
	//   - qc.Probes / qb.Probes from drains while still attached
	// b is detached during qb's drain, so it counts qb.EventsCounted of those.
	// c is detached during qc's drain, so it counts qc.EventsCounted of those.
	expectEvents := []uint64{
		uint64(qa.Probes) + 3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		uint64(qa.Probes) + 2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		uint64(qa.EventsCounted) + 1*uint64(eventsPerWave) + uint64(qc.EventsCounted),              // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mfe_"+plans[i].suffix+"_count"))
		t.Logf("mfe_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mfe_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mfe_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestFentry_LoadAttachDetachUnload tests the full lifecycle of a fentry program.
func TestFentry_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/fentry_exact.bpf.o", []manager.ProgramSpec{
		{Name: "test_fentry", Type: bpfman.ProgramTypeFentry, AttachFunc: "do_unlinkat"},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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

	// Wait for attach to be effective (bpf_trampoline rebuild can lag
	// under suite-wide contention on do_unlinkat) before firing the
	// counted workload, then drive `events` unlinks for exact-equality.
	mapIDFe := mapIDByName(t, prog, "fe_count")
	qa := waitProgramActive(t, AttachActiveProbe{
		AttachedMap: mapIDFe, AttachedWeight: weights[0],
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-attach activation: probes=%d, lostProbes=%d, latency=%s", qa.Probes, qa.LostProbes, qa.Latency)

	const events = 5
	workload.Unlink(events)
	want := uint64(events+qa.EventsCounted) * weights[0]
	got := readArrayCounterByID(t, mapIDFe)
	t.Logf("fentry: events=%d weight=%d want=%d got=%d", events+qa.EventsCounted, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"fentry counter should equal events(%d) * weight(%d) = %d", events+qa.EventsCounted, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "fe_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Unlink(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgFexit_LoadAttachDetachUnload proves that the
// kernel's fexit trampoline correctly multiplexes three fexit
// programs attached to the same target (do_unlinkat) and that
// detaching one removes only that program from the trampoline
// chain. Same property as TestMultiProgFentry but for the
// function-return tracing path.
func TestMultiProgFexit_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	workload := startWorkload(t)

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{
		"expected_pid": uint32LE(uint32(workload.Pid())),
	}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{
			Type:       bpfman.ProgramTypeFexit,
			Name:       "mfx_" + p.suffix,
			AttachFunc: "do_unlinkat",
		}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_fexit_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mfx_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mfx_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeFexit, prog.Record.Load.ProgramType())
		require.Equal(t, "mfx_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mfx_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewFexitAttachSpec(prog.Status.Kernel.ID)
		require.NoError(t, err)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mfx_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindFexit, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c, drain. Wave 2: a, b -> detach b, drain. Wave 3: a.
	mapIDA := mapIDByName(t, programs[0], "mfx_a_count")
	mapIDB := mapIDByName(t, programs[1], "mfx_b_count")
	mapIDC := mapIDByName(t, programs[2], "mfx_c_count")

	// Wait for attach to be effective on c (the last-attached); by
	// the time c counts a probe, the trampoline image contains all
	// three. Fentry/fexit on a shared target function rebuilds the
	// trampoline on every attach and is racy under suite contention.
	qa := waitProgramActive(t, AttachActiveProbe{
		AttachedMap: mapIDC, AttachedWeight: plans[2].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-attach activation c: probes=%d, lostProbes=%d, latency=%s", qa.Probes, qa.LostProbes, qa.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mfx_%s", plans[2].suffix)
	qc := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDC, DetachedWeight: plans[2].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence c: probes=%d, eventsCounted=%d, latency=%s", qc.Probes, qc.EventsCounted, qc.Latency)

	workload.Unlink(eventsPerWave)

	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mfx_%s", plans[1].suffix)
	qb := waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap: mapIDB, DetachedWeight: plans[1].weight,
		ControlMap: mapIDA, ControlWeight: plans[0].weight,
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-detach quiescence b: probes=%d, eventsCounted=%d, latency=%s", qb.Probes, qb.EventsCounted, qb.Latency)

	workload.Unlink(eventsPerWave)

	expectEvents := []uint64{
		uint64(qa.Probes) + 3*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.Probes),        // a
		uint64(qa.Probes) + 2*uint64(eventsPerWave) + uint64(qc.Probes) + uint64(qb.EventsCounted), // b
		uint64(qa.EventsCounted) + 1*uint64(eventsPerWave) + uint64(qc.EventsCounted),              // c
	}

	for i, prog := range programs {
		want := expectEvents[i] * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mfx_"+plans[i].suffix+"_count"))
		t.Logf("mfx_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, expectEvents[i], plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mfx_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, expectEvents[i], plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mfx_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestFexit_LoadAttachDetachUnload tests the full lifecycle of a fexit program.
func TestFexit_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKernelFunction(t, "do_unlinkat")
	lockDoUnlinkAtHook(t)

	env := NewTestEnv(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	workload := startWorkload(t)
	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/fexit_exact.bpf.o", []manager.ProgramSpec{
		{Name: "test_fexit", Type: bpfman.ProgramTypeFexit, AttachFunc: "do_unlinkat"},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"expected_pid": uint32LE(uint32(workload.Pid())),
			"weight":       uint64LE(weights[0]),
		},
	})
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

	// Wait for attach to be effective (bpf_trampoline rebuild can lag
	// under suite-wide contention on do_unlinkat) before firing the
	// counted workload, then drive `events` unlinks for exact-equality.
	mapIDFx := mapIDByName(t, prog, "fx_count")
	qa := waitProgramActive(t, AttachActiveProbe{
		AttachedMap: mapIDFx, AttachedWeight: weights[0],
		FireOne: func() { workload.Unlink(1) },
	})
	t.Logf("post-attach activation: probes=%d, lostProbes=%d, latency=%s", qa.Probes, qa.LostProbes, qa.Latency)

	const events = 5
	workload.Unlink(events)
	want := uint64(events+qa.EventsCounted) * weights[0]
	got := readArrayCounterByID(t, mapIDFx)
	t.Logf("fexit: events=%d weight=%d want=%d got=%d", events+qa.EventsCounted, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"fexit counter should equal events(%d) * weight(%d) = %d", events+qa.EventsCounted, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	waitDetachQuiescent(t, QuiescenceProbe{
		DetachedMap:    mapIDByName(t, prog, "fx_count"),
		DetachedWeight: weights[0],
		FireOne:        func() { workload.Unlink(1) },
	})

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgTC_ChainStopsAtOK_DefaultProceedOn proves the
// negative half of the TC dispatcher's default proceed-on
// contract: a program returning TC_ACT_OK terminates the chain
// (OK is deliberately excluded from the default
// `pipe | dispatcher_return` bitmask), and the dispatcher honours
// that. The middle position runs and stops the chain; the program
// at the next priority never executes. Companion to
// AllProceed_DefaultProceedOn (positive half: PIPE chains).
func TestMultiProgTC_ChainStopsAtOK_DefaultProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const events = 5
	type plan struct {
		name         string // program name (the .bpf.c uses mtc_chain_*)
		mapName      string
		weightGlobal string
		priority     int
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{name: "mtc_chain_a", mapName: "mca_count", weightGlobal: "weight_a", priority: 50, weight: weights[0], expectEvents: events}, // ran, continued
		{name: "mtc_chain_b", mapName: "mcb_count", weightGlobal: "weight_b", priority: 60, weight: weights[1], expectEvents: events}, // ran, STOP (OK excluded)
		{name: "mtc_chain_c", mapName: "mcc_count", weightGlobal: "weight_c", priority: 70, weight: weights[2], expectEvents: 0},      // never ran
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTC, Name: p.name}
		globals[p.weightGlobal] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tc_chain_stops_at_ok.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for %s", plans[i].name)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for %s", plans[i].name)
		require.Equal(t, bpfman.ProgramTypeTC, prog.Record.Load.ProgramType())
		require.Equal(t, plans[i].name, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewTCAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].name)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	veth.PingExact(t, events)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, plans[i].mapName))
		t.Logf("%s: events=%d weight=%d want=%d got=%d", plans[i].name, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"%s should equal events(%d) * weight(%d) = %d",
			plans[i].name, plans[i].expectEvents, plans[i].weight, want)
	}

	for i := range links {
		require.NoError(t, env.Detach(ctx, links[i].ID), "detach %s", plans[i].name)
	}
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestMultiProgTC_AllProceed_CustomProceedOn proves that a custom
// proceed-on bitmask plumbed via WithProceedOn actually changes
// dispatcher behaviour relative to the default. Every program
// returns TC_ACT_OK -- a verdict the default proceed-on
// (`pipe | dispatcher_return`) excludes, which would normally stop
// the chain at the first program. Each program is attached with
// WithProceedOn=[OK, DispatcherReturn], explicitly including OK,
// and every counter advances. Companion to
// TestMultiProgTC_ChainStopsAtPipe_CustomProceedOn.
func TestMultiProgTC_AllProceed_CustomProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const (
		tcActOK   int32 = 0  // chain-continue when in proceed-on
		dispRet   int32 = 30 // bpfman dispatcher-return sentinel
		eventsPer       = 5
	)

	// Three TC programs sharing one veth ingress at distinct
	// priorities. Each row is the complete description of that
	// program's role in the chain.
	type plan struct {
		suffix       string // 'a', 'b', 'c'
		priority     int
		weight       uint64
		verdict      int32
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], verdict: tcActOK, expectEvents: eventsPer},
		{suffix: "b", priority: 60, weight: weights[1], verdict: tcActOK, expectEvents: eventsPer},
		{suffix: "c", priority: 70, weight: weights[2], verdict: tcActOK, expectEvents: eventsPer},
	}
	customProceedOn := []int32{tcActOK, dispRet}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTC, Name: "mtcv_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
		globals["verdict_"+p.suffix] = uint32LE(uint32(p.verdict))
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tc_param_verdict.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for _, prog := range programs {
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	for i, prog := range programs {
		spec, err := bpfman.NewTCAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority).WithProceedOn(customProceedOn)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].suffix)
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	veth.PingExact(t, eventsPer)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mtcv_"+plans[i].suffix+"_count"))
		t.Logf("mtcv_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mtcv_%s should equal events(%d) * weight(%d) = %d (custom proceed-on includes OK)",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}
}

// TestMultiProgTC_ChainStopsAtPipe_CustomProceedOn proves the
// negative half of the WithProceedOn knob. Outer programs return
// TC_ACT_OK (which the default would stop on; custom includes OK
// so they proceed). The middle program returns TC_ACT_PIPE -- which
// the default proceed-on would have permitted, but our custom
// bitmask explicitly excludes. Position 0 runs and continues;
// position 1 runs and stops the chain; position 2 never runs. The
// chain behaviour is the inverse of what the default would produce
// from the same return verdicts -- which only holds if the custom
// bitmask actually reached the dispatcher.
func TestMultiProgTC_ChainStopsAtPipe_CustomProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const (
		tcActOK   int32 = 0  // chain-continue when in proceed-on
		tcActPipe int32 = 3  // chain-continue under default; excluded here
		dispRet   int32 = 30 // bpfman dispatcher-return sentinel
		eventsPer       = 5
	)

	type plan struct {
		suffix       string
		priority     int
		weight       uint64
		verdict      int32
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], verdict: tcActOK, expectEvents: eventsPer},   // ran, continued
		{suffix: "b", priority: 60, weight: weights[1], verdict: tcActPipe, expectEvents: eventsPer}, // ran, STOP (PIPE excluded)
		{suffix: "c", priority: 70, weight: weights[2], verdict: tcActOK, expectEvents: 0},           // never ran
	}
	customProceedOn := []int32{tcActOK, dispRet} // excludes PIPE

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTC, Name: "mtcv_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
		globals["verdict_"+p.suffix] = uint32LE(uint32(p.verdict))
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tc_param_verdict.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for _, prog := range programs {
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	for i, prog := range programs {
		spec, err := bpfman.NewTCAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority).WithProceedOn(customProceedOn)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].suffix)
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	veth.PingExact(t, eventsPer)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mtcv_"+plans[i].suffix+"_count"))
		t.Logf("mtcv_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mtcv_%s should equal events(%d) * weight(%d) = %d (custom proceed-on excludes PIPE)",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}
}

// TestMultiProgTC_AllProceed_DefaultProceedOn proves that under
// the TC dispatcher's default proceed-on bitmask
// (`pipe | dispatcher_return`), every program returning the
// chain-continuation verdict (TC_ACT_PIPE) sees every packet, and
// that detaching one link from the dispatcher chain stops only
// that program. Companion to ChainStopsAtOK_DefaultProceedOn
// (negative half: middle program returns OK, dispatcher stops).
func TestMultiProgTC_AllProceed_DefaultProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		priority     int
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", priority: 60, weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", priority: 70, weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTC, Name: "mtc_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tc_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mtc_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mtc_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeTC, prog.Record.Load.ProgramType())
		require.Equal(t, "mtc_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mtc_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewTCAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mtc_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindTC, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c. Wave 2: a, b -> detach b. Wave 3: a.
	veth.PingExact(t, eventsPerWave)
	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mtc_%s", plans[2].suffix)
	veth.PingExact(t, eventsPerWave)
	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mtc_%s", plans[1].suffix)
	veth.PingExact(t, eventsPerWave)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mtc_"+plans[i].suffix+"_count"))
		t.Logf("mtc_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mtc_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mtc_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestTC_LoadAttachDetachUnload tests the full lifecycle of a TC program.
// TC programs use dispatchers for multi-program support.
func TestTC_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireTC(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/tc_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeTC,
			Name: "stats",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"weight": uint64LE(weights[0]),
		},
	})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeTC, prog.Record.Load.ProgramType())
	// TC programs are loaded as BPF_PROG_TYPE_EXT targeting a test
	// dispatcher, so the kernel reports "extension" not "schedcls".
	require.Equal(t, kernel.ProgramType("extension"), prog.Status.Kernel.ProgramType)

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
	tcSpec, err := bpfman.NewTCAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
	require.NoError(t, err)
	tcSpec = tcSpec.WithPriority(50)
	link, err := env.Attach(ctx, tcSpec)
	require.NoError(t, err)

	// Then: link has expected properties
	require.NotZero(t, link.ID, "kernel should assign link ID")
	require.Equal(t, bpfman.LinkKindTC, link.Kind)

	// Verify tc filter is visible to tc(8) tooling.
	// The dispatcher is attached as a legacy netlink BPF filter with pref 50.
	filterCount := tcFilterCount(t, veth.A.Name, "ingress")
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
	require.Equal(t, veth.A.Name, tcDetails.Interface)
	require.Equal(t, uint32(veth.A.Ifindex), tcDetails.Ifindex)
	require.Equal(t, bpfman.TCDirectionIngress, tcDetails.Direction)
	require.Equal(t, int32(50), tcDetails.Priority)
	require.NotZero(t, tcDetails.DispatcherID, "TC should use dispatcher")
	require.NotZero(t, tcDetails.Revision, "dispatcher should have revision")

	// Verify TC ingress filters exist on the interface via netlink
	filters := tcIngressFilters(t, veth.A.Name)
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

	// Behavioural validation: send a known number of pings; the BPF
	// program filters to IPv4 ICMP echo requests so ARP/ND noise is
	// ignored, and counts events * weight exactly.
	const events = 5
	veth.PingExact(t, events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "tc_count"))
	t.Logf("tc: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"tc counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	assertCounterQuiet(t, prog, "tc_count", func() { veth.PingExact(t, events) })

	// Verify tc filter has been removed by the detach
	filterCountAfter := tcFilterCount(t, veth.A.Name, "ingress")
	require.Equal(t, 0, filterCountAfter, "tc filter should be removed after detach")

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")

	// Verify TC ingress filters are removed after detach/unload
	filtersAfter := tcIngressFilters(t, veth.A.Name)
	require.Empty(t, filtersAfter, "expected no TC ingress filters after detach/unload")
}

// TestMultiProgTCX_ChainStopsAtOK_DefaultProceedOn proves that a
// TCX program returning TC_ACT_OK terminates the kernel's native
// TCX chain at that point. TCX shares TC's verdict numbering for
// terminal codes; the chain-continuation verdict is TC_ACT_UNSPEC
// (TCX_NEXT), and OK is honoured as "accept and stop" just as it
// is in TC. Companion to AllProceed_DefaultProceedOn (positive
// half: UNSPEC chains).
func TestMultiProgTCX_ChainStopsAtOK_DefaultProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelVersion(t, 6, 6)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const events = 5
	type plan struct {
		name         string
		mapName      string
		weightGlobal string
		priority     int
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{name: "mtcx_chain_a", mapName: "mxca_count", weightGlobal: "weight_a", priority: 50, weight: weights[0], expectEvents: events}, // ran, continued
		{name: "mtcx_chain_b", mapName: "mxcb_count", weightGlobal: "weight_b", priority: 60, weight: weights[1], expectEvents: events}, // ran, STOP (OK)
		{name: "mtcx_chain_c", mapName: "mxcc_count", weightGlobal: "weight_c", priority: 70, weight: weights[2], expectEvents: 0},      // never ran
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTCX, Name: p.name}
		globals[p.weightGlobal] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tcx_chain_stops_at_ok.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for %s", plans[i].name)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for %s", plans[i].name)
		require.Equal(t, bpfman.ProgramTypeTCX, prog.Record.Load.ProgramType())
		require.Equal(t, plans[i].name, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewTCXAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].name)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	veth.PingExact(t, events)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, plans[i].mapName))
		t.Logf("%s: events=%d weight=%d want=%d got=%d", plans[i].name, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"%s should equal events(%d) * weight(%d) = %d",
			plans[i].name, plans[i].expectEvents, plans[i].weight, want)
	}

	for i := range links {
		require.NoError(t, env.Detach(ctx, links[i].ID), "detach %s", plans[i].name)
	}
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestMultiProgTCX_AllProceed_DefaultProceedOn proves that under
// the kernel's native TCX chain (no bpfman dispatcher), every
// program returning the chain-continuation verdict (TC_ACT_UNSPEC,
// aliased as TCX_NEXT) sees every packet, and that detaching one
// link from the chain stops only that program. Companion to
// ChainStopsAtOK_DefaultProceedOn (negative half: middle returns
// OK, kernel terminates the chain).
func TestMultiProgTCX_AllProceed_DefaultProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelVersion(t, 6, 6)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		priority     int
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", priority: 60, weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", priority: 70, weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeTCX, Name: "mtcx_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_tcx_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mtcx_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mtcx_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeTCX, prog.Record.Load.ProgramType())
		require.Equal(t, "mtcx_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mtcx_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewTCXAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mtcx_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindTCX, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c. Wave 2: a, b -> detach b. Wave 3: a.
	veth.PingExact(t, eventsPerWave)
	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mtcx_%s", plans[2].suffix)
	veth.PingExact(t, eventsPerWave)
	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mtcx_%s", plans[1].suffix)
	veth.PingExact(t, eventsPerWave)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mtcx_"+plans[i].suffix+"_count"))
		t.Logf("mtcx_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mtcx_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mtcx_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestTCX_LoadAttachDetachUnload tests the full lifecycle of a TCX program.
// TCX requires kernel 6.6+ and uses native multi-program support.
func TestTCX_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireKernelVersion(t, 6, 6)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	// When: load from local file
	weights := uniqueWeights(t, 1)

	programs, err := env.LoadFile(ctx, "testdata/bpf/tcx_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeTCX,
			Name: "tcx_stats",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"weight": uint64LE(weights[0]),
		},
	})
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
	require.Equal(t, "tcx_stats", gotProg.Record.Meta.Name)
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
	require.Equal(t, "tcx_stats", listedProgs[0].Record.Meta.Name)
	require.NotEmpty(t, listedProgs[0].Record.Handles.PinPath)

	// When: attach via client to test interface
	tcxSpec, err := bpfman.NewTCXAttachSpec(prog.Status.Kernel.ID, veth.A.Name, bpfman.TCDirectionIngress)
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
	require.Equal(t, veth.A.Name, tcxDetails.Interface)
	require.Equal(t, uint32(veth.A.Ifindex), tcxDetails.Ifindex)
	require.Equal(t, bpfman.TCDirectionIngress, tcxDetails.Direction)
	require.Equal(t, int32(50), tcxDetails.Priority)
	// TCX uses native kernel multi-prog support, not dispatchers

	// Round-trip: ListLinks should include our link
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// Behavioural validation: send a known number of pings; the BPF
	// program filters to IPv4 ICMP echo requests so ARP/ND noise is
	// ignored, and counts events * weight exactly.
	const events = 5
	veth.PingExact(t, events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "tcx_count"))
	t.Logf("tcx: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"tcx counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	assertCounterQuiet(t, prog, "tcx_count", func() { veth.PingExact(t, events) })

	// When: unload
	err = env.Unload(ctx, prog.Status.Kernel.ID)
	require.NoError(t, err)

	// Then: clean state, and Get should return error
	env.AssertCleanState()
	_, err = env.Get(ctx, prog.Status.Kernel.ID)
	require.Error(t, err, "Get should fail after unload")
}

// TestMultiProgXDP_ChainStopsAtDrop_DefaultProceedOn proves the
// negative half of the XDP dispatcher's default proceed-on
// contract: with proceed-on `[XDP_PASS]`, a program returning
// XDP_DROP is not in the proceed-on set, so the chain terminates
// at that program. The DROP also drops the packet at A's ingress,
// so PingExpectDrop tolerates 100% reply loss. Companion to
// AllProceed_DefaultProceedOn (positive half: PASS chains).
func TestMultiProgXDP_ChainStopsAtDrop_DefaultProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const events = 5
	type plan struct {
		name         string
		mapName      string
		weightGlobal string
		priority     int
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{name: "mxdp_chain_a", mapName: "mxda_count", weightGlobal: "weight_a", priority: 50, weight: weights[0], expectEvents: events}, // ran, continued
		{name: "mxdp_chain_b", mapName: "mxdb_count", weightGlobal: "weight_b", priority: 60, weight: weights[1], expectEvents: events}, // ran, STOP (DROP)
		{name: "mxdp_chain_c", mapName: "mxdc_count", weightGlobal: "weight_c", priority: 70, weight: weights[2], expectEvents: 0},      // never ran
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeXDP, Name: p.name}
		globals[p.weightGlobal] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_xdp_chain_stops_at_drop.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for %s", plans[i].name)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for %s", plans[i].name)
		require.Equal(t, bpfman.ProgramTypeXDP, prog.Record.Load.ProgramType())
		require.Equal(t, plans[i].name, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, veth.A.Name)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].name)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	veth.PingExpectDrop(t, events)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, plans[i].mapName))
		t.Logf("%s: events=%d weight=%d want=%d got=%d", plans[i].name, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"%s should equal events(%d) * weight(%d) = %d",
			plans[i].name, plans[i].expectEvents, plans[i].weight, want)
	}

	for i := range links {
		require.NoError(t, env.Detach(ctx, links[i].ID), "detach %s", plans[i].name)
	}
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestMultiProgXDP_AllProceed_CustomProceedOn proves that a custom
// proceed-on bitmask plumbed via WithProceedOn actually changes
// XDP dispatcher behaviour. Every program returns XDP_DROP -- a
// verdict the default proceed-on `[XDP_PASS]` excludes, which
// would normally stop the chain at the first program. Each
// program is attached with WithProceedOn=[XDP_DROP], explicitly
// including DROP; every counter advances.
//
// Side effect: the dispatcher returns whatever the chain's last
// program returned, and the last program returns XDP_DROP, so the
// kernel drops the packet and userspace ping reports loss. Use
// PingExpectDrop. Companion to ChainStopsAtPass_CustomProceedOn.
func TestMultiProgXDP_AllProceed_CustomProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const (
		xdpPass   int32 = 2 // chain-continue under default
		xdpDrop   int32 = 1 // chain-continue here, packet dropped
		eventsPer       = 5
	)

	type plan struct {
		suffix       string
		priority     int
		weight       uint64
		verdict      int32
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], verdict: xdpDrop, expectEvents: eventsPer},
		{suffix: "b", priority: 60, weight: weights[1], verdict: xdpDrop, expectEvents: eventsPer},
		{suffix: "c", priority: 70, weight: weights[2], verdict: xdpDrop, expectEvents: eventsPer},
	}
	customProceedOn := []int32{xdpDrop}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeXDP, Name: "mxdv_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
		globals["verdict_"+p.suffix] = uint32LE(uint32(p.verdict))
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_xdp_param_verdict.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for _, prog := range programs {
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	for i, prog := range programs {
		spec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, veth.A.Name)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority).WithProceedOn(customProceedOn)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].suffix)
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	veth.PingExpectDrop(t, eventsPer)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mxdv_"+plans[i].suffix+"_count"))
		t.Logf("mxdv_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mxdv_%s should equal events(%d) * weight(%d) = %d (custom proceed-on includes DROP)",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}
}

// TestMultiProgXDP_ChainStopsAtPass_CustomProceedOn proves the
// negative half of the WithProceedOn knob for XDP. Outer programs
// return XDP_DROP (which the default would stop on; custom
// includes DROP so they proceed). The middle program returns
// XDP_PASS -- which the default proceed-on permits, but our
// custom bitmask explicitly excludes. Position 0 runs and
// continues; position 1 runs and stops the chain; position 2
// never runs. The middle program's PASS also tells the kernel
// to deliver the packet, so PingExact works (no drop).
func TestMultiProgXDP_ChainStopsAtPass_CustomProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const (
		xdpPass   int32 = 2 // chain-continue under default; excluded here
		xdpDrop   int32 = 1 // chain-continue under custom
		eventsPer       = 5
	)

	type plan struct {
		suffix       string
		priority     int
		weight       uint64
		verdict      int32
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], verdict: xdpDrop, expectEvents: eventsPer}, // ran, continued
		{suffix: "b", priority: 60, weight: weights[1], verdict: xdpPass, expectEvents: eventsPer}, // ran, STOP (PASS excluded)
		{suffix: "c", priority: 70, weight: weights[2], verdict: xdpDrop, expectEvents: 0},         // never ran
	}
	customProceedOn := []int32{xdpDrop} // excludes PASS

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeXDP, Name: "mxdv_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
		globals["verdict_"+p.suffix] = uint32LE(uint32(p.verdict))
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_xdp_param_verdict.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for _, prog := range programs {
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	for i, prog := range programs {
		spec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, veth.A.Name)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority).WithProceedOn(customProceedOn)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach %s", plans[i].suffix)
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Middle returns PASS -> kernel delivers packet -> ping replies arrive.
	veth.PingExact(t, eventsPer)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mxdv_"+plans[i].suffix+"_count"))
		t.Logf("mxdv_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mxdv_%s should equal events(%d) * weight(%d) = %d (custom proceed-on excludes PASS)",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}
}

// TestMultiProgXDP_AllProceed_DefaultProceedOn proves that under
// the XDP dispatcher's default proceed-on bitmask `[XDP_PASS]`,
// every program returning the chain-continuation verdict
// (XDP_PASS) sees every packet, and that detaching one link from
// the dispatcher chain stops only that program. Companion to
// ChainStopsAtDrop_DefaultProceedOn (negative half: middle returns
// DROP, dispatcher stops).
func TestMultiProgXDP_AllProceed_DefaultProceedOn(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()
	env.AssertCleanState()

	const eventsPerWave = 5
	type plan struct {
		suffix       string
		priority     int
		weight       uint64
		expectEvents uint64
	}
	weights := uniqueWeights(t, 3)
	plans := []plan{
		{suffix: "a", priority: 50, weight: weights[0], expectEvents: 3 * eventsPerWave},
		{suffix: "b", priority: 60, weight: weights[1], expectEvents: 2 * eventsPerWave},
		{suffix: "c", priority: 70, weight: weights[2], expectEvents: 1 * eventsPerWave},
	}

	specs := make([]manager.ProgramSpec, len(plans))
	globals := map[string][]byte{}
	for i, p := range plans {
		specs[i] = manager.ProgramSpec{Type: bpfman.ProgramTypeXDP, Name: "mxdp_" + p.suffix}
		globals["weight_"+p.suffix] = uint64LE(p.weight)
	}

	programs, err := env.LoadFile(ctx, "testdata/bpf/multi_prog_xdp_counter.bpf.o", specs, manager.LoadOpts{
		GlobalData: globals,
	})
	require.NoError(t, err)
	require.Len(t, programs, len(plans))
	for i, prog := range programs {
		require.NotNil(t, prog.Status.Kernel, "kernel info present for mxdp_%s", plans[i].suffix)
		require.NotZero(t, prog.Status.Kernel.ID, "kernel program ID for mxdp_%s", plans[i].suffix)
		require.Equal(t, bpfman.ProgramTypeXDP, prog.Record.Load.ProgramType())
		require.Equal(t, "mxdp_"+plans[i].suffix, prog.Record.Meta.Name)
		t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })
	}

	counterIDs := make(map[kernel.MapID]struct{}, len(plans))
	for i, prog := range programs {
		id := mapIDByName(t, prog, "mxdp_"+plans[i].suffix+"_count")
		_, dup := counterIDs[id]
		require.False(t, dup, "counter map ID %d shared between programs", id)
		counterIDs[id] = struct{}{}
	}

	links := make([]bpfman.LinkRecord, len(plans))
	for i, prog := range programs {
		spec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, veth.A.Name)
		require.NoError(t, err)
		spec = spec.WithPriority(plans[i].priority)
		link, err := env.Attach(ctx, spec)
		require.NoError(t, err, "attach mxdp_%s", plans[i].suffix)
		require.Equal(t, bpfman.LinkKindXDP, link.Kind)
		links[i] = link
		t.Cleanup(func() { env.Detach(context.Background(), link.ID) })
	}

	// Wave 1: a, b, c -> detach c. Wave 2: a, b -> detach b. Wave 3: a.
	veth.PingExact(t, eventsPerWave)
	require.NoError(t, env.Detach(ctx, links[2].ID), "detach mxdp_%s", plans[2].suffix)
	veth.PingExact(t, eventsPerWave)
	require.NoError(t, env.Detach(ctx, links[1].ID), "detach mxdp_%s", plans[1].suffix)
	veth.PingExact(t, eventsPerWave)

	for i, prog := range programs {
		want := plans[i].expectEvents * plans[i].weight
		got := readArrayCounterByID(t, mapIDByName(t, prog, "mxdp_"+plans[i].suffix+"_count"))
		t.Logf("mxdp_%s: events=%d weight=%d want=%d got=%d", plans[i].suffix, plans[i].expectEvents, plans[i].weight, want, got)
		requireCounterEqual(t, want, got,
			"mxdp_%s should equal events(%d) * weight(%d) = %d after staggered detach",
			plans[i].suffix, plans[i].expectEvents, plans[i].weight, want)
	}

	require.NoError(t, env.Detach(ctx, links[0].ID), "detach mxdp_%s", plans[0].suffix)
	env.AssertLinkCount(0)
	for _, prog := range programs {
		require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID), "unload")
	}
	env.AssertCleanState()
}

// TestXDP_LoadAttachDetachUnload tests the full lifecycle of an XDP program.
// XDP programs use dispatchers for multi-program support.
func TestXDP_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)

	env := NewTestEnv(t)
	veth := testnet.NewTestVethPair(t)
	ctx := context.Background()

	// Given: clean state
	env.AssertCleanState()

	weights := uniqueWeights(t, 1)

	// When: load from local file
	programs, err := env.LoadFile(ctx, "testdata/bpf/xdp_exact.bpf.o", []manager.ProgramSpec{
		{
			Type: bpfman.ProgramTypeXDP,
			Name: "pass",
		},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"weight": uint64LE(weights[0]),
		},
	})
	require.NoError(t, err)
	require.Len(t, programs, 1)

	prog := programs[0]

	// Then: program has expected properties
	require.NotNil(t, prog.Status.Kernel, "kernel info should be present")
	require.NotZero(t, prog.Status.Kernel.ID, "kernel should assign program ID")
	require.Equal(t, bpfman.ProgramTypeXDP, prog.Record.Load.ProgramType())
	// XDP programs are loaded as BPF_PROG_TYPE_EXT targeting a test
	// dispatcher, so the kernel reports "extension" not "xdp".
	require.Equal(t, kernel.ProgramType("extension"), prog.Status.Kernel.ProgramType)

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
	xdpSpec, err := bpfman.NewXDPAttachSpec(prog.Status.Kernel.ID, veth.A.Name)
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
	require.Equal(t, veth.A.Name, xdpDetails.Interface)
	require.Equal(t, uint32(veth.A.Ifindex), xdpDetails.Ifindex)
	require.NotZero(t, xdpDetails.DispatcherID, "XDP should use dispatcher")
	require.NotZero(t, xdpDetails.Revision, "dispatcher should have revision")

	// Round-trip: ListLinks should include our link
	// Note: XDP uses dispatchers, so ProgramID is the dispatcher's program ID.
	listedLinks, err := env.ListLinks(ctx)
	require.NoError(t, err)
	require.Len(t, listedLinks, 1)
	require.NotZero(t, listedLinks[0].ID, "should have kernel link ID")
	require.Equal(t, link.Kind, listedLinks[0].Kind)

	// Behavioural validation: send a known number of pings; the BPF
	// program filters to IPv4 ICMP echo requests so ARP/ND noise is
	// ignored, and counts events * weight exactly.
	const events = 5
	veth.PingExact(t, events)
	want := uint64(events) * weights[0]
	got := readArrayCounterByID(t, mapIDByName(t, prog, "xdp_count"))
	t.Logf("xdp: events=%d weight=%d want=%d got=%d", events, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"xdp counter should equal events(%d) * weight(%d) = %d", events, weights[0], want)

	// When: detach
	err = env.Detach(ctx, link.ID)
	require.NoError(t, err)

	// Then: no links, and GetLink should return error
	env.AssertLinkCount(0)
	_, _, err = env.GetLink(ctx, link.ID)
	require.Error(t, err, "GetLink should fail after detach")

	// Then: detach actually stopped the BPF program firing.
	assertCounterQuiet(t, prog, "xdp_count", func() { veth.PingExact(t, events) })

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

	// When: load from local file with metadata and global data
	programs, err := env.LoadFile(ctx, "testdata/bpf/xdp_pass_pinned.bpf.o", []manager.ProgramSpec{
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
// The target is the running e2e.test binary itself, with the cgo'd
// e2e_uprobe_call_malloc symbol as the attach point. Avoids any
// dependency on locating the correct libc path (which breaks on
// NixOS, Guix, musl, and other non-standard layouts) and removes
// the need for a separate helper binary on disk.
func uprobeTarget() (target, fnName string) {
	return selfExe, "e2e_uprobe_call_malloc"
}

// fireUprobe execs the running e2e.test binary in helper mode so
// e2e_uprobe_call_malloc runs in the child, firing the kernel uprobe
// (or uretprobe) attached to the same inode + symbol offset.
func fireUprobe() error {
	cmd := exec.Command(selfExe)
	cmd.Env = append(os.Environ(), e2eModeEnv+"="+e2eModeUprobeTriggerCallMalloc)
	return cmd.Run()
}

// TestFexit_KmodSlot_LoadAttachDetachUnload is the kmod-targeting
// counterpart of TestFexit_LoadAttachDetachUnload. Instead of
// attaching to the public do_unlinkat kernel function -- which
// every fentry/fexit test in the suite (and any host-side BPF
// tooling) shares, forcing trampoline-rebuild contention -- it
// claims a private slot in the bpfman_e2e_targets kernel module
// and attaches there. No other test or process drives that slot's
// function, so the rebuild window is private and exact-equality
// holds without the suite-wide do_unlinkat mutex.
//
// Skipped if the bpfman_e2e_targets module is not loaded.
func TestFexit_KmodSlot_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKmodTargets(t)

	slot := acquireKmodSlot(t)
	t.Logf("kmod slot: index=%d func=%s", slot.Index, slot.Func)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	weights := uniqueWeights(t, 1)
	programs, err := env.LoadFile(ctx, "testdata/bpf/fexit_kmod_exact.bpf.o", []manager.ProgramSpec{
		{Name: "test_fexit", Type: bpfman.ProgramTypeFexit, AttachFunc: slot.Func},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"weight": uint64LE(weights[0]),
		},
	})
	require.NoError(t, err)
	require.Len(t, programs, 1)
	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	fxSpec, err := bpfman.NewFexitAttachSpec(prog.Status.Kernel.ID)
	require.NoError(t, err)
	link, err := env.Attach(ctx, fxSpec)
	require.NoError(t, err)
	require.Equal(t, bpfman.LinkKindFexit, link.Kind)
	t.Cleanup(func() { env.Detach(context.Background(), link.ID) })

	// Wait for attach to be live before counting. On a private
	// slot the trampoline rebuild has no contending writers, so
	// the warm-up typically settles on the first probe.
	mapIDFx := mapIDByName(t, prog, "fx_count")
	qa := waitProgramActive(t, AttachActiveProbe{
		AttachedMap: mapIDFx, AttachedWeight: weights[0],
		FireOne: func() { slot.Fire(t, 1) },
	})
	t.Logf("post-attach activation: probes=%d, lostProbes=%d, latency=%s",
		qa.Probes, qa.LostProbes, qa.Latency)

	const events = 5
	slot.Fire(t, events)
	want := uint64(events+qa.EventsCounted) * weights[0]
	got := readArrayCounterByID(t, mapIDFx)
	t.Logf("fexit: events=%d weight=%d want=%d got=%d",
		events+qa.EventsCounted, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"fexit counter on private slot %s should equal events(%d) * weight(%d) = %d",
		slot.Func, events+qa.EventsCounted, weights[0], want)

	require.NoError(t, env.Detach(ctx, link.ID))
	env.AssertLinkCount(0)
	require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID))
	env.AssertCleanState()
}

// TestFentry_KmodSlot_LoadAttachDetachUnload is the kmod-targeting
// counterpart of TestFentry_LoadAttachDetachUnload. See the fexit
// variant for rationale.
func TestFentry_KmodSlot_LoadAttachDetachUnload(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKmodTargets(t)

	slot := acquireKmodSlot(t)
	t.Logf("kmod slot: index=%d func=%s", slot.Index, slot.Func)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	weights := uniqueWeights(t, 1)
	programs, err := env.LoadFile(ctx, "testdata/bpf/fentry_kmod_exact.bpf.o", []manager.ProgramSpec{
		{Name: "test_fentry", Type: bpfman.ProgramTypeFentry, AttachFunc: slot.Func},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"weight": uint64LE(weights[0]),
		},
	})
	require.NoError(t, err)
	require.Len(t, programs, 1)
	prog := programs[0]
	t.Cleanup(func() { env.Unload(context.Background(), prog.Status.Kernel.ID) })

	feSpec, err := bpfman.NewFentryAttachSpec(prog.Status.Kernel.ID)
	require.NoError(t, err)
	link, err := env.Attach(ctx, feSpec)
	require.NoError(t, err)
	require.Equal(t, bpfman.LinkKindFentry, link.Kind)
	t.Cleanup(func() { env.Detach(context.Background(), link.ID) })

	mapIDFe := mapIDByName(t, prog, "fe_count")
	qa := waitProgramActive(t, AttachActiveProbe{
		AttachedMap: mapIDFe, AttachedWeight: weights[0],
		FireOne: func() { slot.Fire(t, 1) },
	})
	t.Logf("post-attach activation: probes=%d, lostProbes=%d, latency=%s",
		qa.Probes, qa.LostProbes, qa.Latency)

	const events = 5
	slot.Fire(t, events)
	want := uint64(events+qa.EventsCounted) * weights[0]
	got := readArrayCounterByID(t, mapIDFe)
	t.Logf("fentry: events=%d weight=%d want=%d got=%d",
		events+qa.EventsCounted, weights[0], want, got)
	requireCounterEqual(t, want, got,
		"fentry counter on private slot %s should equal events(%d) * weight(%d) = %d",
		slot.Func, events+qa.EventsCounted, weights[0], want)

	require.NoError(t, env.Detach(ctx, link.ID))
	env.AssertLinkCount(0)
	require.NoError(t, env.Unload(ctx, prog.Status.Kernel.ID))
	env.AssertCleanState()
}

// TestLoad_FentryFexit_TypeMismatchFailsLoudly verifies that
// bpfman rejects a load where the caller-specified ProgramType
// disagrees with the .bpf.o's SEC-inferred type for fentry/fexit
// programs. The kernel binds fentry vs fexit at load time via
// expected_attach_type (different verifier rules, retval access
// rules, and trampoline shape), so silently loading the program
// according to SEC while bpfman records the caller's intent
// would be a long-tail bug source. The latent version of this
// only surfaced by accident -- when we wrote the kmod-targeting
// tests with a SEC placeholder different from the runtime
// AttachFunc -- so we lock the invariant in with an explicit
// test.
func TestLoad_FentryFexit_TypeMismatchFailsLoudly(t *testing.T) {
	t.Parallel()
	RequireRoot(t)
	RequireBTF(t)
	RequireKmodTargets(t)

	slot := acquireKmodSlot(t)

	env := NewTestEnv(t)
	ctx := context.Background()
	env.AssertCleanState()

	// fentry_kmod_exact.bpf.o has SEC("fentry/bpfman_e2e_target_0").
	// Asking for Fexit must error before any kernel state is
	// created; AssertCleanState afterwards catches any leak.
	_, err := env.LoadFile(ctx, "testdata/bpf/fentry_kmod_exact.bpf.o", []manager.ProgramSpec{
		{Name: "test_fentry", Type: bpfman.ProgramTypeFexit, AttachFunc: slot.Func},
	}, manager.LoadOpts{
		GlobalData: map[string][]byte{
			"weight": uint64LE(1),
		},
	})
	require.Error(t, err, "load with type=Fexit against fentry SEC should fail")
	require.Contains(t, err.Error(), "program type mismatch",
		"error should name the mismatch explicitly; got: %v", err)
	env.AssertCleanState()
}
