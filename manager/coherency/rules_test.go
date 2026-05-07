package coherency

import (
	"testing"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
)

func newTestState() *ObservedState {
	return &ObservedState{
		kernelAlive:           make(map[kernel.ProgramID]bool),
		fsDispatcherLinkCount: make(map[string]int),
		dbDispatcherExtCount:  make(map[kernel.ProgramID]int),
		tcFilterOK:            make(map[string]bool),
	}
}

func ruleByName(rules []Rule, name string) Rule {
	for _, r := range rules {
		if r.Name == name {
			return r
		}
	}
	panic("rule not found: " + name)
}

func boolPtr(v bool) *bool { return &v }

// assertIntent checks that violations carry exactly the expected
// RepairIntent values in order. Asserting on the typed intent
// (rather than its lowered actions) keeps rule tests focused on
// classification; intent lowering is verified separately in
// intents_test.go.
func assertIntent(t *testing.T, violations []Violation, expected []RepairIntent) {
	t.Helper()
	if len(violations) != len(expected) {
		t.Fatalf("got %d violations, want %d", len(violations), len(expected))
	}
	for i, v := range violations {
		if v.Intent == nil {
			t.Fatalf("violation[%d]: Intent is nil", i)
		}
		if v.Intent != expected[i] {
			t.Errorf("violation[%d].Intent:\n  got:  %#v\n  want: %#v", i, v.Intent, expected[i])
		}
	}
}

// TestAllRules_NamesUnique guards against accidental name collisions
// in the unified rule registry. Duplicates would silently shadow in
// FindRule and double-list in RuleNames; Rules() panics when this
// invariant is violated.
func TestAllRules_NamesUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]int)
	for _, r := range Rules() {
		seen[r.Name]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("rule name %q registered %d times", name, count)
		}
	}
}

func TestStaleDispatcher_XDP(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.dispatchers = []DispatcherState{{
		DB: &dispatcher.State{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      1,
			Ifindex:   2,
			ProgramID: 100,
		},
		ProgPinExist: boolPtr(false),
		LinkCount:    0,
		ProgPin:      "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
		RevDir:       "/bpffs/xdp/dispatcher_1_2_1",
		LinkPin:      "/bpffs/xdp/dispatcher_1_2_link",
	}}

	rule := ruleByName(Rules(), "stale-dispatcher")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		StaleXDPDispatcher{
			Nsid:    1,
			Ifindex: 2,
			ProgPin: "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
			RevDir:  "/bpffs/xdp/dispatcher_1_2_1",
			LinkPin: "/bpffs/xdp/dispatcher_1_2_link",
		},
	})
}

func TestStaleDispatcher_TC(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.tcFilterOK[dispatcherKey(dispatcher.DispatcherTypeTCIngress, 1, 3)] = false
	s.dispatchers = []DispatcherState{{
		DB: &dispatcher.State{
			Type:      dispatcher.DispatcherTypeTCIngress,
			Nsid:      1,
			Ifindex:   3,
			ProgramID: 200,
			Priority:  100,
		},
		TCFilterOK: boolPtr(false),
		LinkCount:  0,
		ProgPin:    "/bpffs/tc_ingress/dispatcher_1_3_1/dispatcher",
		RevDir:     "/bpffs/tc_ingress/dispatcher_1_3_1",
	}}

	rule := ruleByName(Rules(), "stale-dispatcher")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		StaleTCDispatcher{
			Type:    dispatcher.DispatcherTypeTCIngress,
			Nsid:    1,
			Ifindex: 3,
			ProgPin: "/bpffs/tc_ingress/dispatcher_1_3_1/dispatcher",
			RevDir:  "/bpffs/tc_ingress/dispatcher_1_3_1",
		},
	})
}

// TestStaleDispatcher_XDP_OuterLinkDetachedResidue covers the
// post-detach residue class where Manager.unload detached the
// dispatcher's outer kernel link successfully but a later step in
// removeEmptyDispatcher (deleteDispatcherSnapshot, typically due to
// a transient store-transaction failure) left the DB row behind.
// Under the post-detach log-only contract that residue reaches the
// next GC instead of a joined-error retry; the stale-dispatcher rule
// must recognise it and trigger repair.
//
// State: zero members, prog pin reported as still present (so the
// existing prog-pin-missing branch does not fire), but the recorded
// LinkID's kernel link is no longer alive.
func TestStaleDispatcher_XDP_OuterLinkDetachedResidue(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.dispatchers = []DispatcherState{{
		DB: &dispatcher.State{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      1,
			Ifindex:   2,
			ProgramID: 100,
			LinkID:    999,
		},
		ProgPinExist: boolPtr(true),
		KernelLink:   false,
		LinkCount:    0,
		ProgPin:      "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
		RevDir:       "/bpffs/xdp/dispatcher_1_2_1",
		LinkPin:      "/bpffs/xdp/dispatcher_1_2_link",
	}}

	rule := ruleByName(Rules(), "stale-dispatcher")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		StaleXDPDispatcher{
			Nsid:    1,
			Ifindex: 2,
			ProgPin: "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
			RevDir:  "/bpffs/xdp/dispatcher_1_2_1",
			LinkPin: "/bpffs/xdp/dispatcher_1_2_link",
		},
	})
}

// TestStaleDispatcher_XDP_OuterLinkAliveNoResidue verifies the
// negative case for the new branch: an XDP dispatcher with a live
// outer kernel link is not stale, even with zero members. Without
// the LinkID != 0 guard the branch would falsely fire on every
// freshly-loaded dispatcher before its first attach.
func TestStaleDispatcher_XDP_OuterLinkAliveNoResidue(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.dispatchers = []DispatcherState{{
		DB: &dispatcher.State{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      1,
			Ifindex:   2,
			ProgramID: 100,
			LinkID:    999,
		},
		ProgPinExist: boolPtr(true),
		KernelLink:   true,
		LinkCount:    0,
		ProgPin:      "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
		RevDir:       "/bpffs/xdp/dispatcher_1_2_1",
		LinkPin:      "/bpffs/xdp/dispatcher_1_2_link",
	}}

	rule := ruleByName(Rules(), "stale-dispatcher")
	violations := rule.Eval(s)

	if len(violations) != 0 {
		t.Errorf("expected zero violations for live-outer-link dispatcher, got %d", len(violations))
	}
}

func TestStaleDispatcher_NotStale(t *testing.T) {
	t.Parallel()

	s := newTestState()
	// Dispatcher with extensions is not stale.
	s.dispatchers = []DispatcherState{{
		DB: &dispatcher.State{
			Type:      dispatcher.DispatcherTypeXDP,
			Nsid:      1,
			Ifindex:   2,
			ProgramID: 100,
		},
		ProgPinExist: boolPtr(true),
		LinkCount:    1,
		ProgPin:      "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
		RevDir:       "/bpffs/xdp/dispatcher_1_2_1",
	}}

	rule := ruleByName(Rules(), "stale-dispatcher")
	violations := rule.Eval(s)

	if len(violations) != 0 {
		t.Errorf("expected zero violations, got %d", len(violations))
	}
}

func TestOrphanProgramArtefacts_DeadProgPin(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}
	// kernel ID 42 is not alive

	rule := ruleByName(Rules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanProgPin, Path: "/bpffs/prog_42"},
	})
}

func TestOrphanProgramArtefacts_LinkDir(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/links/42", ProgramID: 42, Kind: OrphanLinkDir},
	}

	rule := ruleByName(Rules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanLinkDir, Path: "/bpffs/links/42"},
	})
}

func TestOrphanProgramArtefacts_MapDir(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/maps/42", ProgramID: 42, Kind: OrphanMapDir},
	}

	rule := ruleByName(Rules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanMapDir, Path: "/bpffs/maps/42"},
	})
}

func TestOrphanProgramArtefacts_LiveSkipped(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.kernelAlive[42] = true
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}

	rule := ruleByName(Rules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	if len(violations) != 0 {
		t.Errorf("expected zero violations for live orphan, got %d", len(violations))
	}
}

func TestOrphanDispatcherArtefacts_Dir(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/xdp/dispatcher_1_2_1", Kind: OrphanDispatcherDir},
	}

	rule := ruleByName(Rules(), "orphan-dispatcher-artefacts")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanDispatcherDir, Path: "/bpffs/xdp/dispatcher_1_2_1"},
	})
}

func TestOrphanDispatcherArtefacts_Link(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/xdp/dispatcher_1_2_link", Kind: OrphanDispatcherLink},
	}

	rule := ruleByName(Rules(), "orphan-dispatcher-artefacts")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanDispatcherLink, Path: "/bpffs/xdp/dispatcher_1_2_link"},
	})
}

func TestOrphanProgramDirs(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/data/programs/42", ProgramID: 42, Kind: OrphanProgramDir},
	}

	rule := ruleByName(Rules(), "orphan-program-dirs")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanProgramDir, Path: "/data/programs/42"},
	})
}

func TestOrphanProgramDirs_Unknown(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/data/programs/bad-name", Kind: OrphanProgramDirUnk},
	}

	rule := ruleByName(Rules(), "orphan-program-dirs")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanProgramDirUnk, Path: "/data/programs/bad-name"},
	})
}

func TestOrphanStagingDirs(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/data/.staging/abc123", Kind: OrphanStagingDir},
	}

	rule := ruleByName(Rules(), "orphan-staging-dirs")
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanStagingDir, Path: "/data/.staging/abc123"},
	})
}

func TestPruneLiveOrphans_ProgPin(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.kernelAlive[42] = true
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}

	rule := PruneRule()
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanProgPin, Path: "/bpffs/prog_42"},
	})
}

func TestPruneLiveOrphans_LinkDir(t *testing.T) {
	t.Parallel()

	s := newTestState()
	s.kernelAlive[42] = true
	s.orphans = []FsOrphan{
		{Path: "/bpffs/links/42", ProgramID: 42, Kind: OrphanLinkDir},
	}

	rule := PruneRule()
	violations := rule.Eval(s)

	assertIntent(t, violations, []RepairIntent{
		RemoveOrphanArtefact{Kind: OrphanLinkDir, Path: "/bpffs/links/42"},
	})
}

func TestPruneLiveOrphans_SkipsDeadOrphans(t *testing.T) {
	t.Parallel()

	s := newTestState()
	// kernel ID 42 is NOT alive
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}

	rule := PruneRule()
	violations := rule.Eval(s)

	if len(violations) != 0 {
		t.Errorf("expected zero violations for dead orphan, got %d", len(violations))
	}
}
