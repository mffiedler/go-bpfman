package ebpf

import (
	"time"

	"github.com/cilium/ebpf"

	"github.com/frobware/go-bpfman/kernel"
)

// ToKernelProgram converts a cilium/ebpf ProgramInfo to a kernel.Program.
func ToKernelProgram(info *ebpf.ProgramInfo) *kernel.Program {
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

	var mapIDsTyped []kernel.MapID
	if hasMapIDs && len(mapIDs) > 0 {
		mapIDsTyped = make([]kernel.MapID, len(mapIDs))
		for i, mid := range mapIDs {
			mapIDsTyped[i] = kernel.MapID(mid)
		}
	}

	return &kernel.Program{
		ID:                   kernel.ProgramID(id),
		Name:                 info.Name,
		ProgramType:          kernel.NewProgramType(info.Type.String()),
		Tag:                  info.Tag,
		LoadedAt:             loadedAt,
		UID:                  0,     // Not available from ProgramInfo
		HasUID:               false, // Not available from ProgramInfo
		BTFId:                uint32(btfID),
		HasBTFId:             hasBTFID,
		MapIDs:               mapIDsTyped,
		HasMapIDs:            hasMapIDs,
		JitedSize:            jitedSize,
		XlatedSize:           uint32(xlatedSize),
		VerifiedInstructions: verifiedInsns,
		Memlock:              memlock,
		HasMemlock:           hasMemlock,
		Restricted:           false, // Not available from ProgramInfo
	}
}
