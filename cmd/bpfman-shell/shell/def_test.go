package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseProgram tokenises and parses src, returning the program or
// failing the test with a descriptive error.
func parseProgram(t *testing.T, src string) *Program {
	t.Helper()
	tokens, err := Tokenise(src)
	require.NoError(t, err, "tokenise failed for %q", src)
	prog, err := Parse(tokens)
	require.NoError(t, err, "parse failed for %q", src)
	return prog
}

// runProgram evaluates src against a fresh session whose ExecCommand
// records each call's first-argument text. Returns the session and
// the recorded calls.
func runProgram(t *testing.T, src string) (*Session, [][]string) {
	t.Helper()
	prog := parseProgram(t, src)
	s := NewSession()
	var calls [][]string
	env := &Env{
		Session: s,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			row := make([]string, len(args))
			for i, a := range args {
				switch x := a.(type) {
				case WordArg:
					row[i] = x.Text
				case QuotedArg:
					row[i] = x.Text
				case ScalarValueArg:
					row[i] = x.Text
				case StructuredValueArg:
					s, _ := x.Value.Scalar()
					row[i] = s
				default:
					row[i] = "?"
				}
			}
			calls = append(calls, row)
			return Value{}, nil
		},
	}
	require.NoError(t, EvalProgram(prog, env))
	return s, calls
}

func TestParse_Def_Basic(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `def greet(name) { print $name }`)
	require.Len(t, prog.Stmts, 1)
	d, ok := prog.Stmts[0].(*DefStmt)
	require.True(t, ok)
	assert.Equal(t, "greet", d.Name)
	assert.Equal(t, []string{"name"}, d.Params)
	require.Len(t, d.Body, 1)
}

func TestParse_Def_NoParams(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `def banner() { print "hello" }`)
	require.Len(t, prog.Stmts, 1)
	d, ok := prog.Stmts[0].(*DefStmt)
	require.True(t, ok)
	assert.Equal(t, "banner", d.Name)
	assert.Empty(t, d.Params)
}

func TestParse_Def_WhitespaceSeparatedParams(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `def f(a b c) { print $a }`)
	d := prog.Stmts[0].(*DefStmt)
	assert.Equal(t, []string{"a", "b", "c"}, d.Params)
}

func TestParse_Def_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing parens", `def f { print 1 }`, "'(' after the name"},
		{"missing body", `def f()`, "expected '{'"},
		{"unterminated body", `def f() { print 1`, "unterminated block"},
		{"duplicate param", `def f(a a) { print 1 }`, "duplicate parameter"},
		{"invalid param", `def f(1) { print 1 }`, "invalid parameter name"},
		{"invalid name", `def 1f() { print 1 }`, "invalid def name"},
		{"reserved name", `def let() { print 1 }`, "reserved word"},
		{"missing name", `def () { print 1 }`, "def requires"},
		{"unterminated params", `def f(a b`, "unterminated parameter list"},
		{"comma rejected", `def f(a, b) { print 1 }`, "comma is not a parameter separator"},
		{"trailing comma rejected", `def f(a b,) { print 1 }`, "comma is not a parameter separator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tokens, err := Tokenise(tc.src)
			require.NoError(t, err)
			_, err = Parse(tokens)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestEvalProgram_Def_RegistersInSession(t *testing.T) {
	t.Parallel()
	s, calls := runProgram(t, `def f() { print "x" }`)
	assert.Empty(t, calls)
	d, ok := s.GetDef("f")
	require.True(t, ok)
	assert.Equal(t, "f", d.Name)
}

func TestEvalProgram_Def_CallBindsParams(t *testing.T) {
	t.Parallel()
	_, calls := runProgram(t, `
def hello(name) { greet $name }
hello "world"
`)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"greet", "world"}, calls[0])
}

func TestEvalProgram_Def_ShadowAndRestore(t *testing.T) {
	t.Parallel()
	src := `
let prog = "outer"
def f(prog) { use $prog }
f "inner"
`
	s, calls := runProgram(t, src)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"use", "inner"}, calls[0])
	v, ok := s.Get("prog")
	require.True(t, ok)
	got, _ := v.Scalar()
	assert.Equal(t, "outer", got, "outer binding must be restored after the call")
}

func TestEvalProgram_Def_RestoreUnsetsWhenNoOuter(t *testing.T) {
	t.Parallel()
	src := `
def f(x) { use $x }
f "hi"
`
	s, _ := runProgram(t, src)
	_, ok := s.Get("x")
	assert.False(t, ok, "parameter must be unset after the call when there was no outer binding")
}

func TestEvalProgram_Def_ArityMismatch(t *testing.T) {
	t.Parallel()
	src := `
def f(a b) { use $a $b }
f "only"
`
	prog := parseProgram(t, src)
	env := &Env{
		Session:     NewSession(),
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected 2")
	assert.Contains(t, err.Error(), "got 1")
}

func TestEvalProgram_Def_Redefinition(t *testing.T) {
	t.Parallel()
	src := `
def f(a) { v1 $a }
def f(a) { v2 $a }
f "x"
`
	_, calls := runProgram(t, src)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"v2", "x"}, calls[0])
}

func TestEvalProgram_Def_CallsAnotherDef(t *testing.T) {
	t.Parallel()
	src := `
def inner(x) { used $x }
def outer(x) { inner $x }
outer "value"
`
	_, calls := runProgram(t, src)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"used", "value"}, calls[0])
}

func TestEvalProgram_Def_BodyErrorPropagates(t *testing.T) {
	t.Parallel()
	src := `
def boom() { fail "now" }
boom
`
	prog := parseProgram(t, src)
	env := &Env{
		Session: NewSession(),
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok && w.Text == "fail" {
					return Value{}, assertErr("kaboom")
				}
			}
			return Value{}, nil
		},
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "kaboom"))
}

// assertErr is a tiny error helper used so the test does not pull in
// errors.New at multiple sites.
func assertErr(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
