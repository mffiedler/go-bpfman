package bpfman

import (
	"fmt"
	"strings"
)

// TCAction represents a TC action return code.
// These correspond to TC_ACT_* values from the kernel.
// It is an opaque value; the only valid instances are the
// package-level variables or ParseTCAction.
type TCAction struct{ v string }

var (
	TCActionUnspec           = TCAction{"unspec"}
	TCActionOK               = TCAction{"ok"}
	TCActionReclassify       = TCAction{"reclassify"}
	TCActionShot             = TCAction{"shot"}
	TCActionPipe             = TCAction{"pipe"}
	TCActionStolen           = TCAction{"stolen"}
	TCActionQueued           = TCAction{"queued"}
	TCActionRepeat           = TCAction{"repeat"}
	TCActionRedirect         = TCAction{"redirect"}
	TCActionTrap             = TCAction{"trap"}
	TCActionDispatcherReturn = TCAction{"dispatcher_return"}
)

// tcActionToInt32 maps TCAction values to their kernel int32 codes.
var tcActionToInt32 = map[TCAction]int32{
	TCActionUnspec:           -1,
	TCActionOK:               0,
	TCActionReclassify:       1,
	TCActionShot:             2,
	TCActionPipe:             3,
	TCActionStolen:           4,
	TCActionQueued:           5,
	TCActionRepeat:           6,
	TCActionRedirect:         7,
	TCActionTrap:             8,
	TCActionDispatcherReturn: 30,
}

// int32ToTCAction maps kernel int32 codes to TCAction values.
var int32ToTCAction = func() map[int32]TCAction {
	m := make(map[int32]TCAction, len(tcActionToInt32))
	for action, code := range tcActionToInt32 {
		m[code] = action
	}
	return m
}()

// allTCActions is the canonical list of valid TC actions.
var allTCActions = []TCAction{
	TCActionUnspec,
	TCActionOK,
	TCActionReclassify,
	TCActionShot,
	TCActionPipe,
	TCActionStolen,
	TCActionQueued,
	TCActionRepeat,
	TCActionRedirect,
	TCActionTrap,
	TCActionDispatcherReturn,
}

func (a TCAction) String() string               { return a.v }
func (a TCAction) MarshalText() ([]byte, error) { return []byte(a.v), nil }

func (a *TCAction) UnmarshalText(b []byte) error {
	parsed, err := ParseTCAction(string(b))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// Int32 returns the kernel int32 code for this TC action.
func (a TCAction) Int32() int32 {
	return tcActionToInt32[a]
}

// ParseTCAction parses a string into a TCAction (case-insensitive).
func ParseTCAction(s string) (TCAction, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "unspec":
		return TCActionUnspec, nil
	case "ok":
		return TCActionOK, nil
	case "reclassify":
		return TCActionReclassify, nil
	case "shot":
		return TCActionShot, nil
	case "pipe":
		return TCActionPipe, nil
	case "stolen":
		return TCActionStolen, nil
	case "queued":
		return TCActionQueued, nil
	case "repeat":
		return TCActionRepeat, nil
	case "redirect":
		return TCActionRedirect, nil
	case "trap":
		return TCActionTrap, nil
	case "dispatcher_return":
		return TCActionDispatcherReturn, nil
	default:
		return TCAction{}, fmt.Errorf("unknown TC action %q", s)
	}
}

// TCActionNames returns all valid TC action names as strings.
func TCActionNames() []string {
	names := make([]string, len(allTCActions))
	for i, a := range allTCActions {
		names[i] = a.v
	}
	return names
}

// ParseTCActions parses a slice of TC action strings into int32 codes.
func ParseTCActions(actions []string) ([]TCAction, error) {
	result := make([]TCAction, 0, len(actions))
	for _, s := range actions {
		a, err := ParseTCAction(s)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, nil
}

// TCActionCodes converts TC actions to kernel int32 codes.
func TCActionCodes(actions []TCAction) []int32 {
	result := make([]int32, len(actions))
	for i, action := range actions {
		result[i] = action.Int32()
	}
	return result
}

// TCActionFromInt32 converts a kernel int32 code to a TCAction.
// Returns the zero value and an error if the code is not recognised.
func TCActionFromInt32(code int32) (TCAction, error) {
	if a, ok := int32ToTCAction[code]; ok {
		return a, nil
	}
	return TCAction{}, fmt.Errorf("unknown TC action code %d", code)
}

// TCActionToString converts a TC action int32 value to its string name.
func TCActionToString(action int32) string {
	if a, ok := int32ToTCAction[action]; ok {
		return a.v
	}
	return fmt.Sprintf("unknown(%d)", action)
}

// TCActionsToString converts a slice of TC action values to a
// comma-separated string.
func TCActionsToString(actions []int32) string {
	if len(actions) == 0 {
		return "None"
	}
	names := make([]string, len(actions))
	for i, a := range actions {
		names[i] = TCActionToString(a)
	}
	return strings.Join(names, ", ")
}
