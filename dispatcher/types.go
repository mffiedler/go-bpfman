package dispatcher

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// DispatcherType represents the type of dispatcher (XDP or TC).
// The unexported field prevents construction of invalid values; use the
// package-level variables or ParseDispatcherType.
type DispatcherType struct{ v string }

var (
	DispatcherTypeXDP       = DispatcherType{"xdp"}
	DispatcherTypeTCIngress = DispatcherType{"tc-ingress"}
	DispatcherTypeTCEgress  = DispatcherType{"tc-egress"}
)

func (d DispatcherType) String() string               { return d.v }
func (d DispatcherType) MarshalText() ([]byte, error) { return []byte(d.v), nil }

func (d *DispatcherType) UnmarshalText(b []byte) error {
	parsed, err := ParseDispatcherType(string(b))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// TCParentHandle returns the netlink parent handle for a TC
// dispatcher type.
func TCParentHandle(dispType DispatcherType) uint32 {
	switch dispType {
	case DispatcherTypeTCIngress:
		return netlink.HANDLE_MIN_INGRESS
	case DispatcherTypeTCEgress:
		return netlink.HANDLE_MIN_EGRESS
	default:
		return 0
	}
}

// ChainCallShift returns the bit shift to apply to proceed-on
// bitmasks before writing them to the BPF dispatcher's
// chain_call_actions array. TC dispatchers check (1 << (ret + 1))
// instead of (1 << ret) to accommodate TC_ACT_UNSPEC = -1, so TC
// bitmasks must be shifted left by 1. XDP uses (1 << ret) directly.
func (d DispatcherType) ChainCallShift() uint {
	switch d {
	case DispatcherTypeTCIngress, DispatcherTypeTCEgress:
		return 1
	default:
		return 0
	}
}

// ParseDispatcherType parses a string into a DispatcherType.
func ParseDispatcherType(s string) (DispatcherType, error) {
	switch s {
	case "xdp":
		return DispatcherTypeXDP, nil
	case "tc-ingress":
		return DispatcherTypeTCIngress, nil
	case "tc-egress":
		return DispatcherTypeTCEgress, nil
	default:
		return DispatcherType{}, fmt.Errorf("unknown dispatcher type %q", s)
	}
}
