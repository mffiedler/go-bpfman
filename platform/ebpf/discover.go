package ebpf

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cilium/ebpf"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/platform"
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
	collSpec, err := ebpf.LoadCollectionSpec(objectPath)
	if err != nil {
		return nil, fmt.Errorf("load collection spec: %w", err)
	}

	var programs []platform.DiscoveredProgram

	for name, progSpec := range collSpec.Programs {
		progType := InferProgramType(progSpec.SectionName)

		// Skip programs with unspecified type
		if progType == (bpfman.ProgramType{}) {
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
	sort.Slice(programs, func(i, j int) bool {
		return programs[i].Name < programs[j].Name
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
	if idx := strings.Index(sectionName, "/"); idx != -1 {
		return sectionName[idx+1:]
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

	collSpec, err := ebpf.LoadCollectionSpec(objectPath)
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
		sort.Strings(missing)
		availableList := make([]string, 0, len(available))
		for name := range available {
			availableList = append(availableList, name)
		}
		sort.Strings(availableList)
		return fmt.Errorf("program(s) not found in object file: %v; available programs: %v", missing, availableList)
	}

	return nil
}
