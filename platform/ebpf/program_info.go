package ebpf

import (
	"time"

	"github.com/cilium/ebpf"

	"github.com/frobware/go-bpfman/kernel"
)

// ToKernelProgram converts a cilium/ebpf ProgramInfo to a kernel.Program.
// The license string is used to determine GPL compatibility at load time.
func ToKernelProgram(info *ebpf.ProgramInfo, license string) *kernel.Program {
	if info == nil {
		return nil
	}

	id, _ := info.ID()
	mapIDs, hasMapIDs := info.MapIDs()
	btfID, hasBTFID := info.BTFID()
	jitedSize, _ := info.JitedSize()
	xlatedSize, _ := info.TranslatedSize()
	verifiedInsns, _ := info.VerifiedInstructions()
	memlock, hasMemlock := info.Memlock()
	loadTime, hasLoadTime := info.LoadTime()

	var loadedAt time.Time
	if hasLoadTime {
		loadedAt = bootTime().Add(loadTime)
	}

	var mapIDsU32 []uint32
	if hasMapIDs && len(mapIDs) > 0 {
		mapIDsU32 = make([]uint32, len(mapIDs))
		for i, mid := range mapIDs {
			mapIDsU32[i] = uint32(mid)
		}
	}

	return &kernel.Program{
		ID:                   uint32(id),
		Name:                 info.Name,
		ProgramType:          kernel.NewProgramType(info.Type.String()),
		Tag:                  info.Tag,
		LoadedAt:             loadedAt,
		UID:                  0,     // Not available from ProgramInfo
		HasUID:               false, // Not available from ProgramInfo
		BTFId:                uint32(btfID),
		HasBTFId:             hasBTFID,
		MapIDs:               mapIDsU32,
		HasMapIDs:            hasMapIDs,
		JitedSize:            jitedSize,
		XlatedSize:           uint32(xlatedSize),
		VerifiedInstructions: verifiedInsns,
		Memlock:              memlock,
		HasMemlock:           hasMemlock,
		Restricted:           false, // Not available from ProgramInfo
		GPLCompatible:        isGPLCompatible(license),
	}
}

// isGPLCompatible checks if a license string is GPL compatible.
// This matches the kernel's license_is_gpl_compatible() function.
func isGPLCompatible(license string) bool {
	switch license {
	case "GPL", "GPL v2", "GPL and additional rights",
		"Dual BSD/GPL", "Dual MIT/GPL", "Dual MPL/GPL":
		return true
	default:
		return false
	}
}
