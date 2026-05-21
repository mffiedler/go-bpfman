package shell

import (
	"errors"
	"fmt"
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

// Bind-position tests pin the value-returning contract: a def
// callable on the right of '<-' publishes its return Value as the
// primary, defer failures during return-unwind flip Rc.OK, and
// the existing bind-target shapes (single name, tuple, guard,
// discard) work uniformly across def-dispatch and ExecBind.

// bindEnv builds an Env for tests that exercise the bind path:
// the recorder captures every ExecBind call so a non-def-dispatch
// fallback shows up as a recorded call, and ExecCommand is a
// no-op so command-form invocations (the def's own call site) do
// not interfere. The recorder's rc function lets a test mark
// specific commands as failing -- used by the defer-failure
// tests where a defer fires through ExecBind and must surface a
// non-ok envelope.
func bindEnv(r *recorder) *Env {
	return &Env{
		Session:     NewSession(),
		ExecBind:    r.execBind,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
	}
}

func TestEvalProgram_Return_BindCarriesPrimary(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def f() { return 7 }
let v <- f
seen $v
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	v, ok := env.Session.Get("v")
	require.True(t, ok, "single-name bind must set v")
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "7", got)
}

func TestEvalProgram_Return_BindFromParameter(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def echo(x) { return $x }
let v <- echo "hello"
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	v, ok := env.Session.Get("v")
	require.True(t, ok)
	got, _ := v.Scalar()
	assert.Equal(t, "hello", got)
}

func TestEvalProgram_Return_BindReturnsList(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def triple() { return [1 2 3] }
let xs <- triple
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	xs, ok := env.Session.Get("xs")
	require.True(t, ok)
	raw, ok := xs.Raw().([]any)
	require.True(t, ok, "expected a list, got %T", xs.Raw())
	assert.Len(t, raw, 3)
}

func TestEvalProgram_Return_BindIntoListThenDestructure(t *testing.T) {
	t.Parallel()
	// The documented two-value pattern: return a list, bind it,
	// destructure into named slots. Pins the composition for the
	// load_xdp-shaped helper called out in LANGUAGE-DIRECTION.md.
	r := &recorder{}
	env := bindEnv(r)
	src := `
def pair() { return [left right] }
let p <- pair
let (a b) = $p
seen $a $b
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	a, _ := env.Session.Get("a")
	b, _ := env.Session.Get("b")
	ga, _ := a.Scalar()
	gb, _ := b.Scalar()
	assert.Equal(t, "left", ga)
	assert.Equal(t, "right", gb)
}

func TestEvalProgram_Return_TupleBindSetsBothSlots(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def f() { return "primary-value" }
let (rc p) <- f
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	rc, ok := env.Session.Get("rc")
	require.True(t, ok)
	rawRc, ok := rc.Raw().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, rawRc["ok"], "Rc.OK true for a clean return")
	p, ok := env.Session.Get("p")
	require.True(t, ok)
	gp, err := p.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "primary-value", gp)
}

func TestEvalProgram_Return_GuardOnSuccessBindsAndContinues(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def f() { return 1 }
guard v <- f
after $v
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	v, ok := env.Session.Get("v")
	require.True(t, ok, "guard must bind on success")
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "1", got)
}

func TestEvalProgram_Return_NoReturnBindsEnvelope(t *testing.T) {
	t.Parallel()
	// A def with no `return` in bind position yields the
	// envelope-mirror as the primary, matching no-payload
	// providers (exec, bpftool, wait). The primary's .ok
	// resolves to true on a clean run.
	r := &recorder{}
	env := bindEnv(r)
	src := `
def f() { do-something }
let v <- f
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	v, ok := env.Session.Get("v")
	require.True(t, ok)
	raw, ok := v.Raw().(map[string]any)
	require.True(t, ok, "no-return def's primary must be an envelope-shaped map")
	assert.Equal(t, true, raw["ok"])
}

func TestEvalProgram_Return_DiscardUnderscorePrimary(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def f() { return 42 }
let _ <- f
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	_, ok := env.Session.Get("_")
	assert.False(t, ok, "_ must not establish a binding")
}

func TestEvalProgram_Return_DeferFailureFlipsRcOk(t *testing.T) {
	t.Parallel()
	// A defer that fires inside the def body and returns non-ok
	// must flip the bind-position Rc.OK to false even though
	// `return EXPR` itself evaluated cleanly. The Primary
	// remains the returned Value -- the single-name bind family
	// discards the envelope, so the script sees the value; the
	// tuple bind sees ok:false on rc.
	r := &recorder{rc: func(args []Arg) Envelope {
		if len(args) > 0 {
			if w, ok := args[0].(WordArg); ok && w.Text == "cleanup" {
				return Envelope{OK: false, Code: 1}
			}
		}
		return Envelope{OK: true}
	}}
	env := bindEnv(r)
	// RenderDeferFailure is required so the defer dispatcher
	// counts the failure on the session; without it the run
	// short-circuits before incrementing the counter and the
	// flip never happens.
	env.RenderDeferFailure = func(Pos, []Arg, Envelope) {}
	src := `
def f() {
  defer cleanup "kaboom"
  return 1
}
let (rc p) <- f
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	rc, ok := env.Session.Get("rc")
	require.True(t, ok)
	rawRc, ok := rc.Raw().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, rawRc["ok"], "defer failure during return unwind must flip rc.ok")
	p, _ := env.Session.Get("p")
	gp, err := p.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "1", gp, "primary still carries the returned value")
}

func TestEvalProgram_Return_GuardHaltsOnDeferFailure(t *testing.T) {
	t.Parallel()
	// A guard form sees rc.ok=false from the defer flip and
	// halts via GuardFailure before any post-call statement
	// runs.
	r := &recorder{rc: func(args []Arg) Envelope {
		if len(args) > 0 {
			if w, ok := args[0].(WordArg); ok && w.Text == "cleanup" {
				return Envelope{OK: false, Code: 1}
			}
		}
		return Envelope{OK: true}
	}}
	env := bindEnv(r)
	env.RenderDeferFailure = func(Pos, []Arg, Envelope) {}
	src := `
def f() {
  defer cleanup
  return 1
}
guard p <- f
after $p
`
	prog := parseProgram(t, src)
	err := EvalProgram(prog, env)
	require.Error(t, err)
	var gf *GuardFailure
	require.True(t, errors.As(err, &gf), "expected GuardFailure, got %T: %v", err, err)
	assert.False(t, gf.Envelope.OK)
}

func TestEvalProgram_Return_RecursiveValueReturn(t *testing.T) {
	t.Parallel()
	// Recursion through value-returning defs: each call gets
	// its own frame and its own returnSignal. The outer call's
	// `let v <- inner` routes through callDefAsBind, the inner
	// call's `return` raises a separate returnSignal that the
	// inner callDefAsBind catches, and the bound value crosses
	// the call boundary intact for the outer to publish.
	//
	// Numeric recursion (sum_to N) would force the test to deal
	// with the language's command-arg stringification rule --
	// arguments lose their numeric Kind at the call boundary, by
	// design. String equality lets the test stay focused on the
	// recursion contract.
	r := &recorder{}
	env := bindEnv(r)
	src := `
def chain(depth) {
  if $depth == "stop" {
    return "base"
  }
  let next <- chain stop
  return "wrap:${next}"
}
let v <- chain go
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	v, ok := env.Session.Get("v")
	require.True(t, ok)
	got, err := v.Scalar()
	require.NoError(t, err)
	assert.Equal(t, "wrap:base", got)
}

func TestEvalProgram_Return_BindFatalErrorPropagates(t *testing.T) {
	t.Parallel()
	// A body error unrelated to return -- here, an unbound
	// variable inside the return expression -- escapes
	// callDefAsBind as a Go error; the bind path frames it and
	// no binding happens.
	r := &recorder{}
	env := bindEnv(r)
	src := `
def f() {
  return $missing
}
let v <- f
`
	prog := parseProgram(t, src)
	err := EvalProgram(prog, env)
	require.Error(t, err)
	_, hasV := env.Session.Get("v")
	assert.False(t, hasV, "no binding when the def body errors")
}

func TestEvalProgram_Return_BindCollectFromDefProducer(t *testing.T) {
	t.Parallel()
	// A def callable in bind position is also a valid
	// bind-collect producer. Each iteration calls the def, the
	// primary accumulates into the bound list.
	r := &recorder{}
	env := bindEnv(r)
	src := `
def square(x) { return $x * $x }
guard squares <- foreach n in [1 2 3 4] {
  square $n
}
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	xs, ok := env.Session.Get("squares")
	require.True(t, ok)
	raw, ok := xs.Raw().([]any)
	require.True(t, ok, "expected list, got %T", xs.Raw())
	require.Len(t, raw, 4)
	// Each element is a string of the squared integer.
	gotSquares := make([]string, len(raw))
	for i, el := range raw {
		gotSquares[i] = elementText(el)
	}
	assert.Equal(t, []string{"1", "4", "9", "16"}, gotSquares)
}

// elementText extracts a stringy view of a list element for
// assertion purposes. The bind-collect path stores .Raw() of each
// element; scalars come through as their underlying type so we
// normalise via fmt.Sprint for the test.
func elementText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		// fall through to a string form; json.Number and ints
		// both render via Sprint without quoting.
		return fmt.Sprint(x)
	}
}

// Checker tests pin the static-time rejection. The runtime catch
// in evalProgramBody is the safety net; the checker rejection is
// what scripts hit first, before any side effects fire.

func TestCheck_Return_InsideDefIsClean(t *testing.T) {
	t.Parallel()
	issues := checkSource(t, "def f() { return 1 }")
	assert.Empty(t, issues)
}

func TestCheck_Return_InsideIfInsideDefIsClean(t *testing.T) {
	t.Parallel()
	// A return inside a nested block is fine as long as some
	// enclosing def opens the call context. The depth counter
	// tracks "any enclosing def", not "directly enclosed".
	issues := checkSource(t, "def f(x) { if $x { return 1 } }")
	assert.Empty(t, issues)
}

func TestCheck_Return_InsideForeachInsideDefIsClean(t *testing.T) {
	t.Parallel()
	src := "def f(xs) { foreach x in $xs { return $x } }"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_Return_AtTopLevelIsRejected(t *testing.T) {
	t.Parallel()
	issues := checkSource(t, "return 1")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "return outside a def body")
}

func TestCheck_Return_InsideIfAtTopLevelIsRejected(t *testing.T) {
	t.Parallel()
	// An if at script top level is not a def context; a return
	// inside is still rejected.
	src := "if true { return 1 }"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "return outside a def body")
}

func TestCheck_Return_AfterDefIsStillTopLevel(t *testing.T) {
	t.Parallel()
	// defDepth must unwind when the def body exits: a return
	// written after a def declaration is at top level and must
	// be rejected.
	src := "def f() { print 1 }\nreturn 1"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "return outside a def body")
}

func TestCheck_Return_ExpressionStillChecked(t *testing.T) {
	t.Parallel()
	// A bad expression on the return RHS reports the
	// undefined-variable issue even when the position is also
	// wrong, so a script with multiple problems shows them all.
	src := "return $missing"
	issues := checkSource(t, src)
	require.Len(t, issues, 2)
	msgs := []string{issues[0].Msg, issues[1].Msg}
	assert.Contains(t, strings.Join(msgs, "\n"), "return outside a def body")
	assert.Contains(t, strings.Join(msgs, "\n"), "undefined variable: missing")
}

func TestCheck_Return_NestedDefBodiesEachOwnContext(t *testing.T) {
	t.Parallel()
	// A nested def declaration -- visually unusual but
	// syntactically allowed -- preserves the depth on entry and
	// pops it back on exit, so a sibling return at the outer
	// level is rejected only when it actually falls outside the
	// outer def.
	src := "def outer() { def inner() { return 1 }\nreturn 2 }"
	issues := checkSource(t, src)
	assert.Empty(t, issues, "both returns are inside a def")
}
