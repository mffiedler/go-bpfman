// The zip pure builtin: 'zip A B' walks two lists in lock-step and
// produces a list of 2-element pair lists. Pairs into multi-var
// foreach destructure the elements back into named bindings so a
// script expresses parallel iteration without index bookkeeping:
//
//	foreach prio, po in (zip $priorities $proceed_ons) {
//	    bpfman link attach tc ... -p $prio --proceed-on $po $prog
//	}
//
// Arity is fixed at 2. A variadic 'zip A B C ...' would mirror
// Python / Clojure more closely but the pure-builtin registry holds
// a single arity per name; the wider form can be added when a
// concrete test demands them, alongside whatever extension of the
// registry's arity model that would imply.
//
// Length mismatch is a hard error rather than a silent truncation:
// the parallel-list pattern this primitive exists to serve carries
// an implicit "these lists are paired" invariant, so silently
// dropping the tail of the longer list would convert a bug into
// wrong-shape output. Python 3.10's zip(strict=True) made the same
// trade-off for the same reason.

package main

import (
	"fmt"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// handleZip walks two lists in lock-step and returns a list of
// 2-element pair lists.
//
//	zip [a b c] [x y z]         -> [[a x] [b y] [c z]]
//	zip [] []                   -> []
//	zip [a b] [x]               -> error (length mismatch)
//	zip "scalar" [x]            -> error (non-list)
func handleZip(c builtinCtx) (shell.Value, error) {
	args := c.Args
	if len(args) != 2 {
		return shell.Value{}, fmt.Errorf("zip: expected exactly 2 arguments, got %d", len(args))
	}
	a, err := zipArgAsList(args[0], 0)
	if err != nil {
		return shell.Value{}, err
	}
	b, err := zipArgAsList(args[1], 1)
	if err != nil {
		return shell.Value{}, err
	}
	if len(a) != len(b) {
		return shell.Value{}, fmt.Errorf("zip: length mismatch (arg 0 has %d, arg 1 has %d)", len(a), len(b))
	}
	out := make([]any, 0, len(a))
	for i := range a {
		out = append(out, []any{a[i], b[i]})
	}
	return shell.ValueFromAny(out), nil
}

// zipArgAsList unwraps a shell.Arg as a []any list, citing the
// argument position when the type is wrong. zip is only ever
// invoked from expression position (the pure-builtin parser
// resolves its operands via valueToArg), so the only shapes the
// args can take are ScalarValueArg (which is never a list) and
// StructuredValueArg (which may or may not wrap a list). The
// error message names the observed kind so the script author can
// spot the offending argument without reaching for trace.
func zipArgAsList(a shell.Arg, pos int) ([]any, error) {
	sv, ok := a.(shell.StructuredValueArg)
	if !ok {
		return nil, fmt.Errorf("zip: arg %d must be a list, got %s", pos, argKind(a))
	}
	raw, ok := sv.Value.Raw().([]any)
	if !ok {
		return nil, fmt.Errorf("zip: arg %d must be a list, got %s", pos, sv.Value.Kind())
	}
	return raw, nil
}

// argKind returns a human-readable label for the dynamic type of
// a non-structured shell.Arg. Used by zip's error path so the
// "must be a list" message names what came in instead.
func argKind(a shell.Arg) string {
	switch a.(type) {
	case shell.WordArg:
		return "bareword"
	case shell.QuotedArg:
		return "quoted string"
	case shell.ScalarValueArg:
		return "scalar"
	case shell.AdapterArg:
		return "adapter value"
	default:
		return fmt.Sprintf("%T", a)
	}
}
