package bpfman

import (
	"fmt"
	"strings"
)

// xdpActionNameToInt32 maps XDP action names to their kernel int32
// codes. Values correspond to XDP_* return codes from the kernel,
// plus dispatcher_return (31) used by the bpfman dispatcher.
var xdpActionNameToInt32 = map[string]int32{
	"aborted":           0,
	"drop":              1,
	"pass":              2,
	"tx":                3,
	"redirect":          4,
	"dispatcher_return": 31,
}

// ParseXDPAction parses a string into an XDP action int32 code
// (case-insensitive).
func ParseXDPAction(s string) (int32, error) {
	code, ok := xdpActionNameToInt32[strings.TrimSpace(strings.ToLower(s))]
	if !ok {
		return 0, fmt.Errorf("unknown XDP action %q", s)
	}
	return code, nil
}

// ParseXDPActions parses a slice of XDP action strings into int32
// codes.
func ParseXDPActions(actions []string) ([]int32, error) {
	result := make([]int32, 0, len(actions))
	for _, s := range actions {
		code, err := ParseXDPAction(s)
		if err != nil {
			return nil, err
		}
		result = append(result, code)
	}
	return result, nil
}
