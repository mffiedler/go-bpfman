package compute

import (
	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// FilterPrograms returns programs matching the predicate.
// Pure function.
func FilterPrograms(
	programs []kernel.Program,
	predicate func(kernel.Program) bool,
) []kernel.Program {
	var result []kernel.Program
	for _, p := range programs {
		if predicate(p) {
			result = append(result, p)
		}
	}
	return result
}

// FilterByType returns programs of the specified type.
// Pure function.
func FilterByType(programs []kernel.Program, programType kernel.ProgramType) []kernel.Program {
	return FilterPrograms(programs, func(p kernel.Program) bool {
		return p.ProgramType == programType
	})
}

// FilterByName returns programs matching the name.
// Pure function.
func FilterByName(programs []kernel.Program, name string) []kernel.Program {
	return FilterPrograms(programs, func(p kernel.Program) bool {
		return p.Name == name
	})
}

// FilterMetadata returns metadata matching the predicate.
// Pure function.
func FilterMetadata(
	metadata map[uint32]bpfman.ProgramRecord,
	predicate func(uint32, bpfman.ProgramRecord) bool,
) map[uint32]bpfman.ProgramRecord {
	result := make(map[uint32]bpfman.ProgramRecord)
	for id, m := range metadata {
		if predicate(id, m) {
			result[id] = m
		}
	}
	return result
}

// FilterByOwner returns metadata for the specified owner.
// Pure function.
func FilterByOwner(metadata map[uint32]bpfman.ProgramRecord, owner string) map[uint32]bpfman.ProgramRecord {
	return FilterMetadata(metadata, func(_ uint32, m bpfman.ProgramRecord) bool {
		return m.Meta.Owner == owner
	})
}
