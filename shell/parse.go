package shell

import "fmt"

// Program is the root of a parsed source unit: an ordered sequence
// of statements with the source location of the first token.
type Program struct {
	Stmts []Stmt
	Loc
}

// Stmt is the sealed sum type for statements.
type Stmt interface {
	stmtNode()
}

// LetStmt binds the result of evaluating RHS to Name. Name is
// guaranteed to be a valid identifier by the parser.
type LetStmt struct {
	Name string
	RHS  Expr
	Loc
}

// IfBranch pairs a condition expression with a block body. Used
// for the primary branch and each elif.
type IfBranch struct {
	Cond Expr
	Body []Stmt
	Loc
}

// IfStmt is an if-elif-else conditional.
type IfStmt struct {
	Cond  Expr
	Then  []Stmt
	Elifs []IfBranch
	Else  []Stmt
	Loc
}

// CommandStmt is a plain command invocation. The first element of
// Args names the command.
type CommandStmt struct {
	Args []Expr
	Loc
}

// ForEachStmt iterates a block over the elements of a list.  At
// eval time List is evaluated to a Value; it must be a structured
// list, and each element is bound to Name in the Session for the
// duration of its iteration.  The binding persists after the loop
// ends, matching shell-style for-each semantics.
type ForEachStmt struct {
	Name string
	List Expr
	Body []Stmt
	Loc
}

// BreakStmt terminates the nearest enclosing ForEachStmt. Outside
// a loop it is a runtime error.
type BreakStmt struct {
	Loc
}

// ContinueStmt skips the remainder of the current ForEachStmt
// iteration and advances to the next element.  Outside a loop it
// is a runtime error.
type ContinueStmt struct {
	Loc
}

func (*LetStmt) stmtNode()      {}
func (*IfStmt) stmtNode()       {}
func (*CommandStmt) stmtNode()  {}
func (*ForEachStmt) stmtNode()  {}
func (*BreakStmt) stmtNode()    {}
func (*ContinueStmt) stmtNode() {}

// Parse turns a token stream into a *Program. Every parse error
// carries a source location derived from the offending token.
func Parse(tokens []Token) (*Program, error) {
	p := &parser{tokens: tokens}
	stmts, err := p.parseStmts(p.atEOF)
	if err != nil {
		return nil, err
	}
	var start Loc
	if len(tokens) > 0 {
		start = tokens[0].Loc
	}
	return &Program{Stmts: stmts, Loc: start}, nil
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
		stmt, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			stmts = append(stmts, stmt)
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
		case "break":
			return p.parseBreakStmt()
		case "continue":
			return p.parseContinueStmt()
		}
	}
	return p.parseCommandStmt()
}

func (p *parser) parseBreakStmt() (Stmt, error) {
	t := p.advance()
	if err := p.rejectTrailingArgs("break"); err != nil {
		return nil, err
	}
	return &BreakStmt{Loc: t.Loc}, nil
}

func (p *parser) parseContinueStmt() (Stmt, error) {
	t := p.advance()
	if err := p.rejectTrailingArgs("continue"); err != nil {
		return nil, err
	}
	return &ContinueStmt{Loc: t.Loc}, nil
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
	return locErrorf(t.Loc, "%s takes no arguments; got %q", name, t.Text)
}

func (p *parser) parseLetStmt() (Stmt, error) {
	letTok := p.advance() // "let"
	if p.atEOF() || p.peek().Kind != TokenWord {
		return nil, locErrorf(letTok.Loc, "let requires an identifier, got %q", p.peek().Text)
	}
	nameTok := p.advance()
	name := nameTok.Text
	if !IsIdent(name) {
		return nil, locErrorf(nameTok.Loc, "invalid variable name: %q", name)
	}
	if p.atEOF() || p.peek().Kind != TokenAssign {
		return nil, locErrorf(letTok.Loc, "let requires: let <name> = <value...> (missing '=')")
	}
	p.advance() // "="
	rhsTokens, err := p.takeStmtTokens(true)
	if err != nil {
		return nil, err
	}
	if len(rhsTokens) == 0 {
		return nil, locErrorf(letTok.Loc, "let requires: let <name> = <value...>")
	}
	rhs, err := parseExpression(rhsTokens)
	if err != nil {
		return nil, err
	}
	return &LetStmt{Name: name, RHS: rhs, Loc: letTok.Loc}, nil
}

// takeStmtTokens collects tokens belonging to the current statement
// up to the next separator or block marker. When rejectAssign is
// true a stray TokenAssign inside the collected range is an error —
// used on a let RHS to catch "let x = a = b".
func (p *parser) takeStmtTokens(rejectAssign bool) ([]Token, error) {
	var buf []Token
	for !p.atEOF() {
		t := p.peek()
		if t.Kind == TokenSep {
			break
		}
		if t.Kind == TokenWord && (t.Text == "{" || t.Text == "}") {
			break
		}
		if rejectAssign && t.Kind == TokenAssign {
			return nil, locErrorf(t.Loc, "unexpected '=' in let RHS; use [cmd ...] for command substitution")
		}
		buf = append(buf, t)
		p.pos++
	}
	return buf, nil
}

func (p *parser) parseCommandStmt() (Stmt, error) {
	first := p.peek()
	startLoc := first.Loc
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
			return nil, locErrorf(t.Loc, "unexpected '='; use \"let <name> = <value...>\" for assignment")
		}
		buf = append(buf, t)
		p.pos++
	}
	if len(buf) == 0 {
		return nil, nil
	}
	args, err := parseCommandArgs(buf, isAlias)
	if err != nil {
		return nil, err
	}
	return &CommandStmt{Args: args, Loc: startLoc}, nil
}

func (p *parser) parseForEachStmt() (Stmt, error) {
	feTok := p.advance() // "foreach"
	if p.atEOF() || p.peek().Kind != TokenWord {
		return nil, locErrorf(feTok.Loc, "foreach requires: foreach <name> in <expr> { ... }")
	}
	nameTok := p.advance()
	name := nameTok.Text
	if name == "in" {
		return nil, locErrorf(nameTok.Loc, "foreach requires a variable name before 'in'")
	}
	if !IsIdent(name) {
		return nil, locErrorf(nameTok.Loc, "invalid variable name: %q", name)
	}
	if p.atEOF() || p.peek().Kind != TokenWord || p.peek().Text != "in" {
		return nil, locErrorf(feTok.Loc, "foreach requires 'in' after the loop variable")
	}
	p.advance() // "in"
	listTokens, err := p.takeUntilOpenBrace()
	if err != nil {
		return nil, err
	}
	if len(listTokens) == 0 {
		return nil, locErrorf(feTok.Loc, "foreach requires: foreach <name> in <expr> { ... }")
	}
	list, err := parseExpression(listTokens)
	if err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &ForEachStmt{Name: name, List: list, Body: body, Loc: feTok.Loc}, nil
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
		return nil, fmt.Errorf("if: %w", err)
	}
	then, err := p.parseBlock()
	if err != nil {
		return nil, fmt.Errorf("if: %w", err)
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
				return nil, fmt.Errorf("elif: %w", err)
			}
			eb, err := p.parseBlock()
			if err != nil {
				return nil, fmt.Errorf("elif: %w", err)
			}
			elifs = append(elifs, IfBranch{Cond: ec, Body: eb, Loc: elifTok.Loc})
		case "else":
			p.advance()
			eb, err := p.parseBlock()
			if err != nil {
				return nil, fmt.Errorf("else: %w", err)
			}
			els = eb
			return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els, Loc: ifTok.Loc}, nil
		default:
			return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els, Loc: ifTok.Loc}, nil
		}
	}
	return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els, Loc: ifTok.Loc}, nil
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
//   parseComparison   -- binary comparison (eq, ne, <, >= ...)
//   parseUnaryOr      -- unary predicate (not-empty, true, false)
//   parseThread       -- threading chain (|>)
//   parseTerm         -- primary token (literal, varref, adapter,
//                                       cmdsub)
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
	e, err := ep.parseComparison()
	if err != nil {
		return nil, err
	}
	if !ep.eof() {
		t := ep.peek()
		return nil, locErrorf(t.Loc, "unexpected token %q after expression; wrap commands in [...] for substitution", t.Text)
	}
	return e, nil
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

// parseComparison recognises the optional binary-comparison infix
// around a tighter sub-expression.  At most one binary operator
// per expression matches the current grammar; anything else the
// caller flags via the "unexpected trailing token" check in
// parseExpression.
func (p *exprParser) parseComparison() (Expr, error) {
	left, err := p.parseUnaryOr()
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
	right, err := p.parseUnaryOr()
	if err != nil {
		return nil, err
	}
	return &BinaryExpr{Left: left, Op: op, Right: right, Loc: opTok.Loc}, nil
}

// parseUnaryOr recognises a unary-predicate prefix applied to a
// primary operand.  Because "true" and "false" are both
// unary-predicate words AND common literals (e.g. the RHS of
// "eq true"), the rule is context-sensitive: a pred word
// becomes a UnaryExpr only when the following token could be
// its operand — a primary position, not a binary operator,
// thread, or end of input.  Otherwise it falls through to the
// tighter thread level where it is parsed as a literal.
func (p *exprParser) parseUnaryOr() (Expr, error) {
	if pred, ok := unaryPredFromToken(p.peek()); ok && p.operandFollowsPred() {
		predTok := p.advance()
		operand, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand, Loc: predTok.Loc}, nil
	}
	return p.parseThread()
}

// operandFollowsPred reports whether the token immediately after
// the current one could syntactically be a unary predicate's
// operand: neither a binary-comparison word, a '|>', nor the
// end of input.
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
	return true
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
		args, err := p.parseThreadRHS(threadTok.Loc)
		if err != nil {
			return nil, err
		}
		lhs = &ThreadExpr{LHS: lhs, Args: args, Loc: threadTok.Loc}
	}
	return lhs, nil
}

// parseThreadRHS consumes the command-call tokens that follow a
// '|>'.  It stops at the next '|>', at a binary-op word (so the
// comparison level can pick it up), or at end of input.  A
// literal binary-op word used as a command argument must be
// quoted.
func (p *exprParser) parseThreadRHS(threadLoc Loc) ([]Expr, error) {
	var args []Expr
	for !p.eof() {
		t := p.peek()
		if t.Kind == TokenThread {
			break
		}
		if _, isBinOp := binaryOpFromToken(t); isBinOp {
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

// parseTerm consumes a single token as a primary expression —
// literal, varref, adapter, or command substitution.
func (p *exprParser) parseTerm() (Expr, error) {
	if p.eof() {
		return nil, fmt.Errorf("expected expression, got end of input")
	}
	t := p.advance()
	return parsePrimary(t)
}

// parseCommandArgs turns a command's token run into argument
// expressions. Each token becomes a primary expression; a
// TokenAssign is preserved as a literal "=" only inside the alias
// builtin, which uses the sigil syntactically ("alias name = expansion").
func parseCommandArgs(tokens []Token, allowAssign bool) ([]Expr, error) {
	exprs := make([]Expr, 0, len(tokens))
	for _, t := range tokens {
		if t.Kind == TokenAssign {
			if !allowAssign {
				return nil, locErrorf(t.Loc, "unexpected '='; use \"let <name> = <value...>\" for assignment")
			}
			exprs = append(exprs, &LiteralExpr{Text: "=", Loc: t.Loc})
			continue
		}
		e, err := parsePrimary(t)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, e)
	}
	return exprs, nil
}

// parsePrimary converts a single token into a primary expression.
// Command substitutions are recursively parsed so their inner
// syntax is checked eagerly; errors inside the brackets surface at
// parse time rather than at eval time.
func parsePrimary(t Token) (Expr, error) {
	switch t.Kind {
	case TokenWord:
		return &LiteralExpr{Text: t.Text, Loc: t.Loc}, nil
	case TokenQuoted:
		return &LiteralExpr{Text: t.Text, Quoted: true, Loc: t.Loc}, nil
	case TokenVarRef:
		return &VarRefExpr{Name: t.VarName, Path: t.VarPath, Loc: t.Loc}, nil
	case TokenAdapterRef:
		return &AdapterExpr{Adapter: t.Adapter, Name: t.VarName, Path: t.VarPath, Loc: t.Loc}, nil
	case TokenCmdSub:
		innerTokens, err := Tokenise(t.Inner)
		if err != nil {
			return nil, locErrorf(t.Loc, "command substitution: %v", err)
		}
		inner, err := Parse(innerTokens)
		if err != nil {
			return nil, locErrorf(t.Loc, "command substitution: %v", err)
		}
		return &CmdSubExpr{Inner: inner, Loc: t.Loc}, nil
	default:
		return nil, locErrorf(t.Loc, "unexpected token %q", t.Text)
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

// locErrorf formats a diagnostic with a "line:col:" prefix when the
// location is known. Callers use it for every parse error so the
// REPL and scripted runs can point at the offending token.
func locErrorf(loc Loc, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if loc.Line == 0 {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("%d:%d: %s", loc.Line, loc.Col, msg)
}
