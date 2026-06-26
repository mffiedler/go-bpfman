package kernel

// Link represents a BPF link in the kernel.
// All fields from cilium/ebpf's link.Info are captured here, including
// type-specific information extracted from the various *Info subtypes.
//
// Fields are emitted explicitly; consumers use LinkType as the
// discriminator for which type-specific fields are meaningful. Zero on
// a field that does not apply to this link's LinkType is not a
// meaningful observation, and zero on a field that does apply is.
type Link struct {
	// Core identity
	ID        LinkID    `json:"id"`
	ProgramID ProgramID `json:"program_id"`
	LinkType  string    `json:"link_type"`

	// Type-specific fields (populated based on LinkType)

	// AttachType is set for tracing, cgroup, netns, tcx, netkit links.
	AttachType string `json:"attach_type"`

	// Ifindex is set for xdp, tcx, netkit links.
	Ifindex uint32 `json:"ifindex"`

	// TargetObjID is the target object ID for tracing links, or
	// other type-specific target identifiers.
	TargetObjID uint32 `json:"target_obj_id"`

	// TargetBTFId is the BTF type ID of the attach target for tracing links.
	TargetBTFId uint32 `json:"target_btf_id"`

	// CgroupID is set for cgroup links.
	CgroupID uint64 `json:"cgroup_id"`

	// NetnsIno is set for netns links.
	NetnsIno uint32 `json:"netns_ino"`

	// Netfilter-specific fields
	NetfilterPf       uint32 `json:"netfilter_pf"`
	NetfilterHooknum  uint32 `json:"netfilter_hooknum"`
	NetfilterPriority int32  `json:"netfilter_priority"`
	NetfilterFlags    uint32 `json:"netfilter_flags"`

	// KprobeMulti-specific fields
	KprobeMultiCount  uint32 `json:"kprobe_multi_count"`
	KprobeMultiFlags  uint32 `json:"kprobe_multi_flags"`
	KprobeMultiMissed uint64 `json:"kprobe_multi_missed"`

	// PerfEvent/Kprobe-specific fields
	KprobeAddress uint64 `json:"kprobe_address"`
	KprobeMissed  uint64 `json:"kprobe_missed"`
}
