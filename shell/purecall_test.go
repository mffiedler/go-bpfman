package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerTestPureBuiltin installs a uniquely-named pure builtin
// (suffixed with the test name) in the registry for the duration
// of the test, and returns the registered name. The unique
// suffixing keeps t.Parallel() tests from racing on overlapping
// names. Shape is OriginScalar by default; tests that need a
// different shape register directly via RegisterPureBuiltin.
func registerTestPureBuiltin(t *testing.T, prefix string, arity int) string {
	t.Helper()
	name := prefix + "_" + strings.ReplaceAll(t.Name(), "/", "_")
	RegisterPureBuiltin(name, arity, KindShape(OriginScalar))
	t.Cleanup(func() { delete(pureBuiltinRegistry, name) })
	return name
}

func TestParse_PureCall_BindsAsExprAtLet(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)

	prog, err := parseSource(t, "let x = "+name+" 42")
	require.NoError(t, err)
	let, ok := firstStmt(t, prog).(*LetStmt)
	require.True(t, ok)
	call, ok := let.RHS.(*PureCallExpr)
	require.True(t, ok, "expected PureCallExpr, got %T", let.RHS)
	assert.Equal(t, name, call.Name)
	require.Len(t, call.Args, 1)
	lit, ok := call.Args[0].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "42", lit.Text)
}

func TestParse_PureCall_ArityZeroConsumesNoArgs(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "now", 0)
	prog, err := parseSource(t, "let t = "+name)
	require.NoError(t, err)
	let, _ := firstStmt(t, prog).(*LetStmt)
	call, ok := let.RHS.(*PureCallExpr)
	require.True(t, ok)
	assert.Equal(t, name, call.Name)
	assert.Empty(t, call.Args)
}

func TestParse_PureCall_TrailingArithmeticBindsOutside(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	prog, err := parseSource(t, "let x = "+name+" 5 + 1")
	require.NoError(t, err)
	let, _ := firstStmt(t, prog).(*LetStmt)
	bin, ok := let.RHS.(*BinaryExpr)
	require.True(t, ok, "trailing + should bind outside the call, got %T", let.RHS)
	assert.Equal(t, "+", bin.Op)
	_, ok = bin.Left.(*PureCallExpr)
	require.True(t, ok, "left of + should be the pure call")
}

func TestParse_PureCall_ParenthesisedArgConsumesFullExpression(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	prog, err := parseSource(t, "let x = "+name+" (5 + 1)")
	require.NoError(t, err)
	let, _ := firstStmt(t, prog).(*LetStmt)
	call, ok := let.RHS.(*PureCallExpr)
	require.True(t, ok)
	require.Len(t, call.Args, 1)
	bin, ok := call.Args[0].(*BinaryExpr)
	require.True(t, ok, "parenthesised arg should be the binary expression")
	assert.Equal(t, "+", bin.Op)
}

func TestParse_PureCall_MissingArgsIsParseError(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	_, err := parseSource(t, "let x = "+name)
	require.Error(t, err)
	assert.Contains(t, err.Error(), name)
	assert.Contains(t, err.Error(), "expected 1")
}

func TestParse_PureCall_UnregisteredNameStaysLiteral(t *testing.T) {
	t.Parallel()

	// 'definitely-not-registered' is not in the registry, so it
	// falls through to parsePrimary and lands as a literal.
	prog, err := parseSource(t, "let x = definitely-not-registered")
	require.NoError(t, err)
	let, _ := firstStmt(t, prog).(*LetStmt)
	lit, ok := let.RHS.(*LiteralExpr)
	require.True(t, ok, "unregistered name should fall through to literal, got %T", let.RHS)
	assert.Equal(t, "definitely-not-registered", lit.Text)
}

func TestParse_PureCall_VarRefArg(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	prog, err := parseSource(t, "let x = "+name+" $n")
	require.NoError(t, err)
	let, _ := firstStmt(t, prog).(*LetStmt)
	call, _ := let.RHS.(*PureCallExpr)
	require.NotNil(t, call)
	require.Len(t, call.Args, 1)
	vr, ok := call.Args[0].(*VarRefExpr)
	require.True(t, ok)
	assert.Equal(t, "n", vr.Name)
}

func TestEvalExpr_PureCall_DispatchesThroughExecBind(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	var captured []Arg
	env := &Env{
		Session: NewSession(),
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			captured = args
			return StringValue("primary-result"), nil
		}),
	}
	call := &PureCallExpr{
		Name: name,
		Args: []Expr{&LiteralExpr{Text: "42"}},
	}
	v, err := EvalExpr(call, env)
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "primary-result", s)
	require.Len(t, captured, 2, "name prepended as first arg")
	assert.Equal(t, name, captured[0].(WordArg).Text)
	scalar, ok := captured[1].(ScalarValueArg)
	require.True(t, ok)
	assert.Equal(t, "42", scalar.Text)
}

func TestEvalExpr_PureCall_NoExecBindIsError(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	env := &Env{Session: NewSession()}
	call := &PureCallExpr{Name: name, Args: []Expr{&LiteralExpr{Text: "1"}}}
	_, err := EvalExpr(call, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), name)
}

func TestEvalExpr_PureCall_FailingHandlerIsExpressionError(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	env := &Env{
		Session: NewSession(),
		ExecBind: func(_ []Arg, _ Span) (BindResult, error) {
			return BindResult{Rc: Envelope{OK: false, Code: 7, Stderr: "bad"}}, nil
		},
	}
	call := &PureCallExpr{Name: name, Args: []Expr{&LiteralExpr{Text: "1"}}}
	_, err := EvalExpr(call, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
}

func TestEvalExpr_PureCall_InsideInterpString(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "hex", 1)
	src := `let s = "value=${` + name + ` 255}"`
	tokens, err := Tokenise(src)
	require.NoError(t, err)
	prog, err := Parse(tokens)
	require.NoError(t, err)
	let := prog.Stmts[0].(*LetStmt)
	is, ok := let.RHS.(*InterpStringExpr)
	require.True(t, ok)
	require.Len(t, is.Segments, 2)
	require.NotNil(t, is.Segments[1].Expr)
	_, ok = is.Segments[1].Expr.(*PureCallExpr)
	require.True(t, ok, "${hex 255} inside an interp string should parse as a PureCallExpr")

	env := &Env{
		Session: NewSession(),
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			require.Len(t, args, 2)
			return StringValue("ff"), nil
		}),
	}
	v, err := EvalExpr(let.RHS, env)
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "value=ff", s)
}

func TestCheck_PureCall_ReturnShapeFlowsThroughLet(t *testing.T) {
	t.Parallel()

	// u32le is registered with OriginScalar by shell init via
	// the cmd-side kindshapes, but in shell-only tests we
	// register our own to avoid coupling.
	const name = "__check_ud_scalar__"
	RegisterPureBuiltin(name, 1, KindShape(OriginScalar))
	t.Cleanup(func() { delete(pureBuiltinRegistry, name) })

	src := "let x = " + name + " 42\nprint $x.field"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "x has kind scalar")
}

func TestCheck_PureCall_UnknownReturnShapeIsPermissive(t *testing.T) {
	t.Parallel()

	const name = "__check_anything__"
	RegisterPureBuiltin(name, 1, Shape{Sealed: false, Kind: OriginUnknown})
	t.Cleanup(func() { delete(pureBuiltinRegistry, name) })

	src := "let x = " + name + " 1\nprint $x.deep.path[0]"
	issues := checkSource(t, src)
	assert.Empty(t, issues, "OriginUnknown return propagates as a wildcard")
}

func TestCheck_PureCall_ArgsAreCheckedForUndefinedVars(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	src := "let x = " + name + " $undef"
	issues := checkSource(t, src)
	require.NotEmpty(t, issues)
	combined := joinIssues(issues)
	assert.Contains(t, combined, "undefined variable")
	assert.Contains(t, combined, "undef")
}

func TestCheck_PureBuiltin_RejectedInBindForm(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	src := "let x <- " + name + " 1"
	issues := checkSource(t, src)
	require.NotEmpty(t, issues)
	combined := joinIssues(issues)
	assert.Contains(t, combined, name)
	assert.Contains(t, combined, "pure builtin")
	assert.Contains(t, combined, "let x = "+name)
}

func TestCheck_PureBuiltin_RejectedInGuardForm(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	src := "guard x <- " + name + " 1"
	issues := checkSource(t, src)
	require.NotEmpty(t, issues)
	combined := joinIssues(issues)
	assert.Contains(t, combined, "pure builtin")
}

func TestCheck_PureBuiltin_RejectedInTupleBind(t *testing.T) {
	t.Parallel()

	name := registerTestPureBuiltin(t, "ud", 1)
	src := "let (rc, x) <- " + name + " 1"
	issues := checkSource(t, src)
	require.NotEmpty(t, issues)
	combined := joinIssues(issues)
	assert.Contains(t, combined, "pure builtin")
}

// joinIssues collapses a slice of issues into one string so a
// single assertion can probe for substrings that may appear in
// any entry.
func joinIssues(issues []Issue) string {
	var sb strings.Builder
	for _, i := range issues {
		sb.WriteString(i.Msg)
		sb.WriteString("\n")
	}
	return sb.String()
}
