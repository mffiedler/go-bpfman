package shell

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedCall captures one ExecBind invocation for inspection.
type recordedCall struct {
	args []Arg
	rc   Envelope
}

// recorder builds an ExecBind that records every invocation in
// order and returns the configured rc/primary. ok controls
// whether the rc is treated as successful by guard/defer.
type recorder struct {
	calls []recordedCall
	rc    func(args []Arg) Envelope
}

func (r *recorder) execBind(args []Arg) (BindResult, error) {
	rc := Envelope{OK: true}
	if r.rc != nil {
		rc = r.rc(args)
	}
	r.calls = append(r.calls, recordedCall{args: copyArgs(args), rc: rc})
	return BindResult{Rc: rc, Primary: ValueFromEnvelope(rc)}, nil
}

func copyArgs(args []Arg) []Arg {
	out := make([]Arg, len(args))
	copy(out, args)
	return out
}

// argText extracts the first argument's text for a recorded call,
// matching the syntax tests use to identify which command ran.
func argText0(c recordedCall) string {
	if len(c.args) == 0 {
		return ""
	}
	if w, ok := c.args[0].(WordArg); ok {
		return w.Text
	}
	return ""
}

// joinArgTexts flattens a recorded call's args into a single string
// so tests can match on the full command line.
func joinArgTexts(c recordedCall) string {
	parts := make([]string, 0, len(c.args))
	for _, a := range c.args {
		switch v := a.(type) {
		case WordArg:
			parts = append(parts, v.Text)
		case ScalarValueArg:
			parts = append(parts, v.Text)
		case QuotedArg:
			parts = append(parts, v.Text)
		case StructuredValueArg:
			parts = append(parts, "$"+v.Name)
		default:
			parts = append(parts, fmt.Sprintf("%T", v))
		}
	}
	return joinWithSpace(parts)
}

func joinWithSpace(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

func runProgramWithEnv(t *testing.T, src string, env *Env) error {
	t.Helper()
	prog, err := parseSource(t, src)
	require.NoError(t, err)
	return EvalProgram(prog, env)
}

func TestEvalProgram_Defer_LIFOOrder(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	env := &Env{Session: NewSession(), ExecBind: r.execBind}
	src := "defer cleanup a\ndefer cleanup b\n"
	require.NoError(t, runProgramWithEnv(t, src, env))

	require.Len(t, r.calls, 2)
	assert.Equal(t, "cleanup b", joinArgTexts(r.calls[0]))
	assert.Equal(t, "cleanup a", joinArgTexts(r.calls[1]))
}

func TestEvalProgram_Defer_LifecyclePair(t *testing.T) {
	t.Parallel()

	// The lifecycle case: two resources acquired and registered
	// in order, then a guard fails. Cleanup must fire in reverse
	// (b before a) so the resource graph unwinds correctly.
	guards := 0
	r := &recorder{
		rc: func(args []Arg) Envelope {
			head := argText0(recordedCall{args: args})
			if head == "fail-now" {
				return Envelope{OK: false, Code: 1, Stderr: "boom"}
			}
			if head == "make-resource" {
				guards++
				return Envelope{OK: true}
			}
			return Envelope{OK: true}
		},
	}
	env := &Env{Session: NewSession(), ExecBind: r.execBind}
	src := "guard a <- make-resource a\n" +
		"defer cleanup $a\n" +
		"guard b <- make-resource b\n" +
		"defer cleanup $b\n" +
		"guard _ <- fail-now\n"

	err := runProgramWithEnv(t, src, env)
	require.Error(t, err)
	var gf *GuardFailure
	require.True(t, errors.As(err, &gf), "expected GuardFailure, got %T", err)

	// Recorded order: two make-resource, then fail-now (guard
	// failure), then two cleanups in LIFO order.
	require.Len(t, r.calls, 5)
	assert.Equal(t, "make-resource a", joinArgTexts(r.calls[0]))
	assert.Equal(t, "make-resource b", joinArgTexts(r.calls[1]))
	assert.Equal(t, "fail-now", joinArgTexts(r.calls[2]))
	// The two cleanup calls each receive a captured envelope as
	// their second argument; the rendering reads the variable
	// name from the StructuredValueArg so '$b' precedes '$a'.
	assert.Equal(t, "cleanup $b", joinArgTexts(r.calls[3]))
	assert.Equal(t, "cleanup $a", joinArgTexts(r.calls[4]))
}

func TestEvalProgram_Defer_ArgsCapturedAtRegisterTime(t *testing.T) {
	t.Parallel()

	// Rebinding the variable between defer and scope exit must
	// not change the deferred call's argument: the value at
	// defer time is what runs.
	r := &recorder{}
	env := &Env{Session: NewSession(), ExecBind: r.execBind}
	src := "let target = original\n" +
		"defer cleanup $target\n" +
		"let target = replaced\n"
	require.NoError(t, runProgramWithEnv(t, src, env))

	require.Len(t, r.calls, 1)
	assert.Equal(t, "cleanup original", joinArgTexts(r.calls[0]))
}

func TestEvalProgram_Defer_ShadowRebindingDoesNotLeak(t *testing.T) {
	t.Parallel()

	// The language permits shadowing: 'let' may rebind a name
	// that already exists. The defer at the rebind boundary must
	// see the original binding, regardless of which form did the
	// rebind ('=' assignment or '<-' command capture). Three
	// defers attached at distinct points in the rebind chain
	// each capture a different snapshot, and the deferred calls
	// fire in LIFO order over those frozen values.
	r := &recorder{
		rc: func(args []Arg) Envelope {
			head := argText0(recordedCall{args: args})
			if head == "fetch-third" {
				return Envelope{OK: true}
			}
			return Envelope{OK: true}
		},
	}
	env := &Env{Session: NewSession(), ExecBind: r.execBind}
	src := "let r = first\n" +
		"defer cleanup $r\n" +
		"let r = second\n" +
		"defer cleanup $r\n" +
		"let r <- fetch-third\n" +
		"defer cleanup $r\n"
	require.NoError(t, runProgramWithEnv(t, src, env))

	// Recorded ExecBind calls: one for the bind 'fetch-third',
	// then three cleanups in LIFO order. The third cleanup runs
	// first and saw the rc envelope from fetch-third (a
	// StructuredValueArg). The second cleanup saw 'second' (a
	// scalar). The first cleanup saw 'first'.
	require.Len(t, r.calls, 4)
	assert.Equal(t, "fetch-third", joinArgTexts(r.calls[0]))
	assert.Equal(t, "cleanup $r", joinArgTexts(r.calls[1]))
	assert.Equal(t, "cleanup second", joinArgTexts(r.calls[2]))
	assert.Equal(t, "cleanup first", joinArgTexts(r.calls[3]))
}

func TestEvalProgram_Defer_FailureRendersAndCounts(t *testing.T) {
	t.Parallel()

	// Defer that returns !ok should be rendered (callback fires)
	// and counted in Session.DeferFailures(). Cleanup continues
	// to subsequent defers; the script's body return value is
	// unaffected because the defer is on the success path.
	rendered := 0
	r := &recorder{
		rc: func(args []Arg) Envelope {
			if argText0(recordedCall{args: args}) == "broken-cleanup" {
				return Envelope{OK: false, Code: 2, Stderr: "broken"}
			}
			return Envelope{OK: true}
		},
	}
	session := NewSession()
	env := &Env{
		Session:  session,
		ExecBind: r.execBind,
		RenderDeferFailure: func(stmtLoc Loc, args []Arg, rc Envelope) {
			rendered++
		},
	}
	src := "defer cleanup a\ndefer broken-cleanup\ndefer cleanup b\n"
	require.NoError(t, runProgramWithEnv(t, src, env))

	// All three defers ran (LIFO: b, broken, a).
	require.Len(t, r.calls, 3)
	assert.Equal(t, "cleanup b", joinArgTexts(r.calls[0]))
	assert.Equal(t, "broken-cleanup", joinArgTexts(r.calls[1]))
	assert.Equal(t, "cleanup a", joinArgTexts(r.calls[2]))
	assert.Equal(t, 1, rendered, "renderer fires for the broken cleanup only")
	assert.Equal(t, 1, session.DeferFailures())
}

func TestEvalProgram_Defer_RunsOnGuardHalt(t *testing.T) {
	t.Parallel()

	// Defer registered before the failing guard must still run.
	r := &recorder{
		rc: func(args []Arg) Envelope {
			if argText0(recordedCall{args: args}) == "fail-now" {
				return Envelope{OK: false, Code: 1}
			}
			return Envelope{OK: true}
		},
	}
	env := &Env{Session: NewSession(), ExecBind: r.execBind}
	src := "defer cleanup\nguard _ <- fail-now\n"

	err := runProgramWithEnv(t, src, env)
	require.Error(t, err)
	var gf *GuardFailure
	require.True(t, errors.As(err, &gf))

	// First the failing guard, then the defer.
	require.Len(t, r.calls, 2)
	assert.Equal(t, "fail-now", joinArgTexts(r.calls[0]))
	assert.Equal(t, "cleanup", joinArgTexts(r.calls[1]))
}

func TestEvalProgram_Defer_ForEachRegistersInEnclosing(t *testing.T) {
	t.Parallel()

	// foreach is not a defer scope. defers registered inside the
	// loop attach to the enclosing scope and run after the loop
	// completes, in LIFO order across all iterations.
	r := &recorder{}
	s := NewSession()
	s.Set("xs", ValueFromAny([]any{"a", "b", "c"}))
	env := &Env{Session: s, ExecBind: r.execBind}
	src := "foreach x in $xs {\n  defer cleanup $x\n}\n"
	require.NoError(t, runProgramWithEnv(t, src, env))

	require.Len(t, r.calls, 3)
	assert.Equal(t, "cleanup c", joinArgTexts(r.calls[0]))
	assert.Equal(t, "cleanup b", joinArgTexts(r.calls[1]))
	assert.Equal(t, "cleanup a", joinArgTexts(r.calls[2]))
}

func TestParse_Defer_RequiresCommand(t *testing.T) {
	t.Parallel()

	_, err := parseSource(t, "defer\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "defer requires a command form")
}

func TestParse_Defer_BindsToDeferStmt(t *testing.T) {
	t.Parallel()

	prog, err := parseSource(t, "defer cleanup $x")
	require.NoError(t, err)
	d, ok := firstStmt(t, prog).(*DeferStmt)
	require.True(t, ok, "expected DeferStmt, got %T", firstStmt(t, prog))
	require.NotNil(t, d.Cmd)
	require.Len(t, d.Cmd.Args, 2)
	head, ok := d.Cmd.Args[0].(*LiteralExpr)
	require.True(t, ok)
	assert.Equal(t, "cleanup", head.Text)
}

func TestParse_Defer_IsReservedDefName(t *testing.T) {
	t.Parallel()

	_, err := parseSource(t, "def defer() { print hi }")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved word \"defer\"")
}
