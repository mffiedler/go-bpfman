package cliformat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/dispatcher"
	"github.com/bpfman/bpfman/kernel"
	"github.com/bpfman/bpfman/platform"
)

func TestRenderLinkGetTable_ExposesManagedAndKernelIDs(t *testing.T) {
	t.Parallel()

	kernelLinkID := kernel.LinkID(17)
	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:           8,
			ProgramID:    42,
			KernelLinkID: &kernelLinkID,
			Kind:         bpfman.LinkKindTracepoint,
			Details:      bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
		},
	}

	var buf bytes.Buffer
	if err := RenderLinkGet(&buf, LinkGetView{Link: link}, OutputFormatText); err != nil {
		t.Fatalf("RenderLinkGet() error = %v", err)
	}
	output := buf.String()
	for _, want := range []string{"Link ID: 8", "Kernel Link ID:", "17"} {
		if !strings.Contains(output, want) {
			t.Errorf("link result table missing %q: %s", want, output)
		}
	}
}

func TestRenderLinkGetTable_ShowsMetadata(t *testing.T) {
	t.Parallel()

	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        8,
			ProgramID: 42,
			Kind:      bpfman.LinkKindTracepoint,
			Details:   bpfman.TracepointDetails{Group: "sched", Name: "sched_switch"},
			Metadata:  map[string]string{"owner": "acme"},
		},
	}

	var buf bytes.Buffer
	if err := RenderLinkGet(&buf, LinkGetView{Link: link}, OutputFormatText); err != nil {
		t.Fatalf("RenderLinkGet() error = %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "owner=acme") {
		t.Errorf("link result table should show metadata owner=acme:\n%s", output)
	}
}

func TestRenderLinkAttachTable_PrintsLinkDetails(t *testing.T) {
	t.Parallel()

	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        8,
			ProgramID: 42,
			Kind:      bpfman.LinkKindTC,
			Details: bpfman.TCDetails{
				Interface: "eth0",
				Direction: bpfman.TCDirectionIngress,
				Priority:  50,
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderLinkAttach(&buf, LinkAttachView{Link: link}, OutputFormatText); err != nil {
		t.Fatalf("RenderLinkAttach() error = %v", err)
	}
	output := buf.String()
	for _, want := range []string{
		"Link ID: 8",
		"Spec:",
		"Status:",
		"Type:",
		"tc",
		"Interface:",
		"eth0",
		"Priority:",
		"50",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("link attach table missing %q:\n%s", want, output)
		}
	}
}

func TestRenderLinkGetTable_RendersPresentationFields(t *testing.T) {
	t.Parallel()

	link := bpfman.Link{
		Record: bpfman.LinkRecord{
			ID:        8,
			ProgramID: 42,
			Kind:      bpfman.LinkKindTC,
			Details: bpfman.TCDetails{
				Interface: "eth0",
				Direction: bpfman.TCDirectionIngress,
				Priority:  50,
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderLinkGet(&buf, LinkGetView{Link: link, ProgramName: "stats"}, OutputFormatText); err != nil {
		t.Fatalf("RenderLinkGet() error = %v", err)
	}
	output := buf.String()
	for _, want := range []string{"BPF Function:", "stats", "Network Namespace:", "None"} {
		if !strings.Contains(output, want) {
			t.Errorf("link get table missing %q:\n%s", want, output)
		}
	}
}

func TestRenderDispatcherSnapshotTable_ExposesMemberManagedAndKernelIDs(t *testing.T) {
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
				LinkID:       8,
				KernelLinkID: &memberKernelLinkID,
				Position:     0,
				Priority:     50,
				ProceedOn:    1 << 2,
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderDispatcherSnapshot(&buf, snap, OutputFormatText); err != nil {
		t.Fatalf("RenderDispatcherSnapshot() error = %v", err)
	}
	output := buf.String()
	for _, want := range []string{"LINK_ID", "KERNEL_LINK_ID", "8", "23"} {
		if !strings.Contains(output, want) {
			t.Errorf("dispatcher snapshot table missing %q: %s", want, output)
		}
	}
}

func TestRenderDispatcherSnapshotTable_MissingMemberKernelIDUsesColumnSentinel(t *testing.T) {
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
				LinkID:      8,
				Position:    0,
				Priority:    50,
				ProceedOn:   1 << 0,
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderDispatcherSnapshot(&buf, snap, OutputFormatText); err != nil {
		t.Fatalf("RenderDispatcherSnapshot() error = %v", err)
	}
	output := buf.String()
	for _, want := range []string{"KERNEL_LINK_ID", "<none>"} {
		if !strings.Contains(output, want) {
			t.Errorf("dispatcher snapshot table missing %q: %s", want, output)
		}
	}
}
