package manager

import (
	"testing"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
	"github.com/frobware/go-bpfman/manager/coherency"
	"github.com/frobware/go-bpfman/platform"
)

// testDispSummary creates a DispatcherSummary for testing.
func testDispSummary(dt dispatcher.DispatcherType, nsid uint64, ifindex uint32, programID kernel.ProgramID) platform.DispatcherSummary {
	return platform.DispatcherSummary{
		Key:     dispatcher.Key{Type: dt, Nsid: nsid, Ifindex: ifindex},
		Runtime: platform.DispatcherRuntime{ProgramID: programID},
	}
}

// testProgramRecord returns a minimal ProgramRecord for testing.
func testProgramRecord() bpfman.ProgramRecord {
	return bpfman.ProgramRecord{
		Load: bpfman.LoadSpec{}.
			WithObjectPath("/tmp/test.o").
			WithProgramName("test_prog").
			WithProgramType(bpfman.ProgramTypeTracepoint),
		Handles: bpfman.ProgramHandles{
			PinPath: "/sys/fs/bpf/test",
		},
		Meta: bpfman.ProgramMeta{
			Name: "test_prog",
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func TestComputeStoreGC_EmptySnapshot(t *testing.T) {
	t.Parallel()

	actions := computeStoreGC(
		nil,
		nil,
		nil,
		map[kernel.ProgramID]bool{},
		map[kernel.LinkID]bool{},
	)
	if len(actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(actions))
	}
}

func TestComputeStoreGC_LiveProgramNotDeleted(t *testing.T) {
	t.Parallel()

	programs := map[kernel.ProgramID]bpfman.ProgramRecord{
		100: testProgramRecord(),
	}
	actions := computeStoreGC(
		programs,
		nil,
		nil,
		map[kernel.ProgramID]bool{100: true},
		map[kernel.LinkID]bool{},
	)
	if len(actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(actions))
	}
}

func TestComputeStoreGC_StaleDependentBeforeOwner(t *testing.T) {
	t.Parallel()

	ownerID := kernel.ProgramID(100)
	dep := testProgramRecord()
	dep.Handles.MapOwnerID = &ownerID

	programs := map[kernel.ProgramID]bpfman.ProgramRecord{
		100: testProgramRecord(),
		101: dep,
	}
	actions := computeStoreGC(
		programs,
		nil,
		nil,
		map[kernel.ProgramID]bool{}, // all dead
		map[kernel.LinkID]bool{},
	)

	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}

	// First action must be the dependent (101), second the owner (100).
	first, ok := actions[0].(action.DeleteProgram)
	if !ok {
		t.Fatalf("expected DeleteProgram, got %T", actions[0])
	}
	if first.ProgramID != 101 {
		t.Errorf("expected dependent 101 first, got %d", first.ProgramID)
	}

	second, ok := actions[1].(action.DeleteProgram)
	if !ok {
		t.Fatalf("expected DeleteProgram, got %T", actions[1])
	}
	if second.ProgramID != 100 {
		t.Errorf("expected owner 100 second, got %d", second.ProgramID)
	}
}

func TestComputeStoreGC_StaleDispatcher(t *testing.T) {
	t.Parallel()

	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeXDP, 4026531840, 2, 100),
	}
	actions := computeStoreGC(
		nil,
		dispatchers,
		nil,
		map[kernel.ProgramID]bool{}, // 100 not alive
		map[kernel.LinkID]bool{},
	)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	dd, ok := actions[0].(action.DeleteDispatcher)
	if !ok {
		t.Fatalf("expected DeleteDispatcher, got %T", actions[0])
	}
	if dd.Type != dispatcher.DispatcherTypeXDP || dd.Nsid != 4026531840 || dd.Ifindex != 2 {
		t.Errorf("unexpected DeleteDispatcher fields: %+v", dd)
	}
}

func TestComputeStoreGC_LiveDispatcherNotDeleted(t *testing.T) {
	t.Parallel()

	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeXDP, 4026531840, 2, 100),
	}
	actions := computeStoreGC(
		nil,
		dispatchers,
		nil,
		map[kernel.ProgramID]bool{100: true},
		map[kernel.LinkID]bool{},
	)
	if len(actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(actions))
	}
}

func TestComputeStoreGC_StaleNonSyntheticLink(t *testing.T) {
	t.Parallel()

	links := []bpfman.LinkRecord{
		{
			ID:        200,
			ProgramID: 100,
			Kind:      bpfman.LinkKindTracepoint,
			Details:   bpfman.TracepointDetails{Group: "sched", Name: "switch"},
		},
	}
	actions := computeStoreGC(
		nil,
		nil,
		links,
		map[kernel.ProgramID]bool{100: true},
		map[kernel.LinkID]bool{}, // 200 not alive
	)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	dl, ok := actions[0].(action.DeleteLink)
	if !ok {
		t.Fatalf("expected DeleteLink, got %T", actions[0])
	}
	if dl.LinkID != 200 {
		t.Errorf("expected link 200, got %d", dl.LinkID)
	}
}

func TestComputeStoreGC_SyntheticLinkSkipped(t *testing.T) {
	t.Parallel()

	links := []bpfman.LinkRecord{
		{
			ID:        kernel.LinkID(0x80000001), // synthetic
			ProgramID: 100,
			Kind:      bpfman.LinkKindUprobe,
			Details:   bpfman.UprobeDetails{Target: "/app/bin", FnName: "handler"},
		},
	}
	actions := computeStoreGC(
		nil,
		nil,
		links,
		map[kernel.ProgramID]bool{100: true},
		map[kernel.LinkID]bool{}, // synthetic not in kernel set
	)
	if len(actions) != 0 {
		t.Errorf("expected 0 actions for synthetic link, got %d", len(actions))
	}
}

func TestComputeStoreGC_ExtensionLinkSurvivesWithLiveDispatcher(t *testing.T) {
	t.Parallel()

	// Dispatcher has one XDP extension link. The link's kernel ID
	// is stale (destroyed by a rebuild), but the dispatcher is
	// alive. Both should survive: the extension is still active
	// via the dispatcher.
	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeXDP, 4026531840, 2, 500),
	}
	links := []bpfman.LinkRecord{
		{
			ID:        300,
			ProgramID: 100,
			Kind:      bpfman.LinkKindXDP,
			Details: bpfman.XDPDetails{
				DispatcherID: 500,
				Ifindex:      2,
			},
		},
	}

	actions := computeStoreGC(
		nil,
		dispatchers,
		links,
		map[kernel.ProgramID]bool{500: true}, // dispatcher alive
		map[kernel.LinkID]bool{},             // link 300 stale (rebuilt)
	)

	if len(actions) != 0 {
		t.Errorf("expected 0 actions (extension link survives with live dispatcher), got %d: %+v", len(actions), actions)
	}
}

func TestComputeStoreGC_DispatcherWithSurvivingLinks(t *testing.T) {
	t.Parallel()

	// Dispatcher has two extension links. Both have stale kernel
	// IDs (destroyed by a rebuild), but the dispatcher is alive.
	// Both links should survive because they are managed by the
	// dispatcher lifecycle, not by GC.
	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeXDP, 4026531840, 2, 500),
	}
	links := []bpfman.LinkRecord{
		{
			ID:        300,
			ProgramID: 100,
			Kind:      bpfman.LinkKindXDP,
			Details: bpfman.XDPDetails{
				DispatcherID: 500,
				Ifindex:      2,
			},
		},
		{
			ID:        301,
			ProgramID: 101,
			Kind:      bpfman.LinkKindXDP,
			Details: bpfman.XDPDetails{
				DispatcherID: 500,
				Ifindex:      2,
			},
		},
	}

	actions := computeStoreGC(
		nil,
		dispatchers,
		links,
		map[kernel.ProgramID]bool{500: true},
		map[kernel.LinkID]bool{301: true}, // 300 stale, 301 alive
	)

	// Both extension links reference live dispatcher 500; neither
	// should be deleted. The dispatcher is also not orphaned.
	if len(actions) != 0 {
		t.Errorf("expected 0 actions (extension links survive with live dispatcher), got %d: %+v", len(actions), actions)
	}
}

func TestComputeStoreGC_MixedScenario(t *testing.T) {
	t.Parallel()

	// Stale owner + dependent programs, stale dispatcher, stale link,
	// and an orphaned dispatcher after link GC.
	ownerID := kernel.ProgramID(100)
	dep := testProgramRecord()
	dep.Handles.MapOwnerID = &ownerID

	programs := map[kernel.ProgramID]bpfman.ProgramRecord{
		100: testProgramRecord(), // stale owner
		101: dep,                 // stale dependent
		200: testProgramRecord(), // alive
	}

	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeTCIngress, 4026531840, 3, 100),
		testDispSummary(dispatcher.DispatcherTypeXDP, 4026531840, 2, 500),
	}

	links := []bpfman.LinkRecord{
		{
			ID:        400,
			ProgramID: 200,
			Kind:      bpfman.LinkKindTracepoint,
			Details:   bpfman.TracepointDetails{Group: "sched", Name: "switch"},
		},
		{
			ID:        401,
			ProgramID: 200,
			Kind:      bpfman.LinkKindXDP,
			Details: bpfman.XDPDetails{
				DispatcherID: 500,
				Ifindex:      2,
			},
		},
	}

	actions := computeStoreGC(
		programs,
		dispatchers,
		links,
		map[kernel.ProgramID]bool{200: true, 500: true},
		map[kernel.LinkID]bool{400: true}, // 401 is dead
	)

	// Count by type.
	var delPrograms, delDispatchers, delLinks int
	for _, a := range actions {
		switch a.(type) {
		case action.DeleteProgram:
			delPrograms++
		case action.DeleteDispatcher:
			delDispatchers++
		case action.DeleteLink:
			delLinks++
		}
	}

	if delPrograms != 2 {
		t.Errorf("expected 2 program deletions, got %d", delPrograms)
	}
	if delDispatchers != 1 {
		// Only the stale TC-ingress dispatcher (program 100 dead).
		// XDP dispatcher 500 is alive and its extension link 401
		// survives (live dispatcher), so it is not orphaned.
		t.Errorf("expected 1 dispatcher deletion, got %d", delDispatchers)
	}
	if delLinks != 0 {
		// Link 400 (tracepoint) is alive. Link 401 (XDP extension)
		// has a stale kernel ID but its dispatcher 500 is alive,
		// so it survives.
		t.Errorf("expected 0 link deletions, got %d", delLinks)
	}

	// Verify dependent before owner ordering.
	var deletedProgs []kernel.ProgramID
	for _, a := range actions {
		if dp, ok := a.(action.DeleteProgram); ok {
			deletedProgs = append(deletedProgs, dp.ProgramID)
		}
	}
	if len(deletedProgs) < 2 {
		t.Fatalf("expected at least 2 program deletions, got %d", len(deletedProgs))
	}
	firstProg, secondProg := deletedProgs[0], deletedProgs[1]
	if firstProg != 101 {
		t.Errorf("expected dependent 101 first, got %d", firstProg)
	}
	if secondProg != 100 {
		t.Errorf("expected owner 100 second, got %d", secondProg)
	}
}

// TestComputeStoreGC_ExtensionLinkWithLiveDispatcherSurvives
// verifies that extension links whose dispatcher is alive are not
// deleted by GC, even when their kernel link ID is stale.
//
// Every dispatcher rebuild re-attaches all extensions, creating new
// kernel links. The stored kernel link IDs become stale (the old
// links are gone), but the extension is still active via the
// dispatcher. GC must not delete these links.
func TestComputeStoreGC_ExtensionLinkWithLiveDispatcherSurvives(t *testing.T) {
	t.Parallel()

	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeTCIngress, 4026531840, 2, 500),
	}
	links := []bpfman.LinkRecord{
		{
			ID:        300, // stale kernel link ID (destroyed by rebuild)
			ProgramID: 100,
			Kind:      bpfman.LinkKindTC,
			Details: bpfman.TCDetails{
				DispatcherID: 500, // references the live dispatcher
				Direction:    bpfman.TCDirectionIngress,
				Ifindex:      2,
			},
		},
	}

	actions := computeStoreGC(
		nil,
		dispatchers,
		links,
		map[kernel.ProgramID]bool{500: true}, // dispatcher alive
		map[kernel.LinkID]bool{},             // link 300 NOT in kernel (stale after rebuild)
	)

	// The extension link should survive because its dispatcher is alive.
	// Before the fix, GC would delete link 300 and then orphan the dispatcher.
	if len(actions) != 0 {
		t.Errorf("expected 0 actions (extension link should survive), got %d: %+v", len(actions), actions)
	}
}

// TestComputeStoreGC_ExtensionLinkWithDeadDispatcherDeleted
// verifies that when a dispatcher is dead, phase 2 deletes it via
// DeleteDispatcher (which calls DeleteDispatcherSnapshot, cleaning up
// extension links at the DB level). Phase 3 does not emit DeleteLink
// for dispatcher-backed links; their lifecycle is owned by
// DispatcherStore.
func TestComputeStoreGC_ExtensionLinkWithDeadDispatcherDeleted(t *testing.T) {
	t.Parallel()

	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeXDP, 4026531840, 2, 500),
	}
	links := []bpfman.LinkRecord{
		{
			ID:        300,
			ProgramID: 100,
			Kind:      bpfman.LinkKindXDP,
			Details: bpfman.XDPDetails{
				DispatcherID: 500,
				Ifindex:      2,
			},
		},
	}

	actions := computeStoreGC(
		nil,
		dispatchers,
		links,
		map[kernel.ProgramID]bool{}, // dispatcher 500 is dead
		map[kernel.LinkID]bool{},    // link 300 also dead
	)

	// Dispatcher is dead: Phase 2 deletes it. Extension links are
	// cleaned up by DeleteDispatcherSnapshot at the DB level, not
	// by a separate DeleteLink action.
	var delLinks, delDispatchers int
	for _, a := range actions {
		switch a.(type) {
		case action.DeleteLink:
			delLinks++
		case action.DeleteDispatcher:
			delDispatchers++
		}
	}
	if delDispatchers != 1 {
		t.Errorf("expected 1 dispatcher deletion, got %d", delDispatchers)
	}
	if delLinks != 0 {
		t.Errorf("expected 0 link deletions (dispatcher-backed links cleaned up by DeleteDispatcherSnapshot), got %d", delLinks)
	}
}

func TestComputeStoreGC_TCExtensionSurvivesWithLiveDispatcher(t *testing.T) {
	t.Parallel()

	// TC dispatcher is alive; its extension link has a stale kernel
	// ID (destroyed by a rebuild). The link should survive because
	// the dispatcher is alive. The dispatcher is not orphaned.
	dispatchers := []platform.DispatcherSummary{
		testDispSummary(dispatcher.DispatcherTypeTCIngress, 4026531840, 3, 600),
	}
	links := []bpfman.LinkRecord{
		{
			ID:        500,
			ProgramID: 100,
			Kind:      bpfman.LinkKindTC,
			Details: bpfman.TCDetails{
				DispatcherID: 600,
				Direction:    bpfman.TCDirectionIngress,
				Ifindex:      3,
			},
		},
	}

	actions := computeStoreGC(
		nil,
		dispatchers,
		links,
		map[kernel.ProgramID]bool{600: true},
		map[kernel.LinkID]bool{}, // link 500 stale (rebuilt)
	)

	if len(actions) != 0 {
		t.Errorf("expected 0 actions (TC extension survives with live dispatcher), got %d: %+v", len(actions), actions)
	}
}

// TestGCPlan_IsEmpty covers the matrix that gcOnEntry's lockless
// pre-check relies on. The plan is empty iff there are no store
// actions and no actionable (Intent != nil) coherency violations.
// Diagnostic-only violations (Intent == nil) do not count: they
// surface in audit output but should not pull the writer lock.
func TestGCPlan_IsEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		plan GCPlan
		want bool
	}{
		{
			name: "zero value is empty",
			plan: GCPlan{},
			want: true,
		},
		{
			name: "diagnostic-only violation is empty",
			plan: GCPlan{
				Violations: []coherency.Violation{{
					Severity:    coherency.SeverityWarning,
					RuleName:    "diagnostic-rule",
					Description: "no intent",
					// Intent is nil
				}},
			},
			want: true,
		},
		{
			name: "live orphans without intent are empty",
			plan: GCPlan{LiveOrphans: 5},
			want: true,
		},
		{
			name: "store action makes plan non-empty",
			plan: GCPlan{
				StoreActions: []action.Action{
					action.DeleteProgram{ProgramID: 7},
				},
			},
			want: false,
		},
		{
			name: "actionable violation makes plan non-empty",
			plan: GCPlan{
				Violations: []coherency.Violation{{
					RuleName: "with-intent",
					Intent:   coherency.RemoveOrphanArtefact{Path: "/tmp/x"},
				}},
			},
			want: false,
		},
		{
			name: "mixed actionable and diagnostic non-empty",
			plan: GCPlan{
				Violations: []coherency.Violation{
					{RuleName: "diagnostic"},
					{RuleName: "actionable", Intent: coherency.RemoveOrphanArtefact{Path: "/tmp/y"}},
				},
			},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.plan.IsEmpty(); got != c.want {
				t.Errorf("IsEmpty() = %v, want %v", got, c.want)
			}
		})
	}
}
