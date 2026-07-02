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

// The Spec Path row shows the caller's file-load operand, not
// bpfman's stored bytecode copy; the stored copy remains visible as
// the Status Bytecode row.
func TestFormatProgramTable_PathShowsSourcePath(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{
			ProgramID: 42,
			Load:      bpfman.LoadSpec{}.WithObjectPath("/run/bpfman/programs/42/bytecode.o").WithSourcePath("e2e/testdata/bpf/xdp_pass.bpf.o"),
		},
	}
	if out := formatProgramTable(prog); !strings.Contains(out, "Path:           e2e/testdata/bpf/xdp_pass.bpf.o\n") {
		t.Errorf("Path row should show the source path, got:\n%s", out)
	}
}

// The Spec renders the load source as one concept with variant-specific
// rows: a file load shows the Path operand; an image load shows the
// image provenance instead of the stored-copy path, which Status
// already reports as Bytecode.
func TestFormatProgramTable_ImageLoadShowsProvenance(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{
			ProgramID: 42,
			Load:      bpfman.LoadSpec{}.WithObjectPath("/run/bpfman/programs/42/bytecode.o").WithImageProvenance("quay.io/bpfman-bytecode/xdp_pass:latest", "sha256:abc", bpfman.PullIfNotPresent),
		},
	}

	out := formatProgramTable(prog)
	if !strings.Contains(out, "Image URL:      quay.io/bpfman-bytecode/xdp_pass:latest\n") {
		t.Errorf("image load should show Image URL, got:\n%s", out)
	}
	if !strings.Contains(out, "Pull Policy:    IfNotPresent\n") {
		t.Errorf("image load should show Pull Policy, got:\n%s", out)
	}
	if strings.Contains(out, "\n    Path:") {
		t.Errorf("image load should not show a Path row, got:\n%s", out)
	}
}

// The Status section reports map-sharing membership: every program
// whose records point at this program's map set, space-separated like
// the list table's LINK IDS column. It answers "whose data disappears
// if I unload this?".
func TestFormatProgramTable_MapsUsedByRow(t *testing.T) {
	t.Parallel()

	prog := bpfman.Program{
		Record: bpfman.ProgramRecord{ProgramID: 42},
		Status: bpfman.ProgramStatus{
			Kernel:    &kernel.Program{},
			MapUsedBy: []kernel.ProgramID{42, 57},
		},
	}
	if out := formatProgramTable(prog); !strings.Contains(out, "Maps Used By:") || !strings.Contains(out, "42 57") {
		t.Errorf("Status should carry a Maps Used By row with the sharing program ids, got:\n%s", out)
	}
}
