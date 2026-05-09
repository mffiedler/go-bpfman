// Assertion verbs for the REPL: "assert" and "require".  Both
// share a single dispatch path (replAssertRequire) with an
// isRequire flag deciding whether failure increments the session
// counter (assert) or halts execution via errRequireFailed
// (require).  All the verbs, plus the expression-assertion path
// for predicate/comparison forms ("$a == $b"), live here.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/frobware/go-bpfman/internal/bpfmancli"
	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/shell"
)

// assertResult holds the outcome of evaluating an assertion verb.
type assertResult struct {
	pass    bool
	message string
}

// makeExecAssertStmt returns the Env.ExecAssertStmt callback used
// by the new expression-form path. It evaluates the AssertStmt's
// expression, applies AsBool and the optional negation, and routes
// pass/fail through the same printing, counter, and halt-on-require
// machinery the legacy verb-form path uses. The returned function
// closes over the CLI, session, and source-location prefix so the
// caller does not need to thread them through Env explicitly.
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
			return errRequireFailed
		}
		session.RecordAssertFailure()
		return nil
	}
}

// replAssertRequire handles both "assert" and "require" commands.
// When isRequire is true, failure halts execution immediately via
// errRequireFailed. When false, failure is recorded in the session
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
	if argText(args[0]) == "not" {
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
				return errRequireFailed
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
			return errRequireFailed
		}
		session.RecordAssertFailure()
		return nil
	}

	// Prefix verb dispatch (command assertions and remaining special
	// verbs: ok, fail, path, contains).
	verb := argText(args[0])
	verbArgs := args[1:]

	result, err := evalAssertVerb(ctx, cli, mgr, session, verb, verbArgs)
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
		return errRequireFailed
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
		return !isPrefixVerbName(argText(args[0]))
	case 2:
		return shell.IsUnaryPred(argText(args[0]))
	case 3:
		return shell.IsBinaryOp(argText(args[1]))
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
	case "ok", "fail", "path", "contains", "nil":
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
func evalAssertVerb(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, verb string, args []shell.Arg) (assertResult, error) {
	ss := argTexts(args)
	switch verb {
	case "ok":
		return assertOk(ctx, cli, mgr, session, args)
	case "fail":
		return assertFail(ctx, cli, mgr, session, args)
	case "path":
		return assertPath(ss)
	case "contains":
		return assertContains(ss)
	case "nil":
		return assertNil(session, ss)
	case "==", "!=", "<", "<=", ">", ">=":
		return assertResult{}, fmt.Errorf("%q is not a prefix verb; use infix form: assert <left> %s <right>", verb, verb)
	case "not-empty":
		return assertResult{}, fmt.Errorf("%q requires exactly one operand: assert %s <operand>", verb, verb)
	default:
		return assertResult{}, fmt.Errorf("unknown assertion verb %q", verb)
	}
}

// assertNil checks whether a variable holds a nil Value. The operand
// is a bare variable name, not a value expression: the runtime
// Session can hold nil values but variable expansion refuses to
// carry them through, so the only way to inspect nil-ness is by
// name.
func assertNil(session *shell.Session, args []string) (assertResult, error) {
	if len(args) != 1 {
		return assertResult{}, fmt.Errorf("nil requires exactly 1 argument (bare variable name, no $)")
	}
	v, err := lookupBareVar(session, args[0])
	if err != nil {
		return assertResult{}, err
	}
	return assertResult{
		pass:    v.IsNil(),
		message: fmt.Sprintf("expected %s to be nil", args[0]),
	}, nil
}

// runCommand executes a command through both the shell command layer
// and the domain dispatch layer. It is used by assertion verbs (ok,
// fail) to test whether a sub-command succeeds or fails.
func runCommand(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) error {
	handled, _, err := replShellCmd(ctx, cli, mgr, session, nil, args, sourceLoc{})
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	_, err = replDispatch(ctx, cli, mgr, args)
	return err
}

func assertOk(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, fmt.Errorf("ok requires a command")
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

func assertFail(ctx context.Context, cli *bpfmancli.CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
	if len(args) == 0 {
		return assertResult{}, fmt.Errorf("fail requires a command")
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

func assertPath(args []string) (assertResult, error) {
	if len(args) != 2 || args[0] != "exists" {
		return assertResult{}, fmt.Errorf("path requires: path exists <filepath>")
	}
	_, err := os.Stat(args[1])
	pass := err == nil
	return assertResult{
		pass:    pass,
		message: fmt.Sprintf("expected path %q to exist", args[1]),
	}, nil
}

func assertContains(args []string) (assertResult, error) {
	if len(args) != 2 {
		return assertResult{}, fmt.Errorf("contains requires exactly 2 arguments: <haystack> <needle>")
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
		return assertResult{}, fmt.Errorf("matches requires a structured value as the target (got %s)", argText(target))
	}
	if len(block.Entries) == 0 {
		return assertResult{}, fmt.Errorf("matches block must contain at least one entry")
	}

	// locate prefixes a path message with the entry's source
	// location so multi-mismatch failures point at the specific
	// offending line inside the block. The shell.Loc carried by
	// each entry is relative to the accumulated REPL chunk; when
	// the assert statement has a known file/start-line, translate
	// the chunk-local line into an absolute file line so the
	// diagnostic agrees with the rest of the REPL's "file:line:"
	// convention.
	locate := func(loc shell.Loc, msg string) string {
		if loc.Line == 0 {
			return msg
		}
		if base.file != "" && base.line > 0 {
			absLine := base.line + loc.Line - 1
			return fmt.Sprintf("%s:%d:%d: %s", base.file, absLine, loc.Col, msg)
		}
		return fmt.Sprintf("%d:%d: %s", loc.Line, loc.Col, msg)
	}

	var mismatches []string
	for _, entry := range block.Entries {
		actual, err := sva.Value.LookupValue(sva.Name, entry.Path)
		if err != nil {
			mismatches = append(mismatches, locate(entry.Loc, fmt.Sprintf("%s: %v", entry.Path, err)))
			continue
		}
		if entry.NotEmpty {
			s, err := actual.Scalar()
			if err != nil {
				mismatches = append(mismatches, locate(entry.Loc, fmt.Sprintf("%s: expected non-empty scalar, got %s", entry.Path, actual.Kind())))
				continue
			}
			if s == "" {
				mismatches = append(mismatches, locate(entry.Loc, fmt.Sprintf("%s: expected non-empty, got \"\"", entry.Path)))
			}
			continue
		}
		actualS, err := actual.Scalar()
		if err != nil {
			mismatches = append(mismatches, locate(entry.Loc, fmt.Sprintf("%s: expected scalar value, got %s", entry.Path, actual.Kind())))
			continue
		}
		expected, err := entry.Value.Scalar()
		if err != nil {
			return assertResult{}, fmt.Errorf("matches entry %q: pattern is not a scalar value", entry.Path)
		}
		if actualS != expected {
			mismatches = append(mismatches, locate(entry.Loc, fmt.Sprintf("%s: expected %q, got %q", entry.Path, expected, actualS)))
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
