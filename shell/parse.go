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

func (*LetStmt) stmtNode()     {}
func (*IfStmt) stmtNode()      {}
func (*CommandStmt) stmtNode() {}

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
		}
	}
	return p.parseCommandStmt()
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

// parseExpression handles the single-expression grammar used by let
// RHS and if/elif conditions: primary (1 token), unary predicate
// (2 tokens), or binary comparison (3 tokens). Any other token
// count is a syntax error.
func parseExpression(tokens []Token) (Expr, error) {
	tokens = stripSeps(tokens)
	switch len(tokens) {
	case 0:
		return nil, fmt.Errorf("empty expression")
	case 1:
		return parsePrimary(tokens[0])
	case 2:
		pred, ok := unaryPredFromToken(tokens[0])
		if !ok {
			return nil, locErrorf(tokens[0].Loc, "expected unary predicate as first operand, got %q", tokens[0].Text)
		}
		operand, err := parsePrimary(tokens[1])
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand, Loc: tokens[0].Loc}, nil
	case 3:
		op, ok := binaryOpFromToken(tokens[1])
		if !ok {
			return nil, locErrorf(tokens[1].Loc, "expected binary operator as middle operand, got %q", tokens[1].Text)
		}
		left, err := parsePrimary(tokens[0])
		if err != nil {
			return nil, err
		}
		right, err := parsePrimary(tokens[2])
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Left: left, Op: op, Right: right, Loc: tokens[0].Loc}, nil
	default:
		return nil, locErrorf(tokens[0].Loc, "expression has %d operands; expected 1 (primary), 2 (unary) or 3 (binary)", len(tokens))
	}
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
