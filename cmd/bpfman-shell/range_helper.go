// The range pure builtin: 'range N' produces [0, 1, ..., N-1] as
// a sequence the foreach statement can iterate. Mirrors jq's
// range(N) semantics (0-indexed, upper bound exclusive); chosen
// over coreutils' 'seq' (1-indexed, inclusive) for consistency
// with the corpus's existing 'jq "[range(5)]"' idiom.
//
// Arity is fixed at 1. A two-arg 'range START END' or three-arg
// 'range START END STEP' would mirror Python / jq more closely
// but the pure-builtin registry holds a single arity per name;
// the wider forms can be added when a concrete test demands
// them, alongside whatever extension of the registry's arity
// model that would imply.

package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"encoding/json"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
)

// handleRange produces a Value carrying [json.Number("0"), ...,
// json.Number("N-1")] so foreach reads it as a list and downstream
// jq projections can apply tonumber without surprise.
//
//	range 0   -> []
//	range 5   -> [0, 1, 2, 3, 4]
//	range -1  -> error
func handleRange(c builtinCtx) (shell.Value, error) {
	args := c.Args
	if len(args) != 1 {
		return shell.Value{}, fmt.Errorf("range: expected exactly 1 argument, got %d", len(args))
	}
	text := strings.TrimSpace(repl.ArgText(args[0]))
	if text == "" {
		return shell.Value{}, fmt.Errorf("range: empty argument")
	}
	if strings.HasPrefix(text, "-") {
		return shell.Value{}, fmt.Errorf("range: negative bound is not allowed (got %q)", text)
	}
	n, err := strconv.ParseUint(text, 0, 64)
	if err != nil {
		return shell.Value{}, fmt.Errorf("range: invalid integer %q: %w", text, err)
	}
	// Cap at a sane upper bound to avoid pathological scripts
	// freezing the shell on 'range 4294967295'. math.MaxInt32
	// covers every realistic test loop and keeps the failure
	// mode loud and explicit rather than out-of-memory.
	if n > math.MaxInt32 {
		return shell.Value{}, fmt.Errorf("range: bound %d exceeds the maximum of %d", n, int64(math.MaxInt32))
	}
	out := make([]any, 0, n)
	for i := uint64(0); i < n; i++ {
		out = append(out, json.Number(strconv.FormatUint(i, 10)))
	}
	return shell.ValueFromAny(out), nil
}
