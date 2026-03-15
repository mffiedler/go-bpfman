package replang

import "fmt"

// Line is the result of parsing a tokenised input line.
//
// For a let-assignment (let name = command...): VarName is the
// identifier, Command holds the command tokens after "=".
//
// For a set-binding (set name = value): VarName is the identifier,
// Command holds the single value token, and IsSet is true.
//
// For a plain command: VarName is empty and Command holds all tokens.
type Line struct {
	VarName string
	Command []Token
	IsSet   bool
}

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

// ParseLine inspects the token sequence to determine whether this is
// a let-assignment (let name = command...), a set-binding (set name =
// value), or a plain command. TokenAssign is only valid inside let
// and set expressions; its presence elsewhere is a syntax error.
func ParseLine(tokens []Token) (Line, error) {
	if len(tokens) == 0 {
		return Line{}, nil
	}

	if tokens[0].Kind == TokenWord {
		switch tokens[0].Text {
		case "let":
			name, cmd, err := parseBinding("let", tokens)
			if err != nil {
				return Line{}, err
			}
			return Line{VarName: name, Command: cmd}, nil

		case "set":
			name, rhs, err := parseBinding("set", tokens)
			if err != nil {
				return Line{}, err
			}
			if len(rhs) != 1 {
				return Line{}, fmt.Errorf("set requires exactly one value after '='")
			}
			return Line{VarName: name, Command: rhs, IsSet: true}, nil
		}
	}

	// TokenAssign is only valid inside let/set expressions. Its
	// presence in a plain command is a syntax error.
	for i, tok := range tokens {
		if tok.Kind == TokenAssign {
			return Line{}, fmt.Errorf("unexpected '=' at position %d; use \"let <name> = <command...>\" for assignment", i+1)
		}
	}

	return Line{Command: tokens}, nil
}
