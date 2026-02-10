package manager

import (
	"testing"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

func TestComputeTCXAttachOrder(t *testing.T) {
	tests := []struct {
		name          string
		existingLinks []bpfman.TCXLinkInfo
		newPriority   int32
		wantFirst     bool
		wantLast      bool
		wantBefore    kernel.ProgramID
		wantAfter     kernel.ProgramID
	}{
		{
			name:          "empty chain - attach at head",
			existingLinks: nil,
			newPriority:   50,
			wantFirst:     true,
		},
		{
			name: "lowest priority - attach at head (before all)",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 100, Priority: 100},
				{KernelLinkID: 2, KernelProgramID: 200, Priority: 200},
			},
			newPriority: 50,
			wantBefore:  100, // before program with priority 100
		},
		{
			name: "highest priority - attach at tail (after all)",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 100, Priority: 100},
				{KernelLinkID: 2, KernelProgramID: 200, Priority: 200},
			},
			newPriority: 300,
			wantAfter:   200, // after program with priority 200
		},
		{
			name: "middle priority - attach before higher priority",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 100, Priority: 100},
				{KernelLinkID: 2, KernelProgramID: 300, Priority: 300},
			},
			newPriority: 200,
			wantBefore:  300, // before program with priority 300
		},
		{
			name: "equal priority - attach after existing (FIFO for ties)",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 100, Priority: 100},
				{KernelLinkID: 2, KernelProgramID: 200, Priority: 200},
			},
			newPriority: 200,
			wantAfter:   200, // after existing program with same priority
		},
		{
			name: "single existing link - lower priority inserts before",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 100, Priority: 100},
			},
			newPriority: 50,
			wantBefore:  100,
		},
		{
			name: "single existing link - higher priority inserts after",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 100, Priority: 100},
			},
			newPriority: 200,
			wantAfter:   100,
		},
		{
			name: "TC dispatcher scenario - TCX with priority 500 after dispatcher with priority 50",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 1000, Priority: 50}, // TC dispatcher
			},
			newPriority: 500, // TCX user program
			wantAfter:   1000,
		},
		{
			name: "TCX before TC dispatcher - lower priority runs first",
			existingLinks: []bpfman.TCXLinkInfo{
				{KernelLinkID: 1, KernelProgramID: 1000, Priority: 50}, // TC dispatcher
			},
			newPriority: 25, // TCX user program with lower priority
			wantBefore:  1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTCXAttachOrder(tt.existingLinks, tt.newPriority)

			if tt.wantFirst && !got.First {
				t.Errorf("expected First=true, got %+v", got)
			}
			if tt.wantLast && !got.Last {
				t.Errorf("expected Last=true, got %+v", got)
			}
			if tt.wantBefore != 0 && got.BeforeProgID != tt.wantBefore {
				t.Errorf("expected BeforeProgID=%d, got %+v", tt.wantBefore, got)
			}
			if tt.wantAfter != 0 && got.AfterProgID != tt.wantAfter {
				t.Errorf("expected AfterProgID=%d, got %+v", tt.wantAfter, got)
			}

			// Verify mutual exclusivity
			setFields := 0
			if got.First {
				setFields++
			}
			if got.Last {
				setFields++
			}
			if got.BeforeProgID != 0 {
				setFields++
			}
			if got.AfterProgID != 0 {
				setFields++
			}
			if setFields != 1 {
				t.Errorf("expected exactly one field set, got %d: %+v", setFields, got)
			}
		})
	}
}
