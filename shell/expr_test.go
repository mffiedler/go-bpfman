package shell

import (
	"errors"
	"fmt"
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

// ThreadExpr evaluation: LHS's Value is appended as the last arg to
// the pipe's command, which then dispatches via ExecSubstitution.

func TestEvalExpr_Thread_AppendsScalarValueAsLastArg(t *testing.T) {
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
	pipe := &ThreadExpr{
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

func TestEvalExpr_Thread_AppendsStructuredValueAsLastArg(t *testing.T) {
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
	pipe := &ThreadExpr{
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

func TestEvalExpr_Thread_NilLHSIsError(t *testing.T) {
	s := NewSession()
	s.Set("x", Value{}) // nil value
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			return StringValue("should-not-run"), nil
		},
	}
	pipe := &ThreadExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "jq"}},
	}
	_, err := EvalExpr(pipe, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null")
}

func TestEvalExpr_Thread_NoSubstitutionRunnerIsError(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("42"))
	env := &Env{Session: s}
	e := &ThreadExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "jq"}},
	}
	_, err := EvalExpr(e, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thread")
}

func TestEvalExpr_Thread_Chain_FeedsSuccessively(t *testing.T) {
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
	inner := &ThreadExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "stage1"}},
	}
	outer := &ThreadExpr{
		LHS:  inner,
		Args: []Expr{&LiteralExpr{Text: "stage2"}},
	}
	v, err := EvalExpr(outer, env)
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "<<start>>", got)
}

func TestEvalArgs_Thread_WrapsThreadResultAsArg(t *testing.T) {
	// A ThreadExpr used as a command argument: the evaluator should
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
	pipe := &ThreadExpr{
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

// --- foreach ------------------------------------------------------

func TestEvalProgram_ForEach_IteratesList(t *testing.T) {
	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)

	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg) (Value, error) {
			require.Len(t, args, 1)
			scalar, ok := args[0].(ScalarValueArg)
			require.True(t, ok)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}

	// foreach p in $xs { $p }  — the body is a single command
	// statement whose only arg is the loop variable, so the
	// runner captures each element's text.
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "p",
			List: &VarRefExpr{Name: "xs"},
			Body: []Stmt{
				&CommandStmt{Args: []Expr{&VarRefExpr{Name: "p"}}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, []string{"a", "b", "c"}, captured)
}

func TestEvalProgram_ForEach_LoopVarPersistsAfterLoop(t *testing.T) {
	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`[1,2,3]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "i",
			List: &VarRefExpr{Name: "xs"},
			Body: []Stmt{&CommandStmt{Args: []Expr{&VarRefExpr{Name: "i"}}}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	// After the loop, $i should hold the last element.
	v, ok := s.Get("i")
	require.True(t, ok)
	str, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "3", str)
}

func TestEvalProgram_ForEach_EmptyList(t *testing.T) {
	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`[]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	callCount := 0
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg) (Value, error) {
			callCount++
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "x",
			List: &VarRefExpr{Name: "xs"},
			Body: []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "body"}}}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, 0, callCount, "body must not run for an empty list")
	_, ok := s.Get("x")
	assert.False(t, ok, "loop variable should not be set when the list is empty")
}

func TestEvalProgram_ForEach_NonListIsError(t *testing.T) {
	s := NewSession()
	s.Set("notalist", StringValue("hello"))
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "x",
			List: &VarRefExpr{Name: "notalist"},
			Body: []Stmt{},
		},
	}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foreach")
}

func TestEvalProgram_ForEach_BodyErrorHaltsLoop(t *testing.T) {
	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	seen := 0
	boom := errors.New("boom")
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg) (Value, error) {
			seen++
			if seen == 2 {
				return Value{}, boom
			}
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "x",
			List: &VarRefExpr{Name: "xs"},
			Body: []Stmt{&CommandStmt{Args: []Expr{&VarRefExpr{Name: "x"}}}},
		},
	}}
	evErr := EvalProgram(prog, env)
	require.Error(t, evErr)
	assert.Same(t, boom, evErr, "body error should propagate unwrapped")
	assert.Equal(t, 2, seen, "loop must stop at the first failing iteration")
}

func TestEvalProgram_ForEach_BreakStopsIteration(t *testing.T) {
	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c","d"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg) (Value, error) {
			scalar := args[0].(ScalarValueArg)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}
	// foreach x in $xs {
	//   if $x eq c { break }
	//   $x
	// }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "x",
			List: &VarRefExpr{Name: "xs"},
			Body: []Stmt{
				&IfStmt{
					Cond: &BinaryExpr{
						Left:  &VarRefExpr{Name: "x"},
						Op:    "eq",
						Right: &LiteralExpr{Text: "c"},
					},
					Then: []Stmt{&BreakStmt{}},
				},
				&CommandStmt{Args: []Expr{&VarRefExpr{Name: "x"}}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, []string{"a", "b"}, captured)
}

func TestEvalProgram_ForEach_ContinueSkipsIteration(t *testing.T) {
	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c","d"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg) (Value, error) {
			scalar := args[0].(ScalarValueArg)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}
	// foreach x in $xs {
	//   if $x eq b { continue }
	//   $x
	// }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "x",
			List: &VarRefExpr{Name: "xs"},
			Body: []Stmt{
				&IfStmt{
					Cond: &BinaryExpr{
						Left:  &VarRefExpr{Name: "x"},
						Op:    "eq",
						Right: &LiteralExpr{Text: "b"},
					},
					Then: []Stmt{&ContinueStmt{}},
				},
				&CommandStmt{Args: []Expr{&VarRefExpr{Name: "x"}}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, []string{"a", "c", "d"}, captured)
}

func TestEvalProgram_ForEach_BreakInnerOnly(t *testing.T) {
	// Nested foreach: break in the inner loop must not escape
	// the outer loop.
	s := NewSession()
	outer, err := ValueFromJSON([]byte(`[1,2,3]`))
	require.NoError(t, err)
	inner, err := ValueFromJSON([]byte(`["x","y","z"]`))
	require.NoError(t, err)
	s.Set("outer", outer)
	s.Set("inner", inner)
	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg) (Value, error) {
			scalar := args[0].(ScalarValueArg)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}
	// foreach a in $outer {
	//   foreach b in $inner {
	//     if $b eq y { break }
	//     $b
	//   }
	//   $a
	// }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Name: "a",
			List: &VarRefExpr{Name: "outer"},
			Body: []Stmt{
				&ForEachStmt{
					Name: "b",
					List: &VarRefExpr{Name: "inner"},
					Body: []Stmt{
						&IfStmt{
							Cond: &BinaryExpr{
								Left:  &VarRefExpr{Name: "b"},
								Op:    "eq",
								Right: &LiteralExpr{Text: "y"},
							},
							Then: []Stmt{&BreakStmt{}},
						},
						&CommandStmt{Args: []Expr{&VarRefExpr{Name: "b"}}},
					},
				},
				&CommandStmt{Args: []Expr{&VarRefExpr{Name: "a"}}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	// For each outer iteration: inner emits "x", breaks on "y",
	// then the outer body emits the outer value.
	assert.Equal(t, []string{"x", "1", "x", "2", "x", "3"}, captured)
}

func TestEvalProgram_Break_OutsideLoopIsError(t *testing.T) {
	s := NewSession()
	env := &Env{Session: s}
	prog := &Program{Stmts: []Stmt{&BreakStmt{}}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "break")
	assert.Contains(t, err.Error(), "outside")
}

func TestEvalProgram_Continue_OutsideLoopIsError(t *testing.T) {
	s := NewSession()
	env := &Env{Session: s}
	prog := &Program{Stmts: []Stmt{&ContinueStmt{}}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "continue")
	assert.Contains(t, err.Error(), "outside")
}

// --- logical operators ---------------------------------------------

func TestEvalExpr_And_BothTrue(t *testing.T) {
	v, err := EvalExpr(&LogicalExpr{
		Op:    "and",
		Left:  &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "1"}},
		Right: &BinaryExpr{Left: &LiteralExpr{Text: "2"}, Op: "==", Right: &LiteralExpr{Text: "2"}},
	}, evalEnv(NewSession()))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)
}

func TestEvalExpr_And_ShortCircuitsOnFalseLeft(t *testing.T) {
	// Right operand would error on Scalar() — if the short-circuit
	// fires correctly, it's never evaluated.
	s := NewSession()
	s.Set("m", ValueFromMap(map[string]any{"x": 1}))
	v, err := EvalExpr(&LogicalExpr{
		Op:    "and",
		Left:  &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "2"}},
		Right: &VarRefExpr{Name: "m"},
	}, evalEnv(s))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestEvalExpr_Or_ShortCircuitsOnTrueLeft(t *testing.T) {
	s := NewSession()
	s.Set("m", ValueFromMap(map[string]any{"x": 1}))
	v, err := EvalExpr(&LogicalExpr{
		Op:    "or",
		Left:  &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "1"}},
		Right: &VarRefExpr{Name: "m"},
	}, evalEnv(s))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)
}

func TestEvalExpr_Or_BothFalse(t *testing.T) {
	v, err := EvalExpr(&LogicalExpr{
		Op:    "or",
		Left:  &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "2"}},
		Right: &BinaryExpr{Left: &LiteralExpr{Text: "3"}, Op: "==", Right: &LiteralExpr{Text: "4"}},
	}, evalEnv(NewSession()))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestEvalExpr_Not_Negates(t *testing.T) {
	v, err := EvalExpr(&NotExpr{
		Operand: &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "1"}},
	}, evalEnv(NewSession()))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestEvalExpr_Not_RejectsNonBool(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("hello"))
	_, err := EvalExpr(&NotExpr{Operand: &VarRefExpr{Name: "x"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not")
}

func TestEvalExpr_And_RejectsNonBoolLeft(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("hello"))
	_, err := EvalExpr(&LogicalExpr{
		Op:    "and",
		Left:  &VarRefExpr{Name: "x"},
		Right: &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "1"}},
	}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "and")
}

// --- retry / timeout / iteration -----------------------------------

func TestEvalProgram_Retry_ExitsOnUntilTrue(t *testing.T) {
	// Body succeeds every iteration; until becomes true when
	// iteration count reaches 3.
	s := NewSession()
	callCount := 0
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg) (Value, error) {
			callCount++
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &IterationExpr{Arg: &LiteralExpr{Text: "3"}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, 3, callCount)
}

func TestEvalProgram_Retry_IterationCap_ReturnsLastError(t *testing.T) {
	// Body always errors; until iteration 5 fires; the body's
	// last error propagates out.
	s := NewSession()
	sentinel := errors.New("not yet")
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg) (Value, error) {
			return Value{}, sentinel
		},
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &IterationExpr{Arg: &LiteralExpr{Text: "5"}},
		},
	}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Same(t, sentinel, err, "last body error should propagate unwrapped")
}

func TestEvalProgram_Retry_Timeout_Fires(t *testing.T) {
	// Body always errors; timeout is tiny so we exit in a few
	// iterations.  Verify the last body error propagates.
	s := NewSession()
	sentinel := errors.New("not yet")
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg) (Value, error) {
			return Value{}, sentinel
		},
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &TimeoutExpr{Arg: &LiteralExpr{Text: "50ms"}},
		},
	}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Same(t, sentinel, err)
}

func TestEvalProgram_Retry_Success_ReturnsNil(t *testing.T) {
	// Body succeeds on first iteration; until iteration 1 fires.
	s := NewSession()
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &IterationExpr{Arg: &LiteralExpr{Text: "1"}},
		},
	}}
	assert.NoError(t, EvalProgram(prog, env))
}

func TestEvalProgram_Retry_IterationCap_FromVar(t *testing.T) {
	// The iteration count comes from a session variable, not a
	// literal.  This is the whole point of the relaxed grammar:
	// retry caps are configurable at run time.
	s := NewSession()
	s.Set("max", StringValue("3"))
	callCount := 0
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg) (Value, error) {
			callCount++
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &IterationExpr{Arg: &VarRefExpr{Name: "max"}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, 3, callCount)
}

func TestEvalProgram_Retry_Timeout_FromVar(t *testing.T) {
	s := NewSession()
	s.Set("max_wait", StringValue("50ms"))
	sentinel := errors.New("not yet")
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg) (Value, error) {
			return Value{}, sentinel
		},
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &TimeoutExpr{Arg: &VarRefExpr{Name: "max_wait"}},
		},
	}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Same(t, sentinel, err)
}

func TestEvalProgram_Retry_Iteration_NegativeVarErrors(t *testing.T) {
	s := NewSession()
	s.Set("max", StringValue("-3"))
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "probe"}}}},
			Until: &IterationExpr{Arg: &VarRefExpr{Name: "max"}},
		},
	}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")
}

func TestEvalExpr_Timeout_OutsideRetryIsError(t *testing.T) {
	s := NewSession()
	_, err := EvalExpr(&TimeoutExpr{Arg: &LiteralExpr{Text: "1s"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

func TestEvalExpr_Iteration_OutsideRetryIsError(t *testing.T) {
	s := NewSession()
	_, err := EvalExpr(&IterationExpr{Arg: &LiteralExpr{Text: "3"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iteration")
}

func TestEvalProgram_Retry_NestedRetryScopes(t *testing.T) {
	// Nested retry: inner's timeout / iteration tracks the
	// inner clock, not the outer.  We set both with very
	// different thresholds and ensure the inner exits first.
	s := NewSession()
	seq := []string{}
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg) (Value, error) {
			seq = append(seq, args[0].(WordArg).Text)
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&RetryStmt{
			Body: []Stmt{
				&CommandStmt{Args: []Expr{&LiteralExpr{Text: "outer"}}},
				&RetryStmt{
					Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "inner"}}}},
					Until: &IterationExpr{Arg: &LiteralExpr{Text: "2"}},
				},
			},
			Until: &IterationExpr{Arg: &LiteralExpr{Text: "2"}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	// Outer runs twice.  Each outer iteration runs two inner
	// iterations.  Expected: outer inner inner outer inner inner.
	assert.Equal(t, []string{"outer", "inner", "inner", "outer", "inner", "inner"}, seq)
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

// --- arithmetic ----------------------------------------------------

// scalarTextEval is a small helper that evaluates an expression
// and returns its scalar-formatted result.  Every arithmetic
// test reduces to "evaluate, compare the rendered string" — the
// helper keeps the call sites short.
func scalarTextEval(t *testing.T, e Expr) string {
	t.Helper()
	v, err := EvalExpr(e, evalEnv(NewSession()))
	require.NoError(t, err)
	s, err := v.Scalar()
	require.NoError(t, err)
	return s
}

func TestEvalExpr_Arithmetic_AllOps(t *testing.T) {
	cases := []struct {
		op    string
		left  string
		right string
		want  string
	}{
		// integer-valued operands render without a trailing ".0".
		{"+", "1", "2", "3"},
		{"-", "5", "3", "2"},
		{"*", "4", "3", "12"},
		{"/", "10", "4", "2.5"},
		{"%", "7", "3", "1"},
		// float operands keep their precision.
		{"+", "1.5", "2.25", "3.75"},
		{"*", "2.0", "3.0", "6"},
		// mixed (int + float) still lands on float semantics.
		{"/", "5", "2", "2.5"},
		{"%", "7.5", "2.0", "1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.op+"_"+tc.left+"_"+tc.right, func(t *testing.T) {
			e := &BinaryExpr{
				Left:  &LiteralExpr{Text: tc.left},
				Op:    tc.op,
				Right: &LiteralExpr{Text: tc.right},
			}
			assert.Equal(t, tc.want, scalarTextEval(t, e))
		})
	}
}

func TestEvalExpr_Arithmetic_DivideByZero(t *testing.T) {
	for _, op := range []string{"/", "%"} {
		t.Run(op, func(t *testing.T) {
			e := &BinaryExpr{
				Left:  &LiteralExpr{Text: "1"},
				Op:    op,
				Right: &LiteralExpr{Text: "0"},
			}
			_, err := EvalExpr(e, evalEnv(NewSession()))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "division by zero")
		})
	}
}

func TestEvalExpr_Arithmetic_NonNumericOperand(t *testing.T) {
	// "abc" + 1: Python-style string concat is deliberately out
	// of scope, so this must surface as a numeric-operand error
	// rather than producing a string.
	e := &BinaryExpr{
		Left:  &LiteralExpr{Text: "abc"},
		Op:    "+",
		Right: &LiteralExpr{Text: "1"},
	}
	_, err := EvalExpr(e, evalEnv(NewSession()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not numeric")
}

func TestEvalExpr_Negate_Literal(t *testing.T) {
	e := &NegateExpr{Operand: &LiteralExpr{Text: "5"}}
	assert.Equal(t, "-5", scalarTextEval(t, e))
}

func TestEvalExpr_Negate_DoubleNegate(t *testing.T) {
	// -(-5) → 5: stacks resolve inside-out.
	e := &NegateExpr{Operand: &NegateExpr{Operand: &LiteralExpr{Text: "5"}}}
	assert.Equal(t, "5", scalarTextEval(t, e))
}

func TestEvalExpr_Negate_StructuredIsError(t *testing.T) {
	// Negating a map is nonsense — must error rather than panic.
	s := NewSession()
	s.Set("m", ValueFromMap(map[string]any{"x": 1}))
	_, err := EvalExpr(&NegateExpr{Operand: &VarRefExpr{Name: "m"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negate")
}

func TestEvalExpr_Negate_NonNumericScalarIsError(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("hello"))
	_, err := EvalExpr(&NegateExpr{Operand: &VarRefExpr{Name: "x"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not numeric")
}

func TestEvalExpr_Arithmetic_InComparisonPosition(t *testing.T) {
	// 3 + 4 > 5 → true.  Exercises the full chain:
	// comparison evaluates additive on both sides, reduces each
	// to a numeric scalar, then compares as floats.
	e := &BinaryExpr{
		Left: &BinaryExpr{
			Left:  &LiteralExpr{Text: "3"},
			Op:    "+",
			Right: &LiteralExpr{Text: "4"},
		},
		Op:    ">",
		Right: &LiteralExpr{Text: "5"},
	}
	v, err := EvalExpr(e, evalEnv(NewSession()))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)
}

func TestEvalExpr_Arithmetic_LetRHS(t *testing.T) {
	// let n = $count + 1: parse, evaluate, confirm the session
	// carries a numeric scalar whose text is "11".
	prog, err := parseSource(t, "let count = 10\nlet n = $count + 1")
	require.NoError(t, err)
	s := NewSession()
	require.NoError(t, EvalProgram(prog, evalEnv(s)))
	v, ok := s.Get("n")
	require.True(t, ok)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "11", got)
}

func TestEvalExpr_ExprSub_Arithmetic(t *testing.T) {
	// [[expr]] uses strict tokenisation so '-' and '/' split
	// without surrounding whitespace.  Each case here would
	// either error or return a string under the shell
	// tokenisation used inside [cmd].
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"division no whitespace", "[[4/2]]", "2"},
		{"division fractional", "[[1/2]]", "0.5"},
		{"subtraction no whitespace", "[[3-1]]", "2"},
		{"modulo no whitespace", "[[8%3]]", "2"},
		{"multiplication", "[[10*5]]", "50"},
		{"addition with whitespace still works", "[[4 + 2]]", "6"},
		{"mixed precedence", "[[2+3*4]]", "14"},
		{"grouping with parens", "[[(2+3)*4]]", "20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := parseSource(t, "let x = "+tc.input)
			require.NoError(t, err)
			s := NewSession()
			require.NoError(t, EvalProgram(prog, evalEnv(s)))
			v, ok := s.Get("x")
			require.True(t, ok)
			got, err := v.Scalar()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEvalExpr_ExprSub_VarRef(t *testing.T) {
	// Expression substitutions read session variables and
	// combine them with arithmetic just like any other
	// expression.
	prog, err := parseSource(t, "let count = 21\nlet n = [[$count*2]]")
	require.NoError(t, err)
	s := NewSession()
	require.NoError(t, EvalProgram(prog, evalEnv(s)))
	v, ok := s.Get("n")
	require.True(t, ok)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", got)
}

func TestEvalExpr_InterpString_LiteralOnly(t *testing.T) {
	// An InterpStringExpr with only literal segments (rare in
	// practice — the lexer emits TokenQuoted for that case —
	// but the evaluator is happy to concatenate literals if a
	// caller constructs the node directly).
	s := NewSession()
	e := &InterpStringExpr{
		Segments: []InterpStringSegment{
			{Literal: "hello "},
			{Literal: "world"},
		},
	}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello world", got)
}

func TestEvalExpr_InterpString_VarRef(t *testing.T) {
	s := NewSession()
	s.Set("n", StringValue("60"))
	e := &InterpStringExpr{
		Segments: []InterpStringSegment{
			{Expr: &VarRefExpr{Name: "n"}},
			{Literal: "s"},
		},
	}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "60s", got)
}

func TestEvalExpr_InterpString_MixedSegments(t *testing.T) {
	s := NewSession()
	s.Set("prog", StringValue("42"))
	e := &InterpStringExpr{
		Segments: []InterpStringSegment{
			{Literal: "/sys/fs/bpf/prog-"},
			{Expr: &VarRefExpr{Name: "prog"}},
			{Literal: "/map"},
		},
	}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "/sys/fs/bpf/prog-42/map", got)
}

func TestEvalExpr_InterpString_StructuredValueCompactJSON(t *testing.T) {
	s := NewSession()
	s.Set("r", ValueFromMap(map[string]any{"exit_code": 0, "stdout": "hi"}))
	e := &InterpStringExpr{
		Segments: []InterpStringSegment{
			{Expr: &VarRefExpr{Name: "r"}},
		},
	}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	// json.Marshal sorts map keys alphabetically, so the output is
	// stable regardless of the input map's iteration order.  One
	// line, no indentation.
	assert.Equal(t, `{"exit_code":0,"stdout":"hi"}`, got)
}

func TestEvalExpr_InterpString_ArrayCompactJSON(t *testing.T) {
	s := NewSession()
	s.Set("xs", ValueFromAny([]any{float64(1), float64(2), float64(3)}))
	e := &InterpStringExpr{
		Segments: []InterpStringSegment{
			{Literal: "items="},
			{Expr: &VarRefExpr{Name: "xs"}},
		},
	}
	v, err := EvalExpr(e, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "items=[1,2,3]", got)
}

func TestEvalExpr_InterpString_NilRendersAsNull(t *testing.T) {
	// A nil Value in the interpolation slot renders as "null" so
	// the output string stays well-formed.  We exercise the
	// helper directly because nothing in the expression grammar
	// produces a bare nil Value today — VarRefExpr with a missing
	// path errors at lookup time rather than falling through to
	// nil.
	got, err := renderInterpValue(Value{})
	require.NoError(t, err)
	assert.Equal(t, "null", got)
}

func TestEvalExpr_InterpString_EndToEnd(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Session)
		input string
		want  string
	}{
		{
			name:  "plain literal stays a literal",
			input: `let x = "hello"`,
			want:  "hello",
		},
		{
			name:  "single variable interpolation",
			setup: func(s *Session) { s.Set("n", StringValue("60")) },
			input: `let x = "${n}s"`,
			want:  "60s",
		},
		{
			name:  "path construction",
			setup: func(s *Session) { s.Set("id", StringValue("42")) },
			input: `let x = "/sys/fs/bpf/prog-${id}/map"`,
			want:  "/sys/fs/bpf/prog-42/map",
		},
		{
			name: "adjacent interpolations",
			setup: func(s *Session) {
				s.Set("a", StringValue("hello"))
				s.Set("b", StringValue("world"))
			},
			input: `let x = "${a}${b}"`,
			want:  "helloworld",
		},
		{
			name:  "arithmetic inside interpolation via [[...]]",
			setup: func(s *Session) { s.Set("n", StringValue("30")) },
			input: `let x = "${[[$n * 2]]}s"`,
			want:  "60s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession()
			if tc.setup != nil {
				tc.setup(s)
			}
			prog, err := parseSource(t, tc.input)
			require.NoError(t, err)
			require.NoError(t, EvalProgram(prog, evalEnv(s)))
			v, ok := s.Get("x")
			require.True(t, ok)
			got, err := v.Scalar()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEvalExpr_InterpString_DoubleSigilRejected(t *testing.T) {
	// "${$n}" was the shape a naive "body is a general expression"
	// rule would have exposed; the bash-style "${name}" rule rejects
	// it so there is one spelling of variable interpolation.
	_, err := parseSource(t, `let x = "${$n}"`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "variable reference")
}

func TestEvalExpr_InterpString_ExpressionWithoutBrackets(t *testing.T) {
	// Inside ${...} the body must be a var ref or start with "[".
	// Bare arithmetic is rejected so users reach for the explicit
	// ${[[...]]} form.
	_, err := parseSource(t, `let x = "${n + 1}"`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "variable reference")
}

func TestEvalExpr_InterpString_UndefinedVar(t *testing.T) {
	s := NewSession()
	e := &InterpStringExpr{
		Segments: []InterpStringSegment{
			{Expr: &VarRefExpr{Name: "missing"}},
		},
	}
	_, err := EvalExpr(e, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined variable")
}

func TestEvalExpr_ExprSub_InCondition(t *testing.T) {
	// `if [[$count - 1]] gt 0 { ... }`: condition grammar is
	// already expression-mode at the parser level, so an
	// expression substitution is a legal primary whose numeric
	// result feeds into the comparison.
	s := NewSession()
	s.Set("count", StringValue("5"))
	prog, err := parseSource(t, "let out = 0\nif [[$count - 1]] gt 0 { let out = 1 }")
	require.NoError(t, err)
	require.NoError(t, EvalProgram(prog, evalEnv(s)))
	v, ok := s.Get("out")
	require.True(t, ok)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "1", got)
}

func TestEvalExpr_ExprSub_NestedCmdSub(t *testing.T) {
	// [cmd] is still allowed as an operand inside [[expr]]
	// so expressions can combine arithmetic with command
	// results.
	s := NewSession()
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			return StringValue("5"), nil
		},
	}
	prog, err := parseSource(t, "let n = [[[count] + 1]]")
	require.NoError(t, err)
	require.NoError(t, EvalProgram(prog, env))
	v, ok := s.Get("n")
	require.True(t, ok)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "6", got)
}

func TestEvalExpr_ExprSub_ThreadWithFlagHintsCmdSubForm(t *testing.T) {
	// Threading with a flagged command inside "[[...]]" fails
	// because strict tokenisation splits "-c" into "-" and "c".
	// The error should point the user at "[...]" where shell
	// tokenisation keeps "-c" whole.
	_, err := parseSource(t, `let out = [[$prog |> jq -c "."]]`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "threading with flags")
	assert.Contains(t, err.Error(), "use [$prog |> jq -c \".\"]")
}

func TestEvalExpr_CmdSub_AcceptsThreadExpr(t *testing.T) {
	// "[$x |> cmd -c arg]" is the command-shaped thread form; the
	// parser must accept it under shell tokenisation so flags like
	// "-c" stay whole.  The strict-mode "[[...]]" alternative would
	// split "-c" into "-" and "c" and break the invocation.
	s := NewSession()
	s.Set("prog", StringValue("input"))
	env := &Env{
		Session: s,
		ExecSubstitution: func(args []Arg) (Value, error) {
			require.Len(t, args, 4)
			got := make([]string, len(args))
			for i, a := range args {
				switch v := a.(type) {
				case WordArg:
					got[i] = v.Text
				case QuotedArg:
					got[i] = v.Text
				case ScalarValueArg:
					got[i] = v.Text
				default:
					got[i] = fmt.Sprintf("%T", a)
				}
			}
			// jq receives: "jq", "-c", ".", then the threaded LHS.
			assert.Equal(t, []string{"jq", "-c", ".", "input"}, got)
			return StringValue("\"input\""), nil
		},
	}
	prog, err := parseSource(t, `let out = [$prog |> jq -c "."]`)
	require.NoError(t, err)
	require.NoError(t, EvalProgram(prog, env))
	v, ok := s.Get("out")
	require.True(t, ok)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, `"input"`, got)
}

func TestEvalExpr_CmdSub_RejectsExpressionInner(t *testing.T) {
	// [1/2] no longer masquerades as an expression.  The parser
	// must reject it and point the user at the [[...]] form.
	_, err := parseSource(t, "print [1/2]")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[[")
}

func TestEvalExpr_CmdSub_RejectsBareArithmetic(t *testing.T) {
	_, err := parseSource(t, "print [1 + 1]")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[[")
}
