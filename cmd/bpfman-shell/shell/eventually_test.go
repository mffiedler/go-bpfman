package shell

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventuallyEnv builds an Env wired with the minimum hooks
// runEventually needs: ExecBind for guard/let<-, ExecCommand
// for plain command statements, and ExecAssertStmt for
// expression-form assert/require. The assert dispatcher mirrors
// the driver's behaviour: records counter and returns
// *AssertFailure on plain assert; returns *RequireFailure on
// require so the typed-error layer is exercised.
func eventuallyEnv(s *Session, exec func([]Arg, Span) (Value, error), bind func([]Arg, Span) (BindResult, error)) *Env {
	return &Env{
		Session:     s,
		ExecCommand: exec,
		ExecBind:    bind,
		ExecAssertStmt: func(stmt *AssertStmt, env *Env) error {
			v, err := EvalExpr(stmt.Expr, env)
			if err != nil {
				return err
			}
			pass, err := AsBool(v)
			if err != nil {
				return err
			}
			if pass {
				return nil
			}
			if stmt.IsRequire {
				return &RequireFailure{Span: stmt.Span, Expr: "expr false"}
			}
			env.Session.RecordAssertFailure()
			return &AssertFailure{Span: stmt.Span, Expr: "expr false"}
		},
	}
}

func runEventuallySrc(t *testing.T, src string, env *Env) error {
	t.Helper()
	prog, err := parseSource(t, src)
	require.NoError(t, err)
	return EvalProgram(prog, env)
}

func TestEventually_SucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "eventually timeout 1s {\n  assert true == true\n}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	assert.Equal(t, 0, s.AssertFailures(), "no assertion failures recorded on first-attempt success")
}

func TestEventually_RetriesOnAssertFailureThenSucceeds(t *testing.T) {
	t.Parallel()

	s := NewSession()
	var calls int32
	env := eventuallyEnv(s,
		func(args []Arg, _ Span) (Value, error) {
			// Each call increments the counter; the assert
			// uses the bound count via ExecBind below.
			atomic.AddInt32(&calls, 1)
			return Value{}, nil
		},
		func(args []Arg, _ Span) (BindResult, error) {
			n := atomic.AddInt32(&calls, 1)
			return BindResult{Rc: Envelope{OK: true}, Primary: StringValue(intStr(int(n)))}, nil
		},
	)
	// First two attempts: count < 3, assert fails. Third
	// attempt: count >= 3, assert holds. The captured count is
	// a string (the producer mock returns StringValue), so the
	// expected value is quoted to keep the comparison scalar-
	// to-scalar -- the binary == operator rejects mixed
	// string/number operands.
	src := "eventually timeout 5s interval 1ms {\n" +
		"  let n <- record\n" +
		"  assert $n == \"3\"\n" +
		"}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(3))
	assert.Equal(t, 0, s.AssertFailures(), "successful eventually nets zero assertion failures")
}

func TestEventually_TimesOutAndChargesOneFailure(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "eventually timeout 50ms interval 1ms {\n  assert 1 == 2\n}\n"
	err := runEventuallySrc(t, src, env)
	require.Error(t, err, "statement form must propagate the timeout failure")
	assert.Equal(t, 1, s.AssertFailures(), "timeout charges exactly one assertion failure")
}

func TestEventually_FatalErrorHaltsWithoutRetry(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	// $missing is unbound: the unbound-variable error is fatal
	// regardless of retry budget. Eventually must propagate
	// immediately rather than retry for the full timeout.
	src := "eventually timeout 30s interval 1ms {\n  let x = $missing\n}\n"
	start := time.Now()
	err := runEventuallySrc(t, src, env)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "fatal error must not consume the timeout budget")
	assert.Contains(t, err.Error(), "undefined variable")
	assert.Contains(t, err.Error(), "missing")
}

func TestEventually_BodyLetDoesNotLeak(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "eventually timeout 1s {\n  let scratch = inside\n}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	_, ok := s.Get("scratch")
	assert.False(t, ok, "body-level let must not leak past the attempt frame")
}

func TestEventually_AssertCounterNetZeroOnSuccess(t *testing.T) {
	t.Parallel()

	// Confirm the snapshot/reset protocol: a sequence of failed
	// attempts followed by a successful one nets zero failures
	// in the session counter -- as if the early assert-fails
	// never happened.
	s := NewSession()
	var n int32
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func(args []Arg, _ Span) (BindResult, error) {
			v := atomic.AddInt32(&n, 1)
			return BindResult{Rc: Envelope{OK: true}, Primary: StringValue(intStr(int(v)))}, nil
		},
	)
	src := "eventually timeout 5s interval 1ms {\n" +
		"  let k <- counter\n" +
		"  assert $k == \"3\"\n" +
		"}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	assert.Equal(t, 0, s.AssertFailures(),
		"successful eventually leaves the counter at its pre-construct value")
}

func TestEventually_BindForm_ReturnsResultOnOverallFailure(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	// The bind form catches retry timeout: $r is bound with
	// ok:false, timed_out:true; the script continues past the
	// eventually rather than halting.
	src := "let r <- eventually timeout 50ms interval 1ms {\n  assert 1 == 2\n}\n" +
		"let kept = after-eventually\n"
	require.NoError(t, runEventuallySrc(t, src, env))

	v, ok := s.Get("r")
	require.True(t, ok)
	require.True(t, v.IsStructured())
	raw, ok := v.Raw().(map[string]any)
	require.True(t, ok, "bind result is a structured map")
	assert.Equal(t, false, raw["ok"])
	assert.Equal(t, true, raw["timed_out"])
	// The post-eventually statement ran, confirming the bind
	// form did not halt the script on overall timeout.
	_, ok = s.Get("kept")
	assert.True(t, ok)
}

// Regression: `guard r <- eventually ...` was accepted by
// preflight but evalEventuallyBind never consulted s.Guard,
// so on overall timeout the script continued past the
// construct as if `let r <- eventually ...` had been written.
// The guard form must halt with GuardFailure when res.ok is
// false, matching the ordinary command-bind family's guard
// contract.
// Regression: SCOPE-DESIGN.md Section 3.4 says "If an
// attempt-local defer fails while unwinding, eventually
// stops immediately and propagates that cleanup error as
// fatal." Currently runEventually opens the attempt's
// defer scope with WithDeferScope, which only records
// defer failures on the session counter; the body error
// (here, a returnSignal escaping the attempt) propagates
// as-is and the attempt-local cleanup failure is silently
// dropped. The eventually then reports a clean exit even
// though one of its own defers failed.
func TestEventually_AttemptDeferFailureIsFatal(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func(args []Arg, _ Span) (BindResult, error) {
			// Simulate a defer'd command that fails. The
			// recorder rc-fn pattern is used elsewhere; here
			// any cleanup-named arg produces a non-ok envelope.
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok && w.Text == "cleanup" {
					return BindResult{Rc: Envelope{OK: false, Code: 1}}, nil
				}
			}
			return BindResult{Rc: Envelope{OK: true}}, nil
		},
	)
	env.RenderDeferFailure = func(Pos, []Arg, Envelope) {}
	src := "eventually timeout 1s interval 1ms {\n" +
		"  defer cleanup\n" +
		"  assert true\n" +
		"}\n" +
		"let after = post-eventually\n"
	err := runEventuallySrc(t, src, env)
	require.Error(t, err, "attempt-local defer failure must be fatal to the construct")
	_, ok := s.Get("after")
	assert.False(t, ok, "post-eventually statement must not run after a fatal defer failure")
}

func TestEventually_BindForm_GuardHaltsOnOverallFailure(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "guard r <- eventually timeout 50ms interval 1ms {\n  assert 1 == 2\n}\n" +
		"let after = post-eventually\n"
	err := runEventuallySrc(t, src, env)
	require.Error(t, err, "guard must halt on overall eventually failure")
	var gf *GuardFailure
	require.True(t, errors.As(err, &gf), "expected GuardFailure, got %T: %v", err, err)
	assert.False(t, gf.Envelope.OK)
	// The post-eventually statement must NOT have run, since
	// guard halted the script.
	_, ok := s.Get("after")
	assert.False(t, ok, "post-eventually let must not have executed")
}

// Regression: eventually's bind envelope flipped only OK on
// overall failure, leaving Code at 0. Same internal-
// inconsistency shape as the def-callDefAsBind fix. The
// envelope must carry a non-zero Code alongside OK=false so
// the tuple-bind sees consistent fields and the GuardFailure
// rendering path shows a real exit code.
func TestEventually_BindForm_FailureCodeIsNonZero(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "let (rc r) <- eventually timeout 50ms interval 1ms {\n  assert 1 == 2\n}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	rcVal, ok := s.Get("rc")
	require.True(t, ok)
	rawRc, ok := rcVal.Raw().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, rawRc["ok"])
	codeStr := fmt.Sprint(rawRc["code"])
	assert.NotEqual(t, "0", codeStr, "rc.code must be non-zero alongside rc.ok=false")
}

func TestEventually_DefaultInterval(t *testing.T) {
	t.Parallel()

	// With no `interval` clause the construct uses the
	// documented 100ms default. The test runs a short timeout
	// against a body that always fails; the elapsed time plus
	// observed attempt count tells us the cadence implicitly.
	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "let r <- eventually timeout 350ms {\n  assert 1 == 2\n}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	v, ok := s.Get("r")
	require.True(t, ok)
	raw := v.Raw().(map[string]any)
	// With a 100ms cadence and 350ms timeout we expect roughly
	// 3-4 attempts; allow a generous window for jitter and the
	// very first attempt which runs immediately.
	got := mustReadInt(t, raw["attempts"])
	assert.GreaterOrEqual(t, got, 2, "at least two attempts at the default cadence")
	assert.LessOrEqual(t, got, 6, "default cadence should not over-attempt")
}

// mustReadInt extracts an int from a structured-value field
// whose Go-level representation is either int, int64, or
// json.Number (depending on how the map was constructed). The
// eventually result map stores attempts as int and elapsed_ms
// as int64; the test reads both via this helper.
func mustReadInt(t *testing.T, v any) int {
	t.Helper()
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	}
	t.Fatalf("expected int-like value, got %T", v)
	return 0
}

func TestEventually_AttemptDeferRunsOnFailedAttempt(t *testing.T) {
	t.Parallel()

	// Each attempt's defer fires at attempt end, before the
	// next attempt or before the construct returns. We observe
	// this by counting how many times the recorded cleanup
	// command runs across multiple failing attempts.
	s := NewSession()
	var deferred int32
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func(args []Arg, _ Span) (BindResult, error) {
			if len(args) > 0 {
				if w, ok := args[0].(WordArg); ok && w.Text == "cleanup" {
					atomic.AddInt32(&deferred, 1)
				}
			}
			return BindResult{Rc: Envelope{OK: true}}, nil
		},
	)
	src := "let r <- eventually timeout 30ms interval 1ms {\n" +
		"  defer cleanup\n" +
		"  assert 1 == 2\n" +
		"}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	assert.GreaterOrEqual(t, atomic.LoadInt32(&deferred), int32(2),
		"per-attempt defer must fire on every failed attempt")
}

func TestEventually_BindForm_GuardFailureExposesEnvelope(t *testing.T) {
	t.Parallel()

	// A body that fails its guard surfaces the last command
	// envelope in r.last_command while r.error holds the
	// rendered message. The envelope fields are taken from the
	// driver-supplied Rc -- code, stdout, stderr -- not
	// synthesised by the eventually evaluator.
	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) {
			return BindResult{Rc: Envelope{OK: false, Code: 7, Stdout: "drained", Stderr: "nope"}}, nil
		},
	)
	src := "let r <- eventually timeout 30ms interval 1ms {\n" +
		"  guard _ <- probe\n" +
		"}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	v, ok := s.Get("r")
	require.True(t, ok)
	raw := v.Raw().(map[string]any)
	assert.Equal(t, false, raw["ok"])
	assert.NotNil(t, raw["error"], "error carries the rendered message")
	last, ok := raw["last_command"].(map[string]any)
	require.True(t, ok, "command-shaped failure populates the last_command envelope, got %T", raw["last_command"])
	assert.Equal(t, false, last["ok"])
	assert.Equal(t, 7, last["code"])
	assert.Equal(t, "drained", last["stdout"])
	assert.Equal(t, "nope", last["stderr"])

	// Sanity: confirm the underlying error type is retryable
	// via the same errors.As call shape eventually uses.
	gf := &GuardFailure{Primary: "_", Envelope: Envelope{OK: false}}
	var re RetryableError
	require.True(t, errors.As(error(gf), &re))
	assert.True(t, re.Retryable())
}

func TestEventually_BindForm_AssertFailureLeavesLastCommandNil(t *testing.T) {
	t.Parallel()

	// Assertion-shaped failures have no real envelope -- the
	// bind result keeps r.last_command nil rather than
	// manufacturing a synthetic command envelope. r.error
	// still carries the rendered failure for diagnostics.
	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "let r <- eventually timeout 30ms interval 1ms {\n  assert 1 == 2\n}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	v, ok := s.Get("r")
	require.True(t, ok)
	raw := v.Raw().(map[string]any)
	assert.Equal(t, false, raw["ok"])
	assert.NotNil(t, raw["error"], "error carries the rendered message")
	assert.Nil(t, raw["last_command"], "assert failures leave last_command nil")
}

func TestEventually_BindForm_SuccessLeavesErrorAndLastCommandNil(t *testing.T) {
	t.Parallel()

	s := NewSession()
	env := eventuallyEnv(s,
		func([]Arg, Span) (Value, error) { return Value{}, nil },
		func([]Arg, Span) (BindResult, error) { return BindResult{Rc: Envelope{OK: true}}, nil },
	)
	src := "let r <- eventually timeout 1s {\n  assert true == true\n}\n"
	require.NoError(t, runEventuallySrc(t, src, env))
	v, ok := s.Get("r")
	require.True(t, ok)
	raw := v.Raw().(map[string]any)
	assert.Equal(t, true, raw["ok"])
	assert.Nil(t, raw["error"])
	assert.Nil(t, raw["last_command"])
}

// intStr is a tiny base-10 int -> string helper used to avoid
// pulling fmt into the test for the producer mock.
func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
