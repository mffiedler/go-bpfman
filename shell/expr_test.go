package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evalEnv returns an Env with the given session and no command
// runners.  Suitable for expression tests that stay inside the
// pure-evaluation layer.
func evalEnv(s *Session) *Env {
	return &Env{Session: s}
}

func TestEvalExpr_Literal(t *testing.T) {
	s := NewSession()
	v, err := EvalExpr(&LiteralExpr{Text: "hello"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
}

func TestEvalExpr_VarRef_Bare(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("bound"))
	v, err := EvalExpr(&VarRefExpr{Name: "x"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "bound", got)
}

func TestEvalExpr_VarRef_Path(t *testing.T) {
	s := NewSession()
	s.Set("prog", ValueFromMap(map[string]any{
		"record": map[string]any{"program_id": "42"},
	}))
	v, err := EvalExpr(&VarRefExpr{Name: "prog", Path: "record.program_id"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", got)
}

func TestEvalExpr_VarRef_Undefined(t *testing.T) {
	s := NewSession()
	_, err := EvalExpr(&VarRefExpr{Name: "missing"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `undefined variable "missing"`)
}

func TestEvalExpr_Adapter_RejectedAsExpression(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("hi"))
	_, err := EvalExpr(&AdapterExpr{Adapter: "file", Name: "x"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adapter")
}

func TestEvalExpr_CmdSub_NoRunner(t *testing.T) {
	s := NewSession()
	e := &CmdSubExpr{Inner: &Program{Stmts: []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "foo"}}}}}}
	_, err := EvalExpr(e, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not permitted")
}

func TestEvalExpr_CmdSub_DispatchesViaEnv(t *testing.T) {
	s := NewSession()
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			require.Len(t, args, 1)
			w, ok := args[0].(WordArg)
			require.True(t, ok)
			return StringValue("hello-" + w.Text), nil
		},
	}
	e := &CmdSubExpr{Inner: &Program{Stmts: []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "foo"}}}}}}
	v, err := EvalExpr(e, env)
	require.NoError(t, err)
	s2, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello-foo", s2)
}

func TestEvalExpr_Binary_Textual(t *testing.T) {
	s := NewSession()
	cases := []struct {
		op         string
		left       string
		right      string
		wantResult bool
	}{
		{"eq", "foo", "foo", true},
		{"eq", "foo", "bar", false},
		{"ne", "foo", "bar", true},
		{"ne", "foo", "foo", false},
		{"lt", "a", "b", true},
		{"lt", "b", "a", false},
		{"le", "a", "a", true},
		{"gt", "b", "a", true},
		{"ge", "a", "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.op+" "+tc.left+" "+tc.right, func(t *testing.T) {
			e := &BinaryExpr{
				Left:  &LiteralExpr{Text: tc.left},
				Op:    tc.op,
				Right: &LiteralExpr{Text: tc.right},
			}
			v, err := EvalExpr(e, evalEnv(s))
			require.NoError(t, err)
			assert.Equal(t, OriginBool, v.Kind())
			b, err := AsBool(v)
			require.NoError(t, err)
			assert.Equal(t, tc.wantResult, b)
		})
	}
}

func TestEvalExpr_Binary_Numeric(t *testing.T) {
	s := NewSession()
	cases := []struct {
		op         string
		left       string
		right      string
		wantResult bool
	}{
		{"==", "5", "5", true},
		{"==", "5", "6", false},
		{"!=", "5", "6", true},
		{"<", "3", "4", true},
		{"<=", "3", "3", true},
		{">", "5", "4", true},
		{">=", "5", "5", true},
		{"<", "9", "10", true},
		{">", "10", "9", true},
	}
	for _, tc := range cases {
		t.Run(tc.op+" "+tc.left+" "+tc.right, func(t *testing.T) {
			e := &BinaryExpr{
				Left:  &LiteralExpr{Text: tc.left},
				Op:    tc.op,
				Right: &LiteralExpr{Text: tc.right},
			}
			v, err := EvalExpr(e, evalEnv(s))
			require.NoError(t, err)
			b, err := AsBool(v)
			require.NoError(t, err)
			assert.Equal(t, tc.wantResult, b)
		})
	}
}

func TestEvalExpr_Binary_NumericNonNumericError(t *testing.T) {
	s := NewSession()
	e := &BinaryExpr{
		Left:  &LiteralExpr{Text: "abc"},
		Op:    "<",
		Right: &LiteralExpr{Text: "5"},
	}
	_, err := EvalExpr(e, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not numeric")
}

func TestEvalExpr_Unary_NotEmpty(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("hello"))
	s.Set("y", StringValue(""))

	e := &UnaryExpr{Pred: "not-empty", Operand: &VarRefExpr{Name: "x"}}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)

	e = &UnaryExpr{Pred: "not-empty", Operand: &VarRefExpr{Name: "y"}}
	v, err = EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	b, err = AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestEvalExpr_Unary_TrueFalse(t *testing.T) {
	s := NewSession()
	s.Set("flag", StringValue("true"))
	e := &UnaryExpr{Pred: "true", Operand: &VarRefExpr{Name: "flag"}}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)

	e = &UnaryExpr{Pred: "false", Operand: &VarRefExpr{Name: "flag"}}
	v, err = EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	b, err = AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestExprFromArgs_Primary(t *testing.T) {
	t.Run("word literal", func(t *testing.T) {
		e, err := ExprFromArgs([]Arg{WordArg{Text: "foo"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "foo", lit.Text)
		assert.False(t, lit.Quoted)
	})
	t.Run("quoted literal", func(t *testing.T) {
		e, err := ExprFromArgs([]Arg{QuotedArg{Text: "hello world"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "hello world", lit.Text)
		assert.True(t, lit.Quoted)
	})
	t.Run("scalar var reference", func(t *testing.T) {
		e, err := ExprFromArgs([]Arg{ScalarValueArg{Text: "42"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "42", lit.Text)
	})
	t.Run("bare structured reference", func(t *testing.T) {
		e, err := ExprFromArgs([]Arg{StructuredValueArg{Name: "prog", Value: ValueFromMap(map[string]any{"x": 1})}})
		require.NoError(t, err)
		ref, ok := e.(*VarRefExpr)
		require.True(t, ok)
		assert.Equal(t, "prog", ref.Name)
		assert.Empty(t, ref.Path)
	})
}

func TestExprFromArgs_Unary(t *testing.T) {
	e, err := ExprFromArgs([]Arg{
		WordArg{Text: "not-empty"},
		ScalarValueArg{Text: "foo"},
	})
	require.NoError(t, err)
	unary, ok := e.(*UnaryExpr)
	require.True(t, ok)
	assert.Equal(t, "not-empty", unary.Pred)
}

func TestExprFromArgs_UnaryRejectsNonPred(t *testing.T) {
	_, err := ExprFromArgs([]Arg{
		WordArg{Text: "notapred"},
		WordArg{Text: "operand"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unary predicate")
}

func TestExprFromArgs_Binary(t *testing.T) {
	ops := []string{"eq", "ne", "lt", "le", "gt", "ge", "==", "!=", "<", "<=", ">", ">="}
	for _, op := range ops {
		t.Run(op, func(t *testing.T) {
			e, err := ExprFromArgs([]Arg{
				ScalarValueArg{Text: "1"},
				WordArg{Text: op},
				ScalarValueArg{Text: "2"},
			})
			require.NoError(t, err)
			bin, ok := e.(*BinaryExpr)
			require.True(t, ok)
			assert.Equal(t, op, bin.Op)
		})
	}
}

func TestExprFromArgs_BinaryRejectsNonOp(t *testing.T) {
	_, err := ExprFromArgs([]Arg{
		WordArg{Text: "a"},
		WordArg{Text: "bogus"},
		WordArg{Text: "b"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary operator")
}

func TestExprFromArgs_TooManyArgs(t *testing.T) {
	_, err := ExprFromArgs([]Arg{
		WordArg{Text: "a"}, WordArg{Text: "b"}, WordArg{Text: "c"}, WordArg{Text: "d"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4 operands")
}

func TestExprFromArgs_Empty(t *testing.T) {
	_, err := ExprFromArgs(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty expression")
}

func TestAsBool_RejectsNonBool(t *testing.T) {
	cases := []Value{
		StringValue("true"),
		StringValue(""),
		ValueFromMap(map[string]any{"x": 1}),
	}
	for _, v := range cases {
		_, err := AsBool(v)
		require.Error(t, err, "kind=%s", v.Kind())
		assert.Contains(t, err.Error(), "not a boolean")
	}
}

func TestIsBinaryOp(t *testing.T) {
	true_ := []string{"eq", "ne", "lt", "le", "gt", "ge", "==", "!=", "<", "<=", ">", ">="}
	false_ := []string{"", "foo", "=", "<=>"}
	for _, s := range true_ {
		assert.True(t, IsBinaryOp(s), s)
	}
	for _, s := range false_ {
		assert.False(t, IsBinaryOp(s), s)
	}
}

func TestIsUnaryPred(t *testing.T) {
	true_ := []string{"true", "false", "not-empty"}
	false_ := []string{"", "ok", "fail", "eq", "nil"}
	for _, s := range true_ {
		assert.True(t, IsUnaryPred(s), s)
	}
	for _, s := range false_ {
		assert.False(t, IsUnaryPred(s), s)
	}
}

// Coverage for nested CmdSub dispatch via EvalArgs (replaces the
// old resolveCmdSubs unit tests).

func TestEvalArgs_CmdSub_FlattensScalar(t *testing.T) {
	s := NewSession()
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			require.Len(t, args, 1)
			w, ok := args[0].(WordArg)
			require.True(t, ok)
			assert.Equal(t, "inner", w.Text)
			return StringValue("hello"), nil
		},
	}
	exprs := []Expr{
		&LiteralExpr{Text: "outer"},
		&CmdSubExpr{Inner: &Program{Stmts: []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "inner"}}}}}},
	}
	out, err := EvalArgs(exprs, env)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, WordArg{Text: "outer"}, out[0])
	scalar, ok := out[1].(ScalarValueArg)
	require.True(t, ok)
	assert.Equal(t, "hello", scalar.Text)
}

func TestEvalArgs_CmdSub_PreservesStructured(t *testing.T) {
	s := NewSession()
	structured := ValueFromMap(map[string]any{"id": "42"}).WithKind(OriginProgram)
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			return structured, nil
		},
	}
	exprs := []Expr{
		&LiteralExpr{Text: "outer"},
		&CmdSubExpr{Inner: &Program{Stmts: []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "inner"}}}}}},
	}
	out, err := EvalArgs(exprs, env)
	require.NoError(t, err)
	require.Len(t, out, 2)
	sva, ok := out[1].(StructuredValueArg)
	require.True(t, ok)
	assert.Equal(t, "", sva.Name)
	assert.Equal(t, OriginProgram, sva.Value.Kind())
}

// PipeExpr evaluation: LHS's Value is appended as the last arg to
// the pipe's command, which then dispatches via ExecSubstitution.

func TestEvalExpr_Pipe_AppendsScalarValueAsLastArg(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("42"))
	var captured []Arg
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			captured = args
			return StringValue("ok"), nil
		},
	}
	pipe := &PipeExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "jq"}, &LiteralExpr{Text: ".", Quoted: true}},
	}
	v, err := EvalExpr(pipe, env)
	require.NoError(t, err)
	s2, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "ok", s2)
	// Captured args: [jq, ".", "42"] — LHS value becomes last arg.
	require.Len(t, captured, 3)
	assert.Equal(t, WordArg{Text: "jq"}, captured[0])
	assert.Equal(t, QuotedArg{Text: "."}, captured[1])
	scalar, ok := captured[2].(ScalarValueArg)
	require.True(t, ok)
	assert.Equal(t, "42", scalar.Text)
}

func TestEvalExpr_Pipe_AppendsStructuredValueAsLastArg(t *testing.T) {
	s := NewSession()
	s.Set("p", ValueFromMap(map[string]any{"id": "42"}).WithKind(OriginProgram))
	var captured []Arg
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			captured = args
			return StringValue("ok"), nil
		},
	}
	pipe := &PipeExpr{
		LHS:  &VarRefExpr{Name: "p"},
		Args: []Expr{&LiteralExpr{Text: "jq"}, &LiteralExpr{Text: ".id", Quoted: true}},
	}
	_, err := EvalExpr(pipe, env)
	require.NoError(t, err)
	require.Len(t, captured, 3)
	sva, ok := captured[2].(StructuredValueArg)
	require.True(t, ok, "structured LHS should produce StructuredValueArg, got %T", captured[2])
	assert.Equal(t, OriginProgram, sva.Value.Kind())
}

func TestEvalExpr_Pipe_NilLHSIsError(t *testing.T) {
	s := NewSession()
	s.Set("x", Value{}) // nil value
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			return StringValue("should-not-run"), nil
		},
	}
	pipe := &PipeExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "jq"}},
	}
	_, err := EvalExpr(pipe, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null")
}

func TestEvalExpr_Pipe_NoSubstitutionRunnerIsError(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("42"))
	env := &Env{Session: s}
	pipe := &PipeExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "jq"}},
	}
	_, err := EvalExpr(pipe, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipe")
}

func TestEvalExpr_Pipe_Chain_FeedsSuccessively(t *testing.T) {
	// Build ((x | stage1) | stage2) manually; each stage's runner
	// returns a known Value, and the outer stage asserts that its
	// last arg is the inner stage's return.
	s := NewSession()
	s.Set("x", StringValue("start"))
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			// Return the last arg's text with a prefix so a chain
			// accumulates visible stages.
			last := args[len(args)-1].(ScalarValueArg).Text
			return StringValue("<" + last + ">"), nil
		},
	}
	inner := &PipeExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "stage1"}},
	}
	outer := &PipeExpr{
		LHS:  inner,
		Args: []Expr{&LiteralExpr{Text: "stage2"}},
	}
	v, err := EvalExpr(outer, env)
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "<<start>>", got)
}

func TestEvalArgs_Pipe_WrapsPipeResultAsArg(t *testing.T) {
	// A PipeExpr used as a command argument: the evaluator should
	// dispatch the pipe, then wrap the returned Value as a
	// ScalarValueArg or StructuredValueArg just like CmdSubExpr.
	s := NewSession()
	s.Set("x", StringValue("42"))
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			return StringValue("piped"), nil
		},
	}
	pipe := &PipeExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "stage"}},
	}
	exprs := []Expr{&LiteralExpr{Text: "outer"}, pipe}
	out, err := EvalArgs(exprs, env)
	require.NoError(t, err)
	require.Len(t, out, 2)
	scalar, ok := out[1].(ScalarValueArg)
	require.True(t, ok)
	assert.Equal(t, "piped", scalar.Text)
}

func TestEvalArgs_CmdSub_NilResultIsError(t *testing.T) {
	s := NewSession()
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			return Value{}, nil
		},
	}
	exprs := []Expr{
		&CmdSubExpr{Inner: &Program{Stmts: []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "inner"}}}}}},
	}
	_, err := EvalArgs(exprs, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "produced no value")
}
