package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProgram_IfBasic(t *testing.T) {
	tokens, err := Tokenise("if $count > 0 { bpfman program list }")
	require.NoError(t, err)

	stmts, err := ParseProgram(tokens)
	require.NoError(t, err)
	require.Len(t, stmts, 1)

	ifStmt, ok := stmts[0].(*IfStmt)
	require.True(t, ok)
	assert.Empty(t, ifStmt.Elifs)
	assert.Empty(t, ifStmt.Else)
	assert.Len(t, ifStmt.Then, 1)

	_, ok = ifStmt.Then[0].(*CommandStmt)
	assert.True(t, ok)
}

func TestParseProgram_IfElseMultiLine(t *testing.T) {
	input := "if $count > 0 {\n  let a = yes\n} else {\n  let a = no\n}"
	tokens, err := Tokenise(input)
	require.NoError(t, err)

	stmts, err := ParseProgram(tokens)
	require.NoError(t, err)
	require.Len(t, stmts, 1)

	ifStmt, ok := stmts[0].(*IfStmt)
	require.True(t, ok)
	assert.Empty(t, ifStmt.Elifs)
	require.Len(t, ifStmt.Then, 1)
	require.Len(t, ifStmt.Else, 1)
	_, ok = ifStmt.Then[0].(*LetStmt)
	assert.True(t, ok)
	_, ok = ifStmt.Else[0].(*LetStmt)
	assert.True(t, ok)
}

func TestParseProgram_IfElifChain(t *testing.T) {
	input := "if $x == 1 {\n let a = one\n} elif $x == 2 {\n let a = two\n} elif $x == 3 {\n let a = three\n} else {\n let a = other\n}"
	tokens, err := Tokenise(input)
	require.NoError(t, err)
	stmts, err := ParseProgram(tokens)
	require.NoError(t, err)
	require.Len(t, stmts, 1)

	ifStmt, ok := stmts[0].(*IfStmt)
	require.True(t, ok)
	assert.Len(t, ifStmt.Elifs, 2)
	assert.Len(t, ifStmt.Else, 1)
}

func TestParseProgram_IfNested(t *testing.T) {
	input := "if $a == 1 {\n if $b == 2 {\n let c = yes\n }\n}"
	tokens, err := Tokenise(input)
	require.NoError(t, err)
	stmts, err := ParseProgram(tokens)
	require.NoError(t, err)
	require.Len(t, stmts, 1)

	outer, ok := stmts[0].(*IfStmt)
	require.True(t, ok)
	require.Len(t, outer.Then, 1)
	_, ok = outer.Then[0].(*IfStmt)
	assert.True(t, ok)
}

func TestParseProgram_IfMissingBrace(t *testing.T) {
	tokens, err := Tokenise("if $count > 0 bpfman program list")
	require.NoError(t, err)
	_, err = ParseProgram(tokens)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected '{'")
}

func TestParseProgram_IfUnterminatedBlock(t *testing.T) {
	tokens, err := Tokenise("if $x eq 1 {\n let a = yes")
	require.NoError(t, err)
	_, err = ParseProgram(tokens)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unterminated block")
}

func TestParseProgram_SemicolonSeparator(t *testing.T) {
	tokens, err := Tokenise("let a = 1 ; let b = 2")
	require.NoError(t, err)
	stmts, err := ParseProgram(tokens)
	require.NoError(t, err)
	require.Len(t, stmts, 2)
	_, ok := stmts[0].(*LetStmt)
	assert.True(t, ok)
	_, ok = stmts[1].(*LetStmt)
	assert.True(t, ok)
}

func TestParseProgram_NewlineSeparator(t *testing.T) {
	tokens, err := Tokenise("let a = 1\nlet b = 2\n")
	require.NoError(t, err)
	stmts, err := ParseProgram(tokens)
	require.NoError(t, err)
	require.Len(t, stmts, 2)
}
