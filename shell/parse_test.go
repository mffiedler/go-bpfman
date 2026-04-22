package shell

// Note: additional if-statement parse tests live in parse_if_test.go.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseSource is a convenience that drives tokenisation and parsing
// so tests can speak in terms of surface syntax.
func parseSource(t *testing.T, src string) (*Program, error) {
	t.Helper()
	tokens, err := Tokenise(src)
	require.NoError(t, err)
	return Parse(tokens)
}

// firstStmt returns the single statement of a program, failing the
// test if the program contains zero or more than one statement.
func firstStmt(t *testing.T, prog *Program) Stmt {
	t.Helper()
	require.Len(t, prog.Stmts, 1)
	return prog.Stmts[0]
}

func TestParse_SingleWordCommand(t *testing.T) {
	prog, err := parseSource(t, "help")
	require.NoError(t, err)
	cmd, ok := firstStmt(t, prog).(*CommandStmt)
	require.True(t, ok)
	require.Len(t, cmd.Args, 1)
	lit, ok := cmd.Args[0].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "help", lit.Text)
	assert.False(t, lit.Quoted)
}

func TestParse_PlainCommand(t *testing.T) {
	prog, err := parseSource(t, "show program 123")
	require.NoError(t, err)
	cmd, ok := firstStmt(t, prog).(*CommandStmt)
	require.True(t, ok)
	require.Len(t, cmd.Args, 3)
	for i, want := range []string{"show", "program", "123"} {
		lit, ok := cmd.Args[i].(*LiteralExpr)
		require.True(t, ok, "arg %d", i)
		assert.Equal(t, want, lit.Text)
	}
}

func TestParse_LetAssignment_Literal(t *testing.T) {
	prog, err := parseSource(t, "let prog = 42")
	require.NoError(t, err)
	let, ok := firstStmt(t, prog).(*LetStmt)
	require.True(t, ok)
	assert.Equal(t, "prog", let.Name)
	lit, ok := let.RHS.(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "42", lit.Text)
}

func TestParse_LetRejectsMultiTokenCommand(t *testing.T) {
	// "load file" is two tokens, not a primary/unary/binary; the
	// new parser surfaces this at parse time rather than at eval
	// time as the legacy pipeline did.
	_, err := parseSource(t, "let prog = load file")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unary predicate")
}

func TestParse_LetWithVarRef(t *testing.T) {
	prog, err := parseSource(t, "let link = $prog")
	require.NoError(t, err)
	let, ok := firstStmt(t, prog).(*LetStmt)
	require.True(t, ok)
	assert.Equal(t, "link", let.Name)
	ref, ok := let.RHS.(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "prog", ref.Name)
}

func TestParse_LetWithCmdSub(t *testing.T) {
	prog, err := parseSource(t, "let x = [load file --path p]")
	require.NoError(t, err)
	let, ok := firstStmt(t, prog).(*LetStmt)
	require.True(t, ok)
	sub, ok := let.RHS.(*CmdSubExpr)
	require.True(t, ok)
	require.NotNil(t, sub.Inner)
	require.Len(t, sub.Inner.Stmts, 1)
	inner, ok := sub.Inner.Stmts[0].(*CommandStmt)
	require.True(t, ok)
	require.Len(t, inner.Args, 4)
}

func TestParse_LetErrors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"no command after equals", "let x =", "let requires"},
		{"too few tokens", "let x", "let requires"},
		{"missing equals", "let x load file", "missing '='"},
		{"non-identifier LHS", "let $x = foo", "let requires an identifier"},
		{"invalid identifier", "let 0bad = foo", "invalid variable name"},
		{"second assign in RHS", "let x = a = b", "unexpected '='"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSource(t, tc.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParse_BareAssignIsError(t *testing.T) {
	_, err := parseSource(t, "prog = load file")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected '='")
}

func TestParse_AliasKeepsAssignAsLiteral(t *testing.T) {
	// alias uses the = sigil syntactically; the parser must allow
	// it through as a LiteralExpr so the alias command handler can
	// see the classic "alias name = expansion" shape.
	prog, err := parseSource(t, "alias b = bpfman")
	require.NoError(t, err)
	cmd, ok := firstStmt(t, prog).(*CommandStmt)
	require.True(t, ok)
	require.Len(t, cmd.Args, 4)
	lit, ok := cmd.Args[2].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "=", lit.Text)
}

func TestParse_VarRefOnlyCommand(t *testing.T) {
	prog, err := parseSource(t, "$prog.id")
	require.NoError(t, err)
	cmd, ok := firstStmt(t, prog).(*CommandStmt)
	require.True(t, ok)
	require.Len(t, cmd.Args, 1)
	ref, ok := cmd.Args[0].(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "prog", ref.Name)
	assert.Equal(t, "id", ref.Path)
}

func TestParse_EmptyProgram(t *testing.T) {
	prog, err := parseSource(t, "")
	require.NoError(t, err)
	assert.Empty(t, prog.Stmts)
}

func TestParse_LocPropagation(t *testing.T) {
	// Statements and expressions should carry Loc from their first
	// token.  A multi-line program has different lines on each
	// statement.
	prog, err := parseSource(t, "help\nshow program")
	require.NoError(t, err)
	require.Len(t, prog.Stmts, 2)
	first, ok := prog.Stmts[0].(*CommandStmt)
	require.True(t, ok)
	assert.Equal(t, 1, first.Loc.Line)
	second, ok := prog.Stmts[1].(*CommandStmt)
	require.True(t, ok)
	assert.Equal(t, 2, second.Loc.Line)
}

func TestParse_Pipe_Basic(t *testing.T) {
	prog, err := parseSource(t, "let r = $x | jq \"add\"")
	require.NoError(t, err)
	let, ok := firstStmt(t, prog).(*LetStmt)
	require.True(t, ok)
	pipe, ok := let.RHS.(*PipeExpr)
	require.True(t, ok, "RHS should be a PipeExpr, got %T", let.RHS)
	// LHS is the value expression producing $x.
	lhs, ok := pipe.LHS.(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "x", lhs.Name)
	// Cmd is the pipe's right-hand command: jq "add" (2 args).
	require.Len(t, pipe.Args, 2)
	jqLit, ok := pipe.Args[0].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "jq", jqLit.Text)
	filterLit, ok := pipe.Args[1].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "add", filterLit.Text)
	assert.True(t, filterLit.Quoted)
}

func TestParse_Pipe_Chain_LeftAssociative(t *testing.T) {
	// a | b | c should parse as (a | b) | c.
	prog, err := parseSource(t, "let r = $x | jq \".a\" | jq \"add\"")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	outer, ok := let.RHS.(*PipeExpr)
	require.True(t, ok)
	// Outer's LHS is itself a PipeExpr (the inner one).
	inner, ok := outer.LHS.(*PipeExpr)
	require.True(t, ok, "outer.LHS should be a PipeExpr (left-assoc), got %T", outer.LHS)
	innerLHS, ok := inner.LHS.(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "x", innerLHS.Name)
}

func TestParse_Pipe_TighterThanComparison(t *testing.T) {
	// $x | jq "..." > 0 should parse as ($x | jq "...") > 0.
	prog, err := parseSource(t, "let r = $x | jq \".n\" > 0")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	bin, ok := let.RHS.(*BinaryExpr)
	require.True(t, ok, "RHS should be BinaryExpr, got %T", let.RHS)
	assert.Equal(t, ">", bin.Op)
	// LHS of comparison is the pipe chain.
	_, ok = bin.Left.(*PipeExpr)
	assert.True(t, ok, "binary LHS should be PipeExpr, got %T", bin.Left)
	// RHS is the literal 0.
	_, ok = bin.Right.(*LiteralExpr)
	assert.True(t, ok)
}

func TestParse_Pipe_CmdSubLHS(t *testing.T) {
	// [cmd] | jq "FILTER" is valid: LHS is a CmdSubExpr, RHS is a pipe.
	prog, err := parseSource(t, "let r = [bpfman program list] | jq \"length\"")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	pipe, ok := let.RHS.(*PipeExpr)
	require.True(t, ok)
	_, ok = pipe.LHS.(*CmdSubExpr)
	assert.True(t, ok, "pipe LHS should be CmdSubExpr, got %T", pipe.LHS)
}

func TestParse_Pipe_LocPointsAtPipeToken(t *testing.T) {
	// The Loc on a PipeExpr identifies the `|` itself so errors
	// about the pipe can point at the operator rather than at the
	// LHS or RHS.
	prog, err := parseSource(t, "let r = $x | jq \"add\"")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	pipe := let.RHS.(*PipeExpr)
	assert.Equal(t, 1, pipe.Loc.Line)
	// Column of the `|` in "let r = $x | jq \"add\"":
	//   columns 1..9 = "let r = $"
	//   column 9 = 'x' end of varref at col 10... the `|` is
	//   after `$x ` so at column 12.
	assert.Equal(t, 12, pipe.Loc.Col)
}

func TestParse_Pipe_RejectsTrailingPipe(t *testing.T) {
	_, err := parseSource(t, "let r = $x |")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipe")
}

func TestParse_Pipe_RejectsLeadingPipe(t *testing.T) {
	_, err := parseSource(t, "let r = | jq \"add\"")
	require.Error(t, err)
}

func TestParse_CmdSubInnerSyntaxErrorAtParseTime(t *testing.T) {
	// A syntax error inside [ ... ] surfaces at the outer Parse
	// call: eager inner parsing is a deliberate behavioural change
	// documented in the refactor plan.
	_, err := parseSource(t, "let x = [let y = ]")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command substitution")
}
