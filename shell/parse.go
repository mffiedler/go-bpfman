package shell

import "fmt"

// Stmt is the result of parsing a tokenised input line or block. It
// is a sealed sum type: LetStmt for variable bindings (RHS is an
// expression), IfStmt for conditional branches, and CommandStmt for
// plain commands.
type Stmt interface {
	isStmt()
}

// LetStmt represents a let-assignment: let name = expr...
// Name is the identifier and Command holds the tokens after "=";
// they are expanded and parsed as an expression at evaluation time.
type LetStmt struct {
	Name    string
	Command []Token
}

// CommandStmt represents a plain command with no variable binding.
type CommandStmt struct {
	Tokens []Token
}

// IfBranch pairs a condition with its body. Used for elif chains.
type IfBranch struct {
	Cond []Token // tokens forming the condition expression
	Body []Stmt  // statements in the branch body
}

// IfStmt is a conditional branch: if EXPR { ... } elif EXPR { ... }
// else { ... }. Cond and Then are the primary branch; Elifs is an
// ordered list of alternative branches; Else is the final catch-all
// (may be empty).
type IfStmt struct {
	Cond  []Token
	Then  []Stmt
	Elifs []IfBranch
	Else  []Stmt
}

func (*LetStmt) isStmt()     {}
func (*CommandStmt) isStmt() {}
func (*IfStmt) isStmt()      {}

// parseBinding handles the "let name = tokens..." shape and returns
// the variable name and the RHS tokens. The RHS is returned raw; the
// caller turns it into an expression via ParseExpr at evaluation
// time.
func parseBinding(keyword string, tokens []Token) (string, []Token, error) {
	if len(tokens) < 4 {
		return "", nil, fmt.Errorf("%s requires: %s <name> = <value...>", keyword, keyword)
	}
	if tokens[1].Kind != TokenWord {
		return "", nil, fmt.Errorf("%s requires an identifier, got %q", keyword, tokens[1].Text)
	}
	name := tokens[1].Text
	if !IsIdent(name) {
		return "", nil, fmt.Errorf("invalid variable name: %q", name)
	}
	if tokens[2].Kind != TokenAssign {
		return "", nil, fmt.Errorf("%s requires: %s <name> = <value...> (missing '=')", keyword, keyword)
	}
	rhs := tokens[3:]
	if len(rhs) == 0 {
		return "", nil, fmt.Errorf("expected value after =")
	}
	return name, rhs, nil
}

// ParseStmt parses one statement from the start of tokens and
// returns it along with the number of tokens consumed. It skips
// leading separators. Returns (nil, n, nil) when only separators
// remain.
//
// For backwards-compatible single-statement parsing, callers that
// already have a full token slice representing one statement should
// use ParseProgram or continue to treat the entire slice as one
// statement — a trailing separator is tolerated.
func ParseStmt(tokens []Token) (Stmt, error) {
	stmts, err := ParseProgram(tokens)
	if err != nil {
		return nil, err
	}
	switch len(stmts) {
	case 0:
		return nil, nil
	case 1:
		return stmts[0], nil
	default:
		return nil, fmt.Errorf("expected a single statement, got %d", len(stmts))
	}
}

// ParseProgram parses a sequence of statements separated by
// TokenSep. Statements may be let-assignments, command statements,
// or if-blocks.
func ParseProgram(tokens []Token) ([]Stmt, error) {
	var stmts []Stmt
	i := 0
	for i < len(tokens) {
		// Skip separators.
		if tokens[i].Kind == TokenSep {
			i++
			continue
		}
		stmt, consumed, err := parseOneStmt(tokens[i:])
		if err != nil {
			return nil, err
		}
		i += consumed
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
	}
	return stmts, nil
}

// parseOneStmt parses one statement at the start of tokens and
// returns (stmt, consumed, err). consumed counts the tokens
// belonging to the statement (including any closing `}` for an
// if-block) but does not consume the trailing separator. Callers
// skip separators themselves.
func parseOneStmt(tokens []Token) (Stmt, int, error) {
	if len(tokens) == 0 {
		return nil, 0, nil
	}
	first := tokens[0]
	if first.Kind == TokenWord {
		switch first.Text {
		case "if":
			return parseIfStmt(tokens)
		case "let":
			return parseLetStmt(tokens)
		case "alias":
			return parseCommandStmt(tokens)
		}
	}
	return parseCommandStmt(tokens)
}

// parseLetStmt consumes tokens forming a single let-statement. The
// RHS runs to the next top-level TokenSep.
func parseLetStmt(tokens []Token) (Stmt, int, error) {
	end := findTopLevelSep(tokens)
	stmt, err := parseLetStmtSlice(tokens[:end])
	if err != nil {
		return nil, 0, err
	}
	return stmt, end, nil
}

func parseLetStmtSlice(tokens []Token) (*LetStmt, error) {
	name, cmd, err := parseBinding("let", tokens)
	if err != nil {
		return nil, err
	}
	for _, t := range cmd {
		if t.Kind == TokenAssign {
			return nil, fmt.Errorf("unexpected '=' in let RHS; use [cmd ...] for command substitution")
		}
	}
	return &LetStmt{Name: name, Command: cmd}, nil
}

// parseCommandStmt consumes tokens for a plain command statement. It
// runs until the next top-level separator.
func parseCommandStmt(tokens []Token) (Stmt, int, error) {
	end := findTopLevelSep(tokens)
	slice := tokens[:end]
	if len(slice) == 0 {
		return nil, end, nil
	}
	if slice[0].Kind != TokenWord || slice[0].Text != "alias" {
		for i, tok := range slice {
			if tok.Kind == TokenAssign {
				return nil, 0, fmt.Errorf("unexpected '=' at position %d; use \"let <name> = <value...>\" for assignment", i+1)
			}
		}
	}
	return &CommandStmt{Tokens: slice}, end, nil
}

// parseIfStmt consumes tokens for an if-elif-else statement. The
// condition runs to the next top-level `{`; the body is parsed
// between braces; optional `elif` and `else` branches follow.
func parseIfStmt(tokens []Token) (Stmt, int, error) {
	i := 1 // past "if"
	cond, advance, err := readCondition(tokens[i:])
	if err != nil {
		return nil, 0, fmt.Errorf("if: %w", err)
	}
	i += advance

	then, advance, err := readBlock(tokens[i:])
	if err != nil {
		return nil, 0, fmt.Errorf("if: %w", err)
	}
	i += advance

	var elifs []IfBranch
	var els []Stmt

	for i < len(tokens) {
		// Skip separators between `}` and `elif`/`else`.
		j := i
		for j < len(tokens) && tokens[j].Kind == TokenSep {
			j++
		}
		if j >= len(tokens) || tokens[j].Kind != TokenWord {
			break
		}
		switch tokens[j].Text {
		case "elif":
			i = j + 1
			cond, advance, err := readCondition(tokens[i:])
			if err != nil {
				return nil, 0, fmt.Errorf("elif: %w", err)
			}
			i += advance
			body, advance, err := readBlock(tokens[i:])
			if err != nil {
				return nil, 0, fmt.Errorf("elif: %w", err)
			}
			i += advance
			elifs = append(elifs, IfBranch{Cond: cond, Body: body})
		case "else":
			i = j + 1
			body, advance, err := readBlock(tokens[i:])
			if err != nil {
				return nil, 0, fmt.Errorf("else: %w", err)
			}
			i += advance
			els = body
			return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els}, i, nil
		default:
			return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els}, i, nil
		}
	}
	return &IfStmt{Cond: cond, Then: then, Elifs: elifs, Else: els}, i, nil
}

// readCondition reads tokens up to the next top-level `{`. Returns
// the condition tokens and the number of tokens consumed (the `{`
// itself is not consumed).
func readCondition(tokens []Token) ([]Token, int, error) {
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		if t.Kind == TokenSep {
			i++
			continue
		}
		if t.Kind == TokenWord && t.Text == "{" {
			if i == 0 {
				return nil, 0, fmt.Errorf("expected condition before '{'")
			}
			return stripSeps(tokens[:i]), i, nil
		}
		i++
	}
	return nil, 0, fmt.Errorf("expected '{' after condition")
}

// readBlock reads tokens between a `{` and its matching `}`,
// returning the parsed statements and the total tokens consumed
// (including both braces). Nested braces (e.g. inside inner
// if-statements) are balanced.
func readBlock(tokens []Token) ([]Stmt, int, error) {
	if len(tokens) == 0 || tokens[0].Kind != TokenWord || tokens[0].Text != "{" {
		return nil, 0, fmt.Errorf("expected '{'")
	}
	depth := 1
	i := 1
	for i < len(tokens) && depth > 0 {
		t := tokens[i]
		if t.Kind == TokenWord {
			switch t.Text {
			case "{":
				depth++
			case "}":
				depth--
			}
		}
		if depth == 0 {
			break
		}
		i++
	}
	if depth > 0 {
		return nil, 0, fmt.Errorf("unterminated block: missing '}'")
	}
	inner := tokens[1:i]
	stmts, err := ParseProgram(inner)
	if err != nil {
		return nil, 0, err
	}
	return stmts, i + 1, nil
}

// findTopLevelSep returns the index of the next top-level TokenSep
// in tokens, or len(tokens) if none. Nested braces and brackets are
// tracked so separators inside them do not count.
func findTopLevelSep(tokens []Token) int {
	depth := 0 // braces; brackets are inside TokenCmdSub which is a
	//          single token at this level, so not tracked here.
	for i, t := range tokens {
		if t.Kind == TokenWord {
			switch t.Text {
			case "{":
				depth++
			case "}":
				depth--
			}
		}
		if depth <= 0 && t.Kind == TokenSep {
			return i
		}
	}
	return len(tokens)
}

// stripSeps removes TokenSep entries from the slice. Used for
// condition extraction so that a condition spanning lines retains
// only its operands and operators.
func stripSeps(tokens []Token) []Token {
	out := make([]Token, 0, len(tokens))
	for _, t := range tokens {
		if t.Kind != TokenSep {
			out = append(out, t)
		}
	}
	return out
}
