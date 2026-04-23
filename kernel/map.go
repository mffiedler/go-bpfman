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
	Flags      uint32  `json:"flags"`

	// BTF.
	// Has* fields carry the kernel-version-availability discriminator; when
	// HasX is false, X is not trustworthy regardless of its zero value.
	BTFId    uint32 `json:"btf_id"`
	HasBTFId bool   `json:"has_btf_id"` // Whether BTF ID is available (kernel 4.18+)

	// MapExtra is an opaque field whose meaning is map-specific.
	// Available from kernel 5.16.
	MapExtra    uint64 `json:"map_extra"`
	HasMapExtra bool   `json:"has_map_extra"`

	// Memory and state
	Memlock    uint64 `json:"memlock"`
	HasMemlock bool   `json:"has_memlock"` // Whether Memlock is available (kernel 4.10+)
	Frozen     bool   `json:"frozen"`      // Whether map was frozen (kernel 5.2+)
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
	Programs []PinnedProgram `json:"programs"` // [] when the directory has no programs
	Maps     []PinnedMap     `json:"maps"`     // [] when the directory has no maps
}

// LoadResult contains the result of loading a program via CLI.
type LoadResult struct {
	Program PinnedProgram `json:"program"`
	Maps    []PinnedMap   `json:"maps"` // [] when the program has no maps
	PinDir  string        `json:"pin_dir"`
}
