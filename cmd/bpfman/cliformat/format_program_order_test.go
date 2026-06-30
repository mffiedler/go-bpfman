package cliformat

import (
	"strings"
	"testing"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/kernel"
)

// The Links and Maps sub-sections render after the scalar status fields,
// not wedged among them in alphabetical position.
func TestFormatProgramTable_SubsectionsAfterScalars(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: 42, Meta: bpfman.ProgramMeta{Name: "p"}},
		Status: bpfman.ProgramStatus{
			Kernel: &kernel.Program{},
			Links:  []bpfman.Link{{Record: bpfman.LinkRecord{ID: 8, Kind: bpfman.LinkKindXDP}}},
			Maps:   []bpfman.MapStatus{{Map: kernel.Map{}}},
		},
	}

	out := formatProgramTable(prog)
	instructions := strings.Index(out, "Instructions:")
	links := strings.Index(out, "Links:")
	maps := strings.Index(out, "Maps:")
	if instructions < 0 || links < 0 || maps < 0 {
		t.Fatalf("missing expected sections in:\n%s", out)
	}
	if !(instructions < links && links < maps) {
		t.Errorf("want a scalar (Instructions) before Links before Maps; got offsets %d, %d, %d:\n%s", instructions, links, maps, out)
	}
}
