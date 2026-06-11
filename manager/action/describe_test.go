package action

import (
	"fmt"
	"strings"
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
)

func TestDescribe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		action   Action
		contains string
	}{
		// Store actions
		{
			name:     "SaveProgram",
			action:   SaveProgram{ProgramID: kernel.ProgramID(42)},
			contains: "save program 42 to store",
		},
		{
			name:     "DeleteProgram",
			action:   DeleteProgram{ProgramID: kernel.ProgramID(42)},
			contains: "delete program 42 from store",
		},
		{
			name:     "CreateLink",
			action:   CreateLink{Spec: bpfman.LinkSpec{ProgramID: kernel.ProgramID(7)}},
			contains: "create link in store",
		},
		{
			name:     "DeleteLink",
			action:   DeleteLink{LinkID: bpfman.LinkID(99)},
			contains: "delete link 99 from store",
		},
		{
			name:     "GetProgramFromStore",
			action:   GetProgramFromStore{ProgramID: kernel.ProgramID(10)},
			contains: "get program 10 from store",
		},
		{
			name:     "CheckProgramNotInStore",
			action:   CheckProgramNotInStore{ProgramID: kernel.ProgramID(10)},
			contains: "verify program 10 not in store",
		},

		// Kernel load/unload
		{
			name: "LoadProgram",
			action: LoadProgram{
				Spec: mustLoadSpec(t, "test_prog"),
			},
			contains: "load program test_prog",
		},
		{
			name:     "UnloadProgram",
			action:   UnloadProgram{PinPath: "/sys/fs/bpf/bpfman/prog_42"},
			contains: "unload program at /sys/fs/bpf/bpfman/prog_42",
		},

		// Attach actions
		{
			name:     "AttachTracepoint",
			action:   AttachTracepoint{Group: "sched", Name: "sched_switch"},
			contains: "attach tracepoint sched/sched_switch",
		},
		{
			name:     "AttachKprobe",
			action:   AttachKprobe{FnName: "do_sys_open"},
			contains: "attach kprobe do_sys_open",
		},
		{
			name:     "AttachUprobeLocal",
			action:   AttachUprobeLocal{Target: "/usr/bin/bash", FnName: "readline"},
			contains: "attach uprobe /usr/bin/bash:readline",
		},
		{
			name:     "AttachUprobeContainer",
			action:   AttachUprobeContainer{Target: "/usr/bin/bash", FnName: "readline"},
			contains: "attach uprobe (container) /usr/bin/bash:readline",
		},
		{
			name:     "AttachFentry",
			action:   AttachFentry{FnName: "tcp_connect"},
			contains: "attach fentry tcp_connect",
		},
		{
			name:     "AttachFexit",
			action:   AttachFexit{FnName: "tcp_connect"},
			contains: "attach fexit tcp_connect",
		},
		{
			name:     "AttachTCX",
			action:   AttachTCX{Ifindex: 3, Direction: "ingress"},
			contains: "attach TCX ifindex=3 ingress",
		},

		// Link/pin actions
		{
			name:     "DetachLink",
			action:   DetachLink{PinPath: "/sys/fs/bpf/bpfman/link_10"},
			contains: "detach link at /sys/fs/bpf/bpfman/link_10",
		},
		{
			name:     "PublishBytecode",
			action:   PublishBytecode{ProgramID: kernel.ProgramID(42)},
			contains: "publish bytecode for program 42",
		},

		// Dispatcher actions
		{
			name:     "DeleteDispatcher",
			action:   DeleteDispatcher{Type: dispatcher.DispatcherTypeXDP, Nsid: 4026531840, Ifindex: 2},
			contains: "delete xdp dispatcher nsid=4026531840 ifindex=2 from store",
		},
		{
			name:     "RebuildXDPDispatcher",
			action:   RebuildXDPDispatcher{Ifindex: 3},
			contains: "rebuild XDP dispatcher ifindex=3",
		},
		{
			name:     "RebuildTCDispatcher",
			action:   RebuildTCDispatcher{Ifindex: 5, Direction: bpfman.TCDirectionIngress},
			contains: "rebuild TC dispatcher ifindex=5 ingress",
		},
		{
			name: "RebuildDispatcherForDetach",
			action: RebuildDispatcherForDetach{
				Key:           dispatcher.Key{Type: dispatcher.DispatcherTypeXDP, Nsid: 1, Ifindex: 2},
				ExcludeLinkID: 99,
			},
			contains: "rebuild xdp dispatcher for detach nsid=1 ifindex=2 excluding link 99",
		},
		{
			name: "RemoveDispatcher",
			action: RemoveDispatcher{Key: dispatcher.Key{
				Type: dispatcher.DispatcherTypeXDP, Nsid: 1, Ifindex: 2,
			}},
			contains: "remove xdp dispatcher nsid=1 ifindex=2",
		},

		// GC filesystem cleanup
		{
			name:     "RemoveProgPin",
			action:   RemoveProgPin{Path: "/run/bpfman/fs/prog_42"},
			contains: "remove program pin /run/bpfman/fs/prog_42",
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
			name:     "RemoveProgramDir",
			action:   RemoveProgramDir{Path: "/var/lib/bpfman/bytecode/42"},
			contains: "remove program directory /var/lib/bpfman/bytecode/42",
		},
		{
			name:     "RemoveStagingDir",
			action:   RemoveStagingDir{Path: "/var/lib/bpfman/staging/abc"},
			contains: "remove staging directory /var/lib/bpfman/staging/abc",
		},
		{
			name:     "DetachTCFilter",
			action:   DetachTCFilter{Ifindex: 3, Priority: 100, NetnsPath: "/proc/self/ns/net"},
			contains: "detach TC filter ifindex=3 priority=100 netns=/proc/self/ns/net",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Describe(tt.action)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("Describe(%T) = %q, want substring %q", tt.action, got, tt.contains)
			}
		})
	}
}

// TestDescribe_Exhaustive asserts that every concrete Action type has
// a Describe case that produces a domain-specific description rather
// than falling through to the default %T format. If a new action type
// is added without a corresponding Describe case, this test fails.
func TestDescribe_Exhaustive(t *testing.T) {
	t.Parallel()

	// Every concrete type implementing Action must appear here.
	// The test does not care about the description content (TestDescribe
	// covers that); it only checks that Describe does not fall through
	// to the default branch, which formats as "action.<TypeName>".
	allActions := []Action{
		SaveProgram{},
		DeleteProgram{},
		CreateLink{},
		CreatePendingLink{},
		DeleteLink{},
		FinaliseLink{},
		GetProgramFromStore{},
		CheckProgramNotInStore{},
		LoadProgram{},
		UnloadProgram{},
		RemoveMapsPins{},
		AttachTracepoint{},
		AttachKprobe{},
		AttachUprobeLocal{},
		AttachUprobeContainer{},
		AttachFentry{},
		AttachFexit{},
		AttachTCX{},
		DeleteDispatcher{},
		DetachLink{},
		DetachTCFilter{},
		PublishBytecode{},
		RemoveProgramDir{},
		RemoveProgPin{},
		RemoveMapDir{},
		RemoveDispatcherProgPin{},
		RemoveDispatcherRevDir{},
		RemoveDispatcherLinkPin{},
		RemoveStagingDir{},
		RemoveDispatcher{},
		RebuildXDPDispatcher{},
		RebuildTCDispatcher{},
		RebuildDispatcherForDetach{},
	}

	for _, a := range allActions {
		desc := Describe(a)
		typeName := fmt.Sprintf("%T", a)
		if desc == typeName {
			t.Errorf("Describe(%T) returned default %%T format %q; add a case to Describe", a, desc)
		}
	}
}

// mustLoadSpec creates a minimal LoadSpec for testing Describe.
func mustLoadSpec(t *testing.T, name string) bpfman.LoadSpec {
	t.Helper()
	spec, err := bpfman.NewLoadSpec("/dummy.o", name, bpfman.ProgramTypeTracepoint)
	if err != nil {
		t.Fatalf("NewLoadSpec: %v", err)
	}
	return spec
}
