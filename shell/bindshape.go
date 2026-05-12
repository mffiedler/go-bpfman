package shell

import "sync"

// Bind-shape registry: the static checker queries this to learn what
// primary Shape a `<-` (or `=`-on-effectful) bind RHS produces. The
// shell package owns the registry surface so check.go can consult it
// without importing cmd/bpfman-shell; cmd/bpfman-shell calls
// RegisterBindShape once per effectful builtin at init time,
// mirroring the pattern RegisterPureBuiltin already establishes.
//
// Why the registry exists at all: before it, inferBindShape carried
// a hand-maintained switch (start -> Job, exec/wait/kill -> result,
// file -> Unknown, net -> inferNetBindShape, default -> result). Any
// new effectful builtin had to (1) define its handler in
// cmd/bpfman-shell, (2) register itself in the builtin map for the
// dispatcher, and (3) add a case here to teach the static checker
// the return shape. Forgetting (3) produced a confusing
// "field 'path' does not exist (valid: code, killed, ok, ...)"
// error because the unknown name fell through to the result default.
//
// With the registry, (3) collapses into the registry entry next to
// (1) + (2). Adding a builtin is one place.

// BindShapeFn computes the primary Shape a bind RHS produces given
// the arguments after the command name. Most builtins ignore the
// args and return a fixed Shape; subcommand-aware builtins (net,
// bpfman if it were registered) inspect the first arg to decide.
type BindShapeFn func(args []Expr) Shape

// StaticBindShape wraps a fixed Shape as a BindShapeFn for builtins
// whose return shape does not depend on argument values. Cuts the
// closure boilerplate at registration sites.
func StaticBindShape(s Shape) BindShapeFn {
	return func([]Expr) Shape { return s }
}

var (
	bindShapeRegistryMu sync.RWMutex
	bindShapeRegistry   = map[string]BindShapeFn{}
)

// RegisterBindShape installs fn as the bind-shape provider for the
// named command. Overwrites any prior entry under the same name.
// cmd/bpfman-shell calls this for every entry in its builtinRegistry
// that produces a typed primary; the rest fall through to the
// inferBindShape default (an external-subprocess result).
func RegisterBindShape(name string, fn BindShapeFn) {
	bindShapeRegistryMu.Lock()
	defer bindShapeRegistryMu.Unlock()
	bindShapeRegistry[name] = fn
}

// UnregisterBindShape removes name from the registry. Used by tests
// that register a throwaway entry under t.Cleanup.
func UnregisterBindShape(name string) {
	bindShapeRegistryMu.Lock()
	defer bindShapeRegistryMu.Unlock()
	delete(bindShapeRegistry, name)
}

// LookupBindShape reports the registered shape provider for name,
// if any. The checker calls this on the first word of a bind RHS
// before falling through to its remaining hardcoded cases.
func LookupBindShape(name string) (BindShapeFn, bool) {
	bindShapeRegistryMu.RLock()
	defer bindShapeRegistryMu.RUnlock()
	fn, ok := bindShapeRegistry[name]
	return fn, ok
}
