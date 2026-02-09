package ebpf

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// generateSyntheticLinkID creates a unique ID for perf_event-based links that
// lack kernel link IDs (e.g., container uprobes on kernels < 5.15). IDs are
// generated in the range 0x80000000-0xFFFFFFFF to avoid collision with real
// kernel link IDs which are small sequential numbers.
func generateSyntheticLinkID() uint32 {
	// Generate random ID in high range (SyntheticLinkIDBase+)
	return bpfman.SyntheticLinkIDBase | rand.Uint32()
}

// inferProgramType returns the program type based on the ELF section name.
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
func inferProgramType(sectionName string) bpfman.ProgramType {
	// Remove optional program marking prefix
	sectionName = strings.TrimPrefix(sectionName, "?")

	switch {
	case strings.HasPrefix(sectionName, "kretprobe"):
		return bpfman.ProgramTypeKretprobe
	case strings.HasPrefix(sectionName, "kprobe"):
		return bpfman.ProgramTypeKprobe
	case strings.HasPrefix(sectionName, "uretprobe"):
		return bpfman.ProgramTypeUretprobe
	case strings.HasPrefix(sectionName, "uprobe"):
		return bpfman.ProgramTypeUprobe
	case strings.HasPrefix(sectionName, "tracepoint"):
		return bpfman.ProgramTypeTracepoint
	case strings.HasPrefix(sectionName, "fentry"):
		return bpfman.ProgramTypeFentry
	case strings.HasPrefix(sectionName, "fexit"):
		return bpfman.ProgramTypeFexit
	case strings.HasPrefix(sectionName, "xdp"):
		return bpfman.ProgramTypeXDP
	case strings.HasPrefix(sectionName, "tcx"):
		return bpfman.ProgramTypeTCX
	case strings.HasPrefix(sectionName, "tc") || strings.HasPrefix(sectionName, "classifier"):
		return bpfman.ProgramTypeTC
	default:
		return bpfman.ProgramTypeUnspecified
	}
}

// bootTime returns the system boot time by reading /proc/stat.
// Falls back to time.Now() if /proc/stat cannot be read.
func bootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Now()
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			var btime int64
			if _, err := fmt.Sscanf(line, "btime %d", &btime); err == nil {
				return time.Unix(btime, 0)
			}
		}
	}
	return time.Now()
}

func infoToProgram(info *ebpf.ProgramInfo, id uint32) kernel.Program {
	kp := kernel.Program{
		ID:          id,
		Name:        info.Name,
		ProgramType: kernel.NewProgramType(info.Type.String()),
		Tag:         info.Tag,
	}

	// Map IDs (available from kernel 4.15)
	if ebpfMapIDs, ok := info.MapIDs(); ok {
		kp.HasMapIDs = true
		kp.MapIDs = make([]uint32, len(ebpfMapIDs))
		for i, mid := range ebpfMapIDs {
			kp.MapIDs[i] = uint32(mid)
		}
	}

	// UID (available from kernel 4.15)
	if uid, ok := info.CreatedByUID(); ok {
		kp.UID = uid
		kp.HasUID = true
	}

	// Load time (available from kernel 4.15)
	if loadTime, ok := info.LoadTime(); ok {
		// LoadTime is nanoseconds since boot, convert to wall clock time
		kp.LoadedAt = bootTime().Add(loadTime)
	}

	// BTF ID (available from kernel 5.0)
	if btfID, ok := info.BTFID(); ok {
		kp.BTFId = uint32(btfID)
		kp.HasBTFId = true
	}

	// JITed size (available from kernel 4.13)
	// Error indicates restricted or unsupported
	if jitedSize, err := info.JitedSize(); err == nil {
		kp.JitedSize = jitedSize
	}

	// Translated size (available from kernel 4.13)
	// Error indicates restricted or unsupported
	if xlatedSize, err := info.TranslatedSize(); err == nil {
		kp.XlatedSize = uint32(xlatedSize)
	} else if errors.Is(err, ebpf.ErrRestrictedKernel) {
		kp.Restricted = true
	}

	// Verified instructions (available from kernel 5.16)
	if verifiedInsns, ok := info.VerifiedInstructions(); ok {
		kp.VerifiedInstructions = verifiedInsns
	}

	// Memory locked (available from kernel 4.10)
	if memlock, ok := info.Memlock(); ok {
		kp.Memlock = memlock
		kp.HasMemlock = true
	}

	return kp
}

func infoToMap(info *ebpf.MapInfo, id uint32) kernel.Map {
	km := kernel.Map{
		ID:         id,
		Name:       info.Name,
		MapType:    kernel.NewMapType(info.Type.String()),
		KeySize:    info.KeySize,
		ValueSize:  info.ValueSize,
		MaxEntries: info.MaxEntries,
		Flags:      info.Flags,
		Frozen:     info.Frozen(),
	}

	// BTF ID (available from kernel 4.18)
	if btfID, ok := info.BTFID(); ok {
		km.BTFId = uint32(btfID)
		km.HasBTFId = true
	}

	// MapExtra (available from kernel 5.16)
	if mapExtra, ok := info.MapExtra(); ok {
		km.MapExtra = mapExtra
		km.HasMapExtra = true
	}

	// Memlock (available from kernel 4.10)
	if memlock, ok := info.Memlock(); ok {
		km.Memlock = memlock
		km.HasMemlock = true
	}

	return km
}

func infoToLink(info *link.Info) kernel.Link {
	kl := kernel.Link{
		ID:        uint32(info.ID),
		ProgramID: uint32(info.Program),
		LinkType:  linkTypeString(info.Type),
	}

	// Extract type-specific info where available.
	if tracing := info.Tracing(); tracing != nil {
		kl.AttachType = fmt.Sprintf("%d", tracing.AttachType)
		kl.TargetObjID = tracing.TargetObjId
		kl.TargetBTFId = uint32(tracing.TargetBtfId)
	}

	if xdp := info.XDP(); xdp != nil {
		kl.Ifindex = xdp.Ifindex
	}

	if tcx := info.TCX(); tcx != nil {
		kl.AttachType = fmt.Sprintf("%d", tcx.AttachType)
		kl.Ifindex = tcx.Ifindex
	}

	if cgroup := info.Cgroup(); cgroup != nil {
		kl.AttachType = fmt.Sprintf("%d", cgroup.AttachType)
		kl.CgroupID = cgroup.CgroupId
	}

	if netns := info.NetNs(); netns != nil {
		kl.AttachType = fmt.Sprintf("%d", netns.AttachType)
		kl.NetnsIno = netns.NetnsIno
	}

	if netkit := info.Netkit(); netkit != nil {
		kl.AttachType = fmt.Sprintf("%d", netkit.AttachType)
		kl.Ifindex = netkit.Ifindex
	}

	if netfilter := info.Netfilter(); netfilter != nil {
		kl.NetfilterPf = netfilter.Pf
		kl.NetfilterHooknum = netfilter.Hooknum
		kl.NetfilterPriority = netfilter.Priority
		kl.NetfilterFlags = netfilter.Flags
	}

	if kprobeMulti := info.KprobeMulti(); kprobeMulti != nil {
		if count, ok := kprobeMulti.AddressCount(); ok {
			kl.KprobeMultiCount = count
		}
		if flags, ok := kprobeMulti.Flags(); ok {
			kl.KprobeMultiFlags = flags
		}
		if missed, ok := kprobeMulti.Missed(); ok {
			kl.KprobeMultiMissed = missed
		}
	}

	if perfEvent := info.PerfEvent(); perfEvent != nil {
		if kprobeInfo := perfEvent.Kprobe(); kprobeInfo != nil {
			if addr, ok := kprobeInfo.Address(); ok {
				kl.KprobeAddress = addr
			}
			if missed, ok := kprobeInfo.Missed(); ok {
				kl.KprobeMissed = missed
			}
		}
	}

	return kl
}

// linkTypeString converts a link.Type to a human-readable string.
func linkTypeString(t link.Type) string {
	// These values come from include/uapi/linux/bpf.h (BPF_LINK_TYPE_*)
	names := map[link.Type]string{
		0:  "unspec",
		1:  "raw_tracepoint",
		2:  "tracing",
		3:  "cgroup",
		4:  "iter",
		5:  "netns",
		6:  "xdp",
		7:  "perf_event",
		8:  "kprobe_multi",
		9:  "struct_ops",
		10: "netfilter",
		11: "tcx",
		12: "uprobe_multi",
		13: "netkit",
	}
	if name, ok := names[t]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", t)
}
