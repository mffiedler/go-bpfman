// ContState (the chunk-continuation tracker) and
// CanonicaliseHistory (the multi-line-input history flattener)
// moved out of cmd/bpfman-shell's repl.go to live with the
// rest of the loop mechanism in repl/. Exported because the
// loop driver that will move next still reaches them from
// main during the transition.

package repl

import (
	"strings"
)

// ContState tracks brace, parenthesis, and bracket depth across
// accumulated input lines so the loop knows when a multi-line
// if-block, parenthesised expression, or list literal is
// complete. Quote state persists across lines so multi-line
// quoted strings are treated as a single literal span;
// unterminated strings themselves are surfaced by the tokeniser
// when the accumulated chunk is eventually parsed. LineCont
// records whether the line just consumed ended with an unescaped
// backslash outside quotes (line continuation).
type ContState struct {
	Braces, Parens, Brackets int
	InSingle, InDouble       bool
	LineCont                 bool
}

// Advance walks one line of input, updating the brace, paren,
// and bracket counters. Comments (`#` to end of line) outside a
// quoted string are ignored; quoted content is skipped so braces,
// parens, and brackets inside strings do not count. The
// in-string flags are fields on the struct so they survive
// across line boundaries, matching how the tokeniser actually
// treats multi-line quoted literals.
//
// '[' and ']' are tracked here because the parser's list-literal
// rule allows newlines between elements, so a script that opens
// a list on one line and closes it on the next must keep the
// chunk loop in continuation mode. Without bracket tracking the
// chunk dispatched after the first line, the parser saw 'let xs
// = [1' alone, and erred with "missing ']' to close list
// literal" -- masking the parser's multi-line support behind a
// REPL-level chunking limitation.
func (c *ContState) Advance(line string) {
	c.LineCont = false
	lastNonSpace := -1
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\'' && !c.InDouble:
			c.InSingle = !c.InSingle
		case ch == '"' && !c.InSingle:
			c.InDouble = !c.InDouble
		case c.InSingle || c.InDouble:
			// ignore content inside strings
		case ch == '#':
			return
		case ch == '{':
			c.Braces++
		case ch == '}':
			if c.Braces > 0 {
				c.Braces--
			}
		case ch == '(':
			c.Parens++
		case ch == ')':
			if c.Parens > 0 {
				c.Parens--
			}
		case ch == '[':
			c.Brackets++
		case ch == ']':
			if c.Brackets > 0 {
				c.Brackets--
			}
		}
		if !c.InSingle && !c.InDouble && ch != ' ' && ch != '\t' && ch != '\r' {
			lastNonSpace = i
		}
	}
	if lastNonSpace >= 0 && line[lastNonSpace] == '\\' {
		c.LineCont = true
	}
}

// Open reports whether the accumulated input is still inside an
// open brace, parenthesised group, or list literal, or the line
// just consumed ended with a backslash continuation.
func (c *ContState) Open() bool {
	return c.Braces > 0 || c.Parens > 0 || c.Brackets > 0 || c.LineCont
}

// CanonicaliseHistory rewrites multi-line input into one history
// line: backslash-line continuations and bare newlines outside
// quoted strings become a single space, leading whitespace on
// continuation lines is dropped, and `#` comments outside quoted
// strings are stripped to the end of their line. Newlines inside
// quoted strings are preserved verbatim.
func CanonicaliseHistory(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var inSingle, inDouble bool
	emitSpace := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inSingle && !inDouble && ch == '#' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i >= len(s) {
				break
			}
			ch = s[i]
		}
		if !inSingle && !inDouble && ch == '\\' && i+1 < len(s) && s[i+1] == '\n' {
			i++
			emitSpace = true
			continue
		}
		if !inSingle && !inDouble && ch == '\n' {
			emitSpace = true
			continue
		}
		if emitSpace {
			if ch == ' ' || ch == '\t' || ch == '\r' {
				continue
			}
			out := b.String()
			out = strings.TrimRight(out, " \t")
			b.Reset()
			b.WriteString(out)
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			emitSpace = false
		}
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}
		b.WriteByte(ch)
	}
	return strings.TrimSpace(b.String())
}
