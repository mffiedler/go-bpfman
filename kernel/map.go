package kernel

// Map represents a BPF map in the kernel.
// All fields from cilium/ebpf's MapInfo are captured here.
type Map struct {
	// Core identity and structure
	ID         MapID   `json:"id"`
	Name       string  `json:"name"`
	MapType    MapType `json:"map_type"`
	KeySize    uint32  `json:"key_size"`
	ValueSize  uint32  `json:"value_size"`
	MaxEntries uint32  `json:"max_entries"`
	Flags      uint32  `json:"flags,omitempty"`

	// BTF
	BTFId    uint32 `json:"btf_id,omitempty"`
	HasBTFId bool   `json:"has_btf_id,omitempty"` // Whether BTF ID is available (kernel 4.18+)

	// MapExtra is an opaque field whose meaning is map-specific.
	// Available from kernel 5.16.
	MapExtra    uint64 `json:"map_extra,omitempty"`
	HasMapExtra bool   `json:"has_map_extra,omitempty"`

	// Memory and state
	Memlock    uint64 `json:"memlock,omitempty"`
	HasMemlock bool   `json:"has_memlock,omitempty"` // Whether Memlock is available (kernel 4.10+)
	Frozen     bool   `json:"frozen,omitempty"`      // Whether map was frozen (kernel 5.2+)
}

// PinnedMap represents a BPF map pinned on the filesystem.
type PinnedMap struct {
	ID         MapID   `json:"id"`
	Name       string  `json:"name"`
	Type       MapType `json:"type"`
	KeySize    uint32  `json:"key_size"`
	ValueSize  uint32  `json:"value_size"`
	MaxEntries uint32  `json:"max_entries"`
	PinnedPath string  `json:"pinned_path"`
}

// PinDirContents holds all BPF objects found in a pin directory.
type PinDirContents struct {
	Programs []PinnedProgram `json:"programs,omitempty"`
	Maps     []PinnedMap     `json:"maps,omitempty"`
}

// LoadResult contains the result of loading a program via CLI.
type LoadResult struct {
	Program PinnedProgram `json:"program"`
	Maps    []PinnedMap   `json:"maps,omitempty"`
	PinDir  string        `json:"pin_dir"`
}
