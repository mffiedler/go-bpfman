// Static-check kind shapes for the bpfman domain types.
//
// The shell package's static checker (shell/check.go) carries a
// per-kind Shape registry that names the valid fields of each
// sealed type. Plain shell-level kinds (Job, command result,
// scalar, bool, null) are populated by the shell package itself;
// the richer domain types -- programs, links -- live in the
// top-level bpfman package and would create an import cycle if
// the shell package referenced them directly. Instead, this file
// reflects over the actual Go types via JSON tags at process
// init and pushes the resulting Shape trees into the registry.
//
// The JSON shape is the public surface: every field with a json
// tag goes straight to the user's $prog.foo path-walks, so
// deriving the registry from the same tags the JSON encoder
// honours keeps it in lockstep with the type definitions.

package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/shell"
)

// typeToOriginKind maps Go types whose Values get tagged with a
// known OriginKind back to that kind, so the reflector can stamp
// the right kind on nested fields. Without this, a Program's
// Status.Links slice element would carry OriginUnknown even
// though the runtime wraps each entry as a Link Value: the
// reflector sees the Go type bpfman.Link but has no way to know
// it corresponds to OriginLink. Populating the map alongside
// RegisterShape keeps the schema and the kind table in lockstep.
var typeToOriginKind = map[reflect.Type]shell.OriginKind{
	reflect.TypeOf(bpfman.Program{}): shell.OriginProgram,
	reflect.TypeOf(bpfman.Link{}):    shell.OriginLink,
}

func init() {
	shell.RegisterShape(shell.OriginProgram, shapeFromType(reflect.TypeOf(bpfman.Program{}), shell.OriginProgram))
	shell.RegisterShape(shell.OriginLink, shapeFromType(reflect.TypeOf(bpfman.Link{}), shell.OriginLink))

	// Pure builtins whose handlers live in this command. Tag
	// each entry with its arity (number of primary args the
	// parser consumes in expression position) and the Shape
	// it returns. jq is the only intrinsic the shell package
	// registers from its own init (see shell/purebuiltin.go)
	// because shell-package tests assert on its check-time
	// shape; everything else lands here.
	shell.RegisterPureBuiltin("u32le", 1, shell.KindShape(shell.OriginScalar))
	shell.RegisterPureBuiltin("u64le", 1, shell.KindShape(shell.OriginScalar))
}

// shapeFromType reflects t into a Shape tree honouring JSON
// tags. kind is the OriginKind to attach at the root; nested
// shapes default to OriginUnknown for their leaf kind because
// the reflector has no way to know that, say, bpfman.Link inside
// a Program's Status.Links slice should carry OriginLink (cross-
// type kind tagging would require mapping Go types to
// OriginKinds, which is the next layer up if we ever want it).
//
// Types that implement json.Marshaler are treated as unsealed:
// their JSON output bypasses reflection on the struct fields,
// so the shape we'd derive does not match what the user sees.
// Maps and interfaces are also unsealed because either every
// string key is valid (map) or the dynamic type is unknown at
// schema time (interface).
func shapeFromType(t reflect.Type, kind shell.OriginKind) shell.Shape {
	return buildShape(t, map[reflect.Type]bool{}, kind)
}

func buildShape(t reflect.Type, seen map[reflect.Type]bool, kind shell.OriginKind) shell.Shape {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// Cross-type tagging: a nested field whose Go type has a
	// dedicated OriginKind gets that kind, regardless of the
	// caller-supplied default. Program.Status.Links elements
	// thus carry OriginLink even though the Status struct knew
	// nothing about it; downstream path walks consult the
	// resulting Shape's Kind for further inference.
	if mapped, ok := typeToOriginKind[t]; ok && kind == shell.OriginUnknown {
		kind = mapped
	}
	if t == reflect.TypeOf(time.Time{}) {
		return shell.Shape{Sealed: true, Kind: shell.OriginScalar}
	}
	if t == reflect.TypeOf(json.Number("")) {
		return shell.Shape{Sealed: true, Kind: shell.OriginScalar}
	}
	if implementsJSONMarshaler(t) {
		return shell.Shape{Sealed: false, Kind: kind}
	}
	if seen[t] {
		return shell.Shape{Sealed: false, Kind: kind}
	}
	switch t.Kind() {
	case reflect.Struct:
		seen[t] = true
		defer delete(seen, t)
		fields := map[string]shell.Shape{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, omit := jsonFieldName(f)
			if omit {
				continue
			}
			child := buildShape(f.Type, seen, shell.OriginUnknown)
			// encoding/json flattens any anonymous embedded
			// struct that lacks an explicit json tag, regardless
			// of the Go field name jsonFieldName falls back to.
			// Test the raw tag, not the resolved name, so an
			// embed like `kernel.Map` (no tag) flattens the same
			// way the JSON encoder will at runtime.
			if f.Anonymous && f.Tag.Get("json") == "" {
				if child.Sealed {
					for k, v := range child.Fields {
						fields[k] = v
					}
				}
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = child
		}
		return shell.Shape{Sealed: true, Kind: kind, Fields: fields}
	case reflect.Slice, reflect.Array:
		elem := buildShape(t.Elem(), seen, shell.OriginUnknown)
		return shell.Shape{Sealed: false, Kind: kind, Elem: &elem}
	case reflect.Map, reflect.Interface:
		return shell.Shape{Sealed: false, Kind: shell.OriginMap}
	case reflect.Bool:
		return shell.Shape{Sealed: true, Kind: shell.OriginBool}
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return shell.Shape{Sealed: true, Kind: shell.OriginScalar}
	}
	return shell.Shape{Sealed: false, Kind: kind}
}

// jsonFieldName returns the JSON name a struct field marshals
// under and reports whether the field is omitted. A "-" tag
// means omitted entirely; an empty tag means use the Go field
// name verbatim (matching encoding/json's default).
func jsonFieldName(f reflect.StructField) (name string, omit bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	if tag == "" {
		return f.Name, false
	}
	if comma := strings.Index(tag, ","); comma >= 0 {
		tag = tag[:comma]
	}
	return tag, false
}

var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()

// implementsJSONMarshaler reports whether t (or *t) implements
// json.Marshaler. Pointer receivers are considered too because a
// type whose pointer receiver is a marshaler still surfaces a
// custom JSON shape when the value is reachable as &v at marshal
// time.
func implementsJSONMarshaler(t reflect.Type) bool {
	if t.Implements(jsonMarshalerType) {
		return true
	}
	return reflect.PointerTo(t).Implements(jsonMarshalerType)
}
