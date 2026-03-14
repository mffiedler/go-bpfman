package replang

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenKind classifies a lexed token.
type TokenKind int

const (
	// TokenWord is an unquoted word: command name, flag, path, etc.
	TokenWord TokenKind = iota
	// TokenAssign is a standalone "=" token at a token boundary.
	TokenAssign
	// TokenVarRef is a variable reference such as $prog.id or
	// ${prog.maps[0].name}.
	TokenVarRef
	// TokenQuoted is a single- or double-quoted string. The
	// delimiters are stripped; $ is literal inside quotes.
	TokenQuoted
)

// Token is a single lexical element produced by Tokenise.
type Token struct {
	Kind    TokenKind
	Text    string // content (stripped quotes for TokenQuoted)
	VarName string // variable name for TokenVarRef
	VarPath string // field path for TokenVarRef (empty if bare)
}

// Tokenise lexes input into tokens. It strips comments (# and
// everything after it, unless the # appears inside a quoted string),
// recognises variable references, quoted strings, standalone = at
// token boundaries, and plain words.
func Tokenise(input string) ([]Token, error) {
	// Strip comment: find first unquoted #.
	input = stripComment(input)
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	var tokens []Token
	i := 0
	for i < len(input) {
		ch := input[i]

		// Skip whitespace.
		if ch == ' ' || ch == '\t' {
			i++
			continue
		}

		switch {
		case ch == '=' && isTokenStart(tokens):
			tokens = append(tokens, Token{Kind: TokenAssign, Text: "="})
			i++

		case ch == '$':
			tok, n, err := lexVarRef(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
			i += n

		case ch == '"' || ch == '\'':
			tok, n, err := lexQuoted(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, tok)
			i += n

		default:
			tok, n := lexWord(input, i)
			tokens = append(tokens, tok)
			i += n
		}
	}

	return tokens, nil
}

// isTokenStart returns true when the current position is at a token
// boundary where = should be treated as a standalone TokenAssign.
// This is true when = appears as the entire next token (preceded by
// whitespace or start of input) rather than embedded in a word like
// KEY=VALUE.
func isTokenStart(tokens []Token) bool {
	// = is only standalone when it follows at least one token
	// (the LHS identifier). The caller already skips whitespace
	// before reaching =, so if we get here the = is at a token
	// boundary.
	return len(tokens) > 0
}

// stripComment removes the portion of input from the first unquoted
// # to the end.
func stripComment(input string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case ch == '#' && !inSingle && !inDouble:
			return input[:i]
		}
	}
	return input
}

// lexVarRef lexes a variable reference starting at input[pos] which
// must be '$'. It handles both bare ($name.path) and braced
// (${name.path[0]}) forms.
func lexVarRef(input string, pos int) (Token, int, error) {
	i := pos + 1 // skip $
	if i >= len(input) {
		return Token{}, 0, fmt.Errorf("unexpected end of input after $")
	}

	if input[i] == '{' {
		return lexBracedVarRef(input, pos)
	}
	return lexBareVarRef(input, pos)
}

// lexBareVarRef lexes $name or $name.path.
func lexBareVarRef(input string, pos int) (Token, int, error) {
	i := pos + 1 // skip $

	// The variable name must start with a letter or underscore.
	if i >= len(input) || !isIdentStart(input[i]) {
		return Token{}, 0, fmt.Errorf("invalid variable reference: expected identifier after $")
	}

	// Consume the identifier part of the name.
	nameStart := i
	for i < len(input) && isIdentContinue(input[i]) {
		i++
	}
	name := input[nameStart:i]

	// Consume optional path: dots, identifiers, and [n] indexing.
	pathStart := i
	for i < len(input) {
		if input[i] == '.' {
			i++
			if i >= len(input) || !isIdentStart(input[i]) {
				// Trailing dot -- include it in the path and stop.
				break
			}
			for i < len(input) && isIdentContinue(input[i]) {
				i++
			}
		} else if input[i] == '[' {
			j := i + 1
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			if j >= len(input) || input[j] != ']' {
				break
			}
			i = j + 1
		} else {
			break
		}
	}
	path := input[pathStart:i]

	// Strip leading dot from path.
	if len(path) > 0 && path[0] == '.' {
		path = path[1:]
	}

	tok := Token{
		Kind:    TokenVarRef,
		Text:    input[pos:i],
		VarName: name,
		VarPath: path,
	}
	return tok, i - pos, nil
}

// lexBracedVarRef lexes ${name.path[0]}.
func lexBracedVarRef(input string, pos int) (Token, int, error) {
	i := pos + 2 // skip ${

	// Must not be empty.
	if i >= len(input) || input[i] == '}' {
		return Token{}, 0, fmt.Errorf("empty variable reference: ${}")
	}

	// The variable name must start with a letter or underscore.
	if !isIdentStart(input[i]) {
		return Token{}, 0, fmt.Errorf("invalid variable reference: expected identifier after ${")
	}

	nameStart := i
	for i < len(input) && isIdentContinue(input[i]) {
		i++
	}
	name := input[nameStart:i]

	// Consume optional path inside braces.
	pathStart := i
	for i < len(input) && input[i] != '}' {
		i++
	}
	if i >= len(input) {
		return Token{}, 0, fmt.Errorf("unterminated variable reference: missing }")
	}
	path := input[pathStart:i]
	i++ // skip }

	// Strip leading dot from path.
	if len(path) > 0 && path[0] == '.' {
		path = path[1:]
	}

	tok := Token{
		Kind:    TokenVarRef,
		Text:    input[pos:i],
		VarName: name,
		VarPath: path,
	}
	return tok, i - pos, nil
}

// lexQuoted lexes a single- or double-quoted string. $ is literal
// inside quotes; no backslash escapes.
func lexQuoted(input string, pos int) (Token, int, error) {
	quote := input[pos]
	i := pos + 1
	for i < len(input) && input[i] != quote {
		i++
	}
	if i >= len(input) {
		return Token{}, 0, fmt.Errorf("unterminated %c-quoted string", quote)
	}
	text := input[pos+1 : i]
	i++ // skip closing quote
	tok := Token{Kind: TokenQuoted, Text: text}
	return tok, i - pos, nil
}

// lexWord consumes a word token: everything until whitespace, $, ", ', or #.
func lexWord(input string, pos int) (Token, int) {
	i := pos
	for i < len(input) {
		ch := input[i]
		if ch == ' ' || ch == '\t' || ch == '$' || ch == '"' || ch == '\'' || ch == '#' {
			break
		}
		i++
	}
	tok := Token{Kind: TokenWord, Text: input[pos:i]}
	return tok, i - pos
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isIdentContinue(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// IsIdent reports whether s is a valid identifier: [a-zA-Z_][a-zA-Z0-9_]*.
func IsIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
		} else {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				return false
			}
		}
	}
	return true
}
