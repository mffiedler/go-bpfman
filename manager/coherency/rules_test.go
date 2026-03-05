package coherency

import (
	"testing"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/manager/action"
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

// assertActions checks that the violations contain exactly the
// expected action types in order. Each inner slice corresponds to one
// violation's Op.Actions.
func assertActions(t *testing.T, violations []Violation, expected [][]action.Action) {
	t.Helper()
	if len(violations) != len(expected) {
		t.Fatalf("got %d violations, want %d", len(violations), len(expected))
	}
	for i, v := range violations {
		if v.Op == nil {
			t.Fatalf("violation[%d]: Op is nil", i)
		}
		got := v.Op.Actions
		want := expected[i]
		if len(got) != len(want) {
			t.Fatalf("violation[%d]: got %d actions, want %d\n  got:  %+v\n  want: %+v", i, len(got), len(want), got, want)
		}
		for j := range got {
			if got[j] != want[j] {
				t.Errorf("violation[%d].Actions[%d]:\n  got:  %+v\n  want: %+v", i, j, got[j], want[j])
			}
		}
	}
}

func TestStaleDispatcher_XDP(t *testing.T) {
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

	rule := ruleByName(GCRules(), "stale-dispatcher")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveDispatcherProgPin{Path: "/bpffs/xdp/dispatcher_1_2_1/dispatcher"},
		action.RemoveDispatcherRevDir{Path: "/bpffs/xdp/dispatcher_1_2_1"},
		action.RemoveDispatcherLinkPin{Path: "/bpffs/xdp/dispatcher_1_2_link"},
		action.DeleteDispatcher{Type: dispatcher.DispatcherTypeXDP, Nsid: 1, Ifindex: 2},
	}})
}

func TestStaleDispatcher_TC(t *testing.T) {
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

	rule := ruleByName(GCRules(), "stale-dispatcher")
	violations := rule.Eval(s)

	// TC dispatchers do not have a link pin.
	assertActions(t, violations, [][]action.Action{{
		action.RemoveDispatcherProgPin{Path: "/bpffs/tc_ingress/dispatcher_1_3_1/dispatcher"},
		action.RemoveDispatcherRevDir{Path: "/bpffs/tc_ingress/dispatcher_1_3_1"},
		action.DeleteDispatcher{Type: dispatcher.DispatcherTypeTCIngress, Nsid: 1, Ifindex: 3},
	}})
}

func TestStaleDispatcher_NotStale(t *testing.T) {
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

	rule := ruleByName(GCRules(), "stale-dispatcher")
	violations := rule.Eval(s)

	if len(violations) != 0 {
		t.Errorf("expected zero violations, got %d", len(violations))
	}
}

func TestOrphanProgramArtefacts_DeadProgPin(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}
	// kernel ID 42 is not alive

	rule := ruleByName(GCRules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveProgPin{Path: "/bpffs/prog_42"},
	}})
}

func TestOrphanProgramArtefacts_LinkDir(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/links/42", ProgramID: 42, Kind: OrphanLinkDir},
	}

	rule := ruleByName(GCRules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveLinkDir{Path: "/bpffs/links/42"},
	}})
}

func TestOrphanProgramArtefacts_MapDir(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/maps/42", ProgramID: 42, Kind: OrphanMapDir},
	}

	rule := ruleByName(GCRules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveMapDir{Path: "/bpffs/maps/42"},
	}})
}

func TestOrphanProgramArtefacts_LiveSkipped(t *testing.T) {
	s := newTestState()
	s.kernelAlive[42] = true
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}

	rule := ruleByName(GCRules(), "orphan-program-artefacts")
	violations := rule.Eval(s)

	if len(violations) != 0 {
		t.Errorf("expected zero violations for live orphan, got %d", len(violations))
	}
}

func TestOrphanDispatcherArtefacts_Dir(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/xdp/dispatcher_1_2_1", Kind: OrphanDispatcherDir},
	}

	rule := ruleByName(GCRules(), "orphan-dispatcher-artefacts")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveDispatcherRevDir{Path: "/bpffs/xdp/dispatcher_1_2_1"},
	}})
}

func TestOrphanDispatcherArtefacts_Link(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/bpffs/xdp/dispatcher_1_2_link", Kind: OrphanDispatcherLink},
	}

	rule := ruleByName(GCRules(), "orphan-dispatcher-artefacts")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveDispatcherLinkPin{Path: "/bpffs/xdp/dispatcher_1_2_link"},
	}})
}

func TestOrphanProgramDirs(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/data/programs/42", ProgramID: 42, Kind: OrphanProgramDir},
	}

	rule := ruleByName(GCRules(), "orphan-program-dirs")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveProgramDir{Path: "/data/programs/42"},
	}})
}

func TestOrphanProgramDirs_Unknown(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/data/programs/bad-name", Kind: OrphanProgramDirUnk},
	}

	rule := ruleByName(GCRules(), "orphan-program-dirs")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveProgramDir{Path: "/data/programs/bad-name"},
	}})
}

func TestOrphanStagingDirs(t *testing.T) {
	s := newTestState()
	s.orphans = []FsOrphan{
		{Path: "/data/.staging/abc123", Kind: OrphanStagingDir},
	}

	rule := ruleByName(GCRules(), "orphan-staging-dirs")
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveStagingDir{Path: "/data/.staging/abc123"},
	}})
}

func TestPruneLiveOrphans_ProgPin(t *testing.T) {
	s := newTestState()
	s.kernelAlive[42] = true
	s.orphans = []FsOrphan{
		{Path: "/bpffs/prog_42", ProgramID: 42, Kind: OrphanProgPin},
	}

	rule := PruneRule()
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveProgPin{Path: "/bpffs/prog_42"},
	}})
}

func TestPruneLiveOrphans_LinkDir(t *testing.T) {
	s := newTestState()
	s.kernelAlive[42] = true
	s.orphans = []FsOrphan{
		{Path: "/bpffs/links/42", ProgramID: 42, Kind: OrphanLinkDir},
	}

	rule := PruneRule()
	violations := rule.Eval(s)

	assertActions(t, violations, [][]action.Action{{
		action.RemoveLinkDir{Path: "/bpffs/links/42"},
	}})
}

func TestPruneLiveOrphans_SkipsDeadOrphans(t *testing.T) {
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
