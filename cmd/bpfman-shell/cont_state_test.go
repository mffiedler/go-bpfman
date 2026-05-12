package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// feedLines drives a fresh contState across the given lines and
// returns the final open() verdict.  The REPL feeds one line at a
// time from its line reader, so the per-line call is the right
// granularity to test.
func feedLines(lines ...string) bool {
	var cs contState
	for _, line := range lines {
		cs.advance(line)
	}
	return cs.open()
}

func TestContState_SingleLine_Balanced(t *testing.T) {
	t.Parallel()

	assert.False(t, feedLines("help"), "plain command is closed")
	assert.False(t, feedLines("let x = 1"), "simple let is closed")
	assert.False(t, feedLines("if $x > 0 { bpfman program list }"), "one-line if is closed")
}

func TestContState_LineContinuation(t *testing.T) {
	t.Parallel()

	assert.True(t, feedLines("bpfman program list \\"), "trailing backslash keeps buffer open")
	assert.True(t, feedLines("foo\\"), "trailing backslash with no preceding space also opens")
	assert.False(t,
		feedLines("bpfman load file \\", "  --path foo.o"),
		"continuation closes once the next line does not end with backslash",
	)
	assert.True(t,
		feedLines("bpfman load file \\", "  --path foo.o \\"),
		"chain of continuations stays open",
	)
	assert.False(t,
		feedLines("echo 'literal \\'"),
		"backslash inside a single-quoted literal is not a continuation",
	)
	assert.False(t,
		feedLines("echo \"literal \\\""),
		"backslash inside a double-quoted literal is not a continuation",
	)
}

func TestContState_SingleLine_OpenBrace(t *testing.T) {
	t.Parallel()

	assert.True(t, feedLines("if $x > 0 {"), "unterminated if is open")
}

func TestContState_MultiLine_IfBlockBalanced(t *testing.T) {
	t.Parallel()

	closed := !feedLines(
		"if $x > 0 {",
		"  bpfman program list",
		"}",
	)
	assert.True(t, closed, "multi-line if/block should close after matching '}'")
}

func TestContState_Comment_EndsLine(t *testing.T) {
	t.Parallel()

	assert.False(t, feedLines("help # this comment has a { brace"), "comment body ignored")
}

func TestContState_QuotedBrace_SingleLine(t *testing.T) {
	t.Parallel()

	// Braces inside quoted strings must not count toward depth.
	assert.False(t, feedLines(`exec echo "{ not a block }"`), "quoted braces are text")
	assert.False(t, feedLines(`exec echo '{ not a block }'`), "single-quoted braces are text")
}

// ---- Bug reproductions --------------------------------------------

// When a double-quoted string spans multiple lines, the closing `"`
// appears on a later line than the opening.  Current contState
// resets inDouble at each call, so a `{` or `[` inside the string
// on a middle line is counted as a real depth change.  We detect
// the bug by putting an UNBALANCED `{` inside the quoted content —
// in a correct tracker it's string content; in the buggy one it's
// a block opener that nothing closes.
func TestContState_MultiLine_QuotedString_UnbalancedBraceIsText(t *testing.T) {
	t.Parallel()

	closed := !feedLines(
		`exec bash -c "`,
		`  kill -USR1 $$; echo { only-open`,
		`"`,
	)
	assert.True(t, closed, "unbalanced '{' inside a multi-line double-quoted string must not be counted as an open block")
}

func TestContState_MultiLine_QuotedString_ThenRealBlock(t *testing.T) {
	t.Parallel()

	// Unbalanced quoted `{` on line 2 is text and must not leak
	// into the depth count.  A real `{` on line 4 opens a block
	// that line 5 closes.  If the quoted `{` leaked, final depth
	// would be 1 (open) rather than 0.
	closed := !feedLines(
		`exec bash -c "`,
		`  echo { only-open`,
		`"`,
		`if $x > 0 {`,
		`  help`,
		`}`,
	)
	assert.True(t, closed, "quoted unbalanced brace must not confuse a real trailing block")
}

func TestContState_MultiLine_QuotedString_StillOpenAtEOF(t *testing.T) {
	t.Parallel()

	// A genuinely unterminated multi-line double-quoted string:
	// open() should report open because depth (bracket/brace) is
	// zero but the string never closed.  Current contState tracks
	// only brace/bracket depth, not quote depth, so open() returns
	// false here.  We leave this as documentation: contState is
	// specifically a brace/bracket balancer, not a full lexer.
	// The tokeniser catches unterminated strings when the
	// accumulated chunk is eventually parsed.
	stillOpen := feedLines(
		`exec bash -c "`,
		`  echo hi`,
	)
	assert.False(t, stillOpen, "contState reports only brace/bracket depth; unterminated strings surface at tokenise time")
}
