package bpfman

import (
	"fmt"
	"strings"
)

// XDPAction represents an XDP return code used for proceed-on
// configuration. It is opaque; construct it with ParseXDPAction.
type XDPAction struct {
	name string
	code int32
}

var (
	XDPActionAborted          = XDPAction{"aborted", 0}
	XDPActionDrop             = XDPAction{"drop", 1}
	XDPActionPass             = XDPAction{"pass", 2}
	XDPActionTX               = XDPAction{"tx", 3}
	XDPActionRedirect         = XDPAction{"redirect", 4}
	XDPActionDispatcherReturn = XDPAction{"dispatcher_return", 31}
)

// xdpActionNameToAction maps XDP action names to their domain values.
var xdpActionNameToAction = map[string]XDPAction{
	"aborted":           XDPActionAborted,
	"drop":              XDPActionDrop,
	"pass":              XDPActionPass,
	"tx":                XDPActionTX,
	"redirect":          XDPActionRedirect,
	"dispatcher_return": XDPActionDispatcherReturn,
}

func (a XDPAction) String() string               { return a.name }
func (a XDPAction) Int32() int32                 { return a.code }
func (a XDPAction) MarshalText() ([]byte, error) { return []byte(a.name), nil }

func (a *XDPAction) UnmarshalText(b []byte) error {
	parsed, err := ParseXDPAction(string(b))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// ParseXDPAction parses a string into an XDP action.
func ParseXDPAction(s string) (XDPAction, error) {
	action, ok := xdpActionNameToAction[strings.TrimSpace(strings.ToLower(s))]
	if !ok {
		return XDPAction{}, fmt.Errorf("unknown XDP action %q", s)
	}
	return action, nil
}

// ParseXDPActions parses a slice of XDP action strings into domain values.
func ParseXDPActions(actions []string) ([]XDPAction, error) {
	result := make([]XDPAction, 0, len(actions))
	for _, raw := range actions {
		action, err := ParseXDPAction(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, action)
	}
	return result, nil
}

// XDPActionCodes converts XDP actions to kernel int32 codes.
func XDPActionCodes(actions []XDPAction) []int32 {
	result := make([]int32, len(actions))
	for i, action := range actions {
		result[i] = action.Int32()
	}
	return result
}
