package dispatcher

import "fmt"

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
