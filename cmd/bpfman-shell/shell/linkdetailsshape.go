package shell

import "sync"

// Per-kind Link details shape registry. The Link domain type's
// Details field is a sealed interface (LinkDetails), so reflection
// over bpfman.Link cannot tell which concrete struct any specific
// link carries -- the static checker registers a wildcard for that
// field and field-typo checks stop walking when they reach it.
//
// `bpfman link attach <kind> ...` knows the concrete details type
// at static-check time: the second positional after "attach" names
// the kind (xdp, tc, tcx, tracepoint, kprobe, uprobe, fentry,
// fexit). The cmd/bpfman-shell init code reflects each concrete
// details struct into a Shape and registers it here keyed by that
// word. inferBpfmanBindShape consults this registry to compose a
// kind-aware Link Shape with record.details replaced by the
// reflected one, so a typo inside details (e.g. $l.record.details.priroity)
// fails --check rather than slipping through to runtime.
//
// The registry is keyed by the attach-kind word, not by an
// OriginKind, because the concrete details type is implementation
// detail of the bpfman package and the shell layer only sees the
// command's spelling. A miss returns the generic Link shape so
// scripts that bind from `bpfman link get` / `link list` (where
// the kind is not known statically) still type-check at the
// top level and only lose the deep details check.

var (
	linkDetailsShapesMu sync.RWMutex
	linkDetailsShapes   = map[string]Shape{}
)

// RegisterLinkDetailsShape installs s as the Shape for the
// record.details field when the bind RHS is `bpfman link attach
// <kind> ...`. cmd/bpfman-shell reflects each concrete LinkDetails
// implementer into a Shape and registers it here.
func RegisterLinkDetailsShape(kind string, s Shape) {
	linkDetailsShapesMu.Lock()
	defer linkDetailsShapesMu.Unlock()
	linkDetailsShapes[kind] = s
}

// LookupLinkDetailsShape returns the registered details shape for
// the named attach kind, or (zero, false) if none is registered.
// Used by inferBpfmanBindShape to specialise the Link Shape it
// returns for `bpfman link attach <kind>` bind RHSes.
func LookupLinkDetailsShape(kind string) (Shape, bool) {
	linkDetailsShapesMu.RLock()
	defer linkDetailsShapesMu.RUnlock()
	s, ok := linkDetailsShapes[kind]
	return s, ok
}

// linkShapeWithDetails returns a deep copy of the generic OriginLink
// Shape with record.details replaced by detailsShape. Cloning is
// required because Shape.Fields is a map header; mutating a shared
// instance would leak the substitution back into the kind registry.
//
// Returns the unmodified generic Link Shape when the expected
// record / details structure is absent (e.g. test environments
// that have not installed the reflection-derived OriginLink shape).
func linkShapeWithDetails(detailsShape Shape) Shape {
	link := cloneShape(KindShape(OriginLink))
	record, ok := link.Fields["record"]
	if !ok {
		return link
	}
	if _, ok := record.Fields["details"]; !ok {
		return link
	}
	record.Fields["details"] = detailsShape
	link.Fields["record"] = record
	return link
}

// cloneShape deep-copies a Shape so that mutations to nested
// Fields or Elem maps do not leak back to a shared registry entry.
// The fields and elem of the returned shape are fresh maps /
// pointers; their nested Shape values are themselves clones.
func cloneShape(s Shape) Shape {
	out := Shape{Sealed: s.Sealed, Kind: s.Kind}
	if s.Fields != nil {
		out.Fields = make(map[string]Shape, len(s.Fields))
		for k, v := range s.Fields {
			out.Fields[k] = cloneShape(v)
		}
	}
	if s.Elem != nil {
		e := cloneShape(*s.Elem)
		out.Elem = &e
	}
	return out
}
