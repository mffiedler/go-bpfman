package ebpf

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/cilium/ebpf"

	"github.com/bpfman/bpfman"
	"github.com/bpfman/bpfman/platform"
)

// ProgramDiscoverer implements platform.ProgramDiscoverer using cilium/ebpf.
type ProgramDiscoverer struct{}

// NewProgramDiscoverer creates a new program discoverer.
func NewProgramDiscoverer() *ProgramDiscoverer {
	return &ProgramDiscoverer{}
}

// DiscoverPrograms implements platform.ProgramDiscoverer.
func (d *ProgramDiscoverer) DiscoverPrograms(objectPath string) ([]platform.DiscoveredProgram, error) {
	return DiscoverPrograms(objectPath)
}

// ValidatePrograms implements platform.ProgramDiscoverer.
func (d *ProgramDiscoverer) ValidatePrograms(objectPath string, programNames []string) error {
	return ValidatePrograms(objectPath, programNames)
}

// Ensure ProgramDiscoverer implements the interface.
var _ platform.ProgramDiscoverer = (*ProgramDiscoverer)(nil)

// DiscoverPrograms scans a BPF object file and returns all programs found
// within it. Programs are returned sorted by name for deterministic ordering.
//
// For fentry/fexit programs, the attach function is extracted from the ELF
// section name (e.g., "fentry/vfs_read" -> AttachFunc="vfs_read").
func DiscoverPrograms(objectPath string) ([]platform.DiscoveredProgram, error) {
	f, err := os.Open(objectPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DiscoverProgramsFromReader(f)
}

// DiscoverProgramsFromReader is the io.ReaderAt-based form of
// DiscoverPrograms. Useful for callers that have BPF object bytes
// in memory (e.g. embed.FS) and don't want to stage them to disk.
func DiscoverProgramsFromReader(rd io.ReaderAt) ([]platform.DiscoveredProgram, error) {
	collSpec, err := ebpf.LoadCollectionSpecFromReader(rd)
	if err != nil {
		return nil, fmt.Errorf("load collection spec: %w", err)
	}

	var programs []platform.DiscoveredProgram

	for name, progSpec := range collSpec.Programs {
		progType := InferProgramType(progSpec.SectionName)

		// Skip programs with unspecified type
		if !progType.Valid() {
			continue
		}

		prog := platform.DiscoveredProgram{
			Name:        name,
			SectionName: progSpec.SectionName,
			Type:        progType,
		}

		// Extract attach function from section name for fentry/fexit
		if progType == bpfman.ProgramTypeFentry || progType == bpfman.ProgramTypeFexit {
			prog.AttachFunc = ExtractAttachFunc(progSpec.SectionName)
		}

		programs = append(programs, prog)
	}

	// Sort programs by name for deterministic ordering
	slices.SortFunc(programs, func(a, b platform.DiscoveredProgram) int {
		return cmp.Compare(a.Name, b.Name)
	})

	// If no loadable programs found, return an appropriate error
	if len(programs) == 0 {
		return nil, fmt.Errorf("no programs found in object file")
	}

	return programs, nil
}

// ExtractAttachFunc extracts the attach function from an ELF section name.
// For example, "fentry/vfs_read" returns "vfs_read".
func ExtractAttachFunc(sectionName string) string {
	// Remove optional program marking prefix
	sectionName = strings.TrimPrefix(sectionName, "?")

	// Section format is "type/function" (e.g., "fentry/vfs_read")
	if _, after, ok := strings.Cut(sectionName, "/"); ok {
		return after
	}
	return ""
}

// InferProgramType returns the program type based on the ELF section name.
// This follows the Rust bpfman approach of deriving the type from bytecode
// metadata rather than relying on user-specified types.
//
// Section name patterns (from cilium/ebpf elf_sections.go):
//   - kprobe/*, kprobe.multi/* -> kprobe
//   - kretprobe/*, kretprobe.multi/* -> kretprobe
//   - uprobe/*, uprobe.multi/* -> uprobe
//   - uretprobe/*, uretprobe.multi/* -> uretprobe
//   - tracepoint/* -> tracepoint
//   - xdp*, xdp.frags* -> xdp
//   - tc, classifier/* -> tc
//   - tcx/* -> tcx
//   - fentry/* -> fentry
//   - fexit/* -> fexit
func InferProgramType(sectionName string) bpfman.ProgramType {
	return inferProgramType(sectionName)
}

// ValidatePrograms checks that all specified program names exist in the
// given object file. Returns an error listing any missing programs along
// with the available programs in the object file.
func ValidatePrograms(objectPath string, programNames []string) error {
	if len(programNames) == 0 {
		return nil
	}

	f, err := os.Open(objectPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return ValidateProgramsFromReader(f, programNames)
}

// ValidateProgramsFromReader is the io.ReaderAt-based form of
// ValidatePrograms.
func ValidateProgramsFromReader(rd io.ReaderAt, programNames []string) error {
	if len(programNames) == 0 {
		return nil
	}

	collSpec, err := ebpf.LoadCollectionSpecFromReader(rd)
	if err != nil {
		return fmt.Errorf("load collection spec: %w", err)
	}

	// Build set of available program names
	available := make(map[string]bool)
	for name := range collSpec.Programs {
		available[name] = true
	}

	// Check each requested program
	var missing []string
	for _, name := range programNames {
		if !available[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		slices.Sort(missing)
		availableList := slices.Sorted(maps.Keys(available))
		return fmt.Errorf("program(s) not found in object file: %v; available programs: %v", missing, availableList)
	}

	return nil
}
