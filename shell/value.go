package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Value wraps a JSON-compatible dynamic value for use as a shell
// variable. The underlying representation is one of: map[string]any,
// []any, string, json.Number, bool, or nil.
//
// When created via ValueFromStruct, the original Go value is
// preserved in the origin field so that callers can recover type
// information that the JSON round-trip erases.
//
// The kind field declares what the Value represents (see OriginKind).
// Producers set it at construction time via WithKind; consumers
// check it via ExpectOrigin. OriginUnknown is the default and acts
// as a wildcard for origin-less values (e.g. JSON parsed without
// explicit tagging, map literals, path-lookup results).
type Value struct {
	v      any        // JSON-decoded tree (map[string]any, etc.)
	origin any        // original Go value, nil for non-struct values
	kind   OriginKind // declared origin kind, OriginUnknown by default
}

// ValueFromMap wraps an existing map as a Value.
func ValueFromMap(data map[string]any) Value {
	return Value{v: data}
}

// ValueFromAny wraps an arbitrary JSON-compatible value as a
// Value. Unlike ValueFromJSON it does not parse anything; it
// stores x directly.  Suitable for integration points (e.g. gojq)
// that already produce Go-native types matching the Value's
// internal vocabulary (map[string]any, []any, string, json.Number,
// float64, bool, nil).  Callers that know the domain kind should
// chain WithKind.
func ValueFromAny(x any) Value {
	return Value{v: x}
}

// ValueFromJSON decodes JSON bytes into a Value. Numbers are
// preserved as json.Number to avoid float64 precision loss. The
// resulting Value has OriginUnknown; callers that need a declared
// kind (e.g. the json parse builtin) should chain WithKind.
func ValueFromJSON(b []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Value{}, fmt.Errorf("decode JSON: %w", err)
	}
	if dec.More() {
		return Value{}, fmt.Errorf("decode JSON: trailing data after value")
	}
	return Value{v: v}, nil
}

// ValueFromStruct converts a struct to a Value via JSON round-trip,
// preserving the original Go value for type checking. The resulting
// Value has OriginUnknown; callers that know the domain kind should
// chain WithKind (e.g. WithKind(OriginProgram) for a program record).
func ValueFromStruct(s any) (Value, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return Value{}, fmt.Errorf("marshal struct: %w", err)
	}
	v, err := ValueFromJSON(b)
	if err != nil {
		return Value{}, err
	}
	v.origin = s
	return v, nil
}

// Origin returns the original Go value if this Value was created
// from a struct via ValueFromStruct. Returns nil otherwise.
func (v Value) Origin() any {
	return v.origin
}

// Kind returns the declared origin kind. OriginUnknown is the
// default for values whose producer did not tag them.
func (v Value) Kind() OriginKind {
	return v.kind
}

// WithKind returns a copy of the Value with its origin kind set to
// k. Use at the construction site: `shell.ValueFromStruct(p).WithKind(shell.OriginProgram)`.
func (v Value) WithKind(k OriginKind) Value {
	v.kind = k
	return v
}

// StringValue wraps a plain string as a Value with OriginScalar.
func StringValue(s string) Value {
	return Value{v: s, kind: OriginScalar}
}

// BoolValue wraps a boolean as a Value with OriginBool.
func BoolValue(b bool) Value {
	return Value{v: b, kind: OriginBool}
}

// IsNil reports whether the underlying value is nil.
func (v Value) IsNil() bool {
	return v.v == nil
}

// IsScalar reports whether the value is a scalar (string, number,
// bool) rather than a structured type (map, slice) or nil.
func (v Value) IsScalar() bool {
	switch v.v.(type) {
	case string, json.Number, float64, bool:
		return true
	default:
		return false
	}
}

// IsStructured reports whether the value is a map or slice.
func (v Value) IsStructured() bool {
	switch v.v.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

// Raw returns the underlying dynamic value.
func (v Value) Raw() any {
	return v.v
}

// Keys returns the navigable children of the value. For maps it
// returns sorted field names; for arrays it returns index strings
// ("[0]", "[1]", ...); for scalars and nil it returns nil.
func (v Value) Keys() []string {
	switch x := v.v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	case []any:
		keys := make([]string, len(x))
		for i := range x {
			keys[i] = fmt.Sprintf("[%d]", i)
		}
		return keys
	default:
		return nil
	}
}

// Scalar converts a scalar value to its string representation.
// It handles string, json.Number, float64, and bool. It returns
// an error for nil, map, and slice values.
func (v Value) Scalar() (string, error) {
	switch x := v.v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	case nil:
		return "", fmt.Errorf("value is null")
	default:
		return "", fmt.Errorf("value is not a scalar")
	}
}

// walkPath walks the parsed path steps into the raw value, returning
// the value at the end of the path and the human-readable traversed
// path for error messages. varName seeds the traversed path.
func walkPath(raw any, varName string, steps []pathStep) (any, string, error) {
	current := raw
	traversed := varName

	for _, step := range steps {
		switch s := step.(type) {
		case fieldStep:
			m, ok := current.(map[string]any)
			if !ok {
				if current == nil {
					return nil, traversed, fmt.Errorf("variable %s is null", traversed)
				}
				return nil, traversed, fmt.Errorf("cannot access field %s on non-object in variable %s", s.name, traversed)
			}
			val, exists := m[s.name]
			if !exists {
				return nil, traversed, fmt.Errorf("field %s not found in variable %s", s.name, traversed)
			}
			current = val
			if traversed == varName {
				traversed = varName + "." + s.name
			} else {
				traversed = traversed + "." + s.name
			}

		case indexStep:
			arr, ok := current.([]any)
			if !ok {
				return nil, traversed, fmt.Errorf("cannot index non-array in variable %s", traversed)
			}
			if s.index < 0 || s.index >= len(arr) {
				return nil, traversed, fmt.Errorf("index %d out of range for variable %s (length %d)", s.index, traversed, len(arr))
			}
			current = arr[s.index]
			traversed = fmt.Sprintf("%s[%d]", traversed, s.index)
		}
	}

	return current, traversed, nil
}

// LookupValue walks a dotted field path (with optional [n] indexing)
// into the value and returns whatever is found, including structured
// types and nil. The varName parameter is used only for error
// messages. An empty path returns the value itself.
func (v Value) LookupValue(varName, path string) (Value, error) {
	if path == "" {
		return v, nil
	}

	steps, err := parsePath(path)
	if err != nil {
		return Value{}, err
	}

	current, _, err := walkPath(v.v, varName, steps)
	if err != nil {
		return Value{}, err
	}

	return Value{v: current}, nil
}

// Lookup walks a dotted field path (with optional [n] indexing) into
// the value. The varName parameter is used only for error messages.
// An empty path returns the value itself. Unlike LookupValue, Lookup
// rejects structured and nil results, enforcing scalar access.
func (v Value) Lookup(varName, path string) (Value, error) {
	if path == "" {
		return v, nil
	}

	steps, err := parsePath(path)
	if err != nil {
		return Value{}, err
	}

	current, traversed, err := walkPath(v.v, varName, steps)
	if err != nil {
		return Value{}, err
	}

	if current == nil {
		return Value{}, fmt.Errorf("variable %s is null", traversed)
	}

	switch current.(type) {
	case map[string]any:
		return Value{}, fmt.Errorf("variable %s is an object; use field access to reach a scalar value", traversed)
	case []any:
		return Value{}, fmt.Errorf("variable %s is an array; use indexing to reach a scalar value", traversed)
	}

	return Value{v: current}, nil
}

// pathStep is either a field name or an array index.
type pathStep interface {
	pathStep()
}

type fieldStep struct {
	name string
}

type indexStep struct {
	index int
}

func (fieldStep) pathStep() {}
func (indexStep) pathStep() {}

// parsePath parses a dotted path with optional [n] indexing into a
// sequence of steps. For example, "maps[0].name" becomes
// [fieldStep{"maps"}, indexStep{0}, fieldStep{"name"}].
func parsePath(path string) ([]pathStep, error) {
	var steps []pathStep
	i := 0
	for i < len(path) {
		// Skip leading dot.
		if path[i] == '.' {
			i++
			if i >= len(path) {
				break
			}
		}

		if path[i] == '[' {
			// Parse array index.
			j := strings.IndexByte(path[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("unterminated [ in path %q", path)
			}
			numStr := path[i+1 : i+j]
			n, err := strconv.Atoi(numStr)
			if err != nil {
				return nil, fmt.Errorf("invalid index in path %q: %w", path, err)
			}
			steps = append(steps, indexStep{index: n})
			i += j + 1
		} else {
			// Parse field name.
			j := i
			for j < len(path) && path[j] != '.' && path[j] != '[' {
				j++
			}
			steps = append(steps, fieldStep{name: path[i:j]})
			i = j
		}
	}
	return steps, nil
}

// RenderValue produces the byte representation of a Value suitable
// for writing to a file. Scalar strings are written verbatim (no
// trailing newline added). Numbers, booleans, and null are rendered
// as their text forms. Structured values (maps, slices) are rendered
// as deterministic pretty-printed JSON with sorted keys, two-space
// indentation, and a trailing newline after the final bracket.
func RenderValue(v Value) ([]byte, error) {
	switch x := v.v.(type) {
	case string:
		return []byte(x), nil
	case json.Number:
		return []byte(x.String()), nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'f', -1, 64)), nil
	case bool:
		if x {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case nil:
		return []byte("null"), nil
	default:
		// Structured: map or slice.
		b, err := json.MarshalIndent(v.v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("render value: %w", err)
		}
		return append(b, '\n'), nil
	}
}
