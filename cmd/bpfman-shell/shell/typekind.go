package shell

import (
	"reflect"
	"strings"
	"sync"
)

// Per-type OriginKind registry. The Shape registry maps each
// OriginKind to its static field schema; this one maps the
// concrete Go types those Values may be backed by to the same
// OriginKind. The two together let path access through a
// typed Value propagate both the underlying Go origin and the
// declared kind, so capability dispatch keeps working after
// sub-values are extracted ($prog = $loaded.programs[0]).
//
// The shell package owns the registry surface; cmd-side init
// populates it (bpfman.Program -> OriginProgram, etc.) without
// the shell package depending on bpfman domain types.

var (
	typeKindMu sync.RWMutex
	typeKind   = map[reflect.Type]OriginKind{}
)

// RegisterTypeKind installs k as the OriginKind for values whose
// underlying Go type is t. cmd/bpfman-shell calls this at init
// for the bpfman domain types: bpfman.Program / Link / ProgramRecord
// / LinkRecord -> OriginProgram / OriginLink. Path access through
// a Value consults this map when it walks the origin to a new
// Go type, so the resulting sub-value carries the right Kind.
func RegisterTypeKind(t reflect.Type, k OriginKind) {
	typeKindMu.Lock()
	defer typeKindMu.Unlock()
	typeKind[t] = k
}

// kindForType returns the OriginKind registered for t (or its
// pointer-to-t and elem-of-t alternatives so a *Program still
// resolves to OriginProgram), or OriginUnknown if no
// registration exists.
func kindForType(t reflect.Type) OriginKind {
	if t == nil {
		return OriginUnknown
	}
	typeKindMu.RLock()
	defer typeKindMu.RUnlock()
	if k, ok := typeKind[t]; ok {
		return k
	}
	if t.Kind() == reflect.Ptr {
		if k, ok := typeKind[t.Elem()]; ok {
			return k
		}
	}
	return OriginUnknown
}

// walkOrigin walks the Go value rooted at origin through the
// same path steps walkPath uses on the JSON-tree mirror,
// returning the sub-value at the end of the path or nil if any
// step fails (no field, index out of range, nil pointer,
// incompatible kind). The caller passes steps already parsed
// from parsePath; this mirrors walkPath's traversal so the two
// stay in lockstep.
//
// Struct fields are resolved by JSON tag (or the Go field name
// when no tag is present), matching encoding/json's marshal
// rules. Pointers and interfaces are unwrapped transparently.
// Maps and unrecognised kinds yield nil -- the caller (LookupValue)
// treats nil as "origin lost beyond this point" and continues
// with the JSON-tree walk only.
func walkOrigin(origin any, steps []pathStep) any {
	if origin == nil {
		return nil
	}
	v := reflect.ValueOf(origin)
	for _, step := range steps {
		v = unwrap(v)
		if !v.IsValid() {
			return nil
		}
		switch s := step.(type) {
		case fieldStep:
			if v.Kind() != reflect.Struct {
				return nil
			}
			f, ok := jsonFieldByName(v.Type(), s.name)
			if !ok {
				return nil
			}
			v = v.FieldByIndex(f.Index)
		case indexStep:
			if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
				return nil
			}
			if s.index < 0 || s.index >= v.Len() {
				return nil
			}
			v = v.Index(s.index)
		default:
			return nil
		}
	}
	v = unwrap(v)
	if !v.IsValid() {
		return nil
	}
	if !v.CanInterface() {
		return nil
	}
	return v.Interface()
}

// unwrap dereferences pointers and unwraps interfaces until v
// is a concrete value (or invalid). Nil pointers / interfaces
// resolve to an invalid Value, which walkOrigin treats as a
// terminal failure.
func unwrap(v reflect.Value) reflect.Value {
	for {
		switch v.Kind() {
		case reflect.Ptr, reflect.Interface:
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		default:
			return v
		}
	}
}

// jsonFieldByName finds the struct field of t whose JSON-tag
// name is name. Falls back to the Go field name when the tag
// is absent. Anonymous fields without a json tag are inlined
// (per encoding/json's flattening rule) and their fields are
// searched recursively.
func jsonFieldByName(t reflect.Type, name string) (reflect.StructField, bool) {
	if t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		jsonName, omit := parseJSONTag(tag)
		if omit {
			continue
		}
		// Anonymous fields with no json tag inline their
		// inner fields into the parent's JSON shape; recurse
		// to keep walkOrigin in lockstep with the JSON walk.
		if f.Anonymous && tag == "" {
			ft := f.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if inner, ok := jsonFieldByName(ft, name); ok {
					inner.Index = append([]int{i}, inner.Index...)
					return inner, true
				}
			}
			continue
		}
		if jsonName == "" {
			jsonName = f.Name
		}
		if jsonName == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// parseJSONTag splits a json struct tag, returning the field
// name component and whether the field is omitted entirely
// (tag == "-"). The options after the comma (omitempty etc.)
// are not relevant for name lookup.
func parseJSONTag(tag string) (name string, omit bool) {
	if tag == "-" {
		return "", true
	}
	if comma := strings.Index(tag, ","); comma >= 0 {
		return tag[:comma], false
	}
	return tag, false
}
