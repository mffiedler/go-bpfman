package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Parse-level tests pin the AST shape and the diagnostic for the
// rejected forms. The previous tombstone for `return` lived in
// parse_test.go and rejected the word outright; lifting that into
// a real production stops the diagnostic from firing on every
// well-formed return.

func TestParse_Return_BindsExpression(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `
def f() {
  return 1
}
`)
	require.Len(t, prog.Stmts, 1)
	d, ok := prog.Stmts[0].(*DefStmt)
	require.True(t, ok)
	require.Len(t, d.Body, 1)
	r, ok := d.Body[0].(*ReturnStmt)
	require.True(t, ok)
	require.NotNil(t, r.Expr)
}

func TestParse_Return_BareReturnRejected(t *testing.T) {
	t.Parallel()
	src := `
def f() {
  return
}
`
	tokens, err := Tokenise(src)
	require.NoError(t, err)
	_, err = Parse(tokens)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "return requires an expression")
}

func TestParse_Return_VarRef(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `
def f(x) {
  return $x
}
`)
	d := prog.Stmts[0].(*DefStmt)
	require.Len(t, d.Body, 1)
	r := d.Body[0].(*ReturnStmt)
	_, ok := r.Expr.(*VarRefExpr)
	require.True(t, ok, "return $x must parse the RHS as a VarRefExpr")
}

func TestParse_Return_List(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `
def f() {
  return [1 2 3]
}
`)
	d := prog.Stmts[0].(*DefStmt)
	r := d.Body[0].(*ReturnStmt)
	_, ok := r.Expr.(*ListExpr)
	require.True(t, ok, "return [list] must parse the RHS as a ListExpr")
}

func TestParse_Return_Arithmetic(t *testing.T) {
	t.Parallel()
	prog := parseProgram(t, `
def f(x) {
  return $x + 1
}
`)
	d := prog.Stmts[0].(*DefStmt)
	r := d.Body[0].(*ReturnStmt)
	_, ok := r.Expr.(*BinaryExpr)
	require.True(t, ok, "return $x + 1 must parse the RHS as a BinaryExpr")
}

func TestParse_Return_TopLevelParsesButRejectedAtRuntime(t *testing.T) {
	t.Parallel()
	// The parser does not know context; a top-level return parses
	// as a ReturnStmt and is rejected at evaluation time. The
	// static checker rejects it earlier; the runtime guard is the
	// safety net documented on evalProgramBody.
	prog := parseProgram(t, `return 1`)
	require.Len(t, prog.Stmts, 1)
	_, ok := prog.Stmts[0].(*ReturnStmt)
	require.True(t, ok)
}

// Runtime tests pin the statement-form contract: a return is an
// early exit, the value is discarded at command-form position,
// and the early-exit honours def-local defers and frame
// discipline.

func TestEvalProgram_Return_StatementFormIsEarlyExit(t *testing.T) {
	t.Parallel()
	src := `
def f() {
  before
  return 1
  after
}
f
`
	_, calls := runProgram(t, src)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"before"}, calls[0])
}

func TestEvalProgram_Return_OutsideDefIsRuntimeError(t *testing.T) {
	t.Parallel()
	src := `return 1`
	prog := parseProgram(t, src)
	env := &Env{
		Session:     NewSession(),
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "return outside a def body")
}

func TestEvalProgram_Return_InIfOutsideDefIsRuntimeError(t *testing.T) {
	t.Parallel()
	// A return inside an if at script top level still has no
	// enclosing def and must be caught.
	src := `
if true {
  return 1
}
`
	prog := parseProgram(t, src)
	env := &Env{
		Session:     NewSession(),
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "return outside a def body")
}

func TestEvalProgram_Return_InForeachInsideDefHonoursEarlyExit(t *testing.T) {
	t.Parallel()
	// A return inside a foreach body inside a def stops the
	// iteration and the def. The post-foreach statement does not
	// run, nor does any subsequent foreach element.
	src := `
def f() {
  foreach x in [a b c] {
    if $x == "b" {
      return 1
    }
    seen $x
  }
  after-loop
}
f
`
	_, calls := runProgram(t, src)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"seen", "a"}, calls[0])
}

func TestEvalProgram_Return_RunsDefLocalDefers(t *testing.T) {
	t.Parallel()
	// def-local defers register at body time and unwind on
	// return. The early exit must not skip them; the captured
	// argument vector must survive the frame pop. Defers
	// dispatch via ExecBind, so the recorder pattern from
	// defer_test.go is the right test fixture.
	r := &recorder{}
	env := &Env{
		Session:     NewSession(),
		ExecBind:    r.execBind,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	src := `
def f() {
  defer cleanup "A"
  defer cleanup "B"
  return 1
  defer cleanup "C"
}
f
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	require.Len(t, r.calls, 2, "two defers registered before return; the third is unreachable")
	// LIFO unwind: B first, then A.
	assert.Equal(t, "cleanup B", joinArgTexts(r.calls[0]))
	assert.Equal(t, "cleanup A", joinArgTexts(r.calls[1]))
}

func TestEvalProgram_Return_DoesNotLeakBindings(t *testing.T) {
	t.Parallel()
	// A return must not bleed body-locals into the caller; the
	// frame pop is the same as the normal-exit path.
	src := `
def f() {
  let scratch = inside
  return 1
}
f
`
	s, _ := runProgram(t, src)
	_, ok := s.Get("scratch")
	assert.False(t, ok, "return must pop the call frame and discard body-locals")
}

func TestEvalProgram_Return_ExpressionUsesCallFrame(t *testing.T) {
	t.Parallel()
	// The expression evaluates inside the call frame: a return
	// referencing a body-local sees the body-local, not the
	// caller's same-named variable.
	src := `
let label = outer
def f() {
  let label = inner
  return $label
}
f
seen $label
`
	_, calls := runProgram(t, src)
	require.Len(t, calls, 1)
	// After the call, the caller's "outer" is reinstated.
	assert.Equal(t, []string{"seen", "outer"}, calls[0])
}

func TestEvalProgram_Return_UnboundVariableIsFatal(t *testing.T) {
	t.Parallel()
	// A return expression that itself errors halts the def
	// without raising the return signal -- the error propagates
	// as fatal and the script halts.
	src := `
def f() {
  return $missing
}
f
`
	prog := parseProgram(t, src)
	env := &Env{
		Session:     NewSession(),
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	// One of the unbound-variable messages; not "return outside a def body".
	assert.True(t,
		strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "undefined"),
		"expected unbound-variable diagnostic, got %q", err.Error())
}

func TestEvalProgram_Return_NestedDefsHonourEachOwnReturn(t *testing.T) {
	t.Parallel()
	// An inner def's return must not unwind out of the outer
	// def. The outer's post-call statement runs; the inner's
	// post-return statement does not.
	src := `
def inner() {
  before-inner
  return 1
  after-inner
}
def outer() {
  before-outer
  inner
  after-outer
}
outer
`
	_, calls := runProgram(t, src)
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"before-outer"}, calls[0])
	assert.Equal(t, []string{"before-inner"}, calls[1])
	assert.Equal(t, []string{"after-outer"}, calls[2])
}
