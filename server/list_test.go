package server

import (
	"testing"

	"github.com/bpfman/bpfman"
)

// linkDetailsToProto is the only step between a link's domain details
// and the gRPC bytes a GetLink/ListLinks response carries, so it is the
// wire-mapping locus. TCX details carry a derived chain position that
// XDP and TC details already expose on the wire; assert TCX does too,
// so a raw gRPC client sees it rather than only the CLI (which reads the
// domain value directly and would not catch a dropped proto field).
func TestLinkDetailsToProto_TCXPosition(t *testing.T) {
	t.Parallel()

	details := bpfman.TCXDetails{
		Interface: "eth0",
		Ifindex:   3,
		Direction: bpfman.TCDirectionIngress,
		Priority:  25,
		Position:  2,
		Nsid:      4026531840,
	}

	got := linkDetailsToProto(details)

	tcx := got.GetTcx()
	if tcx == nil {
		t.Fatalf("expected TCX link details, got %T", got.GetDetails())
	}
	if tcx.GetPosition() != details.Position {
		t.Errorf("TCX position not mapped to proto: got %d, want %d", tcx.GetPosition(), details.Position)
	}
	if tcx.GetPriority() != details.Priority {
		t.Errorf("TCX priority: got %d, want %d", tcx.GetPriority(), details.Priority)
	}
}
