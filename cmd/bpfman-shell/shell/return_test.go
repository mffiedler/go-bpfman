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

// Regression: runaway recursion through value-returning defs
// must surface a clean diagnostic rather than a Go runtime
// stack overflow. The corpus's natural shape -- a recursive
// helper that forgets its base case -- would dump pages of
// goroutine traces; the evaluator should catch the depth
// excess and emit "in def NAME: recursion depth limit
// exceeded (N)". The exact limit is implementation-defined
// but must be a few orders of magnitude smaller than Go's
// stack so the diagnostic fires before the runtime panics.
func TestEvalProgram_Return_RecursionDepthGuard(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	env := bindEnv(r)
	src := `
def loop() {
  let next <- loop
  return $next
}
let v <- loop
`
	prog := parseProgram(t, src)
	err := EvalProgram(prog, env)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "recursion", "diagnostic must name the failure class")
	assert.Contains(t, msg, "loop", "diagnostic must name the offending def")
}

// Regression: a single-word alias pointing at a def must
// resolve to the def at command position too, mirroring the
// bind-position resolution shipped in the W11 fix.
// evalCommandStmt's inline def-lookup checked raw def names
// only, so `alias greet = hello; def hello() { print ok };
// greet` fell through to ExecCommand and reported the
// alias's resolved name as an unknown command. The fix is to
// route command-statement dispatch through the same
// lookupDefHead helper used by bind and defer paths so all
// three resolve aliases the same way.
func TestEvalProgram_Return_CommandPositionAliasResolvesToDef(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	var commandCalls []string
	session := NewSession()
	session.SetAlias("greet", "hello")
	env := &Env{
		Session:  session,
		ExecBind: r.execBind,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok {
					commandCalls = append(commandCalls, w.Text)
				}
			}
			return Value{}, nil
		},
	}
	src := `
def hello() {
  ok_marker
}
greet
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	assert.Contains(t, commandCalls, "ok_marker", "alias-resolved def body must run at command position")
	// Neither the alias name "greet" nor the resolved
	// "hello" should appear as an unrecognised ExecCommand
	// dispatch -- both should resolve through the def.
	for _, c := range commandCalls {
		if c == "greet" || c == "hello" {
			t.Fatalf("def name reached ExecCommand as a head (saw %q); the def should have been dispatched", c)
		}
	}
}

// Regression: a single-word alias pointing at a def must
// resolve to the def at the bind-dispatch site, not fall
// through to the external-subprocess path. lookupDefHead used
// to check the head's text against the session's def table
// only; an aliased name (`alias h = helper`; `let v <- h`)
// missed the def-table lookup and slipped through to
// runExternalAsBind, which tried to exec the resolved name as
// a subprocess. The fix is to expand a single-word alias
// before consulting the def table; multi-word aliases stay on
// the ExecBind path because their expansion needs the driver-
// side ApplyAlias to rebuild the args vector.
func TestEvalProgram_Return_BindAliasResolvesToDef(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	var commandCalls []string
	session := NewSession()
	// The shell package has no `alias` keyword; the driver-side
	// alias builtin would install this at run time. Set it
	// directly so the unit test stays focused on lookupDefHead's
	// resolution path.
	session.SetAlias("h", "helper")
	env := &Env{
		Session:  session,
		ExecBind: r.execBind,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok {
					commandCalls = append(commandCalls, w.Text)
				}
			}
			return Value{}, nil
		},
	}
	src := `
def helper() {
  marker
  return "from-helper"
}
let v <- h
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	assert.Contains(t, commandCalls, "marker", "alias-resolved def body must run")
	for _, c := range r.calls {
		if w, ok := c.args[0].(WordArg); ok && (w.Text == "h" || w.Text == "helper") {
			t.Fatalf("alias-resolved def name must not reach ExecBind as a head (saw %q)", w.Text)
		}
	}
}

// Regression: defer-of-aliased-def must also route through
// the def, not the external dispatch path. Same root cause
// as the bind-position case: lookupDefHead consults defs only,
// missing the alias indirection.
func TestEvalProgram_Return_DeferAliasResolvesToDef(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	var commandCalls []string
	session := NewSession()
	session.SetAlias("c", "cleanup")
	env := &Env{
		Session:  session,
		ExecBind: r.execBind,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok {
					commandCalls = append(commandCalls, w.Text)
				}
			}
			return Value{}, nil
		},
	}
	src := `
def cleanup() {
  cleanup_marker
}
defer c
print "main"
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	assert.Contains(t, commandCalls, "cleanup_marker", "alias-resolved defer must run the def body")
}

// Regression: a multi-word alias must NOT be resolved by the
// shell-package def-lookup helper, because rebuilding the args
// vector belongs to the driver-side ApplyAlias. A def shadowing
// a multi-word alias's first word is fine, but bindings like
// `let v <- mylist` where `mylist` expands to `bpfman program list`
// must fall through so ExecBind dispatches the resulting
// multi-token command properly.
func TestEvalProgram_Return_MultiWordAliasFallsThrough(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	session := NewSession()
	session.SetAlias("ml", "bpfman program list")
	env := &Env{Session: session, ExecBind: r.execBind}
	src := "let v <- ml\n"
	require.NoError(t, runProgramWithEnv(t, src, env))
	// The bind reaches ExecBind with the unexpanded head "ml";
	// the driver-side ApplyAlias would replace it. The shell
	// package's def lookup must not invent the def for the
	// first word.
	require.Len(t, r.calls, 1)
	w, ok := r.calls[0].args[0].(WordArg)
	require.True(t, ok)
	assert.Equal(t, "ml", w.Text, "multi-word alias must reach ExecBind unexpanded; driver does the rewrite")
}

// Regression: a `defer my_def` statement must route through the
// def's body, not through env.ExecBind's external-dispatch
// fallback. The bind-statement path already does the def
// lookup ahead of ExecBind; the defer path missed the same
// precedence and so a `defer cleanup` against a user-defined
// `def cleanup` tried to exec a subprocess named `cleanup`.
//
// The recorder's ExecBind records every call it receives, so a
// successful def dispatch must leave no defer-side recording
// for the def name itself: the def's body runs via callDefAsBind
// internally, and any commands the body invokes route through
// ExecCommand at command position. The probe runs a def whose
// body calls a wrapped sentinel command so the test can
// distinguish "def dispatched through callDef" (sentinel
// captured via ExecCommand) from "exec attempted on the def
// name" (defer recorded an exec of "cleanup").
func TestEvalProgram_Return_DeferDispatchesDef(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	var commandCalls []string
	env := &Env{
		Session:  NewSession(),
		ExecBind: r.execBind,
		ExecCommand: func(args []Arg, _ Span) (Value, error) {
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok {
					commandCalls = append(commandCalls, w.Text)
				}
			}
			return Value{}, nil
		},
	}
	src := `
def cleanup() {
  marker
}
defer cleanup
print "main"
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	// The def's body called marker at command position, so it
	// shows up via ExecCommand. The def name itself must not
	// have been seen by ExecBind as a top-level dispatch.
	assert.Contains(t, commandCalls, "marker", "def body must run when the defer fires")
	for _, c := range r.calls {
		if w, ok := c.args[0].(WordArg); ok && w.Text == "cleanup" {
			t.Fatalf("defer dispatched the def name to ExecBind; def should resolve before external dispatch")
		}
	}
}

// Regression: a def declared inside a conditional branch must
// NOT be claimed by the checker as a globally-available def.
// The runtime only registers a def when the DefStmt actually
// executes; `if false { def hidden() {} }; let p <- hidden`
// reached the runtime through preflight because c.defs was
// populated by the walker for every DefStmt regardless of
// branch position. Use-sites then went through the def
// dispatch path, ran into an empty session.defs, and fell
// through to runExternalAsBind which reported "executable
// file not found in $PATH" -- a confusing diagnostic that
// blames the user's path lookup rather than the dead-branch
// declaration.
//
// The fix: only register defs in c.defs when they are
// declared at the top-level walk. Defs inside conditional
// branches (if/elif/else, foreach, eventually) and inside
// other def bodies still get walked for their body content,
// but their names are NOT added to c.defs because the
// runtime cannot guarantee they will be registered. A
// use-site of a conditionally-declared name then goes
// through the same "unknown bind head" path as any other
// undeclared name and produces the W18 typo-suggestion hint.
func TestCheck_Return_ConditionalDefNotRegisteredGlobally(t *testing.T) {
	t.Parallel()
	src := `
if false {
  def hidden() {
    return "v"
  }
}
let p <- hidden
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "use of a conditionally-declared def must trip a diagnostic")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "hidden", "the diagnostic must name the offending head")
	assert.Contains(t, combined, "conditional", "the diagnostic must name the conditional-declaration shape")
}

// Regression: the conditional-def-hint must also fire at
// command position. The W22 fix wired the hint into the
// BindStmt-checker's unknown-head path, but a bare command
// `hidden` (no `<-` bind) went through evalCommandStmt
// without any conditional-def diagnostic. Same shape, three
// other dispatch sites (CommandStmt, bind-collect producer,
// defer) all need the same hint so the user gets a useful
// diagnostic regardless of how they call the conditional def.
func TestCheck_Return_ConditionalDefAtCommandPosition(t *testing.T) {
	t.Parallel()
	src := `
if false {
  def hidden() {
    print "nope"
  }
}
hidden
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "command-position use of a conditional def must trip the diagnostic")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "hidden", "the diagnostic must name the head")
	assert.Contains(t, combined, "conditional", "the diagnostic must name the conditional-declaration shape")
}

// Regression: bind-collect producer must be checked for the
// conditional-def shape too. The producer in `let xs <-
// foreach n in $items { hidden }` is dispatched as a
// command-form at runtime; until the check was wired the
// pre-flight let `hidden` slip through.
func TestCheck_Return_ConditionalDefAtBindCollectProducer(t *testing.T) {
	t.Parallel()
	src := `
let xs = [1]
if false {
  def hidden() {
    return 1
  }
}
let vs <- foreach n in $xs { hidden }
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "bind-collect producer of a conditional def must trip the diagnostic")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "hidden")
	assert.Contains(t, combined, "conditional")
}

// Regression: defer-of-conditional-def. The defer dispatcher
// resolves the head via lookupDefHead at runtime; preflight
// must mirror that resolution and emit the conditional-branch
// hint when the head is conditionally-declared.
func TestCheck_Return_ConditionalDefAtDeferPosition(t *testing.T) {
	t.Parallel()
	src := `
if false {
  def hidden() {
    print "cleaned"
  }
}
defer hidden
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "defer of a conditional def must trip the diagnostic")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "hidden")
	assert.Contains(t, combined, "conditional")
}

// Regression: top-level defs still get registered globally so
// the existing usages work. This pins the boundary between
// the W22 fix (conditional defs don't register) and the W2
// baseline (top-level defs do register and surface in
// bindHeadDef etc.).
func TestCheck_Return_TopLevelDefStillRegistered(t *testing.T) {
	t.Parallel()
	src := `
def visible() {
  return "v"
}
let p <- visible
print $p
`
	issues := checkSource(t, src)
	assert.Empty(t, issues, "a top-level def must still register for bind dispatch to pass preflight")
}

// Regression: a def declared inside a foreach body is
// conditional in the same way as an if-branch def. The
// runtime registers it only when the iteration runs at least
// once; the checker has no way to know how many iterations
// the list produces. Treat the declaration as conditional.
func TestCheck_Return_DefInForeachIsConditional(t *testing.T) {
	t.Parallel()
	src := `
foreach x in [] {
  def hidden() {
    return $x
  }
}
let p <- hidden
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "use of a foreach-body def must trip a diagnostic")
}

// Regression: `let v = my_def` silently binds the literal
// string "my_def" when my_def is a registered def. The two-
// operator distinction between `=` (expression) and `<-`
// (bind) is intentional, but the silent-wrong-thing failure
// mode is steep enough that new users routinely walk off the
// cliff. The checker has the defs map already populated for
// the W2 / W3 work; on a single-name `let v = bareword` where
// the bareword matches a known def, emit a hint pointing at
// the bind form. Does NOT restrict the user -- a def name is
// a valid bareword literal -- just helps when the shape is
// almost certainly a typo.
func TestCheck_Return_LetEqualsDefNameHintsAtBind(t *testing.T) {
	t.Parallel()
	src := `
def maker() {
  return "value"
}
let v = maker
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "the suspicious shape must produce an issue")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "maker", "the diagnostic must name the def")
	assert.Contains(t, combined, "<-", "the diagnostic must point at the bind form")
}

// Regression: the same hint must NOT fire for a let whose RHS
// names something that is NOT a def. A bareword on the RHS
// of `=` is a perfectly valid string literal; we only intercept
// when the bareword is a known def, where the shape is almost
// certainly a typo'd bind. Without this guard the checker
// would emit hints for every bareword-named-string assignment.
func TestCheck_Return_LetEqualsNonDefIsClean(t *testing.T) {
	t.Parallel()
	src := `let v = literal_string_value`
	issues := checkSource(t, src)
	assert.Empty(t, issues, "a bareword RHS that is not a def must not trigger the hint")
}

// Regression: a typo'd def name at the bind RHS used to slip
// through the checker (which sees nothing wrong with an
// unknown bind head -- it might be an external command) and
// land at the runtime "exec NAME: exec NAME: executable file
// not found in $PATH" diagnostic. With the defs map already
// populated, the checker can fuzzy-match the typo'd head
// against the known defs and emit a "did you mean ..." hint.
// Doesn't restrict: an unknown head might genuinely be an
// external command, so the hint is informational.
func TestCheck_Return_BindRHSTypoSuggestsDefName(t *testing.T) {
	t.Parallel()
	src := `
def loader() {
  return "value"
}
let v <- loaderr
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "the typo'd bind head must produce a hint")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "loaderr", "the diagnostic must name the typo")
	assert.Contains(t, combined, "loader", "the diagnostic must suggest the actual def name")
	assert.Contains(t, combined, "did you mean", "the diagnostic must explicitly frame the suggestion")
}

// Regression: a bind head that genuinely is an unknown
// external command -- no defs in scope, or all defs far away
// from the head's text -- must NOT trip the typo hint. The
// strdist threshold gates suggestions to short edit
// distances, but the no-defs-at-all path also needs to stay
// clean.
func TestCheck_Return_BindRHSUnknownNoDefsIsClean(t *testing.T) {
	t.Parallel()
	src := `let v <- some_external_command`
	issues := checkSource(t, src)
	assert.Empty(t, issues, "no defs to match against -- the hint must not fire")
}

// Regression: a comparison operand naming a def must hint
// at the bind form. The W3 arithmetic-operand hint covered
// `let x = two + 3`; this is the parallel for `if two == 2`.
// Same shape, different code path.
func TestCheck_Return_ComparisonHintsAtDefBindForm(t *testing.T) {
	t.Parallel()
	src := `
def two() {
  return 2
}
if two == 2 {
  print "yes"
}
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "the mismatch must still be reported")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "two", "the diagnostic must name the offending operand")
	assert.Contains(t, combined, "<-", "the diagnostic must point at the bind form")
}

// Regression: a non-numeric arithmetic operand whose text
// happens to be a known def name produces a confusing
// "operand 'two' is not numeric" diagnostic. The user almost
// certainly meant to call the def. The checker should append
// a "did you mean `let v <- two`?" hint so the corrective
// shape is obvious.
func TestCheck_Return_ArithmeticHintsAtDefBindForm(t *testing.T) {
	t.Parallel()
	src := `
def two() {
  return 2
}
let x = two + 3
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "non-numeric operand still rejected")
	combined := issues[0].Msg
	for _, i := range issues[1:] {
		combined += "\n" + i.Msg
	}
	assert.Contains(t, combined, "two", "the diagnostic must name the offending operand")
	assert.Contains(t, combined, "<-", "hint must point at the bind form")
}

// Regression: a runtime error inside a def body cited only
// the body location, leaving the caller unable to tell which
// of several call sites tripped it. With value-returning
// helpers designed for reuse, the ambiguity is acute. The
// diagnostic must name the call site so the user can navigate
// to the offending caller.
func TestEvalProgram_Return_RuntimeErrorNamesCallSite(t *testing.T) {
	t.Parallel()
	// The leading newline puts the def on line 2 of the source
	// and the bind on line 6, so the test can assert both the
	// body line in the rust-frame citation and the call line
	// in the embedded "in def echo (called at ...)" note.
	src := `
def echo(x) {
  return $x.field
}

let a <- echo "first"
`
	prog := parseProgram(t, src)
	env := &Env{
		Session: NewSession(),
		ExecCommand: func([]Arg, Span) (Value, error) {
			return Value{}, nil
		},
		ExecBind: func([]Arg, Span) (BindResult, error) {
			return BindResult{Rc: Envelope{OK: true}}, nil
		},
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	msg := err.Error()
	// The body-side citation must still be present so the
	// user sees the inner error location.
	assert.Contains(t, msg, "cannot access field", "the body error must still be cited")
	// The diagnostic must name the called def.
	assert.Contains(t, msg, "echo", "the diagnostic must name the called def")
	// And the call site line must appear in the message. The
	// bind statement is on line 6 of the source; the call
	// itself is at the head of the RHS command.
	assert.Contains(t, msg, "called at 6:", "the call site must be cited at the bind's line")
}

// Regression: with two def-call frames -- outer calls inner --
// the call-site annotation must point at the line WITHIN the
// outer def's body where the call to inner appears, NOT at
// some surrounding chunk's start. Earlier behaviour leaked
// env.ChunkStartLine (the currently-executing top-level
// chunk) into the call-site coordinate, so the annotation
// always reported the top-level user-call line regardless of
// how deep the failure was. The design call: keep "in def
// NAME (called at L:C)" pointing at the innermost call site
// -- the most actionable navigation target -- and treat a
// full stack trace as a separate future enhancement.
//
// The bug only manifests when each def lives in its own chunk
// (the bpfman-shell chunk loop's normal case): body Pos values
// are chunk-relative, def.RegStartLine carries the chunk's
// start line, and translation has to use the def's RegStartLine
// rather than the CURRENT chunk's start. EvalProgram on a
// single-string program uses absolute Pos values throughout, so
// the bug is invisible there. This test simulates the chunked
// path by parsing each def in a separate chunk and bumping
// env.ChunkStartLine in step, exactly as the real loop does.
func TestEvalProgram_Return_DeepChainCallSiteIsInnermost(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := &Env{
		Session:     s,
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
		ExecBind: func([]Arg, Span) (BindResult, error) {
			return BindResult{Rc: Envelope{OK: true}}, nil
		},
	}

	// Chunk 1: inner def at file lines 2-4 inclusive.
	env.ChunkStartLine = 2
	innerProg := parseProgram(t, "def inner(x) {\n  return $x.bad\n}")
	require.NoError(t, EvalProgram(innerProg, env))

	// Chunk 2: outer def at file lines 6-9. The call to
	// inner is on the second body line, file line 7.
	env.ChunkStartLine = 6
	outerProg := parseProgram(t, "def outer(x) {\n  let v <- inner $x\n  return $v\n}")
	require.NoError(t, EvalProgram(outerProg, env))

	// Chunk 3: top-level invocation at file line 11.
	env.ChunkStartLine = 11
	callProg := parseProgram(t, `let r <- outer "hi"`)
	err := EvalProgram(callProg, env)
	require.Error(t, err)
	msg := err.Error()

	assert.Contains(t, msg, "in def inner", "annotation must name the innermost def")
	// The innermost call -- outer's body invoking inner -- is
	// on file line 7. The buggy path reports 11 (the top-level
	// chunk's start) because env.ChunkStartLine, not outer's
	// RegStartLine, was used for the translation.
	assert.Contains(t, msg, "called at 7:", "innermost call site is on line 7 (outer's body invoking inner)")
	assert.NotContains(t, msg, "called at 11", "must not report the top-level chunk's line as the failing call's location")
}

// Regression: when no chunk-start shift is in effect (embedded
// EvalProgram use, this test's path) the call site reads as
// the raw script line. The chunk loop in
// cmd/bpfman-shell/repl/loop.go sets env.ChunkStartLine so a
// real driver shifts the body span; the call-site annotation
// must shift in lockstep so the cited coordinate is always
// file-absolute. This test pins the shift-free behaviour
// because that is the path EvalProgram uses; the live
// bpfman-shell test in the probe sequence pinned the shifted
// path end-to-end.
func TestEvalProgram_Return_CallSiteAtEmbeddedTopLevel(t *testing.T) {
	t.Parallel()
	src := "def f(x) { return $x.y }\nlet v <- f \"hi\"\n"
	prog := parseProgram(t, src)
	env := &Env{
		Session:     NewSession(),
		ExecCommand: func([]Arg, Span) (Value, error) { return Value{}, nil },
		ExecBind: func([]Arg, Span) (BindResult, error) {
			return BindResult{Rc: Envelope{OK: true}}, nil
		},
	}
	err := EvalProgram(prog, env)
	require.Error(t, err)
	msg := err.Error()
	// The bind is on line 2; the call is at the head of the RHS.
	assert.Contains(t, msg, "called at 2:", "call site at line 2 must be cited")
}

// Regression: the static checker must treat a def call on the
// right of '<-' as an open-shape source (the def's return value
// is dynamic), not as a result envelope. The Envelope shape is
// sealed -- its fields are ok/code/stdout/stderr/... -- so a
// field name like `.id` that does not exist there gets
// rejected at preflight when the primary is mis-shaped as an
// envelope. The fix is to mark def-bound primaries as
// OriginUnknown (open) so dynamic field access passes the
// preflight, matching what happens at runtime when the def
// returns a structured value.
func TestCheck_Return_BindFromDef_UnknownShapeAllowsFieldAccess(t *testing.T) {
	t.Parallel()
	src := `
def load_prog() {
  return "hello"
}
let p <- load_prog
print $p.id
`
	issues := checkSource(t, src)
	assert.Empty(t, issues, "field access on a def-returned value must not be rejected by the checker")
}

// Regression: a recursive value-returning def -- one whose
// body contains `let v <- self ...` against itself -- must
// pass preflight. The checker records the def name BEFORE
// walking the body so the inner bind site resolves the head
// to a known def and binds an open shape on $v.
func TestCheck_Return_RecursiveValueReturn_FieldAccess(t *testing.T) {
	t.Parallel()
	src := `
def chain(depth) {
  if $depth == "stop" {
    return "base"
  }
  let next <- chain stop
  return ${next}
}
let v <- chain go
print $v.field
`
	issues := checkSource(t, src)
	assert.Empty(t, issues, "recursive value-returning def must allow field access on its primary")
}

// Regression: a no-return def in bind position produces the
// envelope mirror as the primary (matching the no-payload
// command-bind family); the checker must keep the sealed
// envelope shape for that case so accessing a non-envelope
// field on the bound primary is caught at preflight rather
// than failing at runtime. The earlier fix that introduced
// def-as-bind-head over-broadened to OriginUnknown for every
// def, which masked this class of mistake.
func TestCheck_Return_NoReturnDefBindsSealedEnvelope(t *testing.T) {
	t.Parallel()
	src := `
def side_effect() {
  print "ran"
}
let v <- side_effect
print $v.id
`
	issues := checkSource(t, src)
	require.NotEmpty(t, issues, "no-return def bind must keep the sealed envelope shape so .id is rejected at preflight")
	// The diagnostic still points at the offending field
	// access, not at the def call itself.
	assert.Contains(t, issues[0].Msg, "id")
}

// Regression: a def whose name shadows a registered pure
// builtin must route through the def at preflight, so the
// pure-builtin <- rejection does not fire. The runtime's
// def lookup wins ahead of the external dispatch path; the
// checker must mirror that precedence.
func TestCheck_Return_DefShadowingPureBuiltinIsAccepted(t *testing.T) {
	t.Parallel()
	// `range` is a registered pure builtin; if a script
	// shadows it with a def, the def takes over on the bind
	// RHS at runtime and the checker must agree.
	src := `
def range() {
  return "shadowed"
}
let v <- range
`
	issues := checkSource(t, src)
	assert.Empty(t, issues, "a def shadowing a pure builtin must not trip the pure-builtin <- rejection")
}

// Regression: a defer-failure flip on the bind-position Rc.OK
// must reflect the def's OWN cleanup outcome, not the session-
// wide counter. An inner def whose defer fails -- invoked as a
// command form and discarding its own rc -- must not cause the
// outer def's `let (rc p) <- outer` to land with rc.ok = false.
// The contract is def-local cleanup; nested defer failures must
// not leak across call boundaries.
func TestEvalProgram_Return_NestedDeferFailureDoesNotLeak(t *testing.T) {
	t.Parallel()
	r := &recorder{rc: func(args []Arg) Envelope {
		if len(args) > 0 {
			if w, ok := args[0].(WordArg); ok && w.Text == "cleanup_inner" {
				return Envelope{OK: false, Code: 1}
			}
		}
		return Envelope{OK: true}
	}}
	env := bindEnv(r)
	env.RenderDeferFailure = func(Pos, []Arg, Envelope) {}
	src := `
def inner() {
  defer cleanup_inner
}
def outer() {
  inner
  return 1
}
let (rc p) <- outer
`
	require.NoError(t, runProgramWithEnv(t, src, env))
	rc, ok := env.Session.Get("rc")
	require.True(t, ok)
	rawRc, ok := rc.Raw().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, rawRc["ok"], "outer's rc.ok must reflect outer's OWN cleanup, not the inner def's defer failure")
}
