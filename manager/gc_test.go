package manager

import (
	"testing"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
)

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
	dispatchers := []dispatcher.State{
		{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      4026531840,
			Ifindex:   2,
			ProgramID: 100,
		},
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
	if dd.Type != "xdp" || dd.Nsid != 4026531840 || dd.Ifindex != 2 {
		t.Errorf("unexpected DeleteDispatcher fields: %+v", dd)
	}
}

func TestComputeStoreGC_LiveDispatcherNotDeleted(t *testing.T) {
	dispatchers := []dispatcher.State{
		{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      4026531840,
			Ifindex:   2,
			ProgramID: 100,
		},
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

func TestComputeStoreGC_OrphanedDispatcherAfterLinkGC(t *testing.T) {
	// Dispatcher has one XDP extension link. That link is stale.
	// After link GC, dispatcher has zero extensions and should be deleted.
	dispatchers := []dispatcher.State{
		{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      4026531840,
			Ifindex:   2,
			ProgramID: 500, // alive in kernel
		},
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
		map[kernel.LinkID]bool{},             // link 300 dead
	)

	// Expect: DeleteLink{300}, DeleteDispatcher{xdp, ...}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d: %+v", len(actions), actions)
	}

	if _, ok := actions[0].(action.DeleteLink); !ok {
		t.Errorf("expected DeleteLink first, got %T", actions[0])
	}
	if _, ok := actions[1].(action.DeleteDispatcher); !ok {
		t.Errorf("expected DeleteDispatcher second, got %T", actions[1])
	}
}

func TestComputeStoreGC_DispatcherWithSurvivingLinks(t *testing.T) {
	// Dispatcher has two extension links. One is stale, one survives.
	// Dispatcher should NOT be deleted.
	dispatchers := []dispatcher.State{
		{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      4026531840,
			Ifindex:   2,
			ProgramID: 500,
		},
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
		map[kernel.LinkID]bool{301: true}, // 300 dead, 301 alive
	)

	// Expect: only DeleteLink{300}, no dispatcher deletion
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d: %+v", len(actions), actions)
	}
	dl, ok := actions[0].(action.DeleteLink)
	if !ok {
		t.Fatalf("expected DeleteLink, got %T", actions[0])
	}
	if dl.LinkID != 300 {
		t.Errorf("expected link 300, got %d", dl.LinkID)
	}
}

func TestComputeStoreGC_MixedScenario(t *testing.T) {
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

	dispatchers := []dispatcher.State{
		{
			Type:      dispatcher.DispatcherTypeTCIngress,
			Nsid:      4026531840,
			Ifindex:   3,
			ProgramID: 100, // stale (program 100 is dead)
		},
		{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      4026531840,
			Ifindex:   2,
			ProgramID: 500, // alive
		},
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
	if delDispatchers != 2 {
		// 1 for stale (kernel 100 dead) + 1 orphaned (kernel 500 has 0 remaining links)
		t.Errorf("expected 2 dispatcher deletions, got %d", delDispatchers)
	}
	if delLinks != 1 {
		t.Errorf("expected 1 link deletion, got %d", delLinks)
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

func TestComputeStoreGC_TCDispatcherOrphaned(t *testing.T) {
	// TC dispatcher whose only extension link dies.
	dispatchers := []dispatcher.State{
		{
			Type:      dispatcher.DispatcherTypeTCIngress,
			Nsid:      4026531840,
			Ifindex:   3,
			ProgramID: 600,
		},
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
		map[kernel.LinkID]bool{}, // link 500 dead
	)

	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d: %+v", len(actions), actions)
	}
	if _, ok := actions[0].(action.DeleteLink); !ok {
		t.Errorf("expected DeleteLink first, got %T", actions[0])
	}
	dd, ok := actions[1].(action.DeleteDispatcher)
	if !ok {
		t.Fatalf("expected DeleteDispatcher second, got %T", actions[1])
	}
	if dd.Type != "tc-ingress" {
		t.Errorf("expected tc-ingress, got %s", dd.Type)
	}
}
