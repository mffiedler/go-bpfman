// Assertion verbs for the REPL: "assert" and "require".  Both
// share a single dispatch path (replAssertRequire) with an
// isRequire flag deciding whether failure increments the session
// counter (assert) or halts execution via errRequireFailed
// (require).  All the verbs, plus the expression-assertion path
// for predicate/comparison forms ("$a eq $b"), live here.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/frobware/go-bpfman/manager"
	"github.com/frobware/go-bpfman/shell"
)

// assertResult holds the outcome of evaluating an assertion verb.
type assertResult struct {
	pass    bool
	message string
}

// replAssertRequire handles both "assert" and "require" commands.
// When isRequire is true, failure halts execution immediately via
// errRequireFailed. When false, failure is recorded in the session
// counter and execution continues.
func replAssertRequire(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg, isRequire bool, loc sourceLoc) error {
	if len(args) == 0 {
		return fmt.Errorf("expected an assertion (e.g. \"$a eq $b\", \"true $flag\", \"ok exec ...\")")
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

	// Value-based assertion: binary comparison or unary predicate.
	// These route through the expression grammar. "not" is legal
	// before unary predicates but banned before binary comparisons
	// (use the complementary operator instead).
	if isExprAssertion(args) {
		if negate && len(args) == 3 {
			return fmt.Errorf("\"not\" is not supported with infix comparisons; use the complementary operator (ne, le, ge, !=, <=, >=)")
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
// grammar: either [lhs op rhs] with a binary operator, or [pred
// operand] with a unary predicate.
func isExprAssertion(args []shell.Arg) bool {
	switch len(args) {
	case 2:
		return shell.IsUnaryPred(argText(args[0]))
	case 3:
		return shell.IsBinaryOp(argText(args[1]))
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
		case "eq":
			return fmt.Sprintf("expected %q to equal %q", left, right)
		case "ne":
			return fmt.Sprintf("expected %q to not equal %q", left, right)
		case "lt", "le", "gt", "ge":
			return fmt.Sprintf("expected %q %s %q (lexicographic)", left, x.Op, right)
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
		case "true":
			return fmt.Sprintf("expected %s to be true", operand)
		case "false":
			return fmt.Sprintf("expected %s to be false", operand)
		default:
			return fmt.Sprintf("expected predicate %s to hold on %s", x.Pred, operand)
		}
	}
	return "assertion failed"
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
func evalAssertVerb(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, verb string, args []shell.Arg) (assertResult, error) {
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
	case "eq", "ne", "lt", "le", "gt", "ge":
		return assertResult{}, fmt.Errorf("%q is not a prefix verb; use infix form: assert <left> %s <right>", verb, verb)
	case "true", "false", "not-empty":
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
func runCommand(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) error {
	handled, _, err := replShellCmd(ctx, cli, mgr, session, args, sourceLoc{})
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	_, err = replDispatch(ctx, cli, mgr, args)
	return err
}

func assertOk(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
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

func assertFail(ctx context.Context, cli *CLI, mgr *manager.Manager, session *shell.Session, args []shell.Arg) (assertResult, error) {
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
