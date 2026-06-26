package bpfman

// AttachTarget identifies where a network-based attachment occurs.
type AttachTarget struct {
	// IfIndex is the index of the target network interface.
	IfIndex int `json:"ifindex"`

	// NetNS is the path to the target network namespace (e.g.
	// /proc/<pid>/ns/net); empty means the current netns.
	NetNS string `json:"netns"`
}
