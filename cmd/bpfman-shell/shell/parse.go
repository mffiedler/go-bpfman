package shell

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// parseDurationLiteral wraps time.ParseDuration with a clearer
// error phrasing.  Accepted forms are whatever Go accepts: "30s",
// "200ms", "1h30m", "500us".  An empty string or a bare number
// without a unit is rejected; the DSL insists on explicit units.
func parseDurationLiteral(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (try 30s, 5m, 200ms)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must be non-negative, got %q", s)
	}
	return d, nil
}

// Program is the root of a parsed source unit: an ordered sequence
// of statements with the source location of the first token.
type Program struct {
	Stmts []Stmt
	Span
}

// Stmt is the sealed sum type for statements. Embedding Node
// lets every Stmt be passed to Inspect without an explicit
// type assertion at the call site.
type Stmt interface {
	Node
	stmtNode()
}

// LetStmt binds the result of evaluating RHS to Name. Name is
// guaranteed to be a valid identifier by the parser.
type LetStmt struct {
	Name string
	RHS  Expr
	Span
}

// BindStmt runs Cmd and binds its primary result, and optionally
// its result envelope. Two surface forms parse here:
//
//	let NAME <- CMD              => Primary=NAME, Rc=""
//	let (RC, NAME) <- CMD        => Primary=NAME, Rc=RC
//	guard NAME <- CMD            => same shape, Guard=true
//	guard (RC, NAME) <- CMD      => same shape, Guard=true
//
// A third surface form, bind-collect, sets Collect instead of Cmd:
//
//	let NAME <- foreach X in LIST { BODY }
//	let (RC, NAME) <- foreach X in LIST { BODY }
//	guard NAME <- foreach X in LIST { BODY }
//	guard (RC, NAME) <- foreach X in LIST { BODY }
//
// BODY is iterated once per element of LIST; the body's last
// statement must be a CommandStmt and is executed as the bind's
// producer. The producer's primary value (and rc envelope, when
// the tuple form is used) is accumulated into a list per
// iteration. continue skips a particular iteration's
// accumulation; break terminates iteration and binds the
// partial collection. Guard semantics carry: if the outer bind
// is a guard, a non-ok envelope on any iteration halts the
// whole collect via GuardFailure with no binding.
//
// Exactly one of Cmd and Collect is non-nil. "_" as a target
// name discards that slot. Single-name binding always names
// the primary; tuple binding names rc then primary, matching
// section 6.2 of the design.
type BindStmt struct {
	Primary string
	Rc      string
	Cmd     *CommandStmt
	Collect *ForEachStmt
	Guard   bool
	Span
}

// IfBranch pairs a condition expression with a block body. Used
// for the primary branch and each elif.
type IfBranch struct {
	Cond Expr
	Body []Stmt
	Span
}

// IfStmt is an if-elif-else conditional.
type IfStmt struct {
	Cond  Expr
	Then  []Stmt
	Elifs []IfBranch
	Else  []Stmt
	Span
}

// CommandStmt is a plain command invocation. The first element of
// Args names the command.
type CommandStmt struct {
	Args []Expr
	Span
}

// ExprStmt is an expression appearing in statement position.  It is
// only produced inside a command substitution "[EXPR]" when the
// bracketed content parses as an expression (e.g. "[1 == 1]" or
// "[$x == $y]"). At the program level the parser never emits
// ExprStmt; the only statement forms are the named ones above plus
// a plain CommandStmt.  dispatchCmdSub treats an ExprStmt as
// "evaluate this expression and use its value as the substitution
// result".
type ExprStmt struct {
	Expr Expr
	Span
}

// ForEachStmt iterates a block over the elements of a list. At
// eval time List is evaluated to a Value; it must be a structured
// list, and each element is bound across Names in the Session for
// the duration of its iteration. The bindings are body-scoped:
// any prior binding of a name is restored on exit and a name that
// did not exist before the loop disappears again.
//
// Single-var form is the common case: Names is a single-element
// slice and the element is bound to Names[0] verbatim. Multi-var
// form (len(Names) >= 2) destructures each element as a list of
// length len(Names), binding the i'th sub-element to Names[i]. A
// "_" entry is a discard slot; the corresponding sub-element is
// dropped. Single-var binding never destructures: the loop
// variable carries the whole element through, list or not.
type ForEachStmt struct {
	Names []string
	List  Expr
	Body  []Stmt
	Span
}

// BreakStmt terminates the nearest enclosing ForEachStmt. Outside
// a loop it is a runtime error.
type BreakStmt struct {
	Span
}

// ContinueStmt skips the remainder of the current ForEachStmt
// iteration and advances to the next element.  Outside a loop it
// is a runtime error.
type ContinueStmt struct {
	Span
}

// RetryStmt runs Body repeatedly until the Until expression
// evaluates true.  Body errors do not halt the retry — they are
// expected during polling; the most recent body error is carried
// across iterations and returned as the statement's error if and
// when Until becomes true.  Until is evaluated after each body
// run regardless of whether the body errored, so time-based
// exits fire even when every attempt is failing.
//
// There is no dedicated timeout or iteration clause; those are
// primary-level expressions (TimeoutExpr and IterationExpr) that
// compose into Until via the full expression grammar.  Retry
// scope bookkeeping (start time, iteration counter) lives on
// Env so no magic variables leak into the session.
type RetryStmt struct {
	Body  []Stmt
	Until Expr
	Span
}

// AssertStmt is the expression-form of "assert"/"require": the
// keyword followed by a single boolean expression. Verb-form
// assertions ("assert ok exec ...", "assert nil $var", matches
// blocks) stay on the CommandStmt path; the parser routes between
// the two by peeking the first non-"not" token after the keyword.
//
// Negation is encoded inside Expr as a NotExpr (the expression
// grammar already handles "not"); AssertStmt itself carries no
// separate negate flag. The shell layer does not own the
// assertion's failure-reporting policy: evaluation delegates to
// Env.ExecAssertStmt, which the REPL driver supplies with the
// printing, counter, and halt-on-require behaviour.
type AssertStmt struct {
	IsRequire bool
	Expr      Expr
	Span
}

// DeferStmt registers a cleanup command for the enclosing defer
// scope. Argument values are evaluated when the defer statement
// runs and frozen onto the defer record; the command itself
// dispatches at scope exit, in LIFO order with any other deferred
// commands. The top-level script and def bodies are defer scopes;
// if/foreach/retry blocks are not. Defer failures are rendered
// through the shared formatter and contribute to the script's
// exit code, but they do not halt; cleanup continues across
// failures.
type DeferStmt struct {
	Cmd *CommandStmt
	Span
}

// DefStmt declares a user-defined command. Name is the command name,
// Params is the ordered parameter list (parameter names, no default
// or type information), and Body is the parsed block executed at
// call time. Evaluation registers the def in the session under Name;
// invocation routes through evalCommandStmt's def-lookup path.
type DefStmt struct {
	Name   string
	Params []string
	Body   []Stmt
	Span
}

func (*LetStmt) stmtNode()      {}
func (*BindStmt) stmtNode()     {}
func (*DeferStmt) stmtNode()    {}
func (*IfStmt) stmtNode()       {}
func (*CommandStmt) stmtNode()  {}
func (*ExprStmt) stmtNode()     {}
func (*ForEachStmt) stmtNode()  {}
func (*BreakStmt) stmtNode()    {}
func (*ContinueStmt) stmtNode() {}
func (*RetryStmt) stmtNode()    {}
func (*DefStmt) stmtNode()      {}
func (*AssertStmt) stmtNode()   {}

// Parse turns a token stream into a *Program. Every parse error
// carries a source location derived from the offending token.
func Parse(tokens []Token) (*Program, error) {
	p := &parser{tokens: tokens}
	stmts, err := p.parseStmts(p.atEOF)
	if err != nil {
		return nil, err
	}
	var start Pos
	if len(tokens) > 0 {
		start = tokens[0].Pos
	}
	prog := &Program{Stmts: stmts, Span: p.spanFrom(start)}
	if err := validateLocs(prog); err != nil {
		return nil, err
	}
	return prog, nil
}

// validateLocs walks every node of prog and asserts that each
// has a non-zero Pos (both line and column populated). The
// position infrastructure is end-to-end -- the tokeniser
// builds Pos{Line, Col} from a lineStarts table, every AST
// node copies its Pos from a token, and the renderers print
// 'file:line:col:' for diagnostics. A regression that adds a
// new AST variant without copying its source position would
// silently land an empty Pos on user-facing error messages;
// validateLocs catches that at parse time so the next
// developer to introduce the gap sees a loud failure rather
// than a quiet column drop.
//
// Program nodes are skipped when they have no Stmts: an empty
// input is a valid parse with an empty Pos, and we do not
// want to reject empty programs. Every other node, including
// the Program of a non-empty input, must have Line > 0 and
// Col > 0.
func validateLocs(prog *Program) error {
	var missing []string
	Inspect(prog, func(n Node) bool {
		if n == nil {
			return true
		}
		if p, ok := n.(*Program); ok && len(p.Stmts) == 0 {
			return true
		}
		sp := nodeSpan(n)
		switch {
		case sp.Pos.Line == 0 || sp.Pos.Col == 0:
			missing = append(missing, fmt.Sprintf("%T missing start (line=%d col=%d)",
				n, sp.Pos.Line, sp.Pos.Col))
		case sp.End.Line == 0 || sp.End.Col == 0:
			missing = append(missing, fmt.Sprintf("%T missing end (start=%d:%d, end=%d:%d)",
				n, sp.Pos.Line, sp.Pos.Col, sp.End.Line, sp.End.Col))
		}
		return true
	})
	if len(missing) > 0 {
		return fmt.Errorf("internal: AST node(s) with incomplete source spans: %s",
			strings.Join(missing, ", "))
	}
	return nil
}

// nodeSpan returns the Span value embedded in n. Every AST type
// embeds shell.Span as an anonymous struct field; reflect over
// n to find that field. Used by validateSpans to enforce the
// position-completeness invariant; the rest of the code reaches
// Span through concrete-type access.
func nodeSpan(n Node) Span {
	v := reflect.ValueOf(n)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return Span{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return Span{}
	}
	for i := 0; i < v.NumField(); i++ {
		if sp, ok := v.Field(i).Interface().(Span); ok {
			return sp
		}
	}
	return Span{}
}

// parser is the recursive-descent state: a token stream and a
// cursor. All navigation goes through peek/advance so the cursor
// stays consistent with what has been consumed.
type parser struct {
	tokens []Token
	pos    int
}

func (p *parser) peek() Token {
	if p.pos >= len(p.tokens) {
		return Token{}
	}
	return p.tokens[p.pos]
}

func (p *parser) advance() Token {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

func (p *parser) atEOF() bool {
	return p.pos >= len(p.tokens)
}

// spanFrom builds a Span starting at start and ending at the End of
// the most recently consumed token. Use at AST construction sites
// so every node carries its full source extent: callers capture the
// first token's Pos before parsing the construct, then call
// spanFrom once construction is complete. When no tokens have been
// consumed (start of input) the End collapses to start so the Span
// is well-formed but empty-width.
func (p *parser) spanFrom(start Pos) Span {
	if p.pos == 0 {
		return Span{Pos: start, End: start}
	}
	return Span{Pos: start, End: p.tokens[p.pos-1].End}
}

func (p *parser) atBlockClose() bool {
	t := p.peek()
	return t.Kind == TokenWord && t.Text == "}"
}

// parseStmts consumes statements until isEnd returns true or the
// token stream is exhausted. Separators between statements are
// skipped. Used for both the program root and block bodies.
func (p *parser) parseStmts(isEnd func() bool) ([]Stmt, error) {
	var stmts []Stmt
	for {
		for !p.atEOF() && p.peek().Kind == TokenSep {
			p.pos++
		}
		if p.atEOF() || isEnd() {
			break
		}
		before := p.pos
		stmt, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
		// Forward-progress guard: every parseStmt call must either
		// return an error or consume at least one token. Without
		// this guard a parser branch that silently returns (nil,
		// nil) without advancing causes an infinite loop here. The
		// guard converts that class of bug into an actionable parse
		// error at the offending token rather than a hang.
		if stmt == nil && p.pos == before {
			t := p.peek()
			return nil, spanErrorf(t.Span, "unexpected %q", t.Text)
		}
	}
	return stmts, nil
}

func (p *parser) parseStmt() (Stmt, error) {
	t := p.peek()
	if t.Kind == TokenWord {
		switch t.Text {
		case "if":
			return p.parseIfStmt()
		case "let":
			return p.parseLetStmt()
		case "foreach":
			return p.parseForEachStmt()
		case "retry":
			return p.parseRetryStmt()
		case "break":
			return p.parseBreakStmt()
		case "continue":
			return p.parseContinueStmt()
		case "guard":
			return p.parseGuardStmt()
		case "defer":
			return p.parseDeferStmt()
		case "def":
			return p.parseDefStmt()
		case "assert", "require":
			if p.assertTakesExprForm() {
				return p.parseAssertStmt(t.Text == "require")
			}
		}
	}
	if leadsExpression(t) {
		return p.parseExprStmt()
	}
	return p.parseCommandStmt()
}

// assertTakesExprForm reports whether the assert/require statement
// at the current cursor should be parsed as the expression form
// (AssertStmt) rather than the verb-form CommandStmt. The peek
// must not consume any tokens. The rule: after the keyword,
// optionally skip a leading "not", look at the next meaningful
// token; if it names a verb-style assertion (ok/fail/path/
// contains/nil), or if a "matches {" tail appears anywhere in
// the buffered statement, fall through to the verb-form path.
// Otherwise the statement has expression-grade content.
func (p *parser) assertTakesExprForm() bool {
	pos := p.pos + 1
	if pos < len(p.tokens) && p.tokens[pos].Kind == TokenWord && p.tokens[pos].Text == "not" {
		pos++
	}
	for pos < len(p.tokens) && p.tokens[pos].Kind == TokenSep {
		pos++
	}
	if pos >= len(p.tokens) {
		return true
	}
	t := p.tokens[pos]
	if t.Kind == TokenWord {
		switch t.Text {
		case "ok", "fail", "path", "contains", "nil":
			return false
		}
	}
	for j := pos; j < len(p.tokens); j++ {
		jt := p.tokens[j]
		if jt.Kind == TokenSep {
			break
		}
		if jt.Kind == TokenWord && (jt.Text == "{" || jt.Text == "}") {
			if jt.Text == "{" && j > 0 && p.tokens[j-1].Kind == TokenWord && p.tokens[j-1].Text == "matches" {
				return false
			}
			break
		}
	}
	return true
}

// parseAssertStmt consumes "assert"/"require" followed by an
// expression body, returning an AssertStmt. The caller (parseStmt)
// has already established that the form is expression-shaped via
// assertTakesExprForm. A leading "not" is parsed by the expression
// grammar as a NotExpr, so this function does not consume it
// eagerly: that would race with the expression parser's own
// handling and produce a doubly-negated tree.
func (p *parser) parseAssertStmt(isRequire bool) (Stmt, error) {
	keywordTok := p.advance()
	tokens, err := p.takeStmtTokens(false)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, spanErrorf(keywordTok.Span, "%s requires an expression target", keywordTok.Text)
	}
	expr, err := parseExpression(tokens)
	if err != nil {
		return nil, wrapSyntaxErr(keywordTok.Text, err)
	}
	return &AssertStmt{
		IsRequire: isRequire,
		Expr:      expr,
		Span:      p.spanFrom(keywordTok.Pos),
	}, nil
}

// leadsExpression reports whether a token can only start an
// expression at statement position.  These tokens would otherwise
// be mis-routed into the command-statement grammar and produce
// unhelpful errors (unknown command names that are actually
// variable references, quoted literals, bracketed expressions,
// etc.).  Bare WORDs are excluded because they are the normal
// command-name form; the few WORD texts that can only appear in
// expression position ("(", "not", and the unary predicates) are
// listed explicitly.
func leadsExpression(t Token) bool {
	switch t.Kind {
	case TokenVarRef, TokenQuoted, TokenInterpString, TokenAdapterRef:
		return true
	case TokenWord:
		switch t.Text {
		case "(", "not", "not-empty", "true", "false":
			return true
		}
	}
	return false
}

// parseExprStmt consumes the current statement as an expression
// and wraps it in an ExprStmt.  The tokens between the current
// cursor and the next separator (or end-of-input) are collected
// and handed to parseExpression verbatim, so every construct the
// expression grammar understands -- comparisons, logical
// combinators, threading, unary predicates, parens -- works
// unchanged at the top level.
func (p *parser) parseExprStmt() (Stmt, error) {
	startLoc := p.peek().Pos
	tokens, err := p.takeStmtTokens(false)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, locErrorf(startLoc, "empty expression statement")
	}
	expr, err := parseExpression(tokens)
	if err != nil {
		return nil, err
	}
	return &ExprStmt{Expr: expr, Span: p.spanFrom(startLoc)}, nil
}

func (p *parser) parseBreakStmt() (Stmt, error) {
	t := p.advance()
	if err := p.rejectTrailingArgs("break"); err != nil {
		return nil, err
	}
	return &BreakStmt{Span: p.spanFrom(t.Pos)}, nil
}

func (p *parser) parseContinueStmt() (Stmt, error) {
	t := p.advance()
	if err := p.rejectTrailingArgs("continue"); err != nil {
		return nil, err
	}
	return &ContinueStmt{Span: p.spanFrom(t.Pos)}, nil
}

// rejectTrailingArgs errors when a bare-keyword statement
// (break, continue) has extra tokens on the same statement
// before the next separator or block marker.  Silent tolerance
// would let "break 2" tokenise as if "break" were a command,
// which is not what the user wrote.
func (p *parser) rejectTrailingArgs(name string) error {
	if p.atEOF() {
		return nil
	}
	t := p.peek()
	if t.Kind == TokenSep {
		return nil
	}
	if t.Kind == TokenWord && (t.Text == "{" || t.Text == "}") {
		return nil
	}
	return spanErrorf(t.Span, "%s takes no arguments; got %q", name, t.Text)
}

func (p *parser) parseLetStmt() (Stmt, error) {
	letTok := p.advance() // "let"
	if p.atEOF() {
		return nil, spanErrorf(letTok.Span, "let requires: let <name> = <expr> or let <name> <- <command...> or let (<rc>, <prim>) <- <command...>")
	}
	if t := p.peek(); t.Kind == TokenWord && t.Text == "(" {
		// Tuple form: let (RC, PRIM) <- COMMAND. Tuple is only
		// legal on '<-'; the assign form '=' stays single-name.
		rc, prim, err := p.parseBindTuple(letTok)
		if err != nil {
			return nil, err
		}
		if p.atEOF() || p.peek().Kind != TokenBind {
			return nil, spanErrorf(letTok.Span, "tuple bind requires '<-' after target list")
		}
		return p.parseBindRHS(letTok.Pos, rc, prim, false)
	}
	if p.peek().Kind != TokenWord {
		return nil, spanErrorf(letTok.Span, "let requires an identifier, got %q", p.peek().Text)
	}
	nameTok := p.advance()
	name := nameTok.Text
	if !IsIdent(name) {
		return nil, spanErrorf(nameTok.Span, "invalid variable name: %q", name)
	}
	if p.atEOF() {
		return nil, spanErrorf(letTok.Span, "let requires '=' or '<-' after the name")
	}
	switch p.peek().Kind {
	case TokenAssign:
		p.advance() // "="
		rhsTokens, err := p.takeStmtTokens(true)
		if err != nil {
			return nil, err
		}
		if len(rhsTokens) == 0 {
			return nil, spanErrorf(letTok.Span, "let requires: let <name> = <value...>")
		}
		rhs, err := parseExpression(rhsTokens)
		if err != nil {
			return nil, err
		}
		return &LetStmt{Name: name, RHS: rhs, Span: p.spanFrom(letTok.Pos)}, nil
	case TokenBind:
		return p.parseBindRHS(letTok.Pos, "", name, false)
	default:
		return nil, spanErrorf(letTok.Span, "let requires '=' or '<-' after the name, got %q", p.peek().Text)
	}
}

// parseGuardStmt parses "guard NAME <- COMMAND" or
// "guard (RC, PRIM) <- COMMAND". The form is fixed: the keyword,
// a single identifier or a parenthesised pair, the bind sigil
// '<-', then a non-empty command form. There is no "guard NAME =
// EXPR" spelling.
// parseDeferStmt parses "defer COMMAND". The RHS is a command
// form; argument evaluation happens at run time when the defer
// statement executes (registering the captured invocation), and
// the command itself dispatches at scope exit. There is no
// 'defer { ... }' block form in v1.
func (p *parser) parseDeferStmt() (Stmt, error) {
	deferTok := p.advance() // "defer"
	cmdTokens, err := p.takeStmtTokens(false)
	if err != nil {
		return nil, err
	}
	if len(cmdTokens) == 0 {
		return nil, spanErrorf(deferTok.Span, "defer requires a command form")
	}
	args, err := parseCommandArgs(cmdTokens, false)
	if err != nil {
		return nil, err
	}
	cmd := &CommandStmt{Args: args, Span: p.spanFrom(cmdTokens[0].Pos)}
	return &DeferStmt{Cmd: cmd, Span: p.spanFrom(deferTok.Pos)}, nil
}

func (p *parser) parseGuardStmt() (Stmt, error) {
	guardTok := p.advance() // "guard"
	if p.atEOF() {
		return nil, spanErrorf(guardTok.Span, "guard requires: guard <name> <- <command...> or guard (<rc>, <prim>) <- <command...>")
	}
	if t := p.peek(); t.Kind == TokenWord && t.Text == "(" {
		rc, prim, err := p.parseBindTuple(guardTok)
		if err != nil {
			return nil, err
		}
		if p.atEOF() || p.peek().Kind != TokenBind {
			return nil, spanErrorf(guardTok.Span, "tuple bind requires '<-' after target list")
		}
		return p.parseBindRHS(guardTok.Pos, rc, prim, true)
	}
	if p.peek().Kind != TokenWord {
		return nil, spanErrorf(guardTok.Span, "guard requires an identifier, got %q", p.peek().Text)
	}
	nameTok := p.advance()
	name := nameTok.Text
	if !IsIdent(name) {
		return nil, spanErrorf(nameTok.Span, "invalid variable name: %q", name)
	}
	if p.atEOF() || p.peek().Kind != TokenBind {
		return nil, spanErrorf(guardTok.Span, "guard requires: guard <name> <- <command...> (missing '<-')")
	}
	return p.parseBindRHS(guardTok.Pos, "", name, true)
}

// parseBindTuple consumes a parenthesised tuple target list:
// '(' RC ',' PRIM ')'. RC and PRIM are identifiers or '_'. The
// opening '(' is at the cursor on entry. The tokeniser does not
// split on ',', so a comma may arrive glued to an identifier ("rc,"
// is one TokenWord); the parser strips the trailing comma in that
// case the same way parseDefParams does.
func (p *parser) parseBindTuple(keywordTok Token) (rc, prim string, err error) {
	openTok := p.advance() // "("
	for !p.atEOF() && p.peek().Kind == TokenSep {
		p.pos++
	}
	rc, sawComma, err := p.parseBindTargetName(keywordTok)
	if err != nil {
		return "", "", err
	}
	if !sawComma {
		for !p.atEOF() && p.peek().Kind == TokenSep {
			p.pos++
		}
		if p.atEOF() || p.peek().Kind != TokenWord || p.peek().Text != "," {
			return "", "", spanErrorf(openTok.Span, "tuple bind: expected ',' between targets")
		}
		p.advance() // ","
	}
	for !p.atEOF() && p.peek().Kind == TokenSep {
		p.pos++
	}
	prim, _, err = p.parseBindTargetName(keywordTok)
	if err != nil {
		return "", "", err
	}
	for !p.atEOF() && p.peek().Kind == TokenSep {
		p.pos++
	}
	if p.atEOF() || p.peek().Kind != TokenWord || p.peek().Text != ")" {
		return "", "", spanErrorf(openTok.Span, "tuple bind: expected ')' after targets")
	}
	p.advance() // ")"
	if rc == "_" && prim == "_" {
		return "", "", spanErrorf(openTok.Span, "tuple bind cannot discard both slots")
	}
	return rc, prim, nil
}

// parseBindTargetName reads a single tuple-target name. A name is
// either an identifier or "_". A trailing ',' glued to the
// identifier is honoured (so "rc," advances past the comma);
// sawComma reports that case so the caller can skip the explicit
// comma consumption.
func (p *parser) parseBindTargetName(keywordTok Token) (name string, sawComma bool, err error) {
	if p.atEOF() || p.peek().Kind != TokenWord {
		return "", false, spanErrorf(keywordTok.Span, "tuple bind: expected identifier or '_', got %q", p.peek().Text)
	}
	t := p.advance()
	text := t.Text
	if strings.HasSuffix(text, ",") && len(text) > 1 {
		text = text[:len(text)-1]
		sawComma = true
	}
	if text == "_" {
		return "_", sawComma, nil
	}
	if !IsIdent(text) {
		return "", false, spanErrorf(t.Span, "tuple bind: invalid name %q", text)
	}
	return text, sawComma, nil
}

// parseBindRHS consumes the '<-' sigil and the command form that
// follows, returning a BindStmt. The RHS extends to the next
// statement separator or block marker; a stray '=' or '<-' inside
// the RHS is rejected. parseCommandArgs handles the command-form
// tokens so every primary expression the command-statement grammar
// accepts works on the right of a bind. rc is "" for single-name
// bindings, an identifier (or "_") for tuple bindings.
func (p *parser) parseBindRHS(stmtLoc Pos, rc, primary string, guard bool) (Stmt, error) {
	bindTok := p.advance() // "<-"
	// Bind-collect form: 'let X <- foreach NAME in LIST { BODY }'.
	// The 'foreach' keyword in RHS position triggers a separate
	// parse path because the body is a block and the existing
	// takeBindRHSTokens stops at '{'. Tuple bind targets and the
	// guard prefix carry through unchanged.
	if !p.atEOF() && p.peek().Kind == TokenWord && p.peek().Text == "foreach" {
		feStmt, err := p.parseForEachStmt()
		if err != nil {
			return nil, err
		}
		fe := feStmt.(*ForEachStmt)
		if len(fe.Body) == 0 {
			return nil, spanErrorf(bindTok.Span, "bind-collect: foreach body must produce a command at its last statement")
		}
		last := fe.Body[len(fe.Body)-1]
		if _, ok := last.(*CommandStmt); !ok {
			return nil, spanErrorf(nodeSpan(last), "bind-collect: foreach body's last statement must be a command (got %s); the last statement is the iteration's producer", describeStmt(last))
		}
		return &BindStmt{
			Primary: primary, Rc: rc,
			Collect: fe,
			Guard:   guard,
			Span:    p.spanFrom(stmtLoc),
		}, nil
	}
	cmdTokens, err := p.takeBindRHSTokens(bindTok)
	if err != nil {
		return nil, err
	}
	args, err := parseCommandArgs(cmdTokens, false)
	if err != nil {
		return nil, err
	}
	cmd := &CommandStmt{Args: args, Span: p.spanFrom(cmdTokens[0].Pos)}
	return &BindStmt{Primary: primary, Rc: rc, Cmd: cmd, Guard: guard, Span: p.spanFrom(stmtLoc)}, nil
}

// describeStmt returns a short human-readable name for a
// statement kind, used in error messages so the user sees "got
// let" rather than "got *shell.LetStmt".
func describeStmt(s Stmt) string {
	switch s.(type) {
	case *LetStmt:
		return "let"
	case *BindStmt:
		return "bind"
	case *DeferStmt:
		return "defer"
	case *IfStmt:
		return "if"
	case *ForEachStmt:
		return "foreach"
	case *RetryStmt:
		return "retry"
	case *BreakStmt:
		return "break"
	case *ContinueStmt:
		return "continue"
	case *DefStmt:
		return "def"
	case *AssertStmt:
		return "assert"
	case *ExprStmt:
		return "expression"
	}
	return fmt.Sprintf("%T", s)
}

// takeBindRHSTokens collects the tokens that form the command on the
// right of a '<-' bind. The run terminates at the next separator or
// block marker; nested '=' or '<-' on the RHS are rejected at the
// offending token. Newlines inside a '[...]' list literal are
// transparent: while bracket depth is positive the collector treats
// them as part of the buffer so a long list can wrap across lines
// (parseListLiteral already skips TokenSep tokens between elements).
func (p *parser) takeBindRHSTokens(bindTok Token) ([]Token, error) {
	var buf []Token
	var depth int
	for !p.atEOF() {
		t := p.peek()
		if t.Kind == TokenSep {
			if depth > 0 {
				buf = append(buf, t)
				p.pos++
				continue
			}
			break
		}
		if t.Kind == TokenWord && (t.Text == "{" || t.Text == "}") {
			if depth > 0 {
				buf = append(buf, t)
				p.pos++
				continue
			}
			break
		}
		if t.Kind == TokenAssign {
			return nil, spanErrorf(t.Span, "unexpected '=' on bind RHS; the right of '<-' must be a command form")
		}
		if t.Kind == TokenBind {
			return nil, spanErrorf(t.Span, "unexpected '<-' on bind RHS; chain via separate let/guard statements")
		}
		if t.Kind == TokenWord {
			switch t.Text {
			case "[":
				depth++
			case "]":
				if depth > 0 {
					depth--
				}
			}
		}
		buf = append(buf, t)
		p.pos++
	}
	if len(buf) == 0 {
		return nil, spanErrorf(bindTok.Span, "bind requires a command after '<-'")
	}
	return buf, nil
}

// takeStmtTokens collects tokens belonging to the current statement
// up to the next separator or block marker. When rejectAssign is
// true a stray TokenAssign inside the collected range is an error,
// used on a let RHS to catch "let x = a = b". Newlines inside a
// '[...]' list literal are transparent: while bracket depth is
// positive the collector treats them as part of the buffer so a
// long list can wrap across lines.
func (p *parser) takeStmtTokens(rejectAssign bool) ([]Token, error) {
	var buf []Token
	var depth int
	for !p.atEOF() {
		t := p.peek()
		if t.Kind == TokenSep {
			if depth > 0 {
				buf = append(buf, t)
				p.pos++
				continue
			}
			break
		}
		if t.Kind == TokenWord && (t.Text == "{" || t.Text == "}") {
			if depth > 0 {
				buf = append(buf, t)
				p.pos++
				continue
			}
			break
		}
		if rejectAssign && t.Kind == TokenAssign {
			return nil, spanErrorf(t.Span, "unexpected '=' in let RHS")
		}
		if t.Kind == TokenWord {
			switch t.Text {
			case "[":
				depth++
			case "]":
				if depth > 0 {
					depth--
				}
			}
		}
		buf = append(buf, t)
		p.pos++
	}
	return buf, nil
}

func (p *parser) parseCommandStmt() (Stmt, error) {
	first := p.peek()
	startLoc := first.Pos
	// A bare `{` or `}` at statement position is not the start of a
	// command (parseStmt has already routed if/foreach/retry/...
	// keywords away from here), so reject it explicitly. Returning
	// (nil, nil) without consuming the token would let parseStmts
	// loop forever on the same token; this surfaces a real parse
	// error at the offending location instead.
	if first.Kind == TokenWord && (first.Text == "{" || first.Text == "}") {
		return nil, spanErrorf(first.Span, "unexpected %q at statement start", first.Text)
	}
	isAlias := first.Kind == TokenWord && first.Text == "alias"
	var buf []Token
	for !p.atEOF() {
		t := p.peek()
		if t.Kind == TokenSep {
			break
		}
		if t.Kind == TokenWord && (t.Text == "{" || t.Text == "}") {
			break
		}
		if t.Kind == TokenAssign && !isAlias {
			return nil, spanErrorf(t.Span, "unexpected '='; use \"let <name> = <value...>\" for assignment")
		}
		buf = append(buf, t)
		p.pos++
	}
	if len(buf) == 0 {
		return nil, nil
	}

	// Detect "... matches {" tail: the previous token must be the
	// bare keyword "matches" and the next token must be "{".  When
	// this shape fires, the "matches" word is consumed as part of
	// the block syntax (it does not appear in the host command's
	// argument list) and the block parses into a MatchesBlockExpr
	// that becomes the command's last argument.
	var matchesBlock *MatchesBlockExpr
	if !p.atEOF() && p.peek().Kind == TokenWord && p.peek().Text == "{" &&
		len(buf) > 0 && buf[len(buf)-1].Kind == TokenWord && buf[len(buf)-1].Text == "matches" {
		matchesTok := buf[len(buf)-1]
		buf = buf[:len(buf)-1]
		if len(buf) == 0 {
			return nil, spanErrorf(matchesTok.Span, "matches { ... } requires a target expression and a host command")
		}
		mb, err := p.parseMatchesBlock(matchesTok.Pos)
		if err != nil {
			return nil, err
		}
		matchesBlock = mb
	}

	args, err := parseCommandArgs(buf, isAlias)
	if err != nil {
		return nil, err
	}
	if matchesBlock != nil {
		args = append(args, matchesBlock)
	}
	return &CommandStmt{Args: args, Span: p.spanFrom(startLoc)}, nil
}

// reservedDefNames lists identifiers that cannot be used as a def
// name because the parser routes them away from the command-statement
// grammar. Allowing a def to shadow these would either be unreachable
// (the keyword wins at parseStmt) or break statement parsing.
var reservedDefNames = map[string]bool{
	"def":       true,
	"defer":     true,
	"let":       true,
	"guard":     true,
	"if":        true,
	"elif":      true,
	"else":      true,
	"foreach":   true,
	"in":        true,
	"retry":     true,
	"until":     true,
	"break":     true,
	"continue":  true,
	"and":       true,
	"or":        true,
	"not":       true,
	"matches":   true,
	"timeout":   true,
	"iteration": true,
	"true":      true,
	"false":     true,
}

// parseDefStmt parses a `def NAME(P1, P2, ...) { BODY }` declaration.
// The body is parsed eagerly via parseBlock so a syntactically broken
// body fails at declaration time and the def is never installed.
// Parameter names must be identifiers and must be unique within the
// declaration.
func (p *parser) parseDefStmt() (Stmt, error) {
	defTok := p.advance() // "def"
	if p.atEOF() || p.peek().Kind != TokenWord {
		return nil, spanErrorf(defTok.Span, "def requires: def <name>(<params>) { ... }")
	}
	if p.peek().Text == "(" {
		return nil, spanErrorf(defTok.Span, "def requires a name before '('")
	}
	nameTok := p.advance()
	name := nameTok.Text
	if !IsIdent(name) {
		return nil, spanErrorf(nameTok.Span, "invalid def name: %q", name)
	}
	if reservedDefNames[name] {
		return nil, spanErrorf(nameTok.Span, "cannot use reserved word %q as a def name", name)
	}
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "(") {
		return nil, spanErrorf(defTok.Span, "def requires '(' after the name")
	}
	p.advance() // "("
	params, err := p.parseDefParams(defTok.Pos)
	if err != nil {
		return nil, err
	}
	// Skip separators between ')' and '{'.
	for !p.atEOF() && p.peek().Kind == TokenSep {
		p.pos++
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, wrapSyntaxErr(fmt.Sprintf("def %s", name), err)
	}
	return &DefStmt{Name: name, Params: params, Body: body, Span: p.spanFrom(defTok.Pos)}, nil
}

// parseDefParams consumes the parameter list up to and including the
// closing ')'. Parameters are comma-separated identifiers; a trailing
// comma is permitted; an empty list (immediately closing ')') is
// permitted. Duplicate parameter names are rejected. The tokeniser
// does not split on `,` so a comma may arrive glued to an identifier
// ("a," is one TokenWord); the parser strips the trailing comma in
// that case and treats it as a separator, mirroring how matches
// blocks handle the same pattern.
func (p *parser) parseDefParams(defLoc Pos) ([]string, error) {
	var params []string
	seen := make(map[string]bool)
	expectName := true
	for {
		// Allow newlines/semis inside the parameter list so a long
		// def signature can wrap.
		for !p.atEOF() && p.peek().Kind == TokenSep {
			p.pos++
		}
		if p.atEOF() {
			return nil, locErrorf(defLoc, "def: unterminated parameter list (missing ')')")
		}
		t := p.peek()
		if t.Kind == TokenWord && t.Text == ")" {
			p.advance()
			return params, nil
		}
		if t.Kind == TokenWord && t.Text == "," {
			if expectName {
				return nil, spanErrorf(t.Span, "def: missing parameter name before ','")
			}
			p.advance()
			expectName = true
			continue
		}
		if !expectName {
			return nil, spanErrorf(t.Span, "def: expected ',' or ')' in parameter list, got %q", t.Text)
		}
		if t.Kind != TokenWord {
			return nil, spanErrorf(t.Span, "def: expected parameter name, got %q", t.Text)
		}
		// Strip a trailing comma glued to the identifier ("a," tokenises
		// as one WORD because ',' is not a tokenisation boundary). The
		// stripped identifier is the parameter name; the comma becomes
		// the separator for the next iteration.
		nameText := t.Text
		trailingComma := false
		if strings.HasSuffix(nameText, ",") && len(nameText) > 1 {
			nameText = nameText[:len(nameText)-1]
			trailingComma = true
		}
		if !IsIdent(nameText) {
			return nil, spanErrorf(t.Span, "def: invalid parameter name %q", nameText)
		}
		if seen[nameText] {
			return nil, spanErrorf(t.Span, "def: duplicate parameter name %q", nameText)
		}
		seen[nameText] = true
		params = append(params, nameText)
		p.advance()
		expectName = trailingComma
	}
}

func (p *parser) parseRetryStmt() (Stmt, error) {
	retryTok := p.advance() // "retry"
	body, err := p.parseBlock()
	if err != nil {
		return nil, wrapSyntaxErr("retry", err)
	}
	// Skip separators between `}` and `until`.
	for !p.atEOF() && p.peek().Kind == TokenSep {
		p.pos++
	}
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "until") {
		return nil, spanErrorf(retryTok.Span, "retry requires 'until' after the body")
	}
	p.advance() // "until"
	exprTokens, err := p.takeStmtTokens(false)
	if err != nil {
		return nil, err
	}
	if len(exprTokens) == 0 {
		return nil, spanErrorf(retryTok.Span, "retry until requires an expression")
	}
	until, err := parseExpression(exprTokens)
	if err != nil {
		return nil, err
	}
	return &RetryStmt{Body: body, Until: until, Span: p.spanFrom(retryTok.Pos)}, nil
}

func (p *parser) parseForEachStmt() (Stmt, error) {
	feTok := p.advance() // "foreach"
	names, err := p.parseForEachNames(feTok)
	if err != nil {
		return nil, err
	}
	if p.atEOF() || p.peek().Kind != TokenWord || p.peek().Text != "in" {
		return nil, spanErrorf(feTok.Span, "foreach requires 'in' after the loop variable")
	}
	p.advance() // "in"
	listTokens, err := p.takeUntilOpenBrace()
	if err != nil {
		return nil, err
	}
	if len(listTokens) == 0 {
		return nil, spanErrorf(feTok.Span, "foreach requires: foreach <name> in <expr> { ... }")
	}
	list, err := parseExpression(listTokens)
	if err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &ForEachStmt{Names: names, List: list, Body: body, Span: p.spanFrom(feTok.Pos)}, nil
}

// parseForEachNames reads the loop-variable name list that follows
// the 'foreach' keyword. Accepts a single name (single-var form)
// or a comma-separated list (multi-var form, len >= 2). The
// tokeniser does not split on ',', so a comma may arrive glued to
// an identifier ("a," is one TokenWord); the parser strips the
// trailing comma exactly the way parseBindTuple does. A bare "_"
// is allowed as a discard slot, matching the spirit of tuple-bind
// targets; "in" is reserved as the keyword that ends the name list.
func (p *parser) parseForEachNames(feTok Token) ([]string, error) {
	if p.atEOF() || p.peek().Kind != TokenWord {
		return nil, spanErrorf(feTok.Span, "foreach requires: foreach <name> in <expr> { ... }")
	}
	first, sawComma, err := p.parseForEachNameToken(feTok)
	if err != nil {
		return nil, err
	}
	names := []string{first}
	for sawComma || (!p.atEOF() && p.peek().Kind == TokenWord && p.peek().Text == ",") {
		if !sawComma {
			p.advance() // ","
		}
		next, more, err := p.parseForEachNameToken(feTok)
		if err != nil {
			return nil, err
		}
		names = append(names, next)
		sawComma = more
	}
	allDiscard := true
	for _, n := range names {
		if n != "_" {
			allDiscard = false
			break
		}
	}
	if allDiscard {
		return nil, spanErrorf(feTok.Span, "foreach: all loop variables are '_'; at least one must bind")
	}
	return names, nil
}

// parseForEachNameToken consumes one loop-variable name and
// returns whether a trailing comma was glued to the identifier.
// The 'in' keyword is rejected here so it is reachable as the
// terminator at the call site.
func (p *parser) parseForEachNameToken(feTok Token) (name string, sawComma bool, err error) {
	if p.atEOF() || p.peek().Kind != TokenWord {
		return "", false, spanErrorf(feTok.Span, "foreach: expected variable name, got end of input")
	}
	t := p.advance()
	text := t.Text
	if strings.HasSuffix(text, ",") && len(text) > 1 {
		text = text[:len(text)-1]
		sawComma = true
	}
	if text == "in" {
		return "", false, spanErrorf(t.Span, "foreach requires a variable name before 'in'")
	}
	if text == "_" {
		return "_", sawComma, nil
	}
	if !IsIdent(text) {
		return "", false, spanErrorf(t.Span, "invalid variable name: %q", text)
	}
	return text, sawComma, nil
}

// takeUntilOpenBrace collects tokens up to (but not including) the
// next '{'.  Separator tokens inside the range are skipped so
// multi-line list expressions work.  Returns an error if no '{'
// appears before EOF.
func (p *parser) takeUntilOpenBrace() ([]Token, error) {
	var buf []Token
	for !p.atEOF() {
		t := p.peek()
		if t.Kind == TokenSep {
			p.pos++
			continue
		}
		if t.Kind == TokenWord && t.Text == "{" {
			return buf, nil
		}
		buf = append(buf, t)
		p.pos++
	}
	return nil, fmt.Errorf("expected '{' after expression")
}

func (p *parser) parseIfStmt() (Stmt, error) {
	ifTok := p.advance() // "if"
	cond, err := p.parseCondition()
	if err != nil {
		return nil, wrapSyntaxErr("if", err)
	}
	then, err := p.parseBlock()
	if err != nil {
		return nil, wrapSyntaxErr("if", err)
	}
	var elifs []IfBranch
	var els []Stmt
	for {
		for !p.atEOF() && p.peek().Kind == TokenSep {
			p.pos++
		}
		if p.atEOF() {
			break
		}
		t := p.peek()
		if t.Kind != TokenWord {
			break
		}
		switch t.Text {
		case "elif":
			elifTok := p.advance()
			ec, err := p.parseCondition()
			if err != nil {
				return nil, wrapSyntaxErr("elif", err)
			}
			eb, err := p.parseBlock()
			if err != nil {
				return nil, wrapSyntaxErr("elif", err)
			}
			elifs = append(elifs, IfBranch{Cond: ec, Body: eb, Span: p.spanFrom(elifTok.Pos)})
		case "else":
			p.advance()
			eb, err := p.parseBlock()
			if err != nil {
				return nil, wrapSyntaxErr("else", err)
			}
			els = eb
			return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els, Span: p.spanFrom(ifTok.Pos)}, nil
		default:
			return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els, Span: p.spanFrom(ifTok.Pos)}, nil
		}
	}
	return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els, Span: p.spanFrom(ifTok.Pos)}, nil
}

// parseCondition collects tokens up to the next `{` and parses them
// as an expression. The `{` is not consumed.
func (p *parser) parseCondition() (Expr, error) {
	var buf []Token
	for !p.atEOF() {
		t := p.peek()
		if t.Kind == TokenSep {
			p.pos++
			continue
		}
		if t.Kind == TokenWord && t.Text == "{" {
			break
		}
		buf = append(buf, t)
		p.pos++
	}
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "{") {
		return nil, fmt.Errorf("expected '{' after condition")
	}
	if len(buf) == 0 {
		return nil, fmt.Errorf("expected condition before '{'")
	}
	return parseExpression(buf)
}

// parseBlock consumes a `{` ... `}` block and returns its parsed
// statements. Nested blocks balance naturally via parseStmts.
func (p *parser) parseBlock() ([]Stmt, error) {
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "{") {
		return nil, fmt.Errorf("expected '{'")
	}
	p.advance()
	stmts, err := p.parseStmts(p.atBlockClose)
	if err != nil {
		return nil, err
	}
	if p.atEOF() || !(p.peek().Kind == TokenWord && p.peek().Text == "}") {
		return nil, fmt.Errorf("unterminated block: missing '}'")
	}
	p.advance()
	return stmts, nil
}

// parseExpression parses an expression via a cursor-based
// recursive-descent parser.  Each precedence level has its own
// method, loosest to tightest:
//
//	parseComparison     -- binary comparison (==, !=, <, <=, >, >=)
//	parseAdditive       -- '+' / '-' left-associative
//	parseMultiplicative -- '*' / '/' / '%' left-associative
//	parsePredicate      -- unary predicate (not-empty, true, false)
//	parseNegate         -- unary '-' right-associative
//	parseThread         -- threading chain (|>)
//	parseTerm           -- primary token (literal, varref, adapter,
//	                                      cmdsub)
//
// Each level calls the next-tighter level for its operands and
// loops for any left-associative operator of its own.  The shape
// makes errors self-locating: a mismatched token triggers an
// error from the level that was expecting something else, and
// trailing tokens after a complete expression get a single
// "unexpected token" message at the outer call.
func parseExpression(tokens []Token) (Expr, error) {
	tokens = stripSeps(tokens)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty expression")
	}
	ep := &exprParser{tokens: tokens}
	e, err := ep.parseOr()
	if err != nil {
		return nil, err
	}
	if !ep.eof() {
		t := ep.peek()
		if hint, ok := smushedArithmeticHint(t); ok {
			return nil, spanErrorf(t.Span, "unexpected %q after expression; %s", t.Text, hint)
		}
		return nil, spanErrorf(t.Span, "unexpected %q after expression", t.Text)
	}
	return e, nil
}

// smushedArithmeticHint returns a user-facing hint when the
// trailing token looks like a binary '-' or '/' fused to its
// right operand (e.g. "-1", "/2").  The tokeniser keeps '-' and
// '/' as word-constituents because they appear inside negative
// literals, flags, and paths, so the common "$x -1" / "$x /2"
// shapes tokenise as two adjacent primaries rather than as
// binary arithmetic.  When that shape is the reason parsing
// failed, point at whitespace explicitly.
func smushedArithmeticHint(t Token) (string, bool) {
	if t.Kind != TokenWord || len(t.Text) < 2 {
		return "", false
	}
	c := t.Text[0]
	if c != '-' && c != '/' {
		return "", false
	}
	next := t.Text[1]
	isOperand := (next >= '0' && next <= '9') || next == '.' || next == '$' ||
		(next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || next == '_'
	if !isOperand {
		return "", false
	}
	return fmt.Sprintf("arithmetic '%c' requires whitespace (e.g. \"%c %s\" not %q)", c, c, t.Text[1:], t.Text), true
}

// parseInterpBody turns the raw contents of a "${...}"
// interpolation into the Expr that will be evaluated at run time.
// Three accepted shapes:
//
//   - bare name with optional path: "name", "name.path",
//     "name[0]". Treated as a variable reference; the user
//     does not write "$name" inside the braces.
//   - sigil-led expression: "$n * 2", "$count + 1",
//     "$x |> jq .y".
//   - literal-led expression: "4 * 2", "1 + $count",
//     "(3 + 4) * 2", "true and $flag".
//
// The literal-led form is the bash $((...))-equivalent for
// inline arithmetic in command args: 'print "${4 * 2}"'
// reaches it. Without this branch, command args could only
// reach the expression evaluator via a named intermediate
// ('let x = 4 * 2; print $x'), which is correct but verbose.
// Inside braces is the right place for the relaxation because
// the surrounding double-quoted string already disambiguates
// the syntactic context.
func parseInterpBody(inner string, span Span) (Expr, error) {
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return nil, spanErrorf(span, "empty interpolation")
	}
	// Compute the original-source position of trimmed[0]. The
	// outer span starts at '$', so the body's first byte is
	// two columns past Pos (skip "${"). Walk any leading
	// whitespace inside the body to land trimmedStart on the
	// first non-whitespace byte. The walk handles multi-line
	// bodies (rare but legal) by tracking newlines.
	bodyStart := Pos{Line: span.Pos.Line, Col: span.Pos.Col + 2}
	leadingWS := inner[:len(inner)-len(strings.TrimLeft(inner, " \t\n\r"))]
	trimmedStart := advancePos(bodyStart, leadingWS)

	// translate maps a position from the synthesised
	// re-tokenisation of the trimmed body back to the
	// original source. The first line of the trimmed body is
	// at trimmedStart.Line and starts at trimmedStart.Col;
	// later lines correspond to original lines one-for-one
	// because they begin at column 1 in both encodings.
	translate := func(p Pos) Pos {
		if p.Line == 1 {
			return Pos{Line: trimmedStart.Line, Col: trimmedStart.Col + p.Col - 1}
		}
		return Pos{Line: trimmedStart.Line + p.Line - 1, Col: p.Col}
	}
	translateSpan := func(s Span) Span {
		return Span{Pos: translate(s.Pos), End: translate(s.End)}
	}

	// Bare-name shortcut: "${name}" / "${name.path}" /
	// "${name[0]}" tokenise (after a synthesised "$") to a
	// single TokenVarRef. Use that fast path so the common
	// case stays a simple VarRefExpr; the Span covers the
	// trimmed body in the original source -- excluding the
	// "${" / "}" wrappers -- so the caret underlines just the
	// name.
	if trimmed[0] != '$' {
		if tokens, err := Tokenise("$" + trimmed); err == nil &&
			len(tokens) == 1 && tokens[0].Kind == TokenVarRef {
			t := tokens[0]
			return &VarRefExpr{
				Name: t.VarName,
				Path: t.VarPath,
				Span: Span{
					Pos: trimmedStart,
					End: advancePos(trimmedStart, trimmed),
				},
			}, nil
		}
		// Not a bare-name reference: fall through to the
		// general expression path below, which parses the
		// original (un-prefixed) body. This is what makes
		// literal-led expressions like "${4 * 2}" work.
	}
	tokens, err := Tokenise(trimmed)
	if err != nil {
		return nil, spanErrorf(span, "string interpolation ${%s}: %v", inner, err)
	}
	expr, ok := tryParseExpression(tokens)
	if !ok {
		return nil, spanErrorf(span, "string interpolation ${%s}: not a valid expression", inner)
	}
	rewriteSpansWith(expr, translateSpan)
	return expr, nil
}

// advancePos walks s as if appended after start, returning the
// position after the last byte. Newlines reset the column to 1
// and bump the line; everything else advances the column by one.
// Used by parseInterpBody to convert byte counts (leading
// whitespace, full trimmed-body length) into Pos coordinates so
// inner-parsed spans translate back to the original source.
func advancePos(start Pos, s string) Pos {
	line := start.Line
	col := start.Col
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return Pos{Line: line, Col: col}
}

// rewriteSpansWith walks every node reachable from root and
// replaces its embedded Span with translate(span). Used when a
// sub-AST is parsed from a synthesised slice (interpolation
// bodies) so the original source coordinates are preserved
// across the boundary. Per-node translation keeps caret
// precision: an error inside "${4 * bogus}" can underline just
// "bogus" rather than the whole interpolation.
func rewriteSpansWith(root Node, translate func(Span) Span) {
	Inspect(root, func(n Node) bool {
		if n == nil {
			return true
		}
		setNodeSpan(n, translate(nodeSpan(n)))
		return true
	})
}

// setNodeSpan reflects over n looking for an embedded Span
// field and overwrites it. Mirrors nodeSpan; pulled out so
// rewriteSpans does not duplicate the reflection traversal.
func setNodeSpan(n Node, span Span) {
	v := reflect.ValueOf(n)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		if _, ok := f.Interface().(Span); ok {
			f.Set(reflect.ValueOf(span))
			return
		}
	}
}

// tryParseExpression attempts to interpret tokens as a single
// expression.  It returns (expr, true) only when the expression
// grammar matches and every non-separator token is consumed; any
// parse error or trailing token returns (nil, false).  Used by the
// cmd-sub primary to detect "[EXPR]" misuse and point the user at
// the "[[EXPR]]" form.
func tryParseExpression(tokens []Token) (Expr, bool) {
	e, err := parseExpression(tokens)
	if err != nil {
		return nil, false
	}
	return e, true
}

// exprParser is a cursor over a pre-collected token slice used by
// parseExpression's recursive-descent methods.  Each level calls
// the next-tighter level and loops for any left-associative
// operator of its own.
type exprParser struct {
	tokens []Token
	pos    int
}

func (p *exprParser) peek() Token {
	if p.pos >= len(p.tokens) {
		return Token{}
	}
	return p.tokens[p.pos]
}

func (p *exprParser) advance() Token {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

func (p *exprParser) eof() bool {
	return p.pos >= len(p.tokens)
}

// spanFrom returns Span{start, end-of-last-consumed-token} so every
// expression node carries its full source extent. Mirrors the
// statement parser's helper.
func (p *exprParser) spanFrom(start Pos) Span {
	if p.pos == 0 {
		return Span{Pos: start, End: start}
	}
	return Span{Pos: start, End: p.tokens[p.pos-1].End}
}

// parseOr recognises left-associative 'or' chains.  'or' is the
// loosest logical connective; it binds looser than 'and' and
// looser than the comparison level.  Short-circuit evaluation is
// handled at eval time.
func (p *exprParser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for !p.eof() && isKeywordWord(p.peek(), "or") {
		opTok := p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &LogicalExpr{Op: "or", Left: left, Right: right, Span: p.spanFrom(opTok.Pos)}
	}
	return left, nil
}

// parseAnd recognises left-associative 'and' chains.  'and' is
// tighter than 'or' and looser than 'not'.
func (p *exprParser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for !p.eof() && isKeywordWord(p.peek(), "and") {
		opTok := p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &LogicalExpr{Op: "and", Left: left, Right: right, Span: p.spanFrom(opTok.Pos)}
	}
	return left, nil
}

// parseNot recognises the 'not' prefix.  It binds tighter than
// 'and' / 'or' but looser than the comparison level, matching
// SQL and Python conventions (so "not $a == $b" parses as
// "not ($a == $b)", not "(not $a) == $b"). Multiple 'not's are
// accepted via right-associative recursion.
func (p *exprParser) parseNot() (Expr, error) {
	if isKeywordWord(p.peek(), "not") {
		notTok := p.advance()
		operand, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &NotExpr{Operand: operand, Span: p.spanFrom(notTok.Pos)}, nil
	}
	return p.parseComparison()
}

// isKeywordWord reports whether t is a plain word token whose
// text equals kw.  Used at precedence levels to recognise
// keyword operators (and / or / not) without colliding with
// tokens that happen to have the same text inside other positions.
func isKeywordWord(t Token, kw string) bool {
	return t.Kind == TokenWord && t.Text == kw
}

// parseComparison recognises the optional binary-comparison infix
// around a tighter sub-expression.  At most one binary operator
// per expression matches the current grammar; anything else the
// caller flags via the "unexpected trailing token" check in
// parseExpression.
func (p *exprParser) parseComparison() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	if p.eof() {
		return left, nil
	}
	op, ok := binaryOpFromToken(p.peek())
	if !ok {
		return left, nil
	}
	opTok := p.advance()
	right, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	return &BinaryExpr{Left: left, Op: op, Right: right, Span: p.spanFrom(opTok.Pos)}, nil
}

// parseAdditive recognises left-associative '+' and '-' chains.
// The operands live at the multiplicative level so that
// "1 + 2 * 3" parses as "1 + (2 * 3)".  The '-' here is always
// binary subtraction; unary negation is handled at the negate
// level, below the predicate rung.
func (p *exprParser) parseAdditive() (Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for !p.eof() {
		t := p.peek()
		if t.Kind != TokenWord || (t.Text != "+" && t.Text != "-") {
			break
		}
		opTok := p.advance()
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: opTok.Text, Right: right, Span: p.spanFrom(opTok.Pos)}
	}
	return left, nil
}

// parseMultiplicative recognises left-associative '*', '/', and
// '%' chains.  Operands live at the predicate level.  Division
// by zero and non-numeric operands are caught at evaluation
// time, not here.
func (p *exprParser) parseMultiplicative() (Expr, error) {
	left, err := p.parsePredicate()
	if err != nil {
		return nil, err
	}
	for !p.eof() {
		t := p.peek()
		if t.Kind != TokenWord || (t.Text != "*" && t.Text != "/" && t.Text != "%") {
			break
		}
		opTok := p.advance()
		right, err := p.parsePredicate()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: opTok.Text, Right: right, Span: p.spanFrom(opTok.Pos)}
	}
	return left, nil
}

// parsePredicate recognises a unary-predicate prefix applied to a
// primary operand. The only surviving predicate is "not-empty";
// "true" and "false" are now plain boolean literals. The rule is
// still context-sensitive in shape because the predicate word
// must actually have an operand to its right -- "not-empty" alone
// at end of input falls through to the tighter negate level
// where it is parsed as a literal.
func (p *exprParser) parsePredicate() (Expr, error) {
	if pred, ok := unaryPredFromToken(p.peek()); ok && p.operandFollowsPred() {
		predTok := p.advance()
		operand, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand, Span: p.spanFrom(predTok.Pos)}, nil
	}
	return p.parseNegate()
}

// parseNegate recognises a unary '-' prefix.  Right-associative
// recursion supports stacked negations ("- -$x").  The bare '-'
// WORD token is produced only when whitespace surrounds it;
// "-3" tokenises as a single WORD (a negative literal) and
// never reaches this rule.
func (p *exprParser) parseNegate() (Expr, error) {
	t := p.peek()
	if t.Kind == TokenWord && t.Text == "-" {
		negTok := p.advance()
		operand, err := p.parseNegate()
		if err != nil {
			return nil, err
		}
		return &NegateExpr{Operand: operand, Span: p.spanFrom(negTok.Pos)}, nil
	}
	return p.parseThread()
}

// operandFollowsPred reports whether the token immediately after
// the current one could syntactically be a unary predicate's
// operand.  It rejects anything that belongs to a higher
// precedence level or ends the current expression: binary-
// comparison words, arithmetic operators, logical operators
// (and / or), '|>', a closing ')' that would terminate a
// parenthesised sub-expression, and end of input.  That lets a
// pred word sitting at a comparison-RHS, arithmetic-RHS, or
// logical-RHS position parse as a literal instead of greedily
// swallowing the next token.
func (p *exprParser) operandFollowsPred() bool {
	if p.pos+1 >= len(p.tokens) {
		return false
	}
	next := p.tokens[p.pos+1]
	if next.Kind == TokenThread {
		return false
	}
	if _, isBinOp := binaryOpFromToken(next); isBinOp {
		return false
	}
	if isArithmeticOp(next) {
		return false
	}
	if isKeywordWord(next, "and") || isKeywordWord(next, "or") {
		return false
	}
	if next.Kind == TokenWord && next.Text == ")" {
		return false
	}
	return true
}

// isArithmeticOp reports whether t is a bare WORD carrying one
// of the five arithmetic operators.  The tokeniser does not
// give these tokens a dedicated kind, so recognition is by
// text.  Used at precedence boundaries to keep arithmetic
// operators from being absorbed as operands at a tighter level.
func isArithmeticOp(t Token) bool {
	if t.Kind != TokenWord {
		return false
	}
	switch t.Text {
	case "+", "-", "*", "/", "%":
		return true
	}
	return false
}

// parseThread consumes a primary then zero or more '|>
// command-call' segments, folding left-associatively into a
// chain of ThreadExprs.  The RHS is read by parseThreadRHS,
// which stops at the next '|>' or a binary-op word so the
// comparison level can pick up operators at its own precedence.
func (p *exprParser) parseThread() (Expr, error) {
	lhs, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for !p.eof() && p.peek().Kind == TokenThread {
		threadTok := p.advance()
		args, err := p.parseThreadRHS(threadTok.Pos)
		if err != nil {
			return nil, err
		}
		lhs = &ThreadExpr{LHS: lhs, Args: args, Span: p.spanFrom(threadTok.Pos)}
	}
	return lhs, nil
}

// parseThreadRHS consumes the command-call tokens that follow a
// '|>'. The general rule is that the RHS extends to the end of
// the current expression, not blindly to end-of-input, so any
// token that begins a higher-precedence construct or closes the
// surrounding form terminates the command. Concretely it stops
// at: the next '|>' (so a chain of threads composes); a
// binary-comparison word; an arithmetic operator; a logical
// operator 'and' or 'or' (so a thread can sit inside a
// LogicalExpr); a closing bracket ')', ']', or '}' (so a thread
// nested inside a parenthesised expression, command
// substitution, or interpolation lets the enclosing form close);
// or end of input. A literal binary-op, arithmetic, logical, or
// bracket word used as a command argument must be quoted.
func (p *exprParser) parseThreadRHS(threadLoc Pos) ([]Expr, error) {
	var args []Expr
	for !p.eof() {
		t := p.peek()
		if t.Kind == TokenThread {
			break
		}
		if _, isBinOp := binaryOpFromToken(t); isBinOp {
			break
		}
		if isArithmeticOp(t) {
			break
		}
		if t.Kind == TokenWord && (t.Text == "and" || t.Text == "or") {
			break
		}
		if t.Kind == TokenWord && (t.Text == ")" || t.Text == "]" || t.Text == "}") {
			break
		}
		p.advance()
		e, err := parsePrimary(t)
		if err != nil {
			return nil, err
		}
		args = append(args, e)
	}
	if len(args) == 0 {
		return nil, locErrorf(threadLoc, "thread requires a command on the right-hand side")
	}
	return args, nil
}

// parseTerm consumes one primary expression — a single literal,
// varref, adapter, or command-substitution token, a
// parenthesised sub-expression that recurses back into the full
// expression grammar at the 'or' level, or a 'timeout DURATION'
// primary that evaluates to a boolean against the enclosing
// retry's elapsed-time clock.
func (p *exprParser) parseTerm() (Expr, error) {
	if p.eof() {
		return nil, fmt.Errorf("expected expression, got end of input")
	}
	t := p.peek()
	if t.Kind == TokenWord && t.Text == "(" {
		openTok := p.advance()
		if !p.eof() && p.peek().Kind == TokenWord && p.peek().Text == ")" {
			return nil, spanErrorf(openTok.Span, "empty parenthesised expression")
		}
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.eof() || !(p.peek().Kind == TokenWord && p.peek().Text == ")") {
			return nil, spanErrorf(openTok.Span, "missing ')' to close parenthesised expression")
		}
		p.advance() // consume ')'
		return inner, nil
	}
	if t.Kind == TokenWord && t.Text == "[" {
		return p.parseListLiteral()
	}
	if isKeywordWord(t, "timeout") {
		return p.parseTimeoutExpr()
	}
	if isKeywordWord(t, "iteration") {
		return p.parseIterationExpr()
	}
	if t.Kind == TokenWord {
		if pb, ok := LookupPureBuiltin(t.Text); ok {
			return p.parsePureCall(pb)
		}
	}
	p.advance()
	return parsePrimary(t)
}

// parseListLiteral consumes a '[' EXPR EXPR ... ']' list literal.
// Elements are whitespace-separated and each element parses as a
// primary expression via parseTerm; compound expressions wrap in
// parens, matching every other expression context. An operator at
// element-boundary position (binary, thread, arithmetic, logical)
// is rejected with a hint to parenthesise the compound. Newlines
// inside the brackets are transparent so a long list can wrap
// across lines.
//
// Empty lists ([]) are deliberately not accepted: the grammar
// addition's first cut requires at least one element, mirroring
// the corpus's needs. Adding [] later is purely permissive and
// can land when a use case appears.
func (p *exprParser) parseListLiteral() (Expr, error) {
	openTok := p.advance() // '['
	var elems []Expr
	for {
		// Newlines inside the brackets are transparent.
		for !p.eof() && p.peek().Kind == TokenSep {
			p.advance()
		}
		if p.eof() {
			return nil, spanErrorf(openTok.Span, "missing ']' to close list literal")
		}
		t := p.peek()
		if t.Kind == TokenWord && t.Text == "]" {
			if len(elems) == 0 {
				return nil, spanErrorf(openTok.Span, "empty list literal not supported; list must have at least one element")
			}
			p.advance() // ']'
			return &ListExpr{Elems: elems, Span: p.spanFrom(openTok.Pos)}, nil
		}
		if _, isBinOp := binaryOpFromToken(t); isBinOp {
			return nil, spanErrorf(t.Span, "unexpected %q between list elements; wrap a compound element in parens, e.g. [($x + 1) $y]", t.Text)
		}
		if isArithmeticOp(t) {
			return nil, spanErrorf(t.Span, "unexpected %q between list elements; wrap a compound element in parens, e.g. [($x + 1) $y]", t.Text)
		}
		if t.Kind == TokenThread {
			return nil, spanErrorf(t.Span, "unexpected '|>' between list elements; wrap a threaded element in parens, e.g. [($x |> jq \".id\") $y]")
		}
		if t.Kind == TokenWord && (t.Text == "and" || t.Text == "or") {
			return nil, spanErrorf(t.Span, "unexpected %q between list elements; wrap a compound element in parens", t.Text)
		}
		// Comma is not a tokeniser terminator (CLI arg payloads
		// like '--proceed-on ok,pipe,dispatcher_return' rely on
		// '-bearing barewords staying whole). That means a user
		// who writes [1, 2, 3] out of muscle memory would parse
		// silently as the bareword strings "1,", "2,", "3".
		// Catch any unquoted comma in element position and reject
		// it loudly; quoted strings ("a,b" as an element) escape
		// the check because TokenQuoted is not TokenWord.
		if t.Kind == TokenWord && strings.ContainsRune(t.Text, ',') {
			return nil, spanErrorf(t.Span, "unquoted comma in list literal; elements are whitespace-separated (try [1 2 3] not [1, 2, 3])")
		}
		elem, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		elems = append(elems, elem)
	}
}

// parsePureCall consumes a registered pure-builtin name followed
// by exactly pb.Arity primary arguments. Arguments are parsed as
// primaries (parsePrimary tokens or parenthesised sub-expressions),
// not as full expressions, so trailing operators bind to the
// surrounding expression rather than to the call. The rule keeps
// "range 5 + 1" parsing as "(range 5) + 1" and forces nested calls
// to be explicit via parens: "u32le (jq '.x' $v)".
func (p *exprParser) parsePureCall(pb PureBuiltin) (Expr, error) {
	nameTok := p.advance()
	args := make([]Expr, 0, pb.Arity)
	for i := 0; i < pb.Arity; i++ {
		if p.eof() {
			return nil, spanErrorf(nameTok.Span, "%s: expected %d argument(s), got %d", pb.Name, pb.Arity, i)
		}
		arg, err := p.parsePureCallArg(pb.Name)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
	}
	return &PureCallExpr{Name: pb.Name, Args: args, Span: p.spanFrom(nameTok.Pos)}, nil
}

// parsePureCallArg accepts one primary argument for a pure-builtin
// call: a parenthesised sub-expression (full expression grammar
// inside), a list literal '[...]', a single literal / varref /
// adapter / interp-string token, or a sigil-led varref. Operators
// (and / or / not / + / - / * / / / % / |> / comparison) are not
// primaries and stop the argument list, leaving the outer
// expression to pick them up.
func (p *exprParser) parsePureCallArg(name string) (Expr, error) {
	t := p.peek()
	if t.Kind == TokenWord && t.Text == "(" {
		openTok := p.advance()
		if !p.eof() && p.peek().Kind == TokenWord && p.peek().Text == ")" {
			return nil, spanErrorf(openTok.Span, "empty parenthesised expression")
		}
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.eof() || !(p.peek().Kind == TokenWord && p.peek().Text == ")") {
			return nil, spanErrorf(openTok.Span, "missing ')' to close parenthesised expression")
		}
		p.advance()
		return inner, nil
	}
	if t.Kind == TokenWord && t.Text == "[" {
		return p.parseListLiteral()
	}
	switch t.Kind {
	case TokenWord:
		if _, isBinOp := binaryOpFromToken(t); isBinOp {
			return nil, spanErrorf(t.Span, "%s: unexpected %q in argument position", name, t.Text)
		}
		if isArithmeticOp(t) {
			return nil, spanErrorf(t.Span, "%s: unexpected %q in argument position", name, t.Text)
		}
		if isKeywordWord(t, "and") || isKeywordWord(t, "or") || isKeywordWord(t, "not") {
			return nil, spanErrorf(t.Span, "%s: unexpected %q in argument position", name, t.Text)
		}
	case TokenQuoted, TokenVarRef, TokenAdapterRef, TokenInterpString:
		// Recognised primary tokens; fall through to consume.
	default:
		return nil, spanErrorf(t.Span, "%s: unexpected %q in argument position", name, t.Text)
	}
	p.advance()
	return parsePrimary(t)
}

// parseTimeoutExpr consumes a 'timeout' keyword followed by a
// duration literal (e.g. 30s, 200ms, 1h30m — anything
// time.ParseDuration accepts).  The result is a primary-level
// boolean expression: it can participate in any comparison or
// logical combinator at higher precedence.
func (p *exprParser) parseTimeoutExpr() (Expr, error) {
	tok := p.advance() // "timeout"
	if p.eof() {
		return nil, spanErrorf(tok.Span, "timeout requires a duration (e.g. timeout 30s, timeout $max_wait)")
	}
	arg, err := p.parseTerm()
	if err != nil {
		return nil, spanErrorf(tok.Span, "timeout: %v", err)
	}
	return &TimeoutExpr{Arg: arg, Span: p.spanFrom(tok.Pos)}, nil
}

// parseIterationExpr consumes an 'iteration' keyword followed
// by an argument sub-expression producing a non-negative integer
// at evaluation time.  Literal counts still work ("iteration 10")
// but the argument may also be a variable reference or any
// primary that reduces to a scalar.
func (p *exprParser) parseIterationExpr() (Expr, error) {
	tok := p.advance() // "iteration"
	if p.eof() {
		return nil, spanErrorf(tok.Span, "iteration requires a count (e.g. iteration 10, iteration $max)")
	}
	arg, err := p.parseTerm()
	if err != nil {
		return nil, spanErrorf(tok.Span, "iteration: %v", err)
	}
	return &IterationExpr{Arg: arg, Span: p.spanFrom(tok.Pos)}, nil
}

// parseCommandArgs turns a command's token run into argument
// expressions. Each token becomes a primary expression; a
// TokenAssign is preserved as a literal "=" only inside the alias
// builtin, which uses the sigil syntactically ("alias name = expansion").
//
// A leading '(' starts a parenthesised expression that runs through
// the same parser used by let RHSes and assert operands. The whole
// '(EXPR)' group becomes one argument whose value is computed at
// command-eval time; downstream evalArg evaluates the expression
// and wraps the resulting Value as a Scalar / StructuredValueArg.
// This is the orthogonality the wart entry on "|> in argument
// position" called for: 'print ($snap |> jq ".x")' parses the same
// way 'let v = $snap |> jq ".x"' does.
//
// A leading '[' starts a list literal arg the same way, dispatched
// to parseListLiteral via parseExpression. 'print [1 2 3]' produces
// one ListExpr argument rather than five separate literal tokens.
// A bare ')' or ']' outside any opening paren / bracket is a parse
// error; the tokeniser keeps them as their own tokens and the
// resulting "unmatched" diagnostic mirrors the opening-side check.
func parseCommandArgs(tokens []Token, allowAssign bool) ([]Expr, error) {
	exprs := make([]Expr, 0, len(tokens))
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		if t.Kind == TokenAssign {
			if !allowAssign {
				return nil, spanErrorf(t.Span, "unexpected '='; use \"let <name> = <value...>\" for assignment")
			}
			exprs = append(exprs, &LiteralExpr{Text: "=", Span: t.Span})
			i++
			continue
		}
		if t.Kind == TokenWord && t.Text == "(" {
			end, err := findMatchingParen(tokens, i)
			if err != nil {
				return nil, err
			}
			inner := tokens[i+1 : end]
			if len(inner) == 0 || onlySeparators(inner) {
				return nil, spanErrorf(t.Span, "empty parenthesised expression in argument position")
			}
			e, err := parseExpression(inner)
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, e)
			i = end + 1
			continue
		}
		if t.Kind == TokenWord && t.Text == ")" {
			return nil, spanErrorf(t.Span, "unmatched ')' in command argument")
		}
		if t.Kind == TokenWord && t.Text == "[" {
			end, err := findMatchingBracket(tokens, i)
			if err != nil {
				return nil, err
			}
			// parseExpression routes a run starting with '['
			// through parseTerm to parseListLiteral, so handing
			// it the whole '[ ... ]' slice (brackets included)
			// produces a ListExpr argument.
			e, err := parseExpression(tokens[i : end+1])
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, e)
			i = end + 1
			continue
		}
		if t.Kind == TokenWord && t.Text == "]" {
			return nil, spanErrorf(t.Span, "unmatched ']' in command argument")
		}
		e, err := parsePrimary(t)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
		i++
	}
	return exprs, nil
}

// findMatchingParen returns the index of the ')' that closes the
// '(' at openIdx. Tracks nested parens so '(zip $a (range 3))' is
// returned whole. An unmatched '(' is a parse error cited at the
// opening paren.
func findMatchingParen(tokens []Token, openIdx int) (int, error) {
	depth := 1
	for i := openIdx + 1; i < len(tokens); i++ {
		t := tokens[i]
		if t.Kind != TokenWord {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, spanErrorf(tokens[openIdx].Span, "unmatched '(' in command argument")
}

// findMatchingBracket returns the index of the ']' that closes
// the '[' at openIdx. Mirror of findMatchingParen: tracks nested
// '[' / ']' so '[[1] [2 3]]' is returned whole. An unmatched '['
// is a parse error cited at the opening bracket.
func findMatchingBracket(tokens []Token, openIdx int) (int, error) {
	depth := 1
	for i := openIdx + 1; i < len(tokens); i++ {
		t := tokens[i]
		if t.Kind != TokenWord {
			continue
		}
		switch t.Text {
		case "[":
			depth++
		case "]":
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, spanErrorf(tokens[openIdx].Span, "unmatched '[' in command argument")
}

// onlySeparators reports whether tokens is non-empty but contains
// only TokenSep entries (newlines, semicolons). Used by the
// argument-position '(EXPR)' check so '(\n)' is rejected as empty
// the same way '()' is, rather than reaching parseExpression's
// stripSeps and erroring with "empty expression" further from the
// user's source.
func onlySeparators(tokens []Token) bool {
	for _, t := range tokens {
		if t.Kind != TokenSep {
			return false
		}
	}
	return true
}

// parsePrimary converts a single token into a primary expression.
// Command substitutions are recursively parsed so their inner
// syntax is checked eagerly; errors inside the brackets surface at
// parse time rather than at eval time.
func parsePrimary(t Token) (Expr, error) {
	switch t.Kind {
	case TokenWord:
		return &LiteralExpr{Text: t.Text, Span: t.Span}, nil
	case TokenQuoted:
		return &LiteralExpr{Text: t.Text, Quoted: true, Span: t.Span}, nil
	case TokenVarRef:
		return &VarRefExpr{Name: t.VarName, Path: t.VarPath, Span: t.Span}, nil
	case TokenAdapterRef:
		return &AdapterExpr{Adapter: t.Adapter, Name: t.VarName, Path: t.VarPath, Span: t.Span}, nil
	case TokenInterpString:
		segs := make([]InterpStringSegment, 0, len(t.Segments))
		for _, s := range t.Segments {
			if s.IsLit {
				segs = append(segs, InterpStringSegment{Literal: s.Literal})
				continue
			}
			expr, err := parseInterpBody(s.Inner, s.Span)
			if err != nil {
				return nil, err
			}
			segs = append(segs, InterpStringSegment{Expr: expr})
		}
		return &InterpStringExpr{Segments: segs, Span: t.Span}, nil
	default:
		return nil, spanErrorf(t.Span, "unexpected %q", t.Text)
	}
}

func unaryPredFromToken(t Token) (string, bool) {
	if t.Kind != TokenWord || !IsUnaryPred(t.Text) {
		return "", false
	}
	return t.Text, true
}

func binaryOpFromToken(t Token) (string, bool) {
	if t.Kind != TokenWord || !IsBinaryOp(t.Text) {
		return "", false
	}
	return t.Text, true
}

// stripSeps removes separator tokens from a flat slice. Used when
// folding multi-line condition expressions into a flat operand list.
func stripSeps(tokens []Token) []Token {
	out := make([]Token, 0, len(tokens))
	for _, t := range tokens {
		if t.Kind != TokenSep {
			out = append(out, t)
		}
	}
	return out
}

// SyntaxError is the typed error shape returned by Tokenise,
// Parse, and the runtime evaluators for every diagnosable
// problem. Span carries the offending region; Msg is the
// human-readable message; Cause optionally holds an underlying
// error so errors.Is and errors.As traversals walk through to
// any sentinel a command handler emitted. Error() renders the
// plain "line:col: message" form for string-only callers; the
// renderer-aware paths in cmd/bpfman-shell type-assert via
// errors.As and pull the Span directly so the rust-frame caret
// underlines the actual region.
type SyntaxError struct {
	Span  Span
	Msg   string
	Cause error
}

func (e *SyntaxError) Error() string {
	if e.Span.Pos.Line == 0 {
		return e.Msg
	}
	return fmt.Sprintf("%d:%d: %s", e.Span.Pos.Line, e.Span.Pos.Col, e.Msg)
}

// Unwrap exposes the underlying error so errors.Is and errors.As
// see through the SyntaxError wrapper. Runtime sentinels emitted
// from command handlers stay reachable via errors.Is(err, sentinel)
// after the safety-net wrap at statement boundaries; without
// Unwrap the wrap would erase pointer identity.
func (e *SyntaxError) Unwrap() error { return e.Cause }

// spanErrorf builds a *SyntaxError covering span. Use whenever
// the offending region is known as a Span (the common parser
// case: capture the first token's Span, parse the construct, then
// build the Span from first.Pos through the last consumed token's
// End).
func spanErrorf(span Span, format string, args ...any) error {
	return &SyntaxError{Span: span, Msg: fmt.Sprintf(format, args...)}
}

// SpanErrorf is the exported form of spanErrorf so cmd-side
// handlers (assert verbs, builtins) can build *SyntaxError
// values with a Span and a formatted message without spelling
// out the literal. The internal package-local helper retains
// the lowercase name; this shim is API surface for the rest of
// the bpfman-shell program.
func SpanErrorf(span Span, format string, args ...any) error {
	return spanErrorf(span, format, args...)
}

// wrapSyntaxErr preserves a child *SyntaxError's Span while
// prepending a parent context prefix to its message. Without
// this, fmt.Errorf("%s: %w", prefix, err) builds a wrapper
// whose Error() string carries the prefix but whose underlying
// *SyntaxError.Msg does not, so renderer paths that pull se.Msg
// via errors.As silently lose the prefix. Falls through to a
// plain fmt.Errorf for non-SyntaxError children.
func wrapSyntaxErr(prefix string, err error) error {
	var se *SyntaxError
	if errors.As(err, &se) {
		return &SyntaxError{Span: se.Span, Msg: prefix + ": " + se.Msg, Cause: se.Cause}
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// SpanCarrier marks errors that already carry their own source Span
// and so should pass through frameAtSpan unchanged. cmd-side
// runtime-outcome errors (a subprocess exiting non-zero, future
// launch-failure variants) implement this so they reach the renderer
// as their concrete type instead of being re-wrapped as
// *SyntaxError. The renderer routes them to a citation shape; the
// rust-style frame stays reserved for parser/checker diagnostics and
// runtime errors that identify a wrong construct.
type SpanCarrier interface {
	error
	SourceSpan() Span
}

// frameAtSpan attaches span to err so the renderer can frame the
// diagnostic at a known region. err is preserved as Cause so
// errors.Is/errors.As reach any sentinel underneath; if err is
// already a *SyntaxError the original Span is kept (the inner
// site knew better), and SpanCarrier errors pass through untouched
// so they can be rendered as their own concrete type. Use at every
// point where a runtime error crosses a Span-bearing boundary --
// the command and bind statement evaluators, the program-level
// safety net, future builtin/assert dispatchers.
func frameAtSpan(span Span, err error) error {
	if err == nil {
		return nil
	}
	var se *SyntaxError
	if errors.As(err, &se) {
		return err
	}
	var sc SpanCarrier
	if errors.As(err, &sc) {
		return err
	}
	return &SyntaxError{Span: span, Msg: err.Error(), Cause: err}
}

// FrameAt is the exported form of frameAtSpan. Use from cmd-side
// dispatchers (assert verbs, builtins) that catch a non-typed
// error from a sub-call and want to frame it at the originating
// statement's Span.
func FrameAt(span Span, err error) error { return frameAtSpan(span, err) }

// locErrorf builds a SyntaxError at a single Pos. The Span it
// produces is collapsed at loc; renderers will draw a one-column
// caret. Prefer spanErrorf where the full Span is reachable so
// frames cover the offending run.
func locErrorf(loc Pos, format string, args ...any) error {
	return &SyntaxError{
		Span: Span{Pos: loc, End: loc},
		Msg:  fmt.Sprintf(format, args...),
	}
}
