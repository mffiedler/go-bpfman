package shell

import (
	"fmt"
	"strings"
)

// MatchesBlockExpr is the parsed `matches { path: pattern, ... }`
// block produced by an `assert <expr> matches { ... }` form. The
// block is attached to the host command's argument list as a single
// argument; semantics are defined by the consuming command (today
// only `assert` and `require`).
//
// The shape is intentionally narrow: each entry pairs a dotted path
// against a pattern. The path is resolved against an actual value at
// evaluation time; the pattern decides match/mismatch. See
// MatchEntry for the recognised pattern shapes.
type MatchesBlockExpr struct {
	Entries []MatchEntry
	Loc
}

// MatchEntry is one row inside a matches block. Path is a
// dot-separated field path with optional `[n]` array indexing,
// matching the grammar used by VarRefExpr's Path field. Exactly one
// of NotEmpty / Pattern is meaningful: NotEmpty true means the bare
// `not-empty` keyword was written; otherwise Pattern carries the
// expression whose evaluated scalar is compared for equality with
// the value at Path.
type MatchEntry struct {
	Path     string
	Pattern  Expr
	NotEmpty bool
	Loc
}

func (*MatchesBlockExpr) exprNode() {}

// parseMatchesBlock parses the body of a `matches { ... }` block.
// The opening `{` token must be the next token in the stream.
//
// A matches block is line-oriented: each entry occupies its own
// line, and the only entry separator is the newline between them.
// `,` and `;` are both rejected with a dedicated diagnostic — the
// block is a table of path-pattern relations, not a sequence of
// statements, so the comma-list and semicolon-statement
// punctuations the rest of the language uses elsewhere do not
// apply here.  See the def parameter list for the contrasting
// case where commas *are* required: parameters are a value list,
// not a table.
func (p *parser) parseMatchesBlock(matchesLoc Loc) (*MatchesBlockExpr, error) {
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "{") {
		return nil, locErrorf(matchesLoc, "expected '{' after matches")
	}
	openTok := p.advance()
	expr := &MatchesBlockExpr{Loc: openTok.Loc}
	for {
		// Skip newline separators between entries.  Multiple
		// consecutive newlines (blank lines inside the block) are
		// allowed.
		for !p.atEOF() && isMatchesSep(p.peek()) {
			p.pos++
		}
		if p.atEOF() {
			return nil, locErrorf(openTok.Loc, "unterminated matches block: missing '}'")
		}
		if p.peek().Kind == TokenSep && p.peek().Text == ";" {
			return nil, locErrorf(p.peek().Loc, "matches: ';' is not a valid entry separator; entries are separated by newlines")
		}
		if p.peek().Kind == TokenWord && p.peek().Text == "," {
			return nil, locErrorf(p.peek().Loc, "matches: ',' is not a valid entry separator; entries are separated by newlines")
		}
		if p.peek().Kind == TokenWord && p.peek().Text == "}" {
			p.advance()
			return expr, nil
		}
		if p.peek().Kind == TokenWord && p.peek().Text == "{" {
			return nil, locErrorf(p.peek().Loc, "unexpected '{' inside matches block")
		}
		entryToks, err := p.takeMatchEntryTokens()
		if err != nil {
			return nil, err
		}
		if len(entryToks) == 0 {
			// Defensive: takeMatchEntryTokens must consume at least
			// one token whenever it returns no entry tokens, or we
			// would loop forever. Reaching here means the next
			// token is something the helper should have either
			// consumed or surfaced as a stop condition above.
			return nil, locErrorf(p.peek().Loc, "unexpected token %q inside matches block", p.peek().Text)
		}
		entry, err := parseMatchEntry(entryToks)
		if err != nil {
			return nil, err
		}
		expr.Entries = append(expr.Entries, entry)
	}
}

// takeMatchEntryTokens collects tokens belonging to a single matches
// entry.  The entry ends at a newline or a closing brace (left in
// the stream for the outer loop).  A `;` or `,` -- both stand-alone
// and a `,` glued to the trailing token -- surfaces as a dedicated
// error so the user is pointed at the actual rule (one entry per
// line, separated by newlines) rather than getting a generic parse
// error from a downstream stage.
func (p *parser) takeMatchEntryTokens() ([]Token, error) {
	var toks []Token
	for !p.atEOF() {
		t := p.peek()
		if isMatchesSep(t) {
			return toks, nil
		}
		if t.Kind == TokenSep && t.Text == ";" {
			return nil, locErrorf(t.Loc, "matches: ';' is not a valid entry separator; entries are separated by newlines")
		}
		if t.Kind == TokenWord && (t.Text == "}" || t.Text == "{") {
			return toks, nil
		}
		if t.Kind == TokenWord && t.Text == "," {
			return nil, locErrorf(t.Loc, "matches: ',' is not a valid entry separator; entries are separated by newlines")
		}
		if t.Kind == TokenWord && len(t.Text) > 1 && strings.HasSuffix(t.Text, ",") {
			return nil, locErrorf(t.Loc, "matches: trailing ',' on %q is not allowed; entries are separated by newlines", t.Text)
		}
		toks = append(toks, t)
		p.pos++
	}
	return toks, nil
}

// isMatchesSep reports whether t is a newline separator inside a
// matches block.  `;` is not — a matches block is a table of
// path-pattern relations, not a sequence of statements.
func isMatchesSep(t Token) bool {
	return t.Kind == TokenSep && t.Text == "\n"
}

// parseMatchEntry interprets the token run for a single matches
// entry: a path, a `:` separator, and a pattern. The colon may be
// glued to the path word ("path:"), to the pattern's leading word
// (":pattern"), or stand alone as its own token.
func parseMatchEntry(toks []Token) (MatchEntry, error) {
	if len(toks) == 0 {
		return MatchEntry{}, fmt.Errorf("empty matches entry")
	}
	first := toks[0]
	if first.Kind != TokenWord {
		return MatchEntry{}, locErrorf(first.Loc, "matches entry: path must be a word, got %q", first.Text)
	}

	var pathText string
	var rest []Token
	switch {
	case strings.HasSuffix(first.Text, ":") && first.Text != ":":
		pathText = first.Text[:len(first.Text)-1]
		rest = toks[1:]
	case first.Text == ":":
		return MatchEntry{}, locErrorf(first.Loc, "matches entry: missing path before ':'")
	case len(toks) >= 2 && toks[1].Kind == TokenWord && toks[1].Text == ":":
		pathText = first.Text
		rest = toks[2:]
	case len(toks) >= 2 && toks[1].Kind == TokenWord && strings.HasPrefix(toks[1].Text, ":"):
		pathText = first.Text
		stripped := toks[1]
		stripped.Text = toks[1].Text[1:]
		if stripped.Text == "" {
			rest = toks[2:]
		} else {
			rest = make([]Token, 0, len(toks)-1)
			rest = append(rest, stripped)
			rest = append(rest, toks[2:]...)
		}
	default:
		return MatchEntry{}, locErrorf(first.Loc, "matches entry: missing ':' between path and pattern")
	}
	if pathText == "" {
		return MatchEntry{}, locErrorf(first.Loc, "matches entry: empty path")
	}
	if !isValidMatchPath(pathText) {
		return MatchEntry{}, locErrorf(first.Loc, "matches entry: invalid path %q", pathText)
	}
	if len(rest) == 0 {
		return MatchEntry{}, locErrorf(first.Loc, "matches entry %q: missing pattern after ':'", pathText)
	}

	// `not-empty` written alone (no operand) is the lifted form of
	// the existing unary predicate. Recognise it before handing the
	// remaining tokens to the expression parser, because the
	// expression parser would otherwise treat the bare keyword as a
	// literal at end-of-input.
	if len(rest) == 1 && rest[0].Kind == TokenWord && rest[0].Text == "not-empty" {
		return MatchEntry{Path: pathText, NotEmpty: true, Loc: first.Loc}, nil
	}

	pattern, err := parseExpression(rest)
	if err != nil {
		return MatchEntry{}, locErrorf(first.Loc, "matches entry %q: %v", pathText, err)
	}
	return MatchEntry{Path: pathText, Pattern: pattern, Loc: first.Loc}, nil
}

// evalMatchesBlockArg resolves each entry's pattern eagerly and
// returns a MatchesBlockArg suitable for handing to the host
// command. NotEmpty entries pass through with no evaluation;
// value-pattern entries evaluate their expression and capture the
// resulting Value. The path string is preserved verbatim for the
// host command to walk against the actual record.
func evalMatchesBlockArg(e *MatchesBlockExpr, env *Env) (Arg, error) {
	out := MatchesBlockArg{Entries: make([]MatchesBlockEntry, 0, len(e.Entries))}
	for _, entry := range e.Entries {
		ent := MatchesBlockEntry{
			Path:     entry.Path,
			NotEmpty: entry.NotEmpty,
			Loc:      entry.Loc,
		}
		if !entry.NotEmpty {
			v, err := EvalExpr(entry.Pattern, env)
			if err != nil {
				return nil, locErrorf(entry.Loc, "matches entry %q: %v", entry.Path, err)
			}
			ent.Value = v
		}
		out.Entries = append(out.Entries, ent)
	}
	return out, nil
}

// isValidMatchPath reports whether a path text is a syntactically
// valid dotted-name path with optional `[n]` indexing. Reuses the
// path grammar already used by VarRefExpr.
func isValidMatchPath(path string) bool {
	if path == "" {
		return false
	}
	// Mirror lexBareVarRef's path acceptance: identifier ('.' ident |
	// '[' digits ']')*. Reuse parsePath which validates the same
	// shape; an empty step list means "no path", which we already
	// rejected above.
	steps, err := parsePath(path)
	if err != nil {
		return false
	}
	return len(steps) > 0
}
