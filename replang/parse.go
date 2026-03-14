package replang

import "fmt"

// Line is the result of parsing a tokenised input line. If VarName is
// non-empty the line is an assignment; Command holds the tokens after
// the "=".
type Line struct {
	VarName string
	Command []Token
}

// ParseLine inspects the token sequence to determine whether this is
// an assignment (name = command...) or a plain command. For an
// assignment, tokens[0] must be a Word that is a valid identifier and
// tokens[1] must be TokenAssign.
func ParseLine(tokens []Token) (Line, error) {
	if len(tokens) == 0 {
		return Line{}, nil
	}

	// Check for assignment: Word Assign ...
	if len(tokens) >= 2 && tokens[0].Kind == TokenWord && tokens[1].Kind == TokenAssign {
		name := tokens[0].Text
		if !IsIdent(name) {
			return Line{}, fmt.Errorf("invalid variable name: %q", name)
		}
		cmd := tokens[2:]
		if len(cmd) == 0 {
			return Line{}, fmt.Errorf("expected command after =")
		}
		return Line{VarName: name, Command: cmd}, nil
	}

	return Line{Command: tokens}, nil
}
