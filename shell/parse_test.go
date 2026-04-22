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

func TestParse_Thread_Basic(t *testing.T) {
	prog, err := parseSource(t, "let r = $x |> jq \"add\"")
	require.NoError(t, err)
	let, ok := firstStmt(t, prog).(*LetStmt)
	require.True(t, ok)
	thread, ok := let.RHS.(*ThreadExpr)
	require.True(t, ok, "RHS should be a ThreadExpr, got %T", let.RHS)
	// LHS is the value expression producing $x.
	lhs, ok := thread.LHS.(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "x", lhs.Name)
	// Args is the thread's right-hand command: jq "add" (2 args).
	require.Len(t, thread.Args, 2)
	jqLit, ok := thread.Args[0].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "jq", jqLit.Text)
	filterLit, ok := thread.Args[1].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "add", filterLit.Text)
	assert.True(t, filterLit.Quoted)
}

func TestParse_Thread_Chain_LeftAssociative(t *testing.T) {
	// a |> b |> c should parse as (a |> b) |> c.
	prog, err := parseSource(t, "let r = $x |> jq \".a\" |> jq \"add\"")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	outer, ok := let.RHS.(*ThreadExpr)
	require.True(t, ok)
	// Outer's LHS is itself a ThreadExpr (the inner one).
	inner, ok := outer.LHS.(*ThreadExpr)
	require.True(t, ok, "outer.LHS should be a ThreadExpr (left-assoc), got %T", outer.LHS)
	innerLHS, ok := inner.LHS.(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "x", innerLHS.Name)
}

func TestParse_Thread_TighterThanComparison(t *testing.T) {
	// $x |> jq "..." > 0 should parse as ($x |> jq "...") > 0.
	prog, err := parseSource(t, "let r = $x |> jq \".n\" > 0")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	bin, ok := let.RHS.(*BinaryExpr)
	require.True(t, ok, "RHS should be BinaryExpr, got %T", let.RHS)
	assert.Equal(t, ">", bin.Op)
	// LHS of comparison is the thread chain.
	_, ok = bin.Left.(*ThreadExpr)
	assert.True(t, ok, "binary LHS should be ThreadExpr, got %T", bin.Left)
	// RHS is the literal 0.
	_, ok = bin.Right.(*LiteralExpr)
	assert.True(t, ok)
}

func TestParse_Thread_CmdSubLHS(t *testing.T) {
	// [cmd] |> jq "FILTER" is valid: LHS is a CmdSubExpr, RHS is a thread.
	prog, err := parseSource(t, "let r = [bpfman program list] |> jq \"length\"")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	thread, ok := let.RHS.(*ThreadExpr)
	require.True(t, ok)
	_, ok = thread.LHS.(*CmdSubExpr)
	assert.True(t, ok, "thread LHS should be CmdSubExpr, got %T", thread.LHS)
}

func TestParse_Thread_LocPointsAtThreadToken(t *testing.T) {
	// The Loc on a ThreadExpr identifies the `|>` itself so errors
	// about the threading step can point at the operator rather
	// than at the LHS or RHS.
	prog, err := parseSource(t, "let r = $x |> jq \"add\"")
	require.NoError(t, err)
	let := firstStmt(t, prog).(*LetStmt)
	thread := let.RHS.(*ThreadExpr)
	assert.Equal(t, 1, thread.Loc.Line)
	// Column of the `|>` in "let r = $x |> jq \"add\"":
	//   columns 1..9 = "let r = $"
	//   column 10 = 'x' (end of varref) — the `|>` is
	//   after `$x ` so at column 12.
	assert.Equal(t, 12, thread.Loc.Col)
}

func TestParse_Thread_RejectsTrailingThread(t *testing.T) {
	_, err := parseSource(t, "let r = $x |>")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thread")
}

func TestParse_Thread_RejectsLeadingThread(t *testing.T) {
	_, err := parseSource(t, "let r = |> jq \"add\"")
	require.Error(t, err)
}

// --- foreach ------------------------------------------------------

func TestParse_ForEach_Basic(t *testing.T) {
	prog, err := parseSource(t, "foreach p in $list { dump p }")
	require.NoError(t, err)
	fe, ok := firstStmt(t, prog).(*ForEachStmt)
	require.True(t, ok, "expected ForEachStmt, got %T", prog.Stmts[0])
	assert.Equal(t, "p", fe.Name)
	ref, ok := fe.List.(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "list", ref.Name)
	require.Len(t, fe.Body, 1)
	_, ok = fe.Body[0].(*CommandStmt)
	assert.True(t, ok)
}

func TestParse_ForEach_ListFromPipe(t *testing.T) {
	// Ensure the list expression can be an arbitrary expression,
	// including a threading pipeline like [bpfman ... ] |> jq "..."
	prog, err := parseSource(t, "foreach p in $raw |> jq \".items\" { dump p }")
	require.NoError(t, err)
	fe, ok := firstStmt(t, prog).(*ForEachStmt)
	require.True(t, ok)
	_, ok = fe.List.(*ThreadExpr)
	assert.True(t, ok, "list expression should be a ThreadExpr, got %T", fe.List)
}

func TestParse_ForEach_MultiStatementBody(t *testing.T) {
	input := "foreach p in $items {\n  let x = $p.name\n  dump x\n}"
	prog, err := parseSource(t, input)
	require.NoError(t, err)
	fe, ok := firstStmt(t, prog).(*ForEachStmt)
	require.True(t, ok)
	require.Len(t, fe.Body, 2)
	_, ok = fe.Body[0].(*LetStmt)
	assert.True(t, ok)
	_, ok = fe.Body[1].(*CommandStmt)
	assert.True(t, ok)
}

func TestParse_ForEach_Nested(t *testing.T) {
	input := "foreach a in $xs {\n  foreach b in $ys {\n    dump b\n  }\n}"
	prog, err := parseSource(t, input)
	require.NoError(t, err)
	outer, ok := firstStmt(t, prog).(*ForEachStmt)
	require.True(t, ok)
	require.Len(t, outer.Body, 1)
	inner, ok := outer.Body[0].(*ForEachStmt)
	require.True(t, ok)
	assert.Equal(t, "b", inner.Name)
}

func TestParse_ForEach_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"missing identifier", "foreach in $list { dump x }", "foreach requires"},
		{"invalid identifier", "foreach 1bad in $list { dump x }", "invalid variable name"},
		{"missing in", "foreach p $list { dump x }", "foreach requires 'in'"},
		{"missing expression", "foreach p in { dump x }", "foreach requires"},
		{"missing block", "foreach p in $list dump x", "expected '{'"},
		{"unterminated block", "foreach p in $list { dump x", "unterminated block"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSource(t, tc.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParse_Break_Simple(t *testing.T) {
	prog, err := parseSource(t, "foreach x in $xs { break }")
	require.NoError(t, err)
	fe := firstStmt(t, prog).(*ForEachStmt)
	require.Len(t, fe.Body, 1)
	_, ok := fe.Body[0].(*BreakStmt)
	assert.True(t, ok, "expected BreakStmt, got %T", fe.Body[0])
}

func TestParse_Continue_Simple(t *testing.T) {
	prog, err := parseSource(t, "foreach x in $xs { continue }")
	require.NoError(t, err)
	fe := firstStmt(t, prog).(*ForEachStmt)
	require.Len(t, fe.Body, 1)
	_, ok := fe.Body[0].(*ContinueStmt)
	assert.True(t, ok, "expected ContinueStmt, got %T", fe.Body[0])
}

func TestParse_Break_InsideIf(t *testing.T) {
	prog, err := parseSource(t, "foreach x in $xs {\n  if $x eq skip { break }\n  dump x\n}")
	require.NoError(t, err)
	fe := firstStmt(t, prog).(*ForEachStmt)
	require.Len(t, fe.Body, 2)
	ifStmt, ok := fe.Body[0].(*IfStmt)
	require.True(t, ok)
	require.Len(t, ifStmt.Then, 1)
	_, ok = ifStmt.Then[0].(*BreakStmt)
	assert.True(t, ok)
}

func TestParse_Break_RejectsArguments(t *testing.T) {
	// break and continue take no arguments — a trailing token
	// is a parse-time error so "break 2"-style multi-level
	// escapes don't silently tokenise as a command.
	_, err := parseSource(t, "foreach x in $xs { break 2 }")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "break")
}

func TestParse_Continue_RejectsArguments(t *testing.T) {
	_, err := parseSource(t, "foreach x in $xs { continue now }")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "continue")
}

func TestParse_CmdSubInnerSyntaxErrorAtParseTime(t *testing.T) {
	// A syntax error inside [ ... ] surfaces at the outer Parse
	// call: eager inner parsing is a deliberate behavioural change
	// documented in the refactor plan.
	_, err := parseSource(t, "let x = [let y = ]")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command substitution")
}
