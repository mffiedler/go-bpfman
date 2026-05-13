// Bpfman-CLI-specific bind-shape registration. The shell package
// provides the mechanism -- RegisterBindShape(name, fn), a Shape
// registry keyed by OriginKind, and CloneShape for safe composition
// -- but knows nothing about "bpfman", "program", "link", or any
// other domain verb. This file is where that policy lives: at init
// time it registers a BindShapeFn under "bpfman" that recognises
// the program / link subcommand grammar and returns the right Shape
// for the bind RHS. Adding a new bpfman subcommand or a new
// LinkDetails implementer is a code change in this file alone,
// never in shell/.

package main

import (
	bpfman "github.com/frobware/go-bpfman"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// linkDetailsShapes caches the reflection-derived Shape for each
// attach-subcommand keyword (xdp / tc / tcx / tracepoint / kprobe
// / uprobe / fentry / fexit). The dispatch from keyword to
// concrete Go type lives in bpfman.LinkAttachKindDetailsType
// alongside LinkRecord.UnmarshalJSON; mirroring it here would
// drift. Populated once at init via that single source of truth;
// read-only thereafter, so unsynchronised reads from
// inferBpfmanBindShape are safe.
//
// The bind-shape handler overlays the cached Shape onto the
// generic Link Shape's record.details slot so deep field-typo
// checks against record.details fall under the concrete
// schema instead of stopping at the polymorphic LinkDetails
// interface.
var linkDetailsShapes = map[string]shell.Shape{}

func init() {
	for _, kind := range bpfman.LinkAttachKinds() {
		t := bpfman.LinkAttachKindDetailsType(kind)
		if t == nil {
			continue
		}
		linkDetailsShapes[kind] = shapeFromType(t, shell.OriginUnknown)
	}
	shell.RegisterBindShape("bpfman", inferBpfmanBindShape)
}

// inferBpfmanBindShape recognises the bpfman subcommands whose
// primary slot binds a typed domain record or a list of one:
//
//	bpfman program load ...   -> {programs: [Program]} (one entry per --programs)
//	bpfman program get ...    -> Program
//	bpfman program list       -> list of Program
//	bpfman link attach <kind> -> Link with record.details specialised by <kind>
//	bpfman link get ...       -> Link (generic; kind not known statically)
//	bpfman link list          -> list of Link (generic)
//
// List shapes are returned as an unsealed Shape carrying an Elem
// that points at the registered Program / Link Shape, so a path
// like $progs[0].record.program_id descends through Elem to the
// Program shape and validates against its sealed fields.
//
// Load returns a sealed object with a single recognised field
// `programs` whose elements are Program shapes; this matches the
// runtime contract that load wraps result.Programs as a
// LoadResult so callers can address every loaded program rather
// than silently using only the first.
//
// link attach specialises the generic Link Shape: args[2] names
// the attach kind, which selects the concrete LinkDetails shape
// to overlay onto record.details. Field-path validation then
// reaches inside details against the kind-specific field set
// rather than stopping at the polymorphic interface boundary.
func inferBpfmanBindShape(args []shell.Expr) shell.Shape {
	if len(args) < 2 {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	noun, ok := args[0].(*shell.LiteralExpr)
	if !ok || noun.Quoted {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	verb, ok := args[1].(*shell.LiteralExpr)
	if !ok || verb.Quoted {
		return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
	}
	switch noun.Text {
	case "program":
		switch verb.Text {
		case "load":
			elem := shell.KindShape(shell.OriginProgram)
			return shell.Shape{
				Sealed: true,
				Kind:   shell.OriginUnknown,
				Fields: map[string]shell.Shape{
					"programs": {Sealed: false, Kind: shell.OriginUnknown, Elem: &elem},
				},
			}
		case "get":
			return shell.KindShape(shell.OriginProgram)
		case "list":
			elem := shell.KindShape(shell.OriginProgram)
			return shell.Shape{Sealed: false, Kind: shell.OriginUnknown, Elem: &elem}
		}
	case "link":
		switch verb.Text {
		case "attach":
			if len(args) >= 3 {
				if kind, ok := args[2].(*shell.LiteralExpr); ok && !kind.Quoted {
					if details, ok := linkDetailsShapes[kind.Text]; ok {
						return linkShapeWithDetails(details)
					}
				}
			}
			return shell.KindShape(shell.OriginLink)
		case "get":
			return shell.KindShape(shell.OriginLink)
		case "list":
			elem := shell.KindShape(shell.OriginLink)
			return shell.Shape{Sealed: false, Kind: shell.OriginUnknown, Elem: &elem}
		}
	}
	return shell.Shape{Sealed: false, Kind: shell.OriginUnknown}
}

// linkShapeWithDetails returns a deep copy of the generic
// OriginLink Shape with record.details replaced by detailsShape.
// Cloning is required because Shape.Fields is a map header;
// mutating a shared registry entry's nested maps would leak the
// per-kind substitution to every consumer of the generic Link
// Shape.
//
// Returns the unmodified generic Link Shape when the expected
// record / details structure is absent (e.g. tests that have not
// installed the reflection-derived OriginLink shape).
func linkShapeWithDetails(detailsShape shell.Shape) shell.Shape {
	link := shell.CloneShape(shell.KindShape(shell.OriginLink))
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
