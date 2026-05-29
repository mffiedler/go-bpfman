package bpfmancli

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/kernel"
)

// ProgramID wraps a program ID with hex support.
type ProgramID struct {
	Value kernel.ProgramID
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

	return ProgramID{Value: kernel.ProgramID(val)}, nil
}

// LinkID wraps a link ID with hex support.
type LinkID struct {
	Value kernel.LinkID
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

	return LinkID{Value: kernel.LinkID(val)}, nil
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

	progType, err := bpfman.ParseProgramType(typeStr)
	if err != nil {
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

// ParseProgramTypes parses a slice of program type strings (case-insensitive).
// Returns a set for O(1) lookup during filtering.
func ParseProgramTypes(types []string) (map[bpfman.ProgramType]struct{}, error) {
	result := make(map[bpfman.ProgramType]struct{}, len(types))
	for _, raw := range types {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		pt, err := bpfman.ParseProgramType(strings.ToLower(t))
		if err != nil {
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
		pt, err := bpfman.ParseProgramType(strings.ToLower(t))
		if err != nil {
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
		kind, err := bpfman.ParseLinkKind(strings.ToLower(k))
		if err != nil {
			return nil, fmt.Errorf("unknown link kind %q (valid: %s)",
				raw, strings.Join(bpfman.LinkKindNames(), ", "))
		}
		result = append(result, kind)
	}
	return result, nil
}
