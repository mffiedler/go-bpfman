package shell

import "fmt"

// Stmt is the result of parsing a tokenised input line. It is a
// sealed sum type with three variants: LetStmt for command-result
// bindings, SetStmt for scalar bindings, and CommandStmt for plain
// commands.
type Stmt interface {
	isStmt()
}

// LetStmt represents a let-assignment: let name = command...
// Name is the identifier and Command holds the tokens after "=".
type LetStmt struct {
	Name    string
	Command []Token
}

func (*LetStmt) isStmt() {}

// SetStmt represents a set-binding: set name = value.
// Name is the identifier and Value holds the single value token.
type SetStmt struct {
	Name  string
	Value Token
}

func (*SetStmt) isStmt() {}

// CommandStmt represents a plain command with no variable binding.
type CommandStmt struct {
	Tokens []Token
}

func (*CommandStmt) isStmt() {}

// parseBinding handles the shared grammar for "let" and "set":
// keyword name = tokens... It returns the variable name and the
// tokens after "=".
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

// ParseStmt inspects the token sequence to determine whether this is
// a let-assignment (let name = command...), a set-binding (set name =
// value), or a plain command. TokenAssign is only valid inside let
// and set expressions; its presence elsewhere is a syntax error.
//
// Returns nil for empty input.
func ParseStmt(tokens []Token) (Stmt, error) {
	if len(tokens) == 0 {
		return nil, nil
	}

	if tokens[0].Kind == TokenWord {
		switch tokens[0].Text {
		case "let":
			name, cmd, err := parseBinding("let", tokens)
			if err != nil {
				return nil, err
			}
			return &LetStmt{Name: name, Command: cmd}, nil

		case "set":
			name, rhs, err := parseBinding("set", tokens)
			if err != nil {
				return nil, err
			}
			if len(rhs) != 1 {
				return nil, fmt.Errorf("set requires exactly one value after '='")
			}
			return &SetStmt{Name: name, Value: rhs[0]}, nil
		}
	}

	// The "alias" command uses "=" in its own syntax
	// (alias name = expansion), so it is exempt from the stray
	// assignment check.
	if tokens[0].Kind == TokenWord && tokens[0].Text == "alias" {
		return &CommandStmt{Tokens: tokens}, nil
	}

	// TokenAssign is only valid inside let/set expressions. Its
	// presence in a plain command is a syntax error.
	for i, tok := range tokens {
		if tok.Kind == TokenAssign {
			return nil, fmt.Errorf("unexpected '=' at position %d; use \"let <name> = <command...>\" for assignment", i+1)
		}
	}

	return &CommandStmt{Tokens: tokens}, nil
}
