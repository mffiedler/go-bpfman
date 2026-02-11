package server

import (
	"fmt"

	"github.com/frobware/go-bpfman"
	pb "github.com/frobware/go-bpfman/server/pb"
)

// protoToBpfmanType converts proto program type to bpfman type.
// Returns an error for unknown or unspecified types (parse, don't validate).
func protoToBpfmanType(pt pb.BpfmanProgramType) (bpfman.ProgramType, error) {
	switch pt {
	case pb.BpfmanProgramType_XDP:
		return bpfman.ProgramTypeXDP, nil
	case pb.BpfmanProgramType_TC:
		return bpfman.ProgramTypeTC, nil
	case pb.BpfmanProgramType_TRACEPOINT:
		return bpfman.ProgramTypeTracepoint, nil
	case pb.BpfmanProgramType_KPROBE:
		return bpfman.ProgramTypeKprobe, nil
	case pb.BpfmanProgramType_UPROBE:
		return bpfman.ProgramTypeUprobe, nil
	case pb.BpfmanProgramType_FENTRY:
		return bpfman.ProgramTypeFentry, nil
	case pb.BpfmanProgramType_FEXIT:
		return bpfman.ProgramTypeFexit, nil
	case pb.BpfmanProgramType_TCX:
		return bpfman.ProgramTypeTCX, nil
	default:
		return bpfman.ProgramType{}, fmt.Errorf("unknown program type: %d", pt)
	}
}

// bpfmanTypeToProto converts a bpfman ProgramType to its proto uint32 value.
func bpfmanTypeToProto(pt bpfman.ProgramType) uint32 {
	switch pt {
	case bpfman.ProgramTypeXDP:
		return uint32(pb.BpfmanProgramType_XDP)
	case bpfman.ProgramTypeTC:
		return uint32(pb.BpfmanProgramType_TC)
	case bpfman.ProgramTypeTracepoint:
		return uint32(pb.BpfmanProgramType_TRACEPOINT)
	case bpfman.ProgramTypeKprobe, bpfman.ProgramTypeKretprobe:
		return uint32(pb.BpfmanProgramType_KPROBE)
	case bpfman.ProgramTypeUprobe, bpfman.ProgramTypeUretprobe:
		return uint32(pb.BpfmanProgramType_UPROBE)
	case bpfman.ProgramTypeFentry:
		return uint32(pb.BpfmanProgramType_FENTRY)
	case bpfman.ProgramTypeFexit:
		return uint32(pb.BpfmanProgramType_FEXIT)
	case bpfman.ProgramTypeTCX:
		return uint32(pb.BpfmanProgramType_TCX)
	default:
		return uint32(pb.BpfmanProgramType_XDP) // zero value
	}
}

// actualTypeMetadataKey returns the metadata key used to preserve the actual
// program type when the proto enum doesn't distinguish (e.g., kretprobe vs kprobe).
func actualTypeMetadataKey(programName string) string {
	return "bpfman.io/actual-type:" + programName
}

// resolveActualType checks metadata for type hints and returns the actual
// program type. This handles kretprobe/uretprobe which map to KPROBE/UPROBE
// in the proto enum but need to be distinguished for attach semantics.
func resolveActualType(protoType bpfman.ProgramType, programName string, metadata map[string]string) bpfman.ProgramType {
	if metadata == nil {
		return protoType
	}

	key := actualTypeMetadataKey(programName)
	if actualTypeStr, ok := metadata[key]; ok {
		if actualType, err := bpfman.ParseProgramType(actualTypeStr); err == nil {
			return actualType
		}
	}

	return protoType
}

// protoToPullPolicy converts a proto image pull policy to managed type.
// Proto values: 0=Always, 1=IfNotPresent, 2=Never (matches bpfman.ImagePullPolicy iota).
func protoToPullPolicy(policy int32) bpfman.ImagePullPolicy {
	switch policy {
	case 0:
		return bpfman.PullAlways
	case 1:
		return bpfman.PullIfNotPresent
	case 2:
		return bpfman.PullNever
	default:
		return bpfman.PullIfNotPresent
	}
}
