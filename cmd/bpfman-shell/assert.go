// Assertion verbs for the REPL: "assert" and "require".  Both
// share a single dispatch path (replAssertRequire) with an
// isRequire flag deciding whether failure increments the session
// counter (assert) or halts execution via repl.ErrRequireFailed
// (require).  All the verbs, plus the expression-assertion path
// for predicate/comparison forms ("$a == $b"), live here.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/repl"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell"
	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
)

// assertResult holds the outcome of evaluating an assertion verb.
type assertResult struct {
	pass    bool
	message string
}

// makeExecAssertStmt returns the Env.ExecAssertStmt callback used
// by the expression-form path. It evaluates the AssertStmt's
// expression, applies AsBool and the optional negation, and routes
// pass/fail through the same printing, counter, and halt-on-require
// machinery the verb-form path uses. The returned function closes
// over the CLI, session, and source-location prefix so the caller
// does not need to thread them through Env explicitly.
func makeExecAssertStmt(cli *bpfmancli.CLI, session *shell.Session, loc sourceLoc) func(*shell.AssertStmt, *shell.Env) error {
	return func(s *shell.AssertStmt, env *shell.Env) error {
		v, err := shell.EvalExpr(s.Expr, env)
		if err != nil {
			return err
		}
		pass, err := shell.AsBool(v)
		if err != nil {
			return err
		}
		if pass {
			return nil
		}
		label := "assert"
		if s.IsRequire {
			label = "require"
		}
		message := formatExprFailure(s.Expr, session)
		_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, message)
		if s.IsRequire {
			return repl.ErrRequireFailed
		}
		session.RecordAssertFailure()
		return nil
	}
}

// replAssertRequire handles both "assert" and "require" commands.
// When isRequire is true, failure halts execution immediately via
// repl.ErrRequireFailed. When false, failure is recorded in the session
// counter and execution continues.
func replAssertRequire(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, isRequire bool, loc sourceLoc) error {
	if len(args) == 0 {
		return fmt.Errorf("expected an assertion (e.g. \"$a == $b\", \"true $flag\", \"ok exec ...\")")
	}

	label := "assert"
	if isRequire {
		label = "require"
	}

	// Check for "not" negation.
	negate := false
	if repl.ArgText(args[0]) == "not" {
		negate = true
		args = args[1:]
		if len(args) == 0 {
			return fmt.Errorf("expected a verb after \"not\"")
		}
	}

	// Matches form: <target> followed by a parsed `matches { ... }`
	// block. The shell parser strips the bare "matches" keyword and
	// produces a single MatchesBlockArg; arity is exactly two
	// (target plus block).
	if len(args) == 2 {
		if block, ok := args[1].(shell.MatchesBlockArg); ok {
			if negate {
				return fmt.Errorf("\"not\" is not supported with the matches form")
			}
			result, err := evalAssertMatches(args[0], block, loc)
			if err != nil {
				return err
			}
			if result.pass {
				return nil
			}
			_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, result.message)
			if isRequire {
				return repl.ErrRequireFailed
			}
			session.RecordAssertFailure()
			return nil
		}
	}

	// Value-based assertion: binary comparison or unary predicate.
	// These route through the expression grammar. "not" is legal
	// before unary predicates but banned before binary comparisons
	// (use the complementary operator instead).
	if isExprAssertion(args) {
		if negate && len(args) == 3 {
			return fmt.Errorf("\"not\" is not supported with infix comparisons; use the complementary operator (!=, <=, >=)")
		}
		result, err := evalExprAssertion(session, args)
		if err != nil {
			return err
		}
		if negate {
			result.pass = !result.pass
			result.message = negateMessage(result.message)
		}
		if result.pass {
			return nil
		}
		_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, result.message)
		if isRequire {
			return repl.ErrRequireFailed
		}
		session.RecordAssertFailure()
		return nil
	}

	// Prefix verb dispatch (command assertions and remaining special
	// verbs: ok, fail, path, contains).
	verbArg := args[0]
	verb := repl.ArgText(verbArg)
	verbArgs := args[1:]

	result, err := evalAssertVerb(ctx, cli, mgr, session, verbArg, verb, verbArgs)
	if err != nil {
		return err
	}

	if negate {
		result.pass = !result.pass
		result.message = negateMessage(result.message)
	}

	if result.pass {
		return nil
	}

	// Failure path.
	_ = cli.PrintErrf("%s[%s] FAIL: %s\n", loc, label, result.message)

	if isRequire {
		return repl.ErrRequireFailed
	}

	session.RecordAssertFailure()
	return nil
}

// isExprAssertion reports whether args matches the shape of a
// value-based assertion that should be routed through the expression
// grammar: a single bool-shaped operand, [pred operand] with a
// unary predicate, or [lhs op rhs] with a binary operator. The
// single-arg form covers "assert $flag", "assert true", and any
// parenthesised compound expression that already produces a boolean.
// Bare prefix-verb names (e.g. "assert ok" with no command) fall
// through to verb dispatch so the user gets a helpful arity error
// rather than an opaque "not a boolean" diagnosis.
func isExprAssertion(args []shell.Arg) bool {
	switch len(args) {
	case 1:
		return !isPrefixVerbName(repl.ArgText(args[0]))
	case 2:
		return shell.IsUnaryPred(repl.ArgText(args[0]))
	case 3:
		return shell.IsBinaryOp(repl.ArgText(args[1]))
	}
	return false
}

// isPrefixVerbName reports whether s names one of the prefix-verb
// assertions handled by evalAssertVerb. Used by isExprAssertion's
// single-arg branch to keep "assert ok" (and friends, with the
// command argument forgotten) on the verb-dispatch path so the
// user sees the verb's own arity error.
func isPrefixVerbName(s string) bool {
	switch s {
	case "ok", "fail", "path-exists", "contains", "nil", "present", "missing", "empty":
		return true
	}
	return false
}

// evalExprAssertion rebuilds an expression from the evaluated args,
// evaluates it, and wraps the boolean result with an
// assertion-appropriate failure message that describes the operands
// involved.
func evalExprAssertion(session *shell.Session, args []shell.Arg) (assertResult, error) {
	expr, err := shell.ExprFromArgs(args)
	if err != nil {
		return assertResult{}, err
	}
	env := &shell.Env{Session: session}
	v, err := shell.EvalExpr(expr, env)
	if err != nil {
		return assertResult{}, err
	}
	pass, err := shell.AsBool(v)
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    pass,
		message: formatExprFailure(expr, session),
	}, nil
}

// formatExprFailure produces an assertion failure message describing
// the expression and its operand values. Evaluation errors in the
// operands surface as-is; they should not occur here because Eval
// already succeeded on the top-level expression.
func formatExprFailure(e shell.Expr, session *shell.Session) string {
	switch x := e.(type) {
	case *shell.BinaryExpr:
		left := exprScalar(x.Left, session)
		right := exprScalar(x.Right, session)
		switch x.Op {
		case "==":
			return fmt.Sprintf("expected %q to equal %q", left, right)
		case "!=":
			return fmt.Sprintf("expected %q to not equal %q", left, right)
		default:
			return fmt.Sprintf("expected %s %s %s", left, x.Op, right)
		}
	case *shell.UnaryExpr:
		operand := exprScalar(x.Operand, session)
		switch x.Pred {
		case "nil":
			return fmt.Sprintf("expected %s to be nil", operand)
		case "not-empty":
			return fmt.Sprintf("expected non-empty string, got %q", operand)
		default:
			return fmt.Sprintf("expected predicate %s to hold on %s", x.Pred, operand)
		}
	}
	return fmt.Sprintf("expected %s to be true", exprScalar(e, session))
}

// exprScalar is a best-effort scalar stringification of an expression
// for inclusion in error messages. Non-scalar values render as their
// kind; evaluation errors render as "<err>". The Env has no
// substitution runner, so any CmdSubExpr reached here would error —
// this helper is only called on operand sub-expressions that have
// already been evaluated once via EvalExpr at the top level.
func exprScalar(e shell.Expr, session *shell.Session) string {
	v, err := shell.EvalExpr(e, &shell.Env{Session: session})
	if err != nil {
		return "<err>"
	}
	s, err := v.Scalar()
	if err != nil {
		return "<" + v.Kind().String() + ">"
	}
	return s
}

// evalAssertVerb dispatches to the prefix verb evaluators that are
// not part of the expression grammar: command status checks (ok,
// fail), filesystem checks (path), and string containment
// (contains). Value-based comparisons and unary predicates go
// through the expression path (see evalExprAssertion).
func evalAssertVerb(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, verbArg shell.Arg, verb string, args []shell.Arg) (assertResult, error) {
	ss := repl.ArgTexts(args)
	verbSpan := shell.ArgSpan(verbArg)
	switch verb {
	case "ok":
		return assertOk(ctx, cli, mgr, session, verbSpan, args)
	case "fail":
		return assertFail(ctx, cli, mgr, session, verbSpan, args)
	case "path-exists":
		return assertPathExists(verbSpan, ss)
	case "contains":
		return assertContains(verbSpan, ss)
	case "nil":
		return assertNil(session, verbSpan, args)
	case "present":
		return assertPresent(session, verbSpan, args)
	case "missing":
		return assertMissing(session, verbSpan, args)
	case "empty":
		return assertEmpty(session, verbSpan, args)
	case "==", "!=", "<", "<=", ">", ">=":
		return assertResult{}, shell.SpanErrorf(verbSpan,
			"%q goes between two values: try 'assert <left> %s <right>'", verb, verb)
	case "not-empty":
		return assertResult{}, shell.SpanErrorf(verbSpan,
			"%q takes one value: try 'assert %s $name'", verb, verb)
	default:
		return assertResult{}, shell.SpanErrorf(verbSpan,
			"unknown assertion verb %q", verb)
	}
}

// classifyAssertOperand walks the single value operand of the
// present / missing / nil / empty predicates and returns the
// classification needed by each. Three input shapes are accepted:
//
//   - WordArg: a bareword variable name with optional dotted path
//     (e.g. `prog.status.stats`). Soft-looked up against the
//     session.
//   - NilArg: a $-prefixed variable expression that resolved to
//     JSON null at the arg boundary. The classification is
//     LookupNull.
//   - MissingArg: a $-prefixed variable expression whose path is
//     absent from the value tree. The classification is
//     LookupAbsent.
//   - Any other resolved Arg variant: the path resolved to a
//     non-null value. The classification is LookupPresent and
//     the underlying value is recovered when meaningful.
//
// The returned displayName is a human-readable rendition of the
// operand for diagnostic messages ("prog.status.stats",
// "$got.status.links").
func classifyAssertOperand(session *shell.Session, a shell.Arg) (shell.Value, shell.LookupClass, string, error) {
	switch v := a.(type) {
	case shell.WordArg:
		val, class, err := lookupBareVarSoft(session, v.Text)
		if err != nil {
			return shell.Value{}, shell.LookupAbsent, v.Text, err
		}
		return val, class, v.Text, nil
	case shell.NilArg:
		return shell.Value{}, shell.LookupNull, "<null>", nil
	case shell.MissingArg:
		display := "$" + v.Name
		if v.Path != "" {
			display += "." + v.Path
		}
		return shell.Value{}, shell.LookupAbsent, display, nil
	case shell.ScalarValueArg:
		return shell.StringValue(v.Text), shell.LookupPresent, v.Text, nil
	case shell.StructuredValueArg:
		display := "$" + v.Name
		return v.Value, shell.LookupPresent, display, nil
	case shell.QuotedArg:
		return shell.StringValue(v.Text), shell.LookupPresent, "\"" + v.Text + "\"", nil
	case shell.AdapterArg:
		display := v.Adapter + ":$" + v.Name
		if v.Path != "" {
			display += "." + v.Path
		}
		return v.Value, shell.LookupPresent, display, nil
	default:
		return shell.Value{}, shell.LookupAbsent, "", fmt.Errorf("unsupported argument %T", a)
	}
}

// lookupBareVarSoft is the soft-lookup variant of lookupBareVar.
// Returns a LookupClass alongside the value so the predicate
// handlers can give precise answers for missing / null / present.
func lookupBareVarSoft(session *shell.Session, arg string) (shell.Value, shell.LookupClass, error) {
	varName := arg
	path := ""
	if i := strings.IndexAny(arg, ".["); i >= 0 {
		varName = arg[:i]
		path = arg[i:]
		path = strings.TrimPrefix(path, ".")
	}
	v, ok := session.Get(varName)
	if !ok {
		return shell.Value{}, shell.LookupAbsent, nil
	}
	return v.LookupSoft(varName, path)
}

// assertNil checks whether the operand resolves to JSON null
// (strict). An operand that is absent from the value tree fails;
// use `missing` to assert absence explicitly.
func assertNil(session *shell.Session, verbSpan shell.Span, args []shell.Arg) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "nil requires exactly 1 argument (a value expression or bare variable name)")
	}
	_, class, display, err := classifyAssertOperand(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    class == shell.LookupNull,
		message: fmt.Sprintf("expected %s to be null", display),
	}, nil
}

// assertPresent succeeds when the operand resolves to a value or
// JSON null. Fails only when the path is absent from the value
// tree.
func assertPresent(session *shell.Session, verbSpan shell.Span, args []shell.Arg) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "present requires exactly 1 argument (a value expression or bare variable name)")
	}
	_, class, display, err := classifyAssertOperand(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    class != shell.LookupAbsent,
		message: fmt.Sprintf("expected %s to be present", display),
	}, nil
}

// assertMissing is the inverse of assertPresent: succeeds only
// when the operand's path is absent from the value tree. A null
// terminal value fails because the field exists in the shape.
func assertMissing(session *shell.Session, verbSpan shell.Span, args []shell.Arg) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "missing requires exactly 1 argument (a value expression or bare variable name)")
	}
	_, class, display, err := classifyAssertOperand(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    class == shell.LookupAbsent,
		message: fmt.Sprintf("expected %s to be missing from the shape", display),
	}, nil
}

// assertEmpty succeeds when the operand resolves to an empty
// string, empty list, or empty map. Absent paths and null
// terminals fail: emptiness is a positive shape claim and is
// distinct from the field not existing or being explicitly null.
func assertEmpty(session *shell.Session, verbSpan shell.Span, args []shell.Arg) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "empty requires exactly 1 argument (a value expression or bare variable name)")
	}
	val, class, display, err := classifyAssertOperand(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	if class != shell.LookupPresent {
		return assertResult{
			pass:    false,
			message: fmt.Sprintf("expected %s to be empty (\"\" / [] / {})", display),
		}, nil
	}
	pass := false
	switch x := val.Raw().(type) {
	case string:
		pass = x == ""
	case []any:
		pass = len(x) == 0
	case map[string]any:
		pass = len(x) == 0
	}
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %s to be empty (\"\" / [] / {})", display),
	}, nil
}

// runCommand executes a command through both the shell command layer
// and the domain dispatch layer. It is used by assertion verbs (ok,
// fail) to test whether a sub-command succeeds or fails.
func runCommand(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) error {
	handled, _, err := repl.Dispatch(ctx, cli, mgr, session, nil, args, repl.SourceLoc{}, shell.Span{})
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	_, err = replDispatch(ctx, cli, mgr, args)
	return err
}

func assertOk(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, verbSpan shell.Span, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "ok requires a command")
	}
	err := runCommand(ctx, cli.WithDiscardOutput(), mgr, session, args)
	if err != nil {
		return assertResult{
			pass:    false,
			message: fmt.Sprintf("expected command to succeed, but got: %v", err),
		}, nil
	}
	return assertResult{
		pass:    true,
		message: "expected command to succeed",
	}, nil
}

func assertFail(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, verbSpan shell.Span, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "fail requires a command")
	}
	err := runCommand(ctx, cli.WithDiscardOutput(), mgr, session, args)
	if err != nil {
		return assertResult{
			pass:    true,
			message: "expected command to fail",
		}, nil
	}
	return assertResult{
		pass:    false,
		message: "expected command to fail, but it succeeded",
	}, nil
}

// assertPathExists tests filesystem-path existence (the
// previous `assert path exists FILE` two-arg form, renamed and
// collapsed to a single argument). Reserves the `path` word for
// object-path semantics if we ever revive it; the two notions of
// path (filesystem vs value-tree) deserve distinct vocabulary.
func assertPathExists(verbSpan shell.Span, args []string) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "path-exists requires exactly 1 argument: <filepath>")
	}
	_, err := os.Stat(args[0])
	pass := err == nil
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected path %q to exist", args[0]),
	}, nil
}

func assertContains(verbSpan shell.Span, args []string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, shell.SpanErrorf(verbSpan, "contains requires exactly 2 arguments: <haystack> <needle>")
	}
	pass := strings.Contains(args[0], args[1])
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected %q to contain %q", args[0], args[1]),
	}, nil
}

// evalAssertMatches implements `assert <target> matches { ... }`
// with subset-match semantics: each entry is checked individually
// and all mismatches are collected so the failure message reports
// every diverging path in one go. Extra fields in the actual record
// are ignored — the entry list is the entire contract.
func evalAssertMatches(target shell.Arg, block shell.MatchesBlockArg, base sourceLoc) (assertResult, error) {
	sva, ok := target.(shell.StructuredValueArg)
	if !ok {
		return assertResult{}, fmt.Errorf("matches requires a structured value as the target (got %s)", repl.ArgText(target))
	}
	if len(block.Entries) == 0 {
		return assertResult{}, fmt.Errorf("matches block must contain at least one entry")
	}

	// locate prefixes a path message with the entry's source
	// location so multi-mismatch failures point at the specific
	// offending line inside the block. The shell.Pos carried by
	// each entry is relative to the accumulated REPL chunk; when
	// the assert statement has a known file/start-line, translate
	// the chunk-local line into an absolute file line so the
	// diagnostic agrees with the rest of the REPL's "file:line:"
	// convention.
	locate := func(loc shell.Pos, msg string) string {
		if loc.Line == 0 {
			return msg
		}
		if base.File != "" && base.Line > 0 {
			absLine := base.Line + loc.Line - 1
			return fmt.Sprintf("%s:%d:%d: %s", base.File, absLine, loc.Col, msg)
		}
		return fmt.Sprintf("%d:%d: %s", loc.Line, loc.Col, msg)
	}

	var mismatches []string
	for _, entry := range block.Entries {
		actual, err := sva.Value.LookupValue(sva.Name, entry.Path)
		if err != nil {
			mismatches = append(mismatches, locate(entry.Pos, fmt.Sprintf("%s: %v", entry.Path, err)))
			continue
		}
		if entry.NotEmpty {
			s, err := actual.Scalar()
			if err != nil {
				mismatches = append(mismatches, locate(entry.Pos, fmt.Sprintf("%s: expected non-empty scalar, got %s", entry.Path, actual.Kind())))
				continue
			}
			if s == "" {
				mismatches = append(mismatches, locate(entry.Pos, fmt.Sprintf("%s: expected non-empty, got \"\"", entry.Path)))
			}
			continue
		}
		actualS, err := actual.Scalar()
		if err != nil {
			mismatches = append(mismatches, locate(entry.Pos, fmt.Sprintf("%s: expected scalar value, got %s", entry.Path, actual.Kind())))
			continue
		}
		expected, err := entry.Value.Scalar()
		if err != nil {
			return assertResult{}, fmt.Errorf("matches entry %q: pattern is not a scalar value", entry.Path)
		}
		if actualS != expected {
			mismatches = append(mismatches, locate(entry.Pos, fmt.Sprintf("%s: expected %q, got %q", entry.Path, expected, actualS)))
		}
	}
	if len(mismatches) == 0 {
		return assertResult{pass: true, message: "matches block held"}, nil
	}
	noun := "mismatch"
	if len(mismatches) > 1 {
		noun = "mismatches"
	}
	return assertResult{
		pass:    false,
		message: fmt.Sprintf("matches: %d %s\n  %s", len(mismatches), noun, strings.Join(mismatches, "\n  ")),
	}, nil
}

// negateMessage transforms an assertion message for negated assertions.
// It inserts "not" into the message: "expected X to equal Y" becomes
// "expected X not to equal Y", "expected X to be Y" becomes
// "expected X not to be Y".
func negateMessage(msg string) string {
	// Try "to equal", "to not equal", "to be", "to contain", "to exist", "to succeed", "to fail".
	if i := strings.Index(msg, " to "); i >= 0 {
		return msg[:i] + " not to " + msg[i+4:]
	}
	// Try "expected command to" patterns.
	if strings.HasPrefix(msg, "expected") {
		return "expected not: " + msg[9:]
	}
	return "not: " + msg
}
