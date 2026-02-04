package bpfman

// AttachTarget identifies where a network-based attachment occurs.
type AttachTarget struct {
	IfIndex int    `json:"ifindex"`
	NetNS   string `json:"netns,omitempty"` // path (e.g. /proc/<pid>/ns/net) or empty for current
}
