package shell

import (
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
//
// Exhaustive reflects the `matches exhaustive { ... }` keyword form.
// When set, the evaluator additionally enforces structural coverage:
// every key the actual value carries at the block's level must be
// claimed by an entry, otherwise the match fails. Dotted paths are
// rejected inside an exhaustive block at parse time -- the
// structural nesting is the point. Exhaustiveness chains through
// nested matches blocks; opaque-claim entries (value or predicate)
// claim the key only and do not recurse.
type MatchesBlockExpr struct {
	Entries    []MatchEntry
	Exhaustive bool
	Span
}

// MatchEntry is one row inside a matches block. Path is a
// dot-separated field path with optional `[n]` array indexing,
// matching the grammar used by VarRefExpr's Path field. Exactly
// one of Predicate / Pattern / SubBlock is meaningful: Predicate
// non-empty names a lifted unary predicate (`not-empty`, `nil`,
// `empty`); SubBlock non-nil means the pattern is a nested
// `matches [exhaustive] { ... }` block to evaluate against the
// sub-value at Path; otherwise Pattern carries the expression
// whose evaluated value is compared for equality with the value
// at Path.
type MatchEntry struct {
	Path      string
	Pattern   Expr
	SubBlock  *MatchesBlockExpr
	Predicate string // "", "not-empty", "nil", "empty"
	Span
}

// isMatchesPredicate reports whether s is one of the bareword
// predicates the matches block recognises in pattern position.
// Mirrors the verb-form predicate set so `field: nil` reads the
// same as `assert nil $X.field`.
func isMatchesPredicate(s string) bool {
	switch s {
	case "not-empty", "nil", "empty":
		return true
	}
	return false
}

func (*MatchesBlockExpr) exprNode() {}

// parseMatchesBlock parses the body of a `matches { ... }` block.
// The opening `{` token must be the next token in the stream.
//
// A matches block is line-oriented: each entry occupies its own
// line, and the only entry separator is the newline between them.
// `,` and `;` are both rejected with a dedicated diagnostic --
// the block is a table of path-pattern relations, not a sequence
// of statements, so the comma-list and semicolon-statement
// punctuations the rest of the language uses elsewhere do not
// apply here. See the def parameter list for the contrasting
// case where commas *are* required: parameters are a value list,
// not a table.
//
// exhaustive is set when the caller saw `matches exhaustive {`
// (vs the unadorned `matches {`). It propagates onto the
// resulting MatchesBlockExpr so the evaluator can additionally
// enforce structural coverage of the actual value at this level.
// Dotted paths inside an exhaustive block are rejected here at
// parse time: exhaustive mode requires structural nesting via
// `matches [exhaustive] { ... }` sub-blocks rather than dotted
// reach-across.
func (p *parser) parseMatchesBlock(matchesLoc Pos, exhaustive bool) (*MatchesBlockExpr, error) {
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "{") {
		return nil, locErrorf(matchesLoc, "expected '{' after matches")
	}
	openTok := p.advance()
	expr := &MatchesBlockExpr{Exhaustive: exhaustive, Span: Span{Pos: openTok.Pos, End: openTok.End}}
	for {
		// Skip newline separators between entries. Multiple
		// consecutive newlines (blank lines inside the block) are
		// allowed.
		for !p.atEOF() && isMatchesSep(p.peek()) {
			p.pos++
		}
		if p.atEOF() {
			return nil, spanErrorf(openTok.Span, "unterminated matches block: missing '}'")
		}
		if p.peek().Kind == TokenSep && p.peek().Text == ";" {
			return nil, locErrorf(p.peek().Pos, "matches: ';' is not a valid entry separator; entries are separated by newlines")
		}
		if p.peek().Kind == TokenWord && p.peek().Text == "," {
			return nil, locErrorf(p.peek().Pos, "matches: ',' is not a valid entry separator; entries are separated by newlines")
		}
		if p.peek().Kind == TokenWord && p.peek().Text == "}" {
			closeTok := p.advance()
			expr.End = closeTok.End
			return expr, nil
		}
		entry, err := p.parseMatchEntry(exhaustive)
		if err != nil {
			return nil, err
		}
		expr.Entries = append(expr.Entries, entry)
	}
}

// isMatchesSep reports whether t is a newline separator inside a
// matches block. `;` is not -- a matches block is a table of
// path-pattern relations, not a sequence of statements.
func isMatchesSep(t Token) bool {
	return t.Kind == TokenSep && t.Text == "\n"
}

// parseMatchEntry parses one entry inside a matches block: a
// path, a `:` separator, and a pattern. The colon may be glued
// to the path word ("path:"), to the pattern's leading word
// (":pattern"), or stand alone as its own token. The pattern is
// one of:
//
//   - the bare `not-empty` keyword (lifted unary predicate)
//   - a nested `matches [exhaustive] { ... }` sub-block, which
//     recurses against the sub-value at the entry's path
//   - any other expression, whose evaluated value is compared
//     for equality against the value at the entry's path
//
// inExhaustive carries the surrounding block's exhaustive flag;
// when set, dotted paths are rejected at this entry with a hint
// pointing at the nested-sub-block form.
func (p *parser) parseMatchEntry(inExhaustive bool) (MatchEntry, error) {
	startPos := p.pos
	pathTok := p.peek()
	if p.atEOF() || pathTok.Kind != TokenWord {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: path must be a word, got %q", pathTok.Text)
	}
	if pathTok.Text == "{" {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: missing path before '{'")
	}

	var pathText string
	if strings.HasSuffix(pathTok.Text, ":") && pathTok.Text != ":" {
		pathText = pathTok.Text[:len(pathTok.Text)-1]
		p.pos++
	} else if pathTok.Text == ":" {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: missing path before ':'")
	} else {
		pathText = pathTok.Text
		p.pos++
		// Expect ':' next, either as its own token or glued to
		// the pattern's leading word.
		if p.atEOF() {
			return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: missing ':' after path %q", pathText)
		}
		next := p.peek()
		switch {
		case next.Kind == TokenWord && next.Text == ":":
			p.pos++
		case next.Kind == TokenWord && strings.HasPrefix(next.Text, ":"):
			// Strip the leading ':' from next.Text in place by
			// rewriting the token. The cleanest way is to swap
			// the underlying token's text; since p.tokens is the
			// shared slice, mutate via index.
			tok := next
			tok.Text = next.Text[1:]
			if tok.Text == "" {
				p.pos++
			} else {
				p.tokens[p.pos] = tok
			}
		default:
			return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: missing ':' between path and pattern")
		}
	}

	if pathText == "" {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: empty path")
	}
	if !isValidMatchPath(pathText) {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry: invalid path %q", pathText)
	}
	if inExhaustive && strings.ContainsAny(pathText, ".[") {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches exhaustive: dotted path %q is not allowed; use a nested 'matches [exhaustive] { ... }' sub-block instead", pathText)
	}

	// Pattern position. Three shapes are recognised:
	//   - `not-empty` alone
	//   - `matches [exhaustive] { ... }` sub-block
	//   - any other expression (line-oriented token collection)
	if p.atEOF() {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry %q: missing pattern after ':'", pathText)
	}

	// Sub-block: `matches { ... }` or `matches exhaustive { ... }`.
	if p.peek().Kind == TokenWord && p.peek().Text == "matches" {
		matchesTok := p.advance()
		subExhaustive := false
		if !p.atEOF() && p.peek().Kind == TokenWord && p.peek().Text == "exhaustive" {
			subExhaustive = true
			p.advance()
		}
		sub, err := p.parseMatchesBlock(matchesTok.Pos, subExhaustive)
		if err != nil {
			return MatchEntry{}, err
		}
		endTok := p.tokens[p.pos-1]
		return MatchEntry{
			Path:     pathText,
			SubBlock: sub,
			Span:     Span{Pos: p.tokens[startPos].Pos, End: endTok.End},
		}, nil
	}

	// Expression or `not-empty` pattern: collect tokens up to the
	// next newline (the entry separator) or the outer block's
	// closing `}`. Reject `,` and `;` with their dedicated
	// diagnostics so the user is pointed at the actual rule.
	patternStart := p.pos
	for !p.atEOF() {
		t := p.peek()
		if isMatchesSep(t) {
			break
		}
		if t.Kind == TokenSep && t.Text == ";" {
			return MatchEntry{}, spanErrorf(t.Span, "matches: ';' is not a valid entry separator; entries are separated by newlines")
		}
		if t.Kind == TokenWord && t.Text == "}" {
			break
		}
		if t.Kind == TokenWord && t.Text == "," {
			return MatchEntry{}, spanErrorf(t.Span, "matches: ',' is not a valid entry separator; entries are separated by newlines")
		}
		if t.Kind == TokenWord && len(t.Text) > 1 && strings.HasSuffix(t.Text, ",") {
			return MatchEntry{}, spanErrorf(t.Span, "matches: trailing ',' on %q is not allowed; entries are separated by newlines", t.Text)
		}
		p.pos++
	}
	rest := p.tokens[patternStart:p.pos]
	if len(rest) == 0 {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry %q: missing pattern after ':'", pathText)
	}

	entrySpan := Span{Pos: p.tokens[startPos].Pos, End: rest[len(rest)-1].End}
	if len(rest) == 1 && rest[0].Kind == TokenWord && isMatchesPredicate(rest[0].Text) {
		return MatchEntry{Path: pathText, Predicate: rest[0].Text, Span: entrySpan}, nil
	}
	pattern, err := parseExpression(rest)
	if err != nil {
		return MatchEntry{}, spanErrorf(pathTok.Span, "matches entry %q: %v", pathText, err)
	}
	return MatchEntry{Path: pathText, Pattern: pattern, Span: entrySpan}, nil
}

// evalMatchesBlockArg resolves each entry's pattern eagerly and
// returns a MatchesBlockArg suitable for handing to the host
// command. NotEmpty entries pass through with no evaluation;
// SubBlock entries recurse via evalMatchesBlockArg so the nested
// block's pattern expressions are evaluated under the same
// environment; value-pattern entries evaluate their expression
// and capture the resulting Value. The path string is preserved
// verbatim for the host command to walk against the actual
// record. Exhaustive propagates onto the resulting arg so the
// consumer (today the assert verb dispatcher) can enable its
// structural-coverage check.
func evalMatchesBlockArg(e *MatchesBlockExpr, env *Env) (Arg, error) {
	out := MatchesBlockArg{
		Entries:    make([]MatchesBlockEntry, 0, len(e.Entries)),
		Exhaustive: e.Exhaustive,
		Span:       e.Span,
	}
	for _, entry := range e.Entries {
		ent := MatchesBlockEntry{
			Path:      entry.Path,
			Predicate: entry.Predicate,
			Span:      entry.Span,
		}
		switch {
		case entry.SubBlock != nil:
			sub, err := evalMatchesBlockArg(entry.SubBlock, env)
			if err != nil {
				return nil, err
			}
			subArg := sub.(MatchesBlockArg)
			ent.SubBlock = &subArg
		case entry.Predicate != "":
			// nothing to evaluate; the predicate carries
			// the assertion intent.
		default:
			v, err := EvalExpr(entry.Pattern, env)
			if err != nil {
				return nil, spanErrorf(entry.Span, "matches entry %q: %v", entry.Path, err)
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
