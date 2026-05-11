package shell

import "sync"

// Pure builtins are deterministic value-producing functions: no
// subprocess, no kernel state, no captured-result envelope. The
// classic examples are `u32le` (int -> hex string), `u64le`, `jq`
// (filter+value -> projection), and `range` (int -> sequence).
//
// The shell package needs to know which names are pure for two
// reasons:
//
//  1. Static-check shape inference (shell/check.go). A bind RHS
//     whose first word is a pure builtin produces the builtin's
//     declared return Shape, not the default captured-envelope
//     shape.
//
//  2. Expression-position invocation (shell/parse.go,
//     shell/expr.go). The parser dispatches a bare identifier in
//     expression position to a PureCallExpr when the name is
//     registered, consuming the registered arity of primary
//     expressions as arguments.
//
// The handler itself lives in cmd/bpfman-shell (the dispatcher
// wires it through ExecBind). The shell package only needs to
// know the name, the arity, and the return Shape; that is what
// the registry stores. cmd/bpfman-shell calls RegisterPureBuiltin
// once per pure entry at init time, mirroring the pattern the
// RegisterShape API already establishes for domain-type schemas.

// PureBuiltin describes one entry in the pure-builtin registry.
//
//	Arity       number of positional primary arguments the call
//	            consumes in expression position. The parser
//	            takes exactly this many primaries; the static
//	            checker validates the same count at the bind path.
//	ReturnShape the Shape the call produces, used by the static
//	            checker to propagate types into downstream let
//	            bindings ('let x = u32le N' -> x is OriginScalar).
type PureBuiltin struct {
	Name        string
	Arity       int
	ReturnShape Shape
}

// pureBuiltinRegistry is the source of truth for the pure-builtin
// set. The parser, the checker, and any handler dispatch consult
// it; init-time registrations from cmd/bpfman-shell populate it
// before tests run, and tests register additional throwaway names
// at runtime under t.Cleanup. The mutex makes both paths safe
// when the test runner parallelises:
//
//   - RegisterPureBuiltin / UnregisterPureBuiltin take the write
//     lock so additions and cleanups serialise against each
//     other and against readers.
//   - LookupPureBuiltin takes the read lock so concurrent parses
//     and checks see a consistent snapshot.
//
// Without this guard, a parallel parse calling LookupPureBuiltin
// races against a test cleanup deleting a temporary entry, which
// the Go race detector flags and which can corrupt the map under
// load.
var (
	pureBuiltinRegistryMu sync.RWMutex
	pureBuiltinRegistry   = map[string]PureBuiltin{}
)

func init() {
	// jq is the threading and structured-data primitive the
	// language is defined against; the shell-package tests
	// assert on its check-time shape so the registration must
	// be visible without linking cmd/bpfman-shell. The handler
	// itself lives in cmd/bpfman-shell; the registry only
	// records the contract (name, arity, return Shape). jq's
	// return is unsealed-unknown because a filter can project
	// anything; downstream path checks fall back to the
	// permissive wildcard.
	RegisterPureBuiltin("jq", 2, Shape{Sealed: false, Kind: OriginUnknown})
}

// RegisterPureBuiltin installs name as a pure builtin with the
// given arity and return Shape. Overwrites any prior entry under
// the same name. Mirrors RegisterShape: the shell package stays
// free of cmd-side imports while still letting the parser and
// checker consult an authoritative source of truth.
func RegisterPureBuiltin(name string, arity int, returnShape Shape) {
	pureBuiltinRegistryMu.Lock()
	defer pureBuiltinRegistryMu.Unlock()
	pureBuiltinRegistry[name] = PureBuiltin{
		Name:        name,
		Arity:       arity,
		ReturnShape: returnShape,
	}
}

// UnregisterPureBuiltin removes name from the registry. Tests
// that register a throwaway entry under t.Cleanup must use this
// helper rather than `delete(pureBuiltinRegistry, name)` so the
// removal serialises with concurrent lookups under
// pureBuiltinRegistryMu.
func UnregisterPureBuiltin(name string) {
	pureBuiltinRegistryMu.Lock()
	defer pureBuiltinRegistryMu.Unlock()
	delete(pureBuiltinRegistry, name)
}

// LookupPureBuiltin reports the registration for name, if any.
// The parser calls this on each bare identifier in expression
// position to decide whether to start a PureCallExpr; the
// checker calls it on the first word of a bind RHS to read the
// return Shape.
func LookupPureBuiltin(name string) (PureBuiltin, bool) {
	pureBuiltinRegistryMu.RLock()
	defer pureBuiltinRegistryMu.RUnlock()
	pb, ok := pureBuiltinRegistry[name]
	return pb, ok
}
