package action

import (
	"strings"
	"testing"

	"github.com/frobware/go-bpfman/kernel"
)

func TestDescribe(t *testing.T) {
	tests := []struct {
		name     string
		action   Action
		contains string
	}{
		{
			name:     "DeleteProgram",
			action:   DeleteProgram{KernelID: kernel.ProgramID(42)},
			contains: "delete program 42 from store",
		},
		{
			name:     "DeleteLink",
			action:   DeleteLink{LinkID: kernel.LinkID(99)},
			contains: "delete link 99 from store",
		},
		{
			name:     "DeleteDispatcher",
			action:   DeleteDispatcher{Type: "xdp", Nsid: 4026531840, Ifindex: 2},
			contains: "delete xdp dispatcher nsid=4026531840 ifindex=2 from store",
		},
		{
			name:     "RemoveProgPin",
			action:   RemoveProgPin{Path: "/run/bpfman/fs/prog_42"},
			contains: "remove program pin /run/bpfman/fs/prog_42",
		},
		{
			name:     "RemoveLinkDir",
			action:   RemoveLinkDir{Path: "/run/bpfman/fs/link_10"},
			contains: "remove link directory /run/bpfman/fs/link_10",
		},
		{
			name:     "RemoveMapDir",
			action:   RemoveMapDir{Path: "/run/bpfman/fs/maps_42"},
			contains: "remove map directory /run/bpfman/fs/maps_42",
		},
		{
			name:     "RemoveDispatcherProgPin",
			action:   RemoveDispatcherProgPin{Path: "/run/bpfman/fs/dispatcher/prog"},
			contains: "remove dispatcher program pin /run/bpfman/fs/dispatcher/prog",
		},
		{
			name:     "RemoveDispatcherRevDir",
			action:   RemoveDispatcherRevDir{Path: "/run/bpfman/fs/dispatcher/rev1"},
			contains: "remove dispatcher revision directory /run/bpfman/fs/dispatcher/rev1",
		},
		{
			name:     "RemoveDispatcherLinkPin",
			action:   RemoveDispatcherLinkPin{Path: "/run/bpfman/fs/dispatcher/link"},
			contains: "remove dispatcher link pin /run/bpfman/fs/dispatcher/link",
		},
		{
			name:     "RemoveProgramDirByPath",
			action:   RemoveProgramDirByPath{Path: "/var/lib/bpfman/bytecode/42"},
			contains: "remove program directory /var/lib/bpfman/bytecode/42",
		},
		{
			name:     "RemoveStagingDir",
			action:   RemoveStagingDir{Path: "/var/lib/bpfman/staging/abc"},
			contains: "remove staging directory /var/lib/bpfman/staging/abc",
		},
		{
			name:     "RemoveProgramDir",
			action:   RemoveProgramDir{KernelID: kernel.ProgramID(77)},
			contains: "remove program directory for 77",
		},
		{
			name:     "DetachTCFilter",
			action:   DetachTCFilter{Ifindex: 3, Priority: 100},
			contains: "detach TC filter ifindex=3 priority=100",
		},
		{
			name:     "unknown action falls through to type name",
			action:   SaveProgram{},
			contains: "action.SaveProgram",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Describe(tt.action)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("Describe(%T) = %q, want substring %q", tt.action, got, tt.contains)
			}
		})
	}
}
