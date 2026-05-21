package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkSource tokenises and parses src, runs Check, returns
// the issues. Tests use it as a one-liner so the source
// stays readable.
func checkSource(t *testing.T, src string) []Issue {
	t.Helper()
	tokens, err := Tokenise(src)
	require.NoError(t, err)
	prog, err := Parse(tokens)
	require.NoError(t, err)
	return Check(prog)
}

// TestCheckerFrames_* mirror the runtime Session-frame tests in
// session_test.go: same shape, same names, exercised against the
// checker's frame stack so the two halves of the language stay
// in step.

func TestCheckerFrames_DefineWritesInnermost(t *testing.T) {
	t.Parallel()

	c := newChecker()
	c.define("x", KindShape(OriginScalar), nil)
	c.withFrame(func() {
		c.define("x", KindShape(OriginBool), nil)
		sh, ok := c.lookupShape("x")
		require.True(t, ok)
		assert.Equal(t, OriginBool, sh.Kind)
	})
	sh, ok := c.lookupShape("x")
	require.True(t, ok)
	assert.Equal(t, OriginScalar, sh.Kind)
}

func TestCheckerFrames_LookupWalksOutward(t *testing.T) {
	t.Parallel()

	c := newChecker()
	c.define("a", KindShape(OriginScalar), nil)
	c.withFrame(func() {
		c.define("b", KindShape(OriginBool), nil)
		// Inner sees both: b directly, a through the walk.
		assert.True(t, c.lookupDefined("a"))
		assert.True(t, c.lookupDefined("b"))
	})
	// After pop, only the outer remains visible.
	assert.True(t, c.lookupDefined("a"))
	assert.False(t, c.lookupDefined("b"))
}

func TestCheckerFrames_InnerShadowsOuter(t *testing.T) {
	t.Parallel()

	c := newChecker()
	c.define("x", KindShape(OriginScalar), nil)
	c.withFrame(func() {
		// Inner frame initially sees outer x.
		sh, ok := c.lookupShape("x")
		require.True(t, ok)
		assert.Equal(t, OriginScalar, sh.Kind)
		// Shadowing the name does not touch the outer binding.
		c.define("x", KindShape(OriginBool), nil)
		sh, ok = c.lookupShape("x")
		require.True(t, ok)
		assert.Equal(t, OriginBool, sh.Kind)
	})
	sh, ok := c.lookupShape("x")
	require.True(t, ok)
	assert.Equal(t, OriginScalar, sh.Kind)
}

func TestCheckerFrames_LiteralRecordingScopesToFrame(t *testing.T) {
	t.Parallel()

	// The arithmetic check inspects the recorded RHS literal
	// for non-numeric tokens. The lookup must walk frames so a
	// let inside an inner frame finds its own literal, and
	// popping the frame restores any outer literal.
	c := newChecker()
	outerLit := &LiteralExpr{Text: "outer", Quoted: true}
	c.define("x", KindShape(OriginScalar), outerLit)
	c.withFrame(func() {
		innerLit := &LiteralExpr{Text: "inner", Quoted: true}
		c.define("x", KindShape(OriginScalar), innerLit)
		got, ok := c.lookupLiteral("x")
		require.True(t, ok)
		assert.Equal(t, "inner", got.Text)
	})
	got, ok := c.lookupLiteral("x")
	require.True(t, ok)
	assert.Equal(t, "outer", got.Text)
}

func TestCheckerFrames_DiscardSlotNotBound(t *testing.T) {
	t.Parallel()

	c := newChecker()
	c.define("_", KindShape(OriginScalar), nil)
	assert.False(t, c.lookupDefined("_"))
}

func TestCheckerFrames_WithFramePopsOnPanic(t *testing.T) {
	t.Parallel()

	c := newChecker()
	c.define("x", KindShape(OriginScalar), nil)

	func() {
		defer func() {
			_ = recover()
		}()
		c.withFrame(func() {
			c.define("x", KindShape(OriginBool), nil)
			panic("boom")
		})
	}()

	// Frame popped on panic; outer binding restored.
	sh, ok := c.lookupShape("x")
	require.True(t, ok)
	assert.Equal(t, OriginScalar, sh.Kind)
}

func TestCheck_DefinedThenUsed_Clean(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "let p = \"hello\"\nprint $p")
	assert.Empty(t, issues)
}

func TestCheck_UseBeforeDefIsReported(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "print $porg")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "undefined variable: porg")
}

func TestCheck_LetRHSCheckedBeforeBinding(t *testing.T) {
	t.Parallel()

	// 'let x = $x' on a previously-undefined x must report
	// the RHS reference rather than letting the new binding
	// silently shadow the lookup.
	issues := checkSource(t, "let x = $x")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "undefined variable: x")
}

func TestCheck_BindCollect_TupleBindOnPureBuiltinRejected(t *testing.T) {
	t.Parallel()

	// jq is a pure builtin: it returns a value directly, with
	// no rc envelope. A bind-collect that uses jq as its
	// producer and asks for a tuple bind would silently
	// accumulate synthetic OK envelopes into the rc slot. The
	// checker rejects this shape and points at the single-bind
	// alternative.
	src := "let xs = [10 20 30]\nlet (rc doubled) <- foreach n in $xs { jq \". * 2\" $n }"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "jq is a pure builtin")
	assert.Contains(t, issues[0].Msg, "tuple bind")
	assert.Contains(t, issues[0].Msg, "single-bind")
}

func TestCheck_BindCollect_SingleBindOnPureBuiltinAccepted(t *testing.T) {
	t.Parallel()

	// The single-bind shape is correct for a pure-builtin
	// producer: the primary slot carries the per-element
	// value list; there is no rc list to fabricate.
	src := "let xs = [10 20 30]\nlet doubled <- foreach n in $xs { jq \". * 2\" $n }"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_BindStmtDefinesPrimaryAndRc(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "let (rc p) <- bpfman program list\nprint $p $rc")
	assert.Empty(t, issues)
}

func TestCheck_BindStmtDiscardSlotDoesNotDefine(t *testing.T) {
	t.Parallel()

	// '_' as a target name discards. Subsequent '$_'
	// reference is undefined.
	issues := checkSource(t, "let _ <- bpfman program list\nprint $_")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "undefined variable: _")
}

func TestCheck_ForEachVarVisibleInBody(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "let xs = \"a\"\nforeach x in $xs { print $x }")
	assert.Empty(t, issues)
}

func TestCheck_ForEachVarNotVisibleAfterBody(t *testing.T) {
	t.Parallel()

	src := strings.Join([]string{
		"let xs = \"a\"",
		"foreach x in $xs { print $x }",
		"print $x",
	}, "\n")
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "undefined variable: x")
}

func TestCheck_DefParamsVisibleInBody(t *testing.T) {
	t.Parallel()

	src := "def greet(name) { print $name }"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_DefParamsNotVisibleAfterBody(t *testing.T) {
	t.Parallel()

	src := strings.Join([]string{
		"def greet(name) { print $name }",
		"print $name",
	}, "\n")
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "undefined variable: name")
}

func TestCheck_MultipleIssuesAccumulate(t *testing.T) {
	t.Parallel()

	src := strings.Join([]string{
		"print $a",
		"print $b",
	}, "\n")
	issues := checkSource(t, src)
	require.Len(t, issues, 2)
	assert.Contains(t, issues[0].Msg, "a")
	assert.Contains(t, issues[1].Msg, "b")
}

func TestCheck_DotPathOnDefinedNameIsClean(t *testing.T) {
	t.Parallel()

	// Field access on a defined name reports nothing; the
	// path is checked structurally by the parser, not by
	// the static checker.
	src := "let p <- bpfman program list\nprint $p.id"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_InsideInterpolation(t *testing.T) {
	t.Parallel()

	// '${$missing}' refers to an undefined variable inside
	// the interpolation. The check descends through
	// InterpStringExpr's Segments via Inspect.
	src := "print \"${$missing}\""
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "undefined variable: missing")
}

func TestCheck_LeakedJobIsReported(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 60"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, `started job "p" has no matching wait or kill`)
}

func TestCheck_GuardLeakedJobIsReported(t *testing.T) {
	t.Parallel()

	src := "guard p <- start sleep 60"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, `started job "p" has no matching wait or kill`)
}

func TestCheck_WaitedJobIsClean(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 1\nwait $p"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_KilledJobIsClean(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 60\nkill $p"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_DeferKilledJobIsClean(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 60\ndefer kill $p"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_KillWithSignalFlagIsClean(t *testing.T) {
	t.Parallel()

	// 'kill --signal=USR1 $p' should still match $p as the
	// target. Flag args (starting with '--') are skipped.
	src := "let p <- start sleep 60\nkill --signal=USR1 $p"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_DiscardedJobIsNotChecked(t *testing.T) {
	t.Parallel()

	// 'let _ <- start ...' discards the handle; the start
	// itself is fire-and-forget, no managed lifecycle to
	// expect. We treat that as user-acknowledged and do not
	// report a leak.
	src := "let _ <- start sleep 60"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_LeakReportedAtStartSite(t *testing.T) {
	t.Parallel()

	src := "let x = 1\n\nlet p <- start sleep 60"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Equal(t, 3, issues[0].Pos.Line, "leak should be cited at the start site, not elsewhere")
}

func TestCheck_TupleBindOnStartReportsPrimary(t *testing.T) {
	t.Parallel()

	// 'let (rc p) <- start ...' creates a job named p; rc
	// is the result envelope, not the job handle.
	src := "let (rc p) <- start sleep 60"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, `started job "p"`)
}

func TestCheck_ArithmeticOnNumericLiteralsClean(t *testing.T) {
	t.Parallel()

	src := "let r = 4 * 2 + 1"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_ArithmeticOnFloatLiteralsClean(t *testing.T) {
	t.Parallel()

	src := "let r = 1.5 * 2"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_ArithmeticOnNonNumericLiteralFlagged(t *testing.T) {
	t.Parallel()

	src := "let r = 4 * bogus"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, `arithmetic *: operand "bogus" is not numeric`)
}

func TestCheck_ArithmeticBothNonNumericReported(t *testing.T) {
	t.Parallel()

	src := "let r = A / B"
	issues := checkSource(t, src)
	require.Len(t, issues, 2)
	assert.Contains(t, issues[0].Msg, `operand "A"`)
	assert.Contains(t, issues[1].Msg, `operand "B"`)
}

func TestCheck_ArithmeticVarRefIsTrusted(t *testing.T) {
	t.Parallel()

	// Variable-reference operands are not flagged; we cannot
	// know their value at static time. The undefined-variable
	// check still catches the case where the name is unbound
	// (covered by TestCheck_UseBeforeDefIsReported).
	src := "let n = 4\nlet r = $n * 2"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_ArithmeticInsideInterpolationFlagged(t *testing.T) {
	t.Parallel()

	// The interpolation case the user surfaced: '${4 * Z}'
	// reaches the arithmetic check via Inspect descending
	// through the InterpStringExpr's segments.
	src := `print "${4 * Z}"`
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, `operand "Z"`)
}

func TestCheck_ArithmeticHexLiteralAccepted(t *testing.T) {
	t.Parallel()

	// '0x1a + 1' should not flag: ParseInt with base 0
	// accepts hex prefixes, matching the runtime's de facto
	// acceptance of hex numeric literals.
	src := "let r = 0x1a + 1"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_BreakInsideForeachIsClean(t *testing.T) {
	t.Parallel()

	src := "let xs = \"a\"\nforeach x in $xs { break }"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_BreakOutsideForeachReported(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "break")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "'break' outside any foreach loop")
}

func TestCheck_ContinueOutsideForeachReported(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "continue")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "'continue' outside any foreach loop")
}

func TestCheck_BreakInsideEventuallyReported(t *testing.T) {
	t.Parallel()

	// Eventually is not a foreach loop for break/continue
	// purposes: the runtime errBreak only escapes through
	// ForEachStmt's evaluator, and the language treats
	// eventually as a retrying assertion block rather than an
	// iteration construct. A bare `break` inside the body is
	// caught here so the user does not need to reach the
	// runtime to discover the mismatch.
	src := "eventually timeout 1s { break }"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "'break'")
}

func TestCheck_BreakInsideDefBodyResetsDepth(t *testing.T) {
	t.Parallel()

	// A def body resets the loop depth: a 'break' inside
	// the body but not inside a foreach within the body is
	// flagged even if the def is later called from inside a
	// foreach in the caller.
	src := "def f() { break }"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "'break'")
}

func TestCheck_StartWithoutCommandReported(t *testing.T) {
	t.Parallel()

	// 'let p <- start' triggers both the arity check and
	// the job-leak check (the bound 'p' has no matching
	// wait or kill); assert the arity message is present
	// without constraining the total count, since both are
	// legitimate findings.
	issues := checkSource(t, "let p <- start")
	var msgs []string
	for _, i := range issues {
		msgs = append(msgs, i.Msg)
	}
	assert.Contains(t, strings.Join(msgs, " | "), "start: expected at least 1")
}

func TestCheck_WaitWithoutJobReported(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "wait")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "wait: expected at least 1")
}

func TestCheck_KillWithoutJobReported(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "kill")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "kill: expected at least 1")
}

func TestCheck_KillFlagsAreNotCountedAsArgs(t *testing.T) {
	t.Parallel()

	// 'kill --signal=USR1' has one arg textually but zero
	// non-flag args; the arity check must report it as
	// missing the $job target.
	issues := checkSource(t, "kill --signal=USR1")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "kill: expected at least 1")
}

func TestCheck_JobsTakesNoArgs(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "jobs extra")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "jobs: expected at most 0")
}

func TestCheck_ReapTakesNoArgs(t *testing.T) {
	t.Parallel()

	issues := checkSource(t, "reap extra")
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "reap: expected at most 0")
}

func TestCheck_KillSignalKnownNamesClean(t *testing.T) {
	t.Parallel()

	// Each accepted spelling: bare, SIG-prefixed, and
	// lowercase. The static check mirrors the runtime's
	// acceptance.
	cases := []string{
		"let p <- start sleep 60\nkill --signal=USR1 $p\nwait $p",
		"let p <- start sleep 60\nkill --signal=SIGUSR1 $p\nwait $p",
		"let p <- start sleep 60\nkill --signal=usr1 $p\nwait $p",
		"let p <- start sleep 60\nkill --signal=TERM $p\nwait $p",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			issues := checkSource(t, src)
			assert.Empty(t, issues, "src=%q", src)
		})
	}
}

func TestCheck_KillSignalUnknownReported(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 60\nkill --signal=BLAH $p\nwait $p"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, `unknown signal "BLAH"`)
}

func TestCheck_KillGraceValidDurationsClean(t *testing.T) {
	t.Parallel()

	cases := []string{
		"let p <- start sleep 60\nkill --grace=2s $p\nwait $p",
		"let p <- start sleep 60\nkill --grace=500ms $p\nwait $p",
		"let p <- start sleep 60\nkill --grace=0 $p\nwait $p",
		"let p <- start sleep 60\nkill --grace=1m30s $p\nwait $p",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			issues := checkSource(t, src)
			assert.Empty(t, issues, "src=%q", src)
		})
	}
}

func TestCheck_KillGraceMalformedReported(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 60\nkill --grace=banana $p\nwait $p"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "kill --grace:")
}

func TestCheck_KindFieldAccess_JobBadField(t *testing.T) {
	t.Parallel()

	// 'start' produces a Job whose only sealed field is 'pid'.
	// Any other field name on $p is statically detectable.
	src := "let p <- start sleep 60\nprint $p.pidd\nkill $p"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "p has kind job")
	assert.Contains(t, issues[0].Msg, `"pidd"`)
	assert.Contains(t, issues[0].Msg, "valid: pid")
}

func TestCheck_KindFieldAccess_JobValidField(t *testing.T) {
	t.Parallel()

	src := "let p <- start sleep 60\nprint $p.pid\nkill $p"
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_KindFieldAccess_EnvelopeBadField(t *testing.T) {
	t.Parallel()

	// An external command (here 'which') binds through the
	// envelope path, so $bpftool.exit_code is statically
	// detectable as a typo for $bpftool.code.
	src := `let bpftool <- which bpftool
if $bpftool.exit_code == 0 {
    print "found"
}`
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "bpftool has kind result")
	assert.Contains(t, issues[0].Msg, `"exit_code"`)
	assert.Contains(t, issues[0].Msg, "code")
}

func TestCheck_KindFieldAccess_ScalarRejectsAnyField(t *testing.T) {
	t.Parallel()

	src := "let n = 42\nprint $n.value"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "n has kind scalar")
	assert.Contains(t, issues[0].Msg, "field access is not valid")
}

func TestCheck_KindFieldAccess_BoolRejectsAnyField(t *testing.T) {
	t.Parallel()

	src := "let flag = true\nprint $flag.value"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "flag has kind boolean")
}

func TestCheck_KindFieldAccess_UnknownKindIsPermissive(t *testing.T) {
	t.Parallel()

	// 'jq' returns OriginUnknown so any field access is
	// allowed. Same for 'bpfman' subcommands and 'file'
	// whose shapes are not yet enumerated. jq is a pure
	// builtin so the expression-position '=' form is the
	// only legal call shape.
	src := `let data = jq "." '{"x":1}'
print $data.anything.we.want`
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_KindFieldAccess_LetCopyKindFromVarRef(t *testing.T) {
	t.Parallel()

	// 'let q = $p' copies p's inferred kind onto q, so
	// $q.field is checked the same way $p.field would be.
	src := "let p <- start sleep 60\nlet q = $p\nprint $q.pidd\nkill $p"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "q has kind job")
}

func TestCheck_KindFieldAccess_TupleRcIsEnvelope(t *testing.T) {
	t.Parallel()

	// The rc slot of a tuple bind is always an envelope, so
	// $rc.exit_code (the typo) gets the same treatment as
	// $bpftool.exit_code did.
	src := `let (rc prog) <- bpfman program get 42
if $rc.exit_code == 0 {
    print $prog
}`
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "rc has kind result")
	assert.Contains(t, issues[0].Msg, `"exit_code"`)
}

func TestCheck_KindFieldAccess_DidYouMeanSuggestion(t *testing.T) {
	t.Parallel()

	// A near-miss field name produces a suggestion derived
	// through internal/strdist's nearest-string ranker.
	src := "let p <- start sleep 60\nprint $p.pidd\nkill $p"
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "did you mean")
	assert.Contains(t, issues[0].Msg, `"pid"`)
}

func TestCheck_KindFieldAccess_NestedKindPropagation(t *testing.T) {
	t.Parallel()

	// 'let q = $r.code' inherits Scalar from the result's
	// code field, so $q.field on q reports the scalar
	// constraint rather than silently passing.
	src := `let r <- exec ls
let q = $r.code
print $q.field`
	issues := checkSource(t, src)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Msg, "q has kind scalar")
}

func TestCheck_KindFieldAccess_UnknownBindIsPermissive(t *testing.T) {
	t.Parallel()

	// jq returns a value the static checker has no shape for,
	// so $data.anything.deep is permitted: the alternative is
	// false positives on every dynamic structure. jq is a pure
	// builtin invoked in expression position.
	src := `let data = jq "." '{"x":{"y":1}}'
print $data.x.y.z`
	issues := checkSource(t, src)
	assert.Empty(t, issues)
}

func TestCheck_ForEachScopeRestoresShape(t *testing.T) {
	t.Parallel()

	// 'let x = 5' defines x as a numeric scalar. A foreach
	// that reuses 'x' as the loop variable must not leak the
	// loop's per-iteration shape past the body. After the
	// loop, the original 'let' shape stays in effect.
	src := `let x = 5
foreach x in $list {
    print $x
}
print "${4 * $x}"`
	issues := checkSource(t, src)
	// $list is undefined -> one issue. The trailing "${4 * $x}"
	// arithmetic must not flag $x as non-numeric: x's outer
	// shape (Scalar with literal RHS "5") was restored.
	for _, iss := range issues {
		assert.NotContains(t, iss.Msg, "x is",
			"foreach must restore x's outer shape on exit")
	}
}
