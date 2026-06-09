package cliformat

import (
	"strings"
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/dispatcher"
	"github.com/frobware/go-bpfman/kernel"
	"github.com/frobware/go-bpfman/platform"
)

func TestFormatLinkResultTable_ExposesManagedAndKernelIDs(t *testing.T) {
	t.Parallel()

	kernelLinkID := kernel.LinkID(17)
	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:           2_123_456_789,
			ProgramID:    42,
			KernelLinkID: &kernelLinkID,
			Kind:         bpfman.LinkKindTracepoint,
			Details:      bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
		},
	}

	output, err := FormatLinkResult(link, &OutputFlags{Output: OutputValue{Value: "table"}})
	if err != nil {
		t.Fatalf("FormatLinkResult() error = %v", err)
	}
	for _, want := range []string{"Link ID: 2123456789", "Kernel Link ID:", "17"} {
		if !strings.Contains(output, want) {
			t.Errorf("link result table missing %q: %s", want, output)
		}
	}
}

func TestFormatDispatcherSnapshotTable_ExposesMemberManagedAndKernelIDs(t *testing.T) {
	t.Parallel()

	dispatcherKernelLinkID := kernel.LinkID(19)
	memberKernelLinkID := kernel.LinkID(23)
	snap := platform.DispatcherSnapshot{
		Key: dispatcher.Key{
			Type:    dispatcher.DispatcherTypeXDP,
			Nsid:    1,
			Ifindex: 2,
		},
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID:    100,
			KernelLinkID: &dispatcherKernelLinkID,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:    42,
				ProgramName:  "xdp_pass",
				LinkID:       2_123_456_789,
				KernelLinkID: &memberKernelLinkID,
				Position:     0,
				Priority:     50,
				ProceedOn:    1 << 2,
			},
		},
	}

	output, err := FormatDispatcherSnapshot(snap, &OutputFlags{Output: OutputValue{Value: "table"}})
	if err != nil {
		t.Fatalf("FormatDispatcherSnapshot() error = %v", err)
	}
	for _, want := range []string{"LINK_ID", "KERNEL_LINK_ID", "2123456789", "23"} {
		if !strings.Contains(output, want) {
			t.Errorf("dispatcher snapshot table missing %q: %s", want, output)
		}
	}
}

func TestFormatDispatcherSnapshotTable_MissingMemberKernelIDUsesColumnSentinel(t *testing.T) {
	t.Parallel()

	snap := platform.DispatcherSnapshot{
		Key: dispatcher.Key{
			Type:    dispatcher.DispatcherTypeTCIngress,
			Nsid:    1,
			Ifindex: 2,
		},
		Revision: 1,
		Runtime: platform.DispatcherRuntime{
			ProgramID: 100,
		},
		Members: []platform.DispatcherMember{
			{
				ProgramID:   42,
				ProgramName: "tc_pass",
				LinkID:      2_123_456_789,
				Position:    0,
				Priority:    50,
				ProceedOn:   1 << 0,
			},
		},
	}

	output, err := FormatDispatcherSnapshot(snap, &OutputFlags{Output: OutputValue{Value: "table"}})
	if err != nil {
		t.Fatalf("FormatDispatcherSnapshot() error = %v", err)
	}
	for _, want := range []string{"KERNEL_LINK_ID", "<none>"} {
		if !strings.Contains(output, want) {
			t.Errorf("dispatcher snapshot table missing %q: %s", want, output)
		}
	}
}
