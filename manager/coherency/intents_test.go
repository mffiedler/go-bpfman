package coherency

import (
	"reflect"
	"testing"

	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/manager/action"
)

func TestStaleXDPDispatcher_Actions(t *testing.T) {
	t.Parallel()

	got := StaleXDPDispatcher{
		Nsid:    1,
		Ifindex: 2,
		ProgPin: "/bpffs/xdp/dispatcher_1_2_1/dispatcher",
		RevDir:  "/bpffs/xdp/dispatcher_1_2_1",
		LinkPin: "/bpffs/xdp/dispatcher_1_2_link",
	}.Actions()

	want := []action.Action{
		action.RemoveDispatcherProgPin{Path: "/bpffs/xdp/dispatcher_1_2_1/dispatcher"},
		action.RemoveDispatcherRevDir{Path: "/bpffs/xdp/dispatcher_1_2_1"},
		action.RemoveDispatcherLinkPin{Path: "/bpffs/xdp/dispatcher_1_2_link"},
		action.DeleteDispatcher{Type: dispatcher.DispatcherTypeXDP, Nsid: 1, Ifindex: 2},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("XDP actions mismatch\n  got:  %+v\n  want: %+v", got, want)
	}
}

func TestStaleTCDispatcher_Actions(t *testing.T) {
	t.Parallel()

	got := StaleTCDispatcher{
		Type:    dispatcher.DispatcherTypeTCIngress,
		Nsid:    1,
		Ifindex: 3,
		ProgPin: "/bpffs/tc_ingress/dispatcher_1_3_1/dispatcher",
		RevDir:  "/bpffs/tc_ingress/dispatcher_1_3_1",
	}.Actions()

	// TC has no link pin removal step.
	want := []action.Action{
		action.RemoveDispatcherProgPin{Path: "/bpffs/tc_ingress/dispatcher_1_3_1/dispatcher"},
		action.RemoveDispatcherRevDir{Path: "/bpffs/tc_ingress/dispatcher_1_3_1"},
		action.DeleteDispatcher{Type: dispatcher.DispatcherTypeTCIngress, Nsid: 1, Ifindex: 3},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("TC actions mismatch\n  got:  %+v\n  want: %+v", got, want)
	}
}

func TestRemoveOrphanArtefact_Actions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind OrphanKind
		path string
		want action.Action
	}{
		{"prog-pin", OrphanProgPin, "/bpffs/prog_42", action.RemoveProgPin{Path: "/bpffs/prog_42"}},
		{"link-dir", OrphanLinkDir, "/bpffs/links/42", action.RemoveLinkDir{Path: "/bpffs/links/42"}},
		{"map-dir", OrphanMapDir, "/bpffs/maps/42", action.RemoveMapDir{Path: "/bpffs/maps/42"}},
		{"dispatcher-dir", OrphanDispatcherDir, "/bpffs/xdp/dispatcher_1_2_1", action.RemoveDispatcherRevDir{Path: "/bpffs/xdp/dispatcher_1_2_1"}},
		{"dispatcher-link", OrphanDispatcherLink, "/bpffs/xdp/dispatcher_1_2_link", action.RemoveDispatcherLinkPin{Path: "/bpffs/xdp/dispatcher_1_2_link"}},
		{"program-dir", OrphanProgramDir, "/data/programs/42", action.RemoveProgramDir{Path: "/data/programs/42"}},
		{"program-dir-unknown", OrphanProgramDirUnk, "/data/programs/bad", action.RemoveProgramDir{Path: "/data/programs/bad"}},
		{"shared-map-pin", OrphanSharedMapPin, "/bpffs/shared/map_X", action.RemoveSharedMapPin{Path: "/bpffs/shared/map_X"}},
		{"staging-dir", OrphanStagingDir, "/data/.staging/abc", action.RemoveStagingDir{Path: "/data/.staging/abc"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := RemoveOrphanArtefact{Kind: tc.kind, Path: tc.path}.Actions()
			want := []action.Action{tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("kind %s: got %+v want %+v", tc.kind, got, want)
			}
		})
	}
}
