package kernel

import "time"

// Program represents a BPF program loaded in the kernel.
// This is read from the kernel - we don't create these, we observe them.
//
// All fields from cilium/ebpf's ProgramInfo are captured here. Some fields
// may be zero/empty if the kernel version doesn't support them or if
// permissions restrict access. Optional field availability is indicated
// by the Has* fields where relevant.
type Program struct {
	// Core identity
	ID          ProgramID   `json:"id"`
	Name        string      `json:"name"`
	ProgramType ProgramType `json:"program_type"`
	Tag         string      `json:"tag,omitempty"`
	LoadedAt    time.Time   `json:"loaded_at"`

	// Ownership and BTF
	UID      uint32 `json:"uid"`
	HasUID   bool   `json:"has_uid,omitempty"` // Whether UID is available (kernel 4.15+)
	BTFId    uint32 `json:"btf_id,omitempty"`
	HasBTFId bool   `json:"has_btf_id,omitempty"` // Whether BTF ID is available (kernel 5.0+)

	// Associated maps
	MapIDs    []MapID `json:"map_ids,omitempty"`
	HasMapIDs bool    `json:"has_map_ids,omitempty"` // Whether MapIDs is available (kernel 4.15+)

	// Size information
	JitedSize            uint32 `json:"jited_size,omitempty"`
	XlatedSize           uint32 `json:"xlated_size,omitempty"`
	VerifiedInstructions uint32 `json:"verified_insns,omitempty"`

	// Memory
	Memlock    uint64 `json:"memlock,omitempty"`
	HasMemlock bool   `json:"has_memlock,omitempty"` // Whether Memlock is available (kernel 4.10+)

	// Access restrictions
	// Restricted is true if kernel address information is restricted by
	// kernel.kptr_restrict and/or net.core.bpf_jit_harden sysctls.
	Restricted bool `json:"restricted,omitempty"`

	// GPLCompatible is true if the program was loaded with a GPL-compatible
	// license. This is captured from the ELF at load time, not from the kernel.
	// Only populated for programs loaded by bpfman, not for enumerated programs.
	GPLCompatible bool `json:"gpl_compatible,omitempty"`
}

// PinnedProgram represents a BPF program pinned on the filesystem.
// Used for CLI output when scanning bpffs directories.
type PinnedProgram struct {
	ID         ProgramID   `json:"id"`
	Name       string      `json:"name"`
	Type       ProgramType `json:"type"`
	Tag        string      `json:"tag,omitempty"`
	PinnedPath string      `json:"pinned_path"`
	MapIDs     []MapID     `json:"map_ids,omitempty"`
}
