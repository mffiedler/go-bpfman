package syntax

import "github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"

// MatchesBlockExpr is the parsed `matches { path: pattern, ... }`
// block owned by a MatchesExpr.
type MatchesBlockExpr struct {
	Entries    []MatchEntry
	Exhaustive bool
	source.Span
}

// MatchEntry is one row inside a matches block.
type MatchEntry struct {
	Path      string
	Pattern   Expr
	SubBlock  *MatchesBlockExpr
	Predicate string
	source.Span
}
