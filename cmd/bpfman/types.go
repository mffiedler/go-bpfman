package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/frobware/go-bpfman"
)

// ProgramID wraps a uint32 kernel program ID with hex support.
type ProgramID struct {
	Value uint32
}

// ParseProgramID parses a program ID from string, supporting hex (0x) prefix
// and optional "program/" prefix for k8s-style resource references.
func ParseProgramID(s string) (ProgramID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ProgramID{}, fmt.Errorf("program ID cannot be empty")
	}

	// Strip optional program/ prefix
	s = strings.TrimPrefix(s, "program/")

	var val uint64
	var err error

	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		val, err = strconv.ParseUint(s[2:], 16, 32)
	} else {
		val, err = strconv.ParseUint(s, 10, 32)
	}

	if err != nil {
		return ProgramID{}, fmt.Errorf("invalid program ID %q: %w", s, err)
	}

	return ProgramID{Value: uint32(val)}, nil
}

// LinkID wraps a uint32 kernel link ID with hex support.
type LinkID struct {
	Value uint32
}

// ParseLinkID parses a link ID from string, supporting hex (0x) prefix
// and optional "link/" prefix for k8s-style resource references.
func ParseLinkID(s string) (LinkID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return LinkID{}, fmt.Errorf("link ID cannot be empty")
	}

	// Strip optional link/ prefix
	s = strings.TrimPrefix(s, "link/")

	var val uint64
	var err error

	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		val, err = strconv.ParseUint(s[2:], 16, 32)
	} else {
		val, err = strconv.ParseUint(s, 10, 32)
	}

	if err != nil {
		return LinkID{}, fmt.Errorf("invalid link ID %q: %w", s, err)
	}

	return LinkID{Value: uint32(val)}, nil
}

// KeyValue represents a KEY=VALUE metadata pair.
type KeyValue struct {
	Key   string
	Value string
}

// ParseKeyValue parses a KEY=VALUE string.
func ParseKeyValue(s string) (KeyValue, error) {
	idx := strings.Index(s, "=")
	if idx <= 0 {
		return KeyValue{}, fmt.Errorf("invalid format %q: expected KEY=VALUE", s)
	}

	key := strings.TrimSpace(s[:idx])
	if key == "" {
		return KeyValue{}, fmt.Errorf("invalid format %q: key cannot be empty", s)
	}

	return KeyValue{
		Key:   key,
		Value: s[idx+1:],
	}, nil
}

// GlobalData represents a NAME=HEX global data pair.
type GlobalData struct {
	Name string
	Data []byte
}

// ParseGlobalData parses a NAME=HEX string.
func ParseGlobalData(s string) (GlobalData, error) {
	idx := strings.Index(s, "=")
	if idx <= 0 {
		return GlobalData{}, fmt.Errorf("invalid format %q: expected NAME=HEX", s)
	}

	name := strings.TrimSpace(s[:idx])
	if name == "" {
		return GlobalData{}, fmt.Errorf("invalid format %q: name cannot be empty", s)
	}

	hexStr := strings.TrimSpace(s[idx+1:])
	// Remove optional 0x prefix
	hexStr = strings.TrimPrefix(hexStr, "0x")
	hexStr = strings.TrimPrefix(hexStr, "0X")

	data, err := hex.DecodeString(hexStr)
	if err != nil {
		return GlobalData{}, fmt.Errorf("invalid hex data for %q: %w", name, err)
	}

	return GlobalData{
		Name: name,
		Data: data,
	}, nil
}

// ObjectPath wraps a path to a BPF object file, validated for existence.
type ObjectPath struct {
	Path string
}

// ParseObjectPath parses and validates that the file exists.
func ParseObjectPath(s string) (ObjectPath, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ObjectPath{}, fmt.Errorf("object path cannot be empty")
	}

	info, err := os.Stat(s)
	if err != nil {
		if os.IsNotExist(err) {
			return ObjectPath{}, fmt.Errorf("object file %q does not exist", s)
		}
		return ObjectPath{}, fmt.Errorf("cannot access object file %q: %w", s, err)
	}

	if info.IsDir() {
		return ObjectPath{}, fmt.Errorf("object path %q is a directory, not a file", s)
	}

	return ObjectPath{Path: s}, nil
}

// MetadataMap converts a slice of KeyValue to a map.
func MetadataMap(kvs []KeyValue) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

// GlobalDataMap converts a slice of GlobalData to a map.
func GlobalDataMap(gds []GlobalData) map[string][]byte {
	if len(gds) == 0 {
		return nil
	}
	m := make(map[string][]byte, len(gds))
	for _, gd := range gds {
		m[gd.Name] = gd.Data
	}
	return m
}

// ProgramSpec represents a TYPE:NAME[:ATTACH_FUNC] program specification for loading.
// For fentry and fexit, the attach function is required.
type ProgramSpec struct {
	Type       bpfman.ProgramType // Validated program type
	Name       string             // Program name within the ELF
	AttachFunc string             // Attach function (required for fentry/fexit)
}

// ParseProgramSpec parses a TYPE:NAME or TYPE:NAME:ATTACH_FUNC string.
// Examples:
//   - "xdp:xdp_pass"
//   - "tc:stats"
//   - "fentry:test_fentry:do_unlinkat"
//
// The type is validated against known program types at parse time.
// For fentry and fexit, the attach function (third component) is required.
func ParseProgramSpec(s string) (ProgramSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ProgramSpec{}, fmt.Errorf("program spec cannot be empty")
	}

	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return ProgramSpec{}, fmt.Errorf("invalid program spec %q: expected TYPE:NAME format (e.g., xdp:my_prog)", s)
	}

	typeStr := strings.TrimSpace(parts[0])
	progName := strings.TrimSpace(parts[1])
	var attachFunc string
	if len(parts) == 3 {
		attachFunc = strings.TrimSpace(parts[2])
	}

	if typeStr == "" {
		return ProgramSpec{}, fmt.Errorf("invalid program spec %q: type cannot be empty", s)
	}
	if progName == "" {
		return ProgramSpec{}, fmt.Errorf("invalid program spec %q: name cannot be empty", s)
	}

	progType, ok := bpfman.ParseProgramType(typeStr)
	if !ok {
		return ProgramSpec{}, fmt.Errorf("invalid program spec %q: unknown type %q (valid: xdp, tc, tcx, tracepoint, kprobe, kretprobe, uprobe, uretprobe, fentry, fexit)", s, typeStr)
	}

	// Validate fentry/fexit require attach function
	if (progType == bpfman.ProgramTypeFentry || progType == bpfman.ProgramTypeFexit) && attachFunc == "" {
		return ProgramSpec{}, fmt.Errorf("invalid program spec %q: %s requires attach function (format: %s:FUNC_NAME:ATTACH_FUNC)", s, typeStr, typeStr)
	}

	return ProgramSpec{
		Type:       progType,
		Name:       progName,
		AttachFunc: attachFunc,
	}, nil
}

// ImagePullPolicy represents a CLI-friendly pull policy string.
type ImagePullPolicy struct {
	Value string
}

// ParseImagePullPolicy parses a pull policy string.
func ParseImagePullPolicy(s string) (ImagePullPolicy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ImagePullPolicy{Value: "IfNotPresent"}, nil
	}

	switch strings.ToLower(s) {
	case "always", "ifnotpresent", "never":
		return ImagePullPolicy{Value: s}, nil
	default:
		return ImagePullPolicy{}, fmt.Errorf("invalid pull policy %q: must be Always, IfNotPresent, or Never", s)
	}
}

// TCAction represents a TC action return code.
// These correspond to TC_ACT_* values from the kernel.
type TCAction int32

const (
	TCActionUnspec           TCAction = -1
	TCActionOK               TCAction = 0
	TCActionReclassify       TCAction = 1
	TCActionShot             TCAction = 2
	TCActionPipe             TCAction = 3
	TCActionStolen           TCAction = 4
	TCActionQueued           TCAction = 5
	TCActionRepeat           TCAction = 6
	TCActionRedirect         TCAction = 7
	TCActionTrap             TCAction = 8
	TCActionDispatcherReturn TCAction = 30 // bpfman-specific sentinel value
)

// tcActionNames maps string names to TC action values.
var tcActionNames = map[string]TCAction{
	"unspec":            TCActionUnspec,
	"ok":                TCActionOK,
	"reclassify":        TCActionReclassify,
	"shot":              TCActionShot,
	"pipe":              TCActionPipe,
	"stolen":            TCActionStolen,
	"queued":            TCActionQueued,
	"repeat":            TCActionRepeat,
	"redirect":          TCActionRedirect,
	"trap":              TCActionTrap,
	"dispatcher_return": TCActionDispatcherReturn,
}

// ParseTCAction parses a TC action string (case-insensitive).
func ParseTCAction(s string) (TCAction, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if action, ok := tcActionNames[s]; ok {
		return action, nil
	}
	return 0, fmt.Errorf("invalid TC action %q: valid values are unspec, ok, reclassify, shot, pipe, stolen, queued, repeat, redirect, trap, dispatcher_return", s)
}

// ParseTCActions parses a slice of TC action strings.
func ParseTCActions(actions []string) ([]int32, error) {
	result := make([]int32, 0, len(actions))
	for _, a := range actions {
		action, err := ParseTCAction(a)
		if err != nil {
			return nil, err
		}
		result = append(result, int32(action))
	}
	return result, nil
}

// TCDirection represents the direction for TC attachment.
type TCDirection string

const (
	TCDirectionIngress TCDirection = "ingress"
	TCDirectionEgress  TCDirection = "egress"
)

// ParseTCDirection parses a TC direction string.
func ParseTCDirection(s string) (TCDirection, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "ingress":
		return TCDirectionIngress, nil
	case "egress":
		return TCDirectionEgress, nil
	default:
		return "", fmt.Errorf("invalid TC direction %q: must be ingress or egress", s)
	}
}

// tcActionToString maps TC action values to human-readable names.
var tcActionToString = map[TCAction]string{
	TCActionUnspec:           "unspec",
	TCActionOK:               "ok",
	TCActionReclassify:       "reclassify",
	TCActionShot:             "shot",
	TCActionPipe:             "pipe",
	TCActionStolen:           "stolen",
	TCActionQueued:           "queued",
	TCActionRepeat:           "repeat",
	TCActionRedirect:         "redirect",
	TCActionTrap:             "trap",
	TCActionDispatcherReturn: "dispatcher_return",
}

// TCActionToString converts a TC action int32 value to its string name.
func TCActionToString(action int32) string {
	if name, ok := tcActionToString[TCAction(action)]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", action)
}

// TCActionsToString converts a slice of TC action values to a comma-separated string.
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

// ResourceKind identifies the type of BPF resource.
type ResourceKind string

const (
	ResourceKindLink    ResourceKind = "link"
	ResourceKindProgram ResourceKind = "program"
)

// ResourceRef is a k8s-style resource reference (e.g., "link/123", "program/456").
type ResourceRef struct {
	Kind ResourceKind
	ID   uint32
}

// String returns the canonical string form (e.g., "link/123").
func (r ResourceRef) String() string {
	return fmt.Sprintf("%s/%d", r.Kind, r.ID)
}

// UnmarshalText implements encoding.TextUnmarshaler for Kong parsing.
func (r *ResourceRef) UnmarshalText(text []byte) error {
	parsed, err := ParseResourceRef(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// ParseResourceRef parses a resource reference in the form "kind/id".
// Valid kinds are "link" and "program".
func ParseResourceRef(s string) (ResourceRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ResourceRef{}, fmt.Errorf("resource reference cannot be empty")
	}

	idx := strings.Index(s, "/")
	if idx <= 0 {
		return ResourceRef{}, fmt.Errorf("invalid resource reference %q: expected kind/id (e.g., link/123, program/456)", s)
	}

	kindStr := s[:idx]
	idStr := s[idx+1:]

	var kind ResourceKind
	switch kindStr {
	case "link":
		kind = ResourceKindLink
	case "program":
		kind = ResourceKindProgram
	default:
		return ResourceRef{}, fmt.Errorf("invalid resource kind %q: must be 'link' or 'program'", kindStr)
	}

	var val uint64
	var err error

	if strings.HasPrefix(idStr, "0x") || strings.HasPrefix(idStr, "0X") {
		val, err = strconv.ParseUint(idStr[2:], 16, 32)
	} else {
		val, err = strconv.ParseUint(idStr, 10, 32)
	}

	if err != nil {
		return ResourceRef{}, fmt.Errorf("invalid resource ID in %q: %w", s, err)
	}

	return ResourceRef{Kind: kind, ID: uint32(val)}, nil
}

// ParseProgramTypes parses a slice of program type strings (case-insensitive).
// Returns a set for O(1) lookup during filtering.
func ParseProgramTypes(types []string) (map[bpfman.ProgramType]struct{}, error) {
	result := make(map[bpfman.ProgramType]struct{}, len(types))
	for _, raw := range types {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		pt, ok := bpfman.ParseProgramType(strings.ToLower(t))
		if !ok {
			return nil, fmt.Errorf("unknown program type %q (valid: %s)",
				raw, strings.Join(bpfman.ProgramTypeNames(), ", "))
		}
		result[pt] = struct{}{}
	}
	return result, nil
}

// ParseProgramTypesSlice parses a slice of program type strings (case-insensitive).
// Returns a slice of ProgramType values.
func ParseProgramTypesSlice(types []string) ([]bpfman.ProgramType, error) {
	var result []bpfman.ProgramType
	for _, raw := range types {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		pt, ok := bpfman.ParseProgramType(strings.ToLower(t))
		if !ok {
			return nil, fmt.Errorf("unknown program type %q (valid: %s)",
				raw, strings.Join(bpfman.ProgramTypeNames(), ", "))
		}
		result = append(result, pt)
	}
	return result, nil
}

// ParseLinkKindsSlice parses a slice of link kind strings (case-insensitive).
// Returns a slice of LinkKind values.
func ParseLinkKindsSlice(kinds []string) ([]bpfman.LinkKind, error) {
	var result []bpfman.LinkKind
	for _, raw := range kinds {
		k := strings.TrimSpace(raw)
		if k == "" {
			continue
		}
		kind, ok := bpfman.ParseLinkKind(strings.ToLower(k))
		if !ok {
			return nil, fmt.Errorf("unknown link kind %q (valid: %s)",
				raw, strings.Join(bpfman.LinkKindNames(), ", "))
		}
		result = append(result, kind)
	}
	return result, nil
}
