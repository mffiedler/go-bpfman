package shell

import (
	"fmt"
	"strings"
	"unicode"
)

// adapterPrefixes is the fixed set of known adapter names recognised
// by the tokeniser. Only these names followed by :$ trigger adapter
// token recognition.
var adapterPrefixes = []string{"file"}

// Loc is a source location. Line and Col are 1-based; Col is a byte
// offset within the line, not a rune offset. The zero value means
// "unknown location".
type Loc struct {
	Line int
	Col  int
}

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
	// TokenAdapterRef is an adapter invocation such as
	// file:$var.path. It carries the adapter name, the variable
	// name, and the optional field path.
	TokenAdapterRef
	// TokenCmdSub is a command substitution [cmd args...]. The
	// Inner field carries the raw inner text (without the outer
	// brackets); Expand recursively tokenises and expands it.
	TokenCmdSub
	// TokenSep is a statement separator: a newline or a semicolon.
	// Consecutive separators are collapsed at parse time.
	TokenSep
	// TokenThread is the '|>' operator at a token boundary — the
	// value-threading composition operator that feeds the LHS
	// Value into the RHS command's last argument slot.  Matches
	// the '|>' sigil used by F#, OCaml, Elixir, Julia, and R;
	// semantically equivalent to Clojure's `->>` thread-last
	// macro.  Inside a bare word or quoted string, '|>' stays
	// part of the surrounding literal.
	TokenThread
)

// Token is a single lexical element produced by Tokenise.
type Token struct {
	Kind    TokenKind
	Text    string // content (stripped quotes for TokenQuoted)
	VarName string // variable name for TokenVarRef and TokenAdapterRef
	VarPath string // field path for TokenVarRef and TokenAdapterRef (empty if bare)
	Adapter string // adapter name for TokenAdapterRef (e.g. "file")
	Inner   string // raw inner text for TokenCmdSub (between brackets)
	Loc     Loc    // source location of the token's first byte
}

// Tokenise lexes input into tokens. It strips comments (# and
// everything after it, unless the # appears inside a quoted string),
// recognises variable references, quoted strings, standalone = at
// token boundaries, and plain words.
func Tokenise(input string) ([]Token, error) {
	// stripComment preserves offsets by replacing stripped bytes
	// with spaces, so positions into the returned string still map
	// back to the original input's line/column.
	input = stripComment(input)
	if strings.TrimSpace(input) == "" {
		return nil, nil
	}

	lineStarts := buildLineStarts(input)

	emit := func(tokens []Token, start int, tok Token) []Token {
		tok.Loc = locAt(start, lineStarts)
		return append(tokens, tok)
	}

	var tokens []Token
	i := 0
	for i < len(input) {
		ch := input[i]

		// Skip whitespace (but not newlines, which are separators).
		if ch == ' ' || ch == '\t' || ch == '\r' {
			i++
			continue
		}

		start := i
		switch {
		case ch == '\n' || ch == ';':
			tokens = emit(tokens, start, Token{Kind: TokenSep, Text: string(ch)})
			i++

		case ch == '{' || ch == '}' || ch == '(' || ch == ')':
			tokens = emit(tokens, start, Token{Kind: TokenWord, Text: string(ch)})
			i++

		case ch == '+' || ch == '*' || ch == '%':
			// Arithmetic operators that cannot appear inside a
			// bare word (unlike '-' and '/', which are valid
			// word-interior characters because of negative
			// literals, flags, and paths).  Emitting them as
			// single-char tokens lets "1+1", "$x*2", "7%3" split
			// cleanly without requiring surrounding whitespace.
			tokens = emit(tokens, start, Token{Kind: TokenWord, Text: string(ch)})
			i++

		case ch == '=' && isTokenStart(tokens):
			// Distinguish == (comparison) from = (assignment).
			if i+1 < len(input) && input[i+1] == '=' {
				tokens = emit(tokens, start, Token{Kind: TokenWord, Text: "=="})
				i += 2
			} else {
				tokens = emit(tokens, start, Token{Kind: TokenAssign, Text: "="})
				i++
			}

		case ch == '$':
			tok, n, err := lexVarRef(input, i)
			if err != nil {
				return nil, err
			}
			tokens = emit(tokens, start, tok)
			i += n

		case ch == '"' || ch == '\'':
			tok, n, err := lexQuoted(input, i)
			if err != nil {
				return nil, err
			}
			tokens = emit(tokens, start, tok)
			i += n

		case ch == '[':
			tok, n, err := lexCmdSub(input, i)
			if err != nil {
				return nil, err
			}
			tokens = emit(tokens, start, tok)
			i += n

		case ch == ']':
			return nil, fmt.Errorf("unmatched ']'")

		case ch == '|' && i+1 < len(input) && input[i+1] == '>':
			// Reaching this case means the previous byte was
			// whitespace or absent, so '|>' sits at a token
			// boundary.  The lexWord path keeps '|' as an
			// interior word character, so 'a|>b' stays a word.
			tokens = emit(tokens, start, Token{Kind: TokenThread, Text: "|>"})
			i += 2

		default:
			if tok, n, ok := lexAdapterRef(input, i); ok {
				tokens = emit(tokens, start, tok)
				i += n
			} else {
				tok, n := lexWord(input, i)
				tokens = emit(tokens, start, tok)
				i += n
			}
		}
	}

	return tokens, nil
}

// buildLineStarts returns the byte offset at which each line begins.
// Line 1 starts at offset 0; line k+1 starts at the byte following
// the (k)th newline.
func buildLineStarts(input string) []int {
	starts := []int{0}
	for i := 0; i < len(input); i++ {
		if input[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// locAt returns the 1-based line/column for a byte offset. The
// column is a byte offset within the line, counting from 1.
func locAt(pos int, lineStarts []int) Loc {
	// Binary search for the largest k with lineStarts[k] <= pos.
	lo, hi := 0, len(lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lineStarts[mid] <= pos {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return Loc{Line: lo + 1, Col: pos - lineStarts[lo] + 1}
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

// stripComment replaces inline comments with spaces while preserving
// byte offsets so downstream line/column tracking still matches the
// original input. A comment starts at an unquoted '#' and extends to
// (but does not include) the next newline, which is left intact so
// accumulated multi-line input (e.g. an if block spanning lines) still
// sees the separator.
func stripComment(input string) string {
	b := make([]byte, 0, len(input))
	inSingle := false
	inDouble := false
	for i := 0; i < len(input); {
		ch := input[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
			b = append(b, ch)
			i++
		case ch == '"' && !inSingle:
			inDouble = !inDouble
			b = append(b, ch)
			i++
		case ch == '#' && !inSingle && !inDouble:
			for i < len(input) && input[i] != '\n' {
				b = append(b, ' ')
				i++
			}
		default:
			b = append(b, ch)
			i++
		}
	}
	return string(b)
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
	// The path grammar is: segment+ where segment = '.' ident | '[' digits ']'.
	pathStart := i
	for i < len(input) {
		if input[i] == '.' {
			i++
			if i >= len(input) || !isIdentStart(input[i]) {
				return Token{}, 0, fmt.Errorf("invalid variable reference %q: expected identifier after '.'", input[pos:i])
			}
			for i < len(input) && isIdentContinue(input[i]) {
				i++
			}
		} else if input[i] == '[' {
			j := i + 1
			digitStart := j
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			if j == digitStart {
				return Token{}, 0, fmt.Errorf("invalid variable reference %q: expected digits inside '[]'", input[pos:min(j+1, len(input))])
			}
			if j >= len(input) || input[j] != ']' {
				return Token{}, 0, fmt.Errorf("invalid variable reference %q: expected ']' after index", input[pos:min(j+1, len(input))])
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

	// Validate optional path inside braces using the same grammar
	// as bare refs: segment = '.' ident | '[' digits ']'.
	pathStart := i
	for i < len(input) && input[i] != '}' {
		if input[i] == '.' {
			i++
			if i >= len(input) || input[i] == '}' || !isIdentStart(input[i]) {
				return Token{}, 0, fmt.Errorf("invalid variable reference: expected identifier after '.' in ${...}")
			}
			for i < len(input) && input[i] != '}' && isIdentContinue(input[i]) {
				i++
			}
		} else if input[i] == '[' {
			j := i + 1
			digitStart := j
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			if j == digitStart {
				return Token{}, 0, fmt.Errorf("invalid variable reference: expected digits inside '[]' in ${...}")
			}
			if j >= len(input) || input[j] != ']' {
				return Token{}, 0, fmt.Errorf("invalid variable reference: expected ']' after index in ${...}")
			}
			i = j + 1
		} else {
			return Token{}, 0, fmt.Errorf("invalid variable reference: unexpected character %q in ${...} path", input[i])
		}
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

// lexCmdSub lexes a command substitution [cmd args...]. The brackets
// nest; quoted strings inside are skipped so a `]` inside a string
// does not close the substitution. The returned token's Inner field
// carries the raw content between the outer brackets; Expand
// recursively tokenises and expands it at evaluation time.
func lexCmdSub(input string, pos int) (Token, int, error) {
	depth := 1
	i := pos + 1
	for i < len(input) && depth > 0 {
		ch := input[i]
		switch ch {
		case '[':
			depth++
			i++
		case ']':
			depth--
			i++
		case '"', '\'':
			_, n, err := lexQuoted(input, i)
			if err != nil {
				return Token{}, 0, err
			}
			i += n
		default:
			i++
		}
	}
	if depth > 0 {
		return Token{}, 0, fmt.Errorf("unterminated command substitution: missing ']'")
	}
	inner := input[pos+1 : i-1]
	tok := Token{
		Kind:  TokenCmdSub,
		Text:  input[pos:i],
		Inner: inner,
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

// lexWord consumes a word token: everything until whitespace, a
// separator (newline or semicolon), $, ", ', #, [, ], {, }, (,
// ), or one of the arithmetic operators '+' / '*' / '%'.
// Brackets and braces are terminators because they introduce or
// close command substitution and block syntax respectively.
// '+', '*', and '%' are terminators so "1+1" and "$x*2" split
// without requiring whitespace around the operator.  '-' and
// '/' stay as word-interior characters: '-' is part of negative
// literals ("-3"), flags ("-x", "--long"); '/' is part of file
// paths ("/sys/fs/bpf").  Expressions using '-' or '/' as
// binary operators therefore still need whitespace around them.
func lexWord(input string, pos int) (Token, int) {
	i := pos
	for i < len(input) {
		ch := input[i]
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == ';' ||
			ch == '$' || ch == '"' || ch == '\'' || ch == '#' ||
			ch == '[' || ch == ']' || ch == '{' || ch == '}' ||
			ch == '(' || ch == ')' ||
			ch == '+' || ch == '*' || ch == '%' {
			break
		}
		i++
	}
	tok := Token{Kind: TokenWord, Text: input[pos:i]}
	return tok, i - pos
}

// lexAdapterRef tries to lex an adapter reference at input[pos].
// Known adapter prefixes (e.g. "file") immediately followed by :$
// trigger recognition. The variable reference after : is lexed by
// lexVarRef. Returns (token, consumed, true) on success, or
// (Token{}, 0, false) if this position is not an adapter reference.
func lexAdapterRef(input string, pos int) (Token, int, bool) {
	for _, prefix := range adapterPrefixes {
		full := prefix + ":"
		if !strings.HasPrefix(input[pos:], full) {
			continue
		}
		afterColon := pos + len(full)
		if afterColon >= len(input) || input[afterColon] != '$' {
			continue
		}
		tok, n, err := lexVarRef(input, afterColon)
		if err != nil {
			// Let normal flow handle the error via lexWord + lexVarRef.
			return Token{}, 0, false
		}
		adapterTok := Token{
			Kind:    TokenAdapterRef,
			Text:    input[pos : afterColon+n],
			Adapter: prefix,
			VarName: tok.VarName,
			VarPath: tok.VarPath,
		}
		return adapterTok, afterColon + n - pos, true
	}
	return Token{}, 0, false
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
