package kernel

// Link represents a BPF link in the kernel.
// All fields from cilium/ebpf's link.Info are captured here, including
// type-specific information extracted from the various *Info subtypes.
//
// Type-specific fields are only populated when relevant to the LinkType.
type Link struct {
	// Core identity
	ID        LinkID    `json:"id"`
	ProgramID ProgramID `json:"program_id"`
	LinkType  string    `json:"link_type"`

	// Type-specific fields (populated based on LinkType)

	// AttachType is set for tracing, cgroup, netns, tcx, netkit links.
	AttachType string `json:"attach_type,omitempty"`

	// Ifindex is set for xdp, tcx, netkit links.
	Ifindex uint32 `json:"ifindex,omitempty"`

	// TargetObjID is the target object ID for tracing links, or
	// other type-specific target identifiers.
	TargetObjID uint32 `json:"target_obj_id,omitempty"`

	// TargetBTFId is the BTF type ID of the attach target for tracing links.
	TargetBTFId uint32 `json:"target_btf_id,omitempty"`

	// CgroupID is set for cgroup links.
	CgroupID uint64 `json:"cgroup_id,omitempty"`

	// NetnsIno is set for netns links.
	NetnsIno uint32 `json:"netns_ino,omitempty"`

	// Netfilter-specific fields
	NetfilterPf       uint32 `json:"netfilter_pf,omitempty"`
	NetfilterHooknum  uint32 `json:"netfilter_hooknum,omitempty"`
	NetfilterPriority int32  `json:"netfilter_priority,omitempty"`
	NetfilterFlags    uint32 `json:"netfilter_flags,omitempty"`

	// KprobeMulti-specific fields
	KprobeMultiCount  uint32 `json:"kprobe_multi_count,omitempty"`
	KprobeMultiFlags  uint32 `json:"kprobe_multi_flags,omitempty"`
	KprobeMultiMissed uint64 `json:"kprobe_multi_missed,omitempty"`

	// PerfEvent/Kprobe-specific fields
	KprobeAddress uint64 `json:"kprobe_address,omitempty"`
	KprobeMissed  uint64 `json:"kprobe_missed,omitempty"`
}
