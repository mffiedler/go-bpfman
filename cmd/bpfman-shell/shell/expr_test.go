package shell

import (
	"encoding/json"
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

// bindFromValue adapts a (Value, error)-returning closure to the
// ExecBind signature so tests that drove ExecSubstitution can keep
// the same body shape under the new hook.
func bindFromValue(f func([]Arg, Span) (Value, error)) func([]Arg, Span) (BindResult, error) {
	return func(args []Arg, span Span) (BindResult, error) {
		v, err := f(args, span)
		if err != nil {
			return BindResult{}, err
		}
		return BindResult{Rc: Envelope{OK: true}, Primary: v}, nil
	}
}

func TestEvalExpr_Literal(t *testing.T) {
	t.Parallel()

	s := NewSession()
	v, err := EvalExpr(&LiteralExpr{Text: "hello"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
}

func TestEvalExpr_Literal_Classification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expr    *LiteralExpr
		wantRaw any
	}{
		{"unquoted_int", &LiteralExpr{Text: "5"}, json.Number("5")},
		{"unquoted_float", &LiteralExpr{Text: "5.5"}, json.Number("5.5")},
		{"unquoted_negative", &LiteralExpr{Text: "-3"}, json.Number("-3")},
		{"unquoted_zero_padded", &LiteralExpr{Text: "007"}, json.Number("007")},
		{"unquoted_true", &LiteralExpr{Text: "true"}, true},
		{"unquoted_false", &LiteralExpr{Text: "false"}, false},
		{"unquoted_word", &LiteralExpr{Text: "fentry"}, "fentry"},
		{"unquoted_path", &LiteralExpr{Text: "/tmp/x"}, "/tmp/x"},
		{"unquoted_hex_stays_string", &LiteralExpr{Text: "0xff"}, "0xff"},
		{"quoted_numeric_text_stays_string", &LiteralExpr{Text: "5", Quoted: true}, "5"},
		{"quoted_true_stays_string", &LiteralExpr{Text: "true", Quoted: true}, "true"},
		{"quoted_word", &LiteralExpr{Text: "hello", Quoted: true}, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := NewSession()
			v, err := EvalExpr(tt.expr, evalEnv(s))
			require.NoError(t, err)
			assert.Equal(t, tt.wantRaw, v.Raw())
		})
	}
}

func TestEvalExpr_VarRef_Bare(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("bound"))
	v, err := EvalExpr(&VarRefExpr{Name: "x"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "bound", got)
}

func TestEvalExpr_VarRef_Path(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	s := NewSession()
	_, err := EvalExpr(&VarRefExpr{Name: "missing"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `undefined variable "missing"`)
}

func TestEvalExpr_VarRef_DynamicIndex(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{json.Number("100"), json.Number("200"), json.Number("300")}))
	s.Set("i", ValueFromAny(json.Number("1")))
	v, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i]"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "200", got)
}

func TestEvalExpr_VarRef_DynamicIndex_StringInteger(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b", "c"}))
	s.Set("i", StringValue("2"))
	v, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i]"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "c", got)
}

func TestEvalExpr_VarRef_DynamicIndex_NestedPath(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{
		map[string]any{"name": "alpha"},
		map[string]any{"name": "beta"},
	}))
	s.Set("i", ValueFromAny(json.Number("0")))
	v, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i].name"}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "alpha", got)
}

func TestEvalExpr_VarRef_DynamicIndex_UndefinedIndex(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b"}))
	_, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i]"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "index variable $i is not defined")
}

func TestEvalExpr_VarRef_DynamicIndex_NonInteger(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b"}))
	s.Set("i", StringValue("not-a-number"))
	_, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i]"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "index variable $i:")
	assert.Contains(t, err.Error(), "must be an integer")
}

func TestEvalExpr_VarRef_DynamicIndex_OutOfRange(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b"}))
	s.Set("i", ValueFromAny(json.Number("5")))
	_, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i]"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

func TestEvalExpr_VarRef_DynamicIndex_Negative(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b"}))
	s.Set("i", ValueFromAny(json.Number("-1")))
	_, err := EvalExpr(&VarRefExpr{Name: "xs", Path: "[$i]"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

// TestEvalExpr_VarRef_DynamicIndex_BracedForm confirms the
// "${xs[$i]}" form resolves identically to the bare "$xs[$i]"
// form. Both tokeniser shapes store the path text the same way,
// so a single eval-side check is sufficient.
func TestEvalExpr_VarRef_DynamicIndex_BracedForm(t *testing.T) {
	t.Parallel()

	const src = "${xs[$i]}"
	tokens, err := Tokenise(src)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.Equal(t, TokenVarRef, tokens[0].Kind)
	require.Equal(t, "xs", tokens[0].VarName)
	require.Equal(t, "[$i]", tokens[0].VarPath)

	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b", "c"}))
	s.Set("i", ValueFromAny(json.Number("2")))
	v, err := EvalExpr(&VarRefExpr{Name: tokens[0].VarName, Path: tokens[0].VarPath}, evalEnv(s))
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "c", got)
}

// TestEvalExpr_AdapterArg_DynamicIndex covers the adapter
// reference path through resolveAdapterArg. The tokeniser
// recognises file:$x with the same path grammar as $x, so the
// dynamic-index resolution must travel through the adapter
// arg builder identically.
func TestEvalExpr_AdapterArg_DynamicIndex(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("paths", ValueFromAny([]any{"/a", "/b", "/c"}))
	s.Set("i", ValueFromAny(json.Number("1")))

	arg, err := resolveAdapterArg(&AdapterExpr{
		Adapter: "file",
		Name:    "paths",
		Path:    "[$i]",
	}, evalEnv(s))
	require.NoError(t, err)
	aa, ok := arg.(AdapterArg)
	require.True(t, ok)
	got, err := aa.Value.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "/b", got)
}

// TestEvalProgram_DynamicIndex_ParallelLists exercises the full
// tokenise -> parse -> eval pipeline for the parallel-list iteration
// shape that motivated $xs[$i]: two slot-aligned lists indexed by a
// foreach counter, with the chosen elements appearing as command
// args.
func TestEvalProgram_DynamicIndex_ParallelLists(t *testing.T) {
	t.Parallel()

	const src = `
let xs = [10 20 30]
let ys = ["a" "b" "c"]
foreach i in [0 1 2] {
    record $xs[$i] $ys[$i]
}
`
	tokens, err := Tokenise(src)
	require.NoError(t, err)
	prog, err := Parse(tokens)
	require.NoError(t, err)

	var captured [][]string
	env := &Env{
		Session: NewSession(),
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			row := make([]string, 0, len(args))
			for _, a := range args {
				switch x := a.(type) {
				case WordArg:
					row = append(row, x.Text)
				case ScalarValueArg:
					row = append(row, x.Text)
				default:
					return Value{}, fmt.Errorf("unexpected arg %T", a)
				}
			}
			captured = append(captured, row)
			return Value{}, nil
		},
	}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, [][]string{
		{"record", "10", "a"},
		{"record", "20", "b"},
		{"record", "30", "c"},
	}, captured)
}

func TestEvalExpr_Adapter_RejectedAsExpression(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("hi"))
	_, err := EvalExpr(&AdapterExpr{Adapter: "file", Name: "x"}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adapter")
}

func TestEvalExpr_Binary_Textual(t *testing.T) {
	t.Parallel()

	s := NewSession()
	cases := []struct {
		op         string
		left       string
		right      string
		wantResult bool
	}{
		{"==", "foo", "foo", true},
		{"==", "foo", "bar", false},
		{"!=", "foo", "bar", true},
		{"!=", "foo", "foo", false},
		{"<", "a", "b", true},
		{"<", "b", "a", false},
		{"<=", "a", "a", true},
		{">", "b", "a", true},
		{">=", "a", "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.op+" "+tc.left+" "+tc.right, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()

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
			t.Parallel()
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
	t.Parallel()

	s := NewSession()
	e := &BinaryExpr{
		Left:  &LiteralExpr{Text: "abc"},
		Op:    "<",
		Right: &LiteralExpr{Text: "5"},
	}
	_, err := EvalExpr(e, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot compare string to number")
}

func TestEvalExpr_Binary_StrictDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		left      Expr
		op        string
		right     Expr
		wantBool  bool
		wantError string
	}{
		{"number_eq_number_true", &LiteralExpr{Text: "10"}, "==", &LiteralExpr{Text: "10.0"}, true, ""},
		{"number_eq_number_false", &LiteralExpr{Text: "10"}, "==", &LiteralExpr{Text: "11"}, false, ""},
		{"number_lt_number", &LiteralExpr{Text: "9"}, "<", &LiteralExpr{Text: "10"}, true, ""},
		{"string_eq_string_true", &LiteralExpr{Text: "fentry"}, "==", &LiteralExpr{Text: "fentry"}, true, ""},
		{"string_eq_string_false", &LiteralExpr{Text: "fentry"}, "==", &LiteralExpr{Text: "fexit"}, false, ""},
		{"string_lt_string_lex", &LiteralExpr{Text: "9"}, "<", &LiteralExpr{Text: "10", Quoted: true}, false, "cannot compare number to string"},
		{"bool_eq_bool_true", &LiteralExpr{Text: "true"}, "==", &LiteralExpr{Text: "true"}, true, ""},
		{"bool_ne_bool", &LiteralExpr{Text: "true"}, "!=", &LiteralExpr{Text: "false"}, true, ""},
		{"bool_ordering_rejected", &LiteralExpr{Text: "true"}, "<", &LiteralExpr{Text: "false"}, false, "booleans support only == and !="},
		{"cross_type_string_number", &LiteralExpr{Text: "fentry"}, "==", &LiteralExpr{Text: "5"}, false, "cannot compare string to number"},
		{"cross_type_bool_number", &LiteralExpr{Text: "true"}, "==", &LiteralExpr{Text: "1"}, false, "cannot compare bool to number"},
		{"cross_type_bool_string", &LiteralExpr{Text: "true"}, "==", &LiteralExpr{Text: "true", Quoted: true}, false, "cannot compare bool to string"},
		{"quoted_numeric_is_string", &LiteralExpr{Text: "5", Quoted: true}, "==", &LiteralExpr{Text: "5", Quoted: true}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := NewSession()
			e := &BinaryExpr{Left: tt.left, Op: tt.op, Right: tt.right}
			v, err := EvalExpr(e, evalEnv(s))
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				return
			}
			require.NoError(t, err)
			b, err := AsBool(v)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBool, b)
		})
	}
}

func TestEvalExpr_Unary_NotEmpty(t *testing.T) {
	t.Parallel()

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

func TestExprFromArgs_Primary(t *testing.T) {
	t.Parallel()

	t.Run("word literal", func(t *testing.T) {
		t.Parallel()
		e, err := ExprFromArgs([]Arg{WordArg{Text: "foo"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "foo", lit.Text)
		assert.False(t, lit.Quoted)
	})
	t.Run("quoted literal", func(t *testing.T) {
		t.Parallel()
		e, err := ExprFromArgs([]Arg{QuotedArg{Text: "hello world"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "hello world", lit.Text)
		assert.True(t, lit.Quoted)
	})
	t.Run("scalar var reference", func(t *testing.T) {
		t.Parallel()
		e, err := ExprFromArgs([]Arg{ScalarValueArg{Text: "42"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "42", lit.Text)
	})
	t.Run("bare structured reference", func(t *testing.T) {
		t.Parallel()
		e, err := ExprFromArgs([]Arg{StructuredValueArg{Name: "prog", Value: ValueFromMap(map[string]any{"x": 1})}})
		require.NoError(t, err)
		ref, ok := e.(*VarRefExpr)
		require.True(t, ok)
		assert.Equal(t, "prog", ref.Name)
		assert.Empty(t, ref.Path)
	})
}

func TestExprFromArgs_Unary(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	_, err := ExprFromArgs([]Arg{
		WordArg{Text: "notapred"},
		WordArg{Text: "operand"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected a check like")
}

func TestExprFromArgs_Binary(t *testing.T) {
	t.Parallel()

	ops := []string{"==", "!=", "<", "<=", ">", ">="}
	for _, op := range ops {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()

	_, err := ExprFromArgs([]Arg{
		WordArg{Text: "a"},
		WordArg{Text: "bogus"},
		WordArg{Text: "b"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected an operator")
}

func TestExprFromArgs_TooManyArgs(t *testing.T) {
	t.Parallel()

	_, err := ExprFromArgs([]Arg{
		WordArg{Text: "a"}, WordArg{Text: "b"}, WordArg{Text: "c"}, WordArg{Text: "d"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "got 4 argument")
}

func TestExprFromArgs_Empty(t *testing.T) {
	t.Parallel()

	_, err := ExprFromArgs(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty expression")
}

func TestAsBool_RejectsNonBool(t *testing.T) {
	t.Parallel()

	cases := []Value{
		StringValue("true"),
		StringValue(""),
		ValueFromMap(map[string]any{"x": 1}),
	}
	for _, v := range cases {
		_, err := AsBool(v)
		require.Error(t, err, "kind=%s", v.Kind())
		assert.Contains(t, err.Error(), "use a comparison")
	}
}

func TestIsBinaryOp(t *testing.T) {
	t.Parallel()

	true_ := []string{"==", "!=", "<", "<=", ">", ">="}
	false_ := []string{"", "foo", "=", "<=>", "eq", "ne", "lt", "le", "gt", "ge"}
	for _, s := range true_ {
		assert.True(t, IsBinaryOp(s), s)
	}
	for _, s := range false_ {
		assert.False(t, IsBinaryOp(s), s)
	}
}

func TestIsUnaryPred(t *testing.T) {
	t.Parallel()

	true_ := []string{"not-empty"}
	false_ := []string{"", "ok", "fail", "eq", "nil", "true", "false"}
	for _, s := range true_ {
		assert.True(t, IsUnaryPred(s), s)
	}
	for _, s := range false_ {
		assert.False(t, IsUnaryPred(s), s)
	}
}

// ThreadExpr evaluation: LHS's Value is appended as the last arg to
// the pipe's command, which then dispatches via ExecBind.

func TestEvalExpr_Thread_AppendsScalarValueAsLastArg(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("42"))
	var captured []Arg
	env := &Env{
		Session: s,
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			captured = args
			return StringValue("ok"), nil
		}),
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
	t.Parallel()

	s := NewSession()
	s.Set("p", ValueFromMap(map[string]any{"id": "42"}).WithKind(OriginProgram))
	var captured []Arg
	env := &Env{
		Session: s,
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			captured = args
			return StringValue("ok"), nil
		}),
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
	t.Parallel()

	s := NewSession()
	s.Set("x", Value{}) // nil value
	env := &Env{
		Session: s,
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			return StringValue("should-not-run"), nil
		}),
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
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("42"))
	env := &Env{Session: s}
	e := &ThreadExpr{
		LHS:  &VarRefExpr{Name: "x"},
		Args: []Expr{&LiteralExpr{Text: "jq"}},
	}
	_, err := EvalExpr(e, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'|>'")
}

func TestEvalExpr_Thread_Chain_FeedsSuccessively(t *testing.T) {
	t.Parallel()

	// Build ((x | stage1) | stage2) manually; each stage's runner
	// returns a known Value, and the outer stage asserts that its
	// last arg is the inner stage's return.
	s := NewSession()
	s.Set("x", StringValue("start"))
	env := &Env{
		Session: s,
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			// Return the last arg's text with a prefix so a chain
			// accumulates visible stages.
			last := args[len(args)-1].(ScalarValueArg).Text
			return StringValue("<" + last + ">"), nil
		}),
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
	t.Parallel()

	// A ThreadExpr used as a command argument: the evaluator should
	// dispatch the pipe, then wrap the returned Value as a
	// ScalarValueArg or StructuredValueArg.
	s := NewSession()
	s.Set("x", StringValue("42"))
	env := &Env{
		Session: s,
		ExecBind: bindFromValue(func(args []Arg, _ Span) (Value, error) {
			return StringValue("piped"), nil
		}),
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
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)

	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
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
			Names: []string{"p"},
			List:  &VarRefExpr{Name: "xs"},
			Body: []Stmt{
				&CommandStmt{Args: []Expr{&VarRefExpr{Name: "p"}}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, []string{"a", "b", "c"}, captured)
}

func TestEvalProgram_ForEach_LoopVarBodyScoped(t *testing.T) {
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`[1,2,3]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"i"},
			List:  &VarRefExpr{Name: "xs"},
			Body:  []Stmt{&CommandStmt{Args: []Expr{&VarRefExpr{Name: "i"}}}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	// The loop variable is body-scoped; it must not leak.
	_, ok := s.Get("i")
	assert.False(t, ok, "loop variable $i should not be defined after the loop")
}

func TestEvalProgram_ForEach_LoopVarRestoresPriorBinding(t *testing.T) {
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`[1,2,3]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	s.Set("i", StringValue("outer"))
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"i"},
			List:  &VarRefExpr{Name: "xs"},
			Body:  []Stmt{&CommandStmt{Args: []Expr{&VarRefExpr{Name: "i"}}}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	// A prior binding of $i in the enclosing scope is restored.
	v, ok := s.Get("i")
	require.True(t, ok)
	str, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "outer", str)
}

func TestEvalProgram_ForEach_EmptyList(t *testing.T) {
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`[]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	callCount := 0
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg, Span) (Value, error) {
			callCount++
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"x"},
			List:  &VarRefExpr{Name: "xs"},
			Body:  []Stmt{&CommandStmt{Args: []Expr{&LiteralExpr{Text: "body"}}}},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, 0, callCount, "body must not run for an empty list")
	_, ok := s.Get("x")
	assert.False(t, ok, "loop variable should not be set when the list is empty")
}

func TestEvalProgram_ForEach_NonListIsError(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("notalist", StringValue("hello"))
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"x"},
			List:  &VarRefExpr{Name: "notalist"},
			Body:  []Stmt{},
		},
	}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foreach")
}

func TestEvalProgram_ForEach_BodyErrorHaltsLoop(t *testing.T) {
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	seen := 0
	boom := errors.New("boom")
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			seen++
			if seen == 2 {
				return Value{}, boom
			}
			return Value{}, nil
		},
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"x"},
			List:  &VarRefExpr{Name: "xs"},
			Body:  []Stmt{&CommandStmt{Args: []Expr{&VarRefExpr{Name: "x"}}}},
		},
	}}
	evErr := EvalProgram(prog, env)
	require.Error(t, evErr)
	require.ErrorIs(t, evErr, boom, "body error must remain reachable via errors.Is after the statement-level frame wrap")
	assert.Equal(t, 2, seen, "loop must stop at the first failing iteration")
}

func TestEvalProgram_ForEach_BreakStopsIteration(t *testing.T) {
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c","d"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			scalar := args[0].(ScalarValueArg)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}
	// foreach x in $xs {
	//   if $x == c { break }
	//   $x
	// }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"x"},
			List:  &VarRefExpr{Name: "xs"},
			Body: []Stmt{
				&IfStmt{
					Cond: &BinaryExpr{
						Left:  &VarRefExpr{Name: "x"},
						Op:    "==",
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
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`["a","b","c","d"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	var captured []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			scalar := args[0].(ScalarValueArg)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}
	// foreach x in $xs {
	//   if $x == b { continue }
	//   $x
	// }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"x"},
			List:  &VarRefExpr{Name: "xs"},
			Body: []Stmt{
				&IfStmt{
					Cond: &BinaryExpr{
						Left:  &VarRefExpr{Name: "x"},
						Op:    "==",
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
	t.Parallel()

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
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			scalar := args[0].(ScalarValueArg)
			captured = append(captured, scalar.Text)
			return Value{}, nil
		},
	}
	// foreach a in $outer {
	//   foreach b in $inner {
	//     if $b == y { break }
	//     $b
	//   }
	//   $a
	// }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"a"},
			List:  &VarRefExpr{Name: "outer"},
			Body: []Stmt{
				&ForEachStmt{
					Names: []string{"b"},
					List:  &VarRefExpr{Name: "inner"},
					Body: []Stmt{
						&IfStmt{
							Cond: &BinaryExpr{
								Left:  &VarRefExpr{Name: "b"},
								Op:    "==",
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

func TestEvalProgram_ForEach_MultiVarDestructures(t *testing.T) {
	t.Parallel()

	s := NewSession()
	// Two pairs: ["a","1"] and ["b","2"].
	listValue, err := ValueFromJSON([]byte(`[["a","1"],["b","2"]]`))
	require.NoError(t, err)
	s.Set("pairs", listValue)
	var firsts, seconds []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			require.Len(t, args, 2)
			firsts = append(firsts, args[0].(ScalarValueArg).Text)
			seconds = append(seconds, args[1].(ScalarValueArg).Text)
			return Value{}, nil
		},
	}
	// foreach k, v in $pairs { print $k $v }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"k", "v"},
			List:  &VarRefExpr{Name: "pairs"},
			Body: []Stmt{
				&CommandStmt{Args: []Expr{
					&VarRefExpr{Name: "k"},
					&VarRefExpr{Name: "v"},
				}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, []string{"a", "b"}, firsts)
	assert.Equal(t, []string{"1", "2"}, seconds)

	_, kOk := s.Get("k")
	_, vOk := s.Get("v")
	assert.False(t, kOk, "loop var $k must not persist after the loop")
	assert.False(t, vOk, "loop var $v must not persist after the loop")
}

func TestEvalProgram_ForEach_MultiVarLengthMismatchIsError(t *testing.T) {
	t.Parallel()

	s := NewSession()
	// One element is a 3-tuple but the foreach asks for two names.
	listValue, err := ValueFromJSON([]byte(`[["a","1"],["b","2","extra"]]`))
	require.NoError(t, err)
	s.Set("pairs", listValue)
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"a", "b"},
			List:  &VarRefExpr{Name: "pairs"},
			Body:  []Stmt{},
		},
	}}
	evErr := EvalProgram(prog, env)
	require.Error(t, evErr)
	assert.Contains(t, evErr.Error(), "cannot destructure")
}

func TestEvalProgram_ForEach_MultiVarNonListElementIsError(t *testing.T) {
	t.Parallel()

	s := NewSession()
	// An element is a scalar, not a list.
	listValue, err := ValueFromJSON([]byte(`["scalar"]`))
	require.NoError(t, err)
	s.Set("xs", listValue)
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"a", "b"},
			List:  &VarRefExpr{Name: "xs"},
			Body:  []Stmt{},
		},
	}}
	evErr := EvalProgram(prog, env)
	require.Error(t, evErr)
	assert.Contains(t, evErr.Error(), "not a list")
}

func TestEvalProgram_ForEach_MultiVarDiscardSlot(t *testing.T) {
	t.Parallel()

	s := NewSession()
	listValue, err := ValueFromJSON([]byte(`[["a","1"],["b","2"]]`))
	require.NoError(t, err)
	s.Set("pairs", listValue)
	var seen []string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			seen = append(seen, args[0].(ScalarValueArg).Text)
			return Value{}, nil
		},
	}
	// foreach _, v in $pairs { $v }
	prog := &Program{Stmts: []Stmt{
		&ForEachStmt{
			Names: []string{"_", "v"},
			List:  &VarRefExpr{Name: "pairs"},
			Body: []Stmt{
				&CommandStmt{Args: []Expr{&VarRefExpr{Name: "v"}}},
			},
		},
	}}
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, []string{"1", "2"}, seen)
	// The discard slot must not leak as a real binding.
	_, ok := s.Get("_")
	assert.False(t, ok, "underscore must not become a variable")
}

func TestEvalProgram_CommandArg_ParenExprArithmetic(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("5"))
	var captured string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			require.Len(t, args, 2)
			captured = args[1].(ScalarValueArg).Text
			return Value{}, nil
		},
	}
	// print ($x + 1) -- the BinaryExpr evaluates and wraps as
	// ScalarValueArg via the default evalArg path.
	prog, err := parseSource(t, `print ($x + 1)`)
	require.NoError(t, err)
	require.NoError(t, EvalProgram(prog, env))
	assert.Equal(t, "6", captured)
}

func TestEvalProgram_CommandArg_ParenExprListLiteral(t *testing.T) {
	t.Parallel()

	s := NewSession()
	var captured Arg
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			require.Len(t, args, 2)
			captured = args[1]
			return Value{}, nil
		},
	}
	// print ([1 2 3]) -- a list literal in argument position
	// resolves to a StructuredValueArg via the default path.
	prog, err := parseSource(t, `print ([1 2 3])`)
	require.NoError(t, err)
	require.NoError(t, EvalProgram(prog, env))
	sv, ok := captured.(StructuredValueArg)
	require.True(t, ok, "arg should be StructuredValueArg, got %T", captured)
	raw, ok := sv.Value.Raw().([]any)
	require.True(t, ok)
	assert.Len(t, raw, 3)
}

func TestEvalProgram_Break_OutsideLoopIsError(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := &Env{Session: s}
	prog := &Program{Stmts: []Stmt{&BreakStmt{}}}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "break")
	assert.Contains(t, err.Error(), "outside")
}

func TestEvalProgram_Continue_OutsideLoopIsError(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	v, err := EvalExpr(&NotExpr{
		Operand: &BinaryExpr{Left: &LiteralExpr{Text: "1"}, Op: "==", Right: &LiteralExpr{Text: "1"}},
	}, evalEnv(NewSession()))
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestEvalExpr_Not_RejectsNonBool(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("hello"))
	_, err := EvalExpr(&NotExpr{Operand: &VarRefExpr{Name: "x"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not")
}

func TestEvalExpr_And_RejectsNonBoolLeft(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	// Body succeeds every iteration; until becomes true when
	// iteration count reaches 3.
	s := NewSession()
	callCount := 0
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg, Span) (Value, error) {
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
	t.Parallel()

	// Body always errors; until iteration 5 fires; the body's
	// last error propagates out.
	s := NewSession()
	sentinel := errors.New("not yet")
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg, Span) (Value, error) {
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
	require.ErrorIs(t, err, sentinel, "the last body error must remain reachable via errors.Is")
}

func TestEvalProgram_Retry_Timeout_Fires(t *testing.T) {
	t.Parallel()

	// Body always errors; timeout is tiny so we exit in a few
	// iterations.  Verify the last body error propagates.
	s := NewSession()
	sentinel := errors.New("not yet")
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg, Span) (Value, error) {
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
	require.ErrorIs(t, err, sentinel)
}

func TestEvalProgram_Retry_Success_ReturnsNil(t *testing.T) {
	t.Parallel()

	// Body succeeds on first iteration; until iteration 1 fires.
	s := NewSession()
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
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
	t.Parallel()

	// The iteration count comes from a session variable, not a
	// literal.  This is the whole point of the relaxed grammar:
	// retry caps are configurable at run time.
	s := NewSession()
	s.Set("max", StringValue("3"))
	callCount := 0
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg, Span) (Value, error) {
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
	t.Parallel()

	s := NewSession()
	s.Set("max_wait", StringValue("50ms"))
	sentinel := errors.New("not yet")
	env := &Env{
		Session: s,
		ExecCommand: func([]Arg, Span) (Value, error) {
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
	require.ErrorIs(t, err, sentinel)
}

func TestEvalProgram_Retry_Iteration_NegativeVarErrors(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("max", StringValue("-3"))
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
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
	t.Parallel()

	s := NewSession()
	_, err := EvalExpr(&TimeoutExpr{Arg: &LiteralExpr{Text: "1s"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

func TestEvalExpr_Iteration_OutsideRetryIsError(t *testing.T) {
	t.Parallel()

	s := NewSession()
	_, err := EvalExpr(&IterationExpr{Arg: &LiteralExpr{Text: "3"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iteration")
}

func TestEvalProgram_Retry_NestedRetryScopes(t *testing.T) {
	t.Parallel()

	// Nested retry: inner's timeout / iteration tracks the
	// inner clock, not the outer.  We set both with very
	// different thresholds and ensure the inner exits first.
	s := NewSession()
	seq := []string{}
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
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
	t.Parallel()

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
			t.Parallel()
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
	t.Parallel()

	for _, op := range []string{"/", "%"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()

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
	t.Parallel()

	e := &NegateExpr{Operand: &LiteralExpr{Text: "5"}}
	assert.Equal(t, "-5", scalarTextEval(t, e))
}

func TestEvalExpr_Negate_DoubleNegate(t *testing.T) {
	t.Parallel()

	// -(-5) → 5: stacks resolve inside-out.
	e := &NegateExpr{Operand: &NegateExpr{Operand: &LiteralExpr{Text: "5"}}}
	assert.Equal(t, "5", scalarTextEval(t, e))
}

func TestEvalExpr_Negate_StructuredIsError(t *testing.T) {
	t.Parallel()

	// Negating a map is nonsense — must error rather than panic.
	s := NewSession()
	s.Set("m", ValueFromMap(map[string]any{"x": 1}))
	_, err := EvalExpr(&NegateExpr{Operand: &VarRefExpr{Name: "m"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negate")
}

func TestEvalExpr_Negate_NonNumericScalarIsError(t *testing.T) {
	t.Parallel()

	s := NewSession()
	s.Set("x", StringValue("hello"))
	_, err := EvalExpr(&NegateExpr{Operand: &VarRefExpr{Name: "x"}}, evalEnv(s))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not numeric")
}

func TestEvalExpr_Arithmetic_InComparisonPosition(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestEvalExpr_InterpString_LiteralOnly(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	// A nil Value in the interpolation slot renders as "null" so
	// the output string stays well-formed.  We exercise the
	// helper directly because nothing in the expression grammar
	// produces a bare nil Value today — VarRefExpr with a missing
	// path errors at lookup time rather than falling through to
	// nil.
	got, err := RenderCompact(Value{})
	require.NoError(t, err)
	assert.Equal(t, "null", got)
}

func TestEvalExpr_InterpString_EndToEnd(t *testing.T) {
	t.Parallel()

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
			name:  "arithmetic inside interpolation",
			setup: func(s *Session) { s.Set("n", StringValue("30")) },
			input: `let x = "${$n * 2}s"`,
			want:  "60s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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

func TestEvalExpr_InterpString_UndefinedVar(t *testing.T) {
	t.Parallel()

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
