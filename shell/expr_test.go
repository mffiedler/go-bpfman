package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExpr_Empty(t *testing.T) {
	_, err := ParseExpr(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty expression")
}

func TestParseExpr_Primary(t *testing.T) {
	t.Run("word literal", func(t *testing.T) {
		e, err := ParseExpr([]Arg{WordArg{Text: "foo"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "foo", lit.Text)
		assert.False(t, lit.Quoted)
	})

	t.Run("quoted literal", func(t *testing.T) {
		e, err := ParseExpr([]Arg{QuotedArg{Text: "hello world"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "hello world", lit.Text)
		assert.True(t, lit.Quoted)
	})

	t.Run("scalar var reference", func(t *testing.T) {
		e, err := ParseExpr([]Arg{ScalarValueArg{Text: "42"}})
		require.NoError(t, err)
		lit, ok := e.(*LiteralExpr)
		require.True(t, ok)
		assert.Equal(t, "42", lit.Text)
	})

	t.Run("bare structured reference", func(t *testing.T) {
		e, err := ParseExpr([]Arg{StructuredValueArg{Name: "prog", Value: ValueFromMap(map[string]any{"x": 1})}})
		require.NoError(t, err)
		ref, ok := e.(*VarRefExpr)
		require.True(t, ok)
		assert.Equal(t, "prog", ref.Name)
		assert.Empty(t, ref.Path)
	})
}

func TestParseExpr_Unary(t *testing.T) {
	e, err := ParseExpr([]Arg{
		WordArg{Text: "not-empty"},
		ScalarValueArg{Text: "foo"},
	})
	require.NoError(t, err)
	unary, ok := e.(*UnaryExpr)
	require.True(t, ok)
	assert.Equal(t, "not-empty", unary.Pred)
}

func TestParseExpr_UnaryRejectsNonPred(t *testing.T) {
	_, err := ParseExpr([]Arg{
		WordArg{Text: "notapred"},
		WordArg{Text: "operand"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unary predicate")
}

func TestParseExpr_Binary(t *testing.T) {
	cases := []string{"eq", "ne", "lt", "le", "gt", "ge", "==", "!=", "<", "<=", ">", ">="}
	for _, op := range cases {
		t.Run(op, func(t *testing.T) {
			e, err := ParseExpr([]Arg{
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

func TestParseExpr_BinaryRejectsNonOp(t *testing.T) {
	_, err := ParseExpr([]Arg{
		WordArg{Text: "a"},
		WordArg{Text: "bogus"},
		WordArg{Text: "b"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary operator")
}

func TestParseExpr_TooManyArgs(t *testing.T) {
	_, err := ParseExpr([]Arg{
		WordArg{Text: "a"}, WordArg{Text: "b"}, WordArg{Text: "c"}, WordArg{Text: "d"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4 operands")
}

func TestEval_Literal(t *testing.T) {
	s := NewSession()
	v, err := Eval(&LiteralExpr{Text: "hello"}, s, nil)
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
}

func TestEval_VarRef_Bare(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("bound"))
	v, err := Eval(&VarRefExpr{Name: "x"}, s, nil)
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "bound", got)
}

func TestEval_VarRef_Path(t *testing.T) {
	s := NewSession()
	s.Set("prog", ValueFromMap(map[string]any{
		"record": map[string]any{"program_id": "42"},
	}))
	v, err := Eval(&VarRefExpr{Name: "prog", Path: "record.program_id"}, s, nil)
	require.NoError(t, err)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "42", got)
}

func TestEval_VarRef_Undefined(t *testing.T) {
	s := NewSession()
	_, err := Eval(&VarRefExpr{Name: "missing"}, s, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `undefined variable "missing"`)
}

func TestEval_Binary_Textual(t *testing.T) {
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
			v, err := Eval(e, s, nil)
			require.NoError(t, err)
			assert.Equal(t, OriginBool, v.Kind())
			b, err := AsBool(v)
			require.NoError(t, err)
			assert.Equal(t, tc.wantResult, b)
		})
	}
}

func TestEval_Binary_Numeric(t *testing.T) {
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
		// Lexicographic would disagree with numeric on these.
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
			v, err := Eval(e, s, nil)
			require.NoError(t, err)
			b, err := AsBool(v)
			require.NoError(t, err)
			assert.Equal(t, tc.wantResult, b)
		})
	}
}

func TestEval_Binary_NumericNonNumericError(t *testing.T) {
	s := NewSession()
	e := &BinaryExpr{
		Left:  &LiteralExpr{Text: "abc"},
		Op:    "<",
		Right: &LiteralExpr{Text: "5"},
	}
	_, err := Eval(e, s, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not numeric")
}

func TestEval_Unary_NotEmpty(t *testing.T) {
	s := NewSession()
	s.Set("x", StringValue("hello"))
	s.Set("y", StringValue(""))

	e := &UnaryExpr{Pred: "not-empty", Operand: &VarRefExpr{Name: "x"}}
	v, err := Eval(e, s, nil)
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)

	e = &UnaryExpr{Pred: "not-empty", Operand: &VarRefExpr{Name: "y"}}
	v, err = Eval(e, s, nil)
	require.NoError(t, err)
	b, err = AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
}

func TestEval_Unary_TrueFalse(t *testing.T) {
	s := NewSession()
	s.Set("flag", StringValue("true"))
	e := &UnaryExpr{Pred: "true", Operand: &VarRefExpr{Name: "flag"}}
	v, err := Eval(e, s, nil)
	require.NoError(t, err)
	b, err := AsBool(v)
	require.NoError(t, err)
	assert.True(t, b)

	e = &UnaryExpr{Pred: "false", Operand: &VarRefExpr{Name: "flag"}}
	v, err = Eval(e, s, nil)
	require.NoError(t, err)
	b, err = AsBool(v)
	require.NoError(t, err)
	assert.False(t, b)
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
