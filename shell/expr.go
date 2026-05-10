package shell

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Expr is the sealed sum type for REPL expressions. The evaluator
// walks Expr nodes against an *Env to produce a [Value].
//
// The grammar:
//
//	expr    := primary | unary | binary
//	primary := LiteralExpr | VarRefExpr | AdapterExpr | InterpStringExpr
//	unary   := UnaryExpr (pred operand)
//	binary  := BinaryExpr (left op right)
type Expr interface {
	Node
	exprNode()
}

// LiteralExpr wraps a word or quoted-string token. Quoted records
// whether the operand came from a quoted literal; callers use it
// to preserve quoting semantics when rebuilding arguments for
// dispatch.
type LiteralExpr struct {
	Text   string
	Quoted bool
	Span
}

// VarRefExpr is a variable reference with an optional field/index
// path. The referenced [Value] is resolved at evaluation time
// against the Session on the Env.
type VarRefExpr struct {
	Name string
	Path string
	Span
}

// AdapterExpr is an adapter-decorated variable reference such as
// file:$var.path. Adapters only make sense in command-argument
// position; using one as an expression operand is a runtime error.
type AdapterExpr struct {
	Adapter string
	Name    string
	Path    string
	Span
}

// InterpStringExpr is a double-quoted string with one or more
// "${expr}" interpolation points.  Segments alternates literal
// text and parsed sub-expressions in source order.  Evaluation
// walks the segments, evaluates each expression to a scalar, and
// concatenates the pieces into a single StringValue.  A plain
// double-quoted string with no interpolation is a LiteralExpr, so
// an InterpStringExpr always carries at least one non-literal
// segment.
type InterpStringExpr struct {
	Segments []InterpStringSegment
	Span
}

// InterpStringSegment is one alternation inside an
// InterpStringExpr.  Exactly one of Literal / Expr is the
// meaningful field: if Expr is nil, the segment is a run of
// literal text carried in Literal; otherwise it is an
// interpolation whose value replaces the "${...}" at eval time.
type InterpStringSegment struct {
	Literal string
	Expr    Expr
}

// BinaryExpr is a two-operand comparison. Op is one of the
// recognised binary operators (==, !=, <, <=, >, >=). The
// comparison's semantics is selected at evaluation time by the
// operand types, not by the operator's spelling: see evalCompare
// for the strict-dispatch rules. Evaluation produces a BoolValue.
type BinaryExpr struct {
	Left  Expr
	Op    string
	Right Expr
	Span
}

// UnaryExpr is a single-operand predicate. Pred is the only
// surviving unary predicate, "not-empty"; truthiness is read
// directly via AsBool on a single-arg expression assertion, and
// "true"/"false" are bare boolean literals (see literalValue).
// Evaluation produces a BoolValue.
type UnaryExpr struct {
	Pred    string
	Operand Expr
	Span
}

// ThreadExpr is a value-threading composition: evaluate LHS to a
// Value, append that Value as the last argument of the command
// described by Args, and dispatch. The operator binds tighter than
// comparison operators but looser than the primary forms. Pos
// identifies the '|>' token itself.
type ThreadExpr struct {
	LHS  Expr
	Args []Expr
	Span
}

// LogicalExpr is a short-circuit boolean combinator.  Op is
// either "and" or "or"; both operands must evaluate to a
// BoolValue at runtime.  Pos identifies the operator token.
type LogicalExpr struct {
	Op          string
	Left, Right Expr
	Span
}

// NotExpr is boolean negation.  The operand must evaluate to a
// BoolValue at runtime.  Pos identifies the 'not' token.
type NotExpr struct {
	Operand Expr
	Span
}

// NegateExpr is arithmetic negation.  The operand must evaluate
// to a numeric scalar at runtime; anything else is a type error
// cited at the '-' token.  Kept distinct from NotExpr so the
// two unary forms live in separate namespaces.
type NegateExpr struct {
	Operand Expr
	Span
}

// TimeoutExpr is a retry-scoped primary expression that evaluates
// to true when the enclosing retry loop has been running for at
// least the duration produced by Arg.  Arg is a sub-expression
// evaluated at check time, so the duration can be a literal
// ("timeout 60s"), a variable ("timeout $max_wait"), an
// interpolated string ('timeout "${n}s"'), or any other primary
// that reduces to a scalar string acceptable to
// time.ParseDuration.  Outside a retry context evaluation is a
// runtime error, cited at the 'timeout' token.
//
//	until $phase == ready or timeout 60s
//	until not timeout 5s and $converged
//	until $ready or timeout $max_wait
type TimeoutExpr struct {
	Arg Expr
	Span
}

// IterationExpr is a retry-scoped primary expression that
// evaluates to true when the enclosing retry loop has executed
// at least the count produced by Arg.  Arg is a sub-expression
// evaluated at check time, so the count can be a literal
// ("iteration 10"), a variable ("iteration $max"), or any other
// primary that reduces to a non-negative integer scalar.
// Outside a retry context evaluation is a runtime error, cited at
// the 'iteration' token.
//
//	until iteration 10                 -- cap at ten attempts
//	until $done or iteration 100       -- success or give up
//	until $done or iteration $max      -- configurable cap
type IterationExpr struct {
	Arg Expr
	Span
}

func (*LiteralExpr) exprNode()      {}
func (*VarRefExpr) exprNode()       {}
func (*AdapterExpr) exprNode()      {}
func (*InterpStringExpr) exprNode() {}
func (*BinaryExpr) exprNode()       {}
func (*UnaryExpr) exprNode()        {}
func (*ThreadExpr) exprNode()       {}
func (*LogicalExpr) exprNode()      {}
func (*NotExpr) exprNode()          {}
func (*NegateExpr) exprNode()       {}
func (*TimeoutExpr) exprNode()      {}
func (*IterationExpr) exprNode()    {}

// Env is the execution environment for the evaluator. Session is
// the variable and alias store; ExecCommand dispatches top-level
// commands to the REPL's shell and domain pipelines; ExecBind
// dispatches command forms on the right of a '<-' bind.
//
// A nil ExecCommand makes any top-level CommandStmt a runtime
// error; a nil ExecBind makes any BindStmt a runtime error.
// Tests that only exercise expression evaluation can leave both
// unset.
type Env struct {
	Session *Session

	// ExecCommand runs a top-level CommandStmt. span is the
	// originating statement's source extent so handlers (and any
	// errors they emit) can frame diagnostics at the failing
	// command. The returned Value may be nil; any output is
	// visible on the CLI.
	ExecCommand func(args []Arg, span Span) (Value, error)

	// ExecBind runs a command form on the right of a '<-' bind.
	// span is the bind statement's source extent. The returned
	// BindResult carries the result envelope (Rc) and the
	// provider's primary result (Primary). Command failure
	// (non-zero exit, in-process error) is encoded on Rc as OK:
	// false with code, stdout, and stderr set, not as a Go
	// error. A Go error is reserved for structural failures
	// (empty argv, malformed adapter, no provider for this
	// hook). Set by the REPL driver; nil makes any BindStmt a
	// runtime error.
	ExecBind func(args []Arg, span Span) (BindResult, error)

	// ExecAssertStmt runs an AssertStmt: evaluate its expression,
	// AsBool the result, apply the optional Negate, and dispatch
	// failure-handling (printing, the session's assertion-failure
	// counter, halt-on-require). Set by the REPL driver; nil
	// makes any AssertStmt a runtime error.
	ExecAssertStmt func(*AssertStmt, *Env) error

	// PrintResult is called when a top-level ExprStmt produces a
	// value.  It is the "REPL-style auto-print" hook: typing "$x"
	// or "$x == 5" at the prompt lands here.  A nil callback
	// discards the value silently, which is the right behaviour
	// for embedded evaluators and for tests that do not care
	// about side output.
	PrintResult func(Value) error

	// RenderDeferFailure formats a defer-failure for the user.
	// The shell layer evaluates the deferred command via
	// ExecBind and, when the rc is not ok, calls this callback
	// so the driver can emit the labelled-block diagnostic. A
	// nil callback discards the rendering; the failure still
	// counts toward the script's exit code via Session.
	RenderDeferFailure func(stmtLoc Pos, args []Arg, rc Envelope)

	// HandleJobLeak is called once per unmanaged job at scope
	// exit. The driver renders the diagnostic ('[job] FAIL at
	// file:line: argv') and is responsible for any cleanup
	// signal (typically SIGKILL so a leaked background process
	// does not survive the script). The shell layer increments
	// Session.RecordJobLeak regardless of whether HandleJobLeak
	// is set, so the exit code reflects the leak even with a
	// nil callback.
	HandleJobLeak func(*Job)

	// defers is the active defer scope's stack. evalDeferStmt
	// appends; runDefers drains LIFO at scope exit. The
	// top-level program and def bodies establish new scopes by
	// saving and replacing the field; if/foreach/retry blocks
	// share the enclosing scope.
	defers *[]deferEntry

	// jobs is the active scope's started-job registry. start
	// appends via RegisterJob; the scope-exit leak check walks
	// the slice after defers have run (so 'defer kill $job' has
	// the chance to mark Managed first) and reports any
	// unmanaged entries. Saved/restored alongside defers so
	// nested scopes compose.
	jobs *[]*Job

	// retryStart is the time when the current retry loop began,
	// or the zero value when no retry is active.  TimeoutExpr
	// reads it to decide whether a duration has elapsed.
	// evalRetryStmt owns the save / set / restore dance so
	// nested retries behave sensibly.
	retryStart time.Time

	// retryIter is the current retry iteration (1-based,
	// incremented before each body run), or zero outside a
	// retry.  IterationExpr reads it.  evalRetryStmt manages
	// the save / set / restore dance alongside retryStart.
	retryIter int
}

// IsBinaryOp reports whether s is a recognised binary operator.
// The DSL provides one comparison family in symbol form; semantics
// is selected by operand type, not by operator spelling. See
// evalCompare for the strict-dispatch rules.
func IsBinaryOp(s string) bool {
	switch s {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// IsUnaryPred reports whether s is a recognised unary predicate in
// the expression grammar. The nil check is handled as a prefix
// verb in the assertion layer, not as a unary expression. Truthy
// boolean checks live in the strict comparison family: bare "true"
// and "false" literals evaluate to BoolValue, "assert $flag"
// reads truthiness directly via AsBool, and "$flag == true" stays
// available for the explicit form.
func IsUnaryPred(s string) bool {
	return s == "not-empty"
}

// EvalProgram executes each statement in prog against env in order.
// The first error halts evaluation and is returned to the caller,
// which decorates it with source-file context.  A break or
// continue that escapes every enclosing ForEachStmt is reported
// as a runtime error rather than silently swallowed.
func EvalProgram(prog *Program, env *Env) error {
	return runWithDeferScope(env, func() error {
		return evalProgramBody(prog, env)
	})
}

// EvalProgramInScope evaluates prog against env without opening a
// new defer scope. The caller must already have established one
// (typically via WithDeferScope). Used by script mode, where the
// REPL drives one balanced chunk at a time but every chunk must
// share the script-level defer scope so 'defer cleanup' near the
// top of the file runs at script exit, not at end-of-chunk.
func EvalProgramInScope(prog *Program, env *Env) error {
	return evalProgramBody(prog, env)
}

// WithDeferScope runs fn inside a fresh defer scope, restoring
// the outer scope on return and executing every registered
// deferred statement in LIFO order regardless of fn's outcome.
// Exposed so the REPL can wrap a sequence of EvalProgramInScope
// calls in one shared scope.
func WithDeferScope(env *Env, fn func() error) error {
	return runWithDeferScope(env, fn)
}

func evalProgramBody(prog *Program, env *Env) error {
	for _, stmt := range prog.Stmts {
		err := evalStmt(stmt, env)
		switch {
		case err == nil:
			continue
		case errors.Is(err, errBreak):
			return locErrorf(stmtLoc(stmt), "break outside a foreach loop")
		case errors.Is(err, errContinue):
			return locErrorf(stmtLoc(stmt), "continue outside a foreach loop")
		default:
			// Defensive safety net: any runtime error that
			// reached the program level without a Span gets
			// framed at the offending statement. Most paths
			// already return a *SyntaxError (frameAtSpan
			// short-circuits); this catches anything that
			// slipped through future evaluators or escape
			// hatches.
			return frameAtSpan(nodeSpan(stmt), err)
		}
	}
	return nil
}

// stmtLoc returns the Pos on any Stmt variant.  Used by
// EvalProgram to cite a source location when break / continue
// reach the top level without being caught by a loop.
func stmtLoc(s Stmt) Pos {
	switch v := s.(type) {
	case *LetStmt:
		return v.Pos
	case *BindStmt:
		return v.Pos
	case *DeferStmt:
		return v.Pos
	case *IfStmt:
		return v.Pos
	case *CommandStmt:
		return v.Pos
	case *ExprStmt:
		return v.Pos
	case *ForEachStmt:
		return v.Pos
	case *RetryStmt:
		return v.Pos
	case *BreakStmt:
		return v.Pos
	case *ContinueStmt:
		return v.Pos
	case *DefStmt:
		return v.Pos
	case *AssertStmt:
		return v.Pos
	}
	return Pos{}
}

// errBreak and errContinue are sentinel errors that carry a
// ForEachStmt control-flow signal up through evalStmt and its
// helpers.  evalForEachStmt catches them; anything else that sees
// them converts to a user-facing "outside loop" error.
var (
	errBreak    = fmt.Errorf("break outside a foreach loop")
	errContinue = fmt.Errorf("continue outside a foreach loop")
)

func evalStmt(stmt Stmt, env *Env) error {
	switch s := stmt.(type) {
	case *LetStmt:
		val, err := EvalExpr(s.RHS, env)
		if err != nil {
			return err
		}
		if val.IsNil() {
			return spanErrorf(s.Span, "expression produced no result to assign")
		}
		env.Session.Set(s.Name, val)
		return nil
	case *BindStmt:
		return evalBindStmt(s, env)
	case *DeferStmt:
		return evalDeferStmt(s, env)
	case *IfStmt:
		return evalIfStmt(s, env)
	case *CommandStmt:
		return evalCommandStmt(s, env)
	case *ExprStmt:
		return evalExprStmt(s, env)
	case *ForEachStmt:
		return evalForEachStmt(s, env)
	case *RetryStmt:
		return evalRetryStmt(s, env)
	case *BreakStmt:
		return errBreak
	case *ContinueStmt:
		return errContinue
	case *DefStmt:
		return evalDefStmt(s, env)
	case *AssertStmt:
		if env.ExecAssertStmt == nil {
			return spanErrorf(s.Span, "assert/require has no executor on the env")
		}
		return env.ExecAssertStmt(s, env)
	default:
		return fmt.Errorf("unknown statement type %T", stmt)
	}
}

// evalDefStmt registers s in the session's def table. Redefining an
// existing def replaces it silently; this matches let and alias.
func evalDefStmt(s *DefStmt, env *Env) error {
	env.Session.SetDef(&DefValue{
		Name:   s.Name,
		Params: s.Params,
		Body:   s.Body,
		Span:   s.Span,
	})
	return nil
}

// callDef binds def parameters from args, runs def.Body in env, and
// restores the prior session bindings on return. Arity is checked
// against len(def.Params) and a mismatch yields a runtime error
// citing both the call site and the def's declaration site.
//
// The shadow-and-restore is implemented over the existing flat
// session map: each parameter's prior value is captured (or noted as
// absent) before the body runs and reinstated afterwards. Recursion
// works naturally because each call's saves slice is local to its
// invocation.
func callDef(def *DefValue, args []Arg, callLoc Pos, env *Env) error {
	if len(args) != len(def.Params) {
		return locErrorf(callLoc, "%s: expected %d argument(s), got %d (def declared at %d:%d)",
			def.Name, len(def.Params), len(args), def.Pos.Line, def.Pos.Col)
	}
	type saved struct {
		name string
		val  Value
		had  bool
	}
	saves := make([]saved, len(def.Params))
	for i, p := range def.Params {
		v, ok := env.Session.Get(p)
		saves[i] = saved{name: p, val: v, had: ok}
		env.Session.Set(p, argToValue(args[i]))
	}
	defer func() {
		for _, s := range saves {
			if s.had {
				env.Session.Set(s.name, s.val)
			} else {
				env.Session.Delete(s.name)
			}
		}
	}()
	return runWithDeferScope(env, func() error {
		for _, stmt := range def.Body {
			if err := evalStmt(stmt, env); err != nil {
				return err
			}
		}
		return nil
	})
}

// argToValue converts a post-expansion Arg into a Value suitable for
// binding to a def parameter. Word and quoted args become string
// values; resolved scalar args become string values; structured and
// adapter args carry their already-resolved Value through; a
// MatchesBlockArg has no positional value form and is rejected.
func argToValue(a Arg) Value {
	switch v := a.(type) {
	case WordArg:
		return StringValue(v.Text)
	case QuotedArg:
		return StringValue(v.Text)
	case ScalarValueArg:
		return StringValue(v.Text)
	case StructuredValueArg:
		return v.Value
	case AdapterArg:
		return v.Value
	default:
		return Value{}
	}
}

// evalForEachStmt evaluates the list expression, validates that
// it is a structured list, and runs the body once per element
// with the loop variable bound in the Session.  An error from
// the body halts iteration and propagates unwrapped.  The
// binding persists after the loop ends, matching shell-style
// for-each semantics — callers that want the previous value
// back must save it explicitly.
// retryBackoff is the fixed sleep between retry iterations.
// Small enough to keep short condition windows responsive, large
// enough to avoid pegging the CPU for long-running checks.
const retryBackoff = 100 * time.Millisecond

// evalRetryStmt runs s.Body repeatedly until s.Until evaluates
// to true.  Each iteration rebinds $iter (1-based iteration
// count) in the session so Until expressions can cap by
// iteration count.  Elapsed-time checks go through the
// TimeoutExpr primary ("timeout 30s") rather than a magic
// variable; Env.retryStart carries the clock.
//
// A failing body does not halt the retry; the body's error is
// carried across iterations and returned only when Until
// finally becomes true.  That way a timeout-style exit
// surfaces the reason the body was failing at the moment the
// budget ran out.  Until runs after each body regardless of
// whether the body errored, so a time cap fires even when
// every attempt is failing.
//
// Nested retries are supported: Env.retryStart is saved and
// restored, so an inner retry's 'timeout' evaluates against
// the inner's clock.
//
// Ctrl-C interrupts the process; there is no context plumbing
// through the evaluator yet, so a retry with a never-satisfied
// Until loops until the process is killed.  Users writing
// long-running retries are expected to include a cap in their
// Until expression ("$x == done or timeout 60s").
func evalRetryStmt(s *RetryStmt, env *Env) error {
	prevStart := env.retryStart
	prevIter := env.retryIter
	env.retryStart = time.Now()
	env.retryIter = 0
	defer func() {
		env.retryStart = prevStart
		env.retryIter = prevIter
	}()

	var bodyErr error
	for {
		env.retryIter++
		bodyErr = nil
		for _, stmt := range s.Body {
			if err := evalStmt(stmt, env); err != nil {
				bodyErr = err
				break
			}
		}

		untilV, err := EvalExpr(s.Until, env)
		if err != nil {
			return err
		}
		untilB, err := AsBool(untilV)
		if err != nil {
			return spanErrorf(s.Span, "retry: until expression: %v", err)
		}
		if untilB {
			return bodyErr
		}
		time.Sleep(retryBackoff)
	}
}

// evalTimeoutExpr returns true when the current retry has been
// running for at least e.Duration.  Outside a retry context
// (Env.retryStart zero) it errors, since there is no clock to
// measure against.
func evalTimeoutExpr(e *TimeoutExpr, env *Env) (Value, error) {
	if env.retryStart.IsZero() {
		return Value{}, spanErrorf(e.Span, "timeout expression is only valid inside a retry body or its until clause")
	}
	v, err := EvalExpr(e.Arg, env)
	if err != nil {
		return Value{}, err
	}
	s, err := v.Scalar()
	if err != nil {
		return Value{}, spanErrorf(e.Span, "timeout: argument must be a scalar duration: %v", err)
	}
	d, err := parseDurationLiteral(s)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "timeout: %v", err)
	}
	return BoolValue(time.Since(env.retryStart) >= d), nil
}

// evalIterationExpr returns true when the current retry has
// executed at least the iteration count produced by e.Arg.
// Outside a retry context (Env.retryIter zero) it errors.
func evalIterationExpr(e *IterationExpr, env *Env) (Value, error) {
	if env.retryIter == 0 {
		return Value{}, spanErrorf(e.Span, "iteration expression is only valid inside a retry body or its until clause")
	}
	v, err := EvalExpr(e.Arg, env)
	if err != nil {
		return Value{}, err
	}
	s, err := v.Scalar()
	if err != nil {
		return Value{}, spanErrorf(e.Span, "iteration: argument must be a scalar count: %v", err)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "iteration: %q is not a valid integer count", s)
	}
	if n < 0 {
		return Value{}, spanErrorf(e.Span, "iteration: count %d is negative", n)
	}
	return BoolValue(env.retryIter >= n), nil
}

func evalForEachStmt(s *ForEachStmt, env *Env) error {
	v, err := EvalExpr(s.List, env)
	if err != nil {
		return err
	}
	if v.IsNil() {
		return spanErrorf(s.Span, "foreach: list expression is null")
	}
	list, ok := v.Raw().([]any)
	if !ok {
		return spanErrorf(s.Span, "foreach: expected a list, got %s", v.Kind())
	}
	// The loop variable is body-scoped: any prior binding of the
	// same name is restored after the loop ends, and a name that
	// did not exist before the loop disappears again. This drops
	// shell-style "loop variable persists" semantics, which is the
	// only place the language was copying bash without a reason --
	// the rest of the DSL deliberately avoids shell foot-guns.
	prior, hadPrior := env.Session.Get(s.Name)
	defer func() {
		if hadPrior {
			env.Session.Set(s.Name, prior)
		} else {
			env.Session.Delete(s.Name)
		}
	}()
iter:
	for _, elem := range list {
		env.Session.Set(s.Name, ValueFromAny(elem))
		for _, stmt := range s.Body {
			err := evalStmt(stmt, env)
			switch {
			case err == nil:
				continue
			case errors.Is(err, errBreak):
				break iter
			case errors.Is(err, errContinue):
				continue iter
			default:
				return err
			}
		}
	}
	return nil
}

func evalIfStmt(s *IfStmt, env *Env) error {
	check := func(cond Expr) (bool, error) {
		v, err := EvalExpr(cond, env)
		if err != nil {
			return false, err
		}
		b, err := AsBool(v)
		if err != nil {
			return false, spanErrorf(nodeSpan(cond), "if: %v", err)
		}
		return b, nil
	}
	runBody := func(body []Stmt) error {
		for _, stmt := range body {
			if err := evalStmt(stmt, env); err != nil {
				return err
			}
		}
		return nil
	}
	ok, err := check(s.Cond)
	if err != nil {
		return err
	}
	if ok {
		return runBody(s.Then)
	}
	for _, br := range s.Elifs {
		ok, err := check(br.Cond)
		if err != nil {
			return err
		}
		if ok {
			return runBody(br.Body)
		}
	}
	if len(s.Else) > 0 {
		return runBody(s.Else)
	}
	return nil
}

// deferEntry is one captured invocation in a defer scope. Args
// are evaluated at register time and frozen onto the entry; Cmd
// holds the original command form so the diagnostic renderer can
// cite the source location of the defer statement.
type deferEntry struct {
	Span
	Args []Arg
}

// evalDeferStmt evaluates the deferred command's arguments now
// (so values are captured at register time, not at scope exit)
// and appends the entry to the active defer scope's stack. A
// missing scope is a runtime error; the parser does not enforce
// that defer is reachable, so a malformed driver could trip this
// check.
func evalDeferStmt(s *DeferStmt, env *Env) error {
	if env.defers == nil {
		return spanErrorf(s.Span, "defer outside any defer scope")
	}
	args, err := EvalArgs(s.Cmd.Args, env)
	if err != nil {
		return err
	}
	*env.defers = append(*env.defers, deferEntry{
		Span: s.Span,
		Args: args,
	})
	return nil
}

// runDefers drains stack in LIFO order, dispatching each entry
// via env.ExecBind. A non-ok rc is rendered through
// RenderDeferFailure (when set) and counted via Session so the
// script's exit code reflects the failure; cleanup continues
// across failures. Structural errors from ExecBind are rare;
// they are rendered with an empty-rc envelope so the user still
// sees a labelled block.
func runDefers(env *Env, stack []deferEntry) {
	if env.ExecBind == nil {
		return
	}
	for i := len(stack) - 1; i >= 0; i-- {
		entry := stack[i]
		result, err := env.ExecBind(entry.Args, entry.Span)
		if err != nil {
			rc := Envelope{OK: false, Code: 1, Stderr: err.Error()}
			if env.RenderDeferFailure != nil {
				env.RenderDeferFailure(entry.Pos, entry.Args, rc)
			}
			env.Session.RecordDeferFailure()
			continue
		}
		if !result.Rc.OK {
			if env.RenderDeferFailure != nil {
				env.RenderDeferFailure(entry.Pos, entry.Args, result.Rc)
			}
			env.Session.RecordDeferFailure()
		}
	}
}

// RegisterJob appends a started Job to the active job scope's
// registry so the scope-exit leak check can detect an unmanaged
// lifecycle. Outside any job scope (no driver-established
// WithJobScope) the call is a no-op: there is nothing to leak
// from. j must be non-nil; nil is reserved for "no job" rather
// than used as a sentinel here.
func (e *Env) RegisterJob(j *Job) {
	if e.jobs == nil {
		return
	}
	*e.jobs = append(*e.jobs, j)
}

// ActiveJobs returns a snapshot of the jobs registered in the
// innermost active job scope, in registration order. The slice
// is a fresh copy so callers may sort or filter without
// disturbing the registry. Outside any job scope the result is
// nil. Used by the 'jobs' builtin to list everything alive in
// the current session.
func (e *Env) ActiveJobs() []*Job {
	if e.jobs == nil {
		return nil
	}
	out := make([]*Job, len(*e.jobs))
	copy(out, *e.jobs)
	return out
}

// ReapJobs removes every job from the active job scope's
// registry for which shouldReap returns true. Registration
// order is preserved for survivors. Outside any job scope
// the call is a no-op. Used by the 'reap' builtin to drop
// completed entries while leaving running jobs alone; the
// predicate shape lets callers choose their own definition
// of 'done' (closed Done channel, Managed flag, both, ...).
func (e *Env) ReapJobs(shouldReap func(*Job) bool) {
	if e.jobs == nil {
		return
	}
	src := *e.jobs
	dst := src[:0]
	for _, j := range src {
		if !shouldReap(j) {
			dst = append(dst, j)
		}
	}
	// Clear any tail references so reaped jobs are not held
	// alive by the underlying array.
	for i := len(dst); i < len(src); i++ {
		src[i] = nil
	}
	*e.jobs = dst
}

// runWithDeferScope establishes a defer scope around fn. The
// previous scope is saved and restored on exit so nested scopes
// (program, def body) compose. fn's error is returned verbatim;
// defer execution happens regardless of fn's outcome.
//
// Defer scopes are independent of job scopes (see WithJobScope)
// because they nest differently: a def body opens its own defer
// scope (defers fire at def return) but inherits the caller's
// job scope (a job started in a def joins the caller's
// registry, so returning the handle for the caller to wait is
// not flagged as a leak).
func runWithDeferScope(env *Env, fn func() error) error {
	saved := env.defers
	var stack []deferEntry
	env.defers = &stack
	bodyErr := fn()
	env.defers = saved
	runDefers(env, stack)
	return bodyErr
}

// WithJobScope establishes a job scope around fn: any Job that
// fn registers via Env.RegisterJob is tracked in this scope's
// registry, and on exit each unmanaged entry is reported through
// HandleJobLeak and counted on the session.
//
// Drivers open exactly one job scope per session unit: the whole
// script in script mode, the whole sourced file in source mode,
// the whole interactive session in interactive mode. Inner
// blocks (def bodies, foreach, retry) deliberately do not open
// new job scopes, so a job started inside a def joins the
// caller's registry and survives the def's return without
// being flagged.
//
// Job leak reporting runs after fn returns. When the driver
// composes WithJobScope around an outer WithDeferScope (the
// usual shape: defers nest inside jobs), the outer defers run
// before the leak walk, so 'defer kill $job' marks a job
// Managed before the leak walk sees it.
func WithJobScope(env *Env, fn func() error) error {
	saved := env.jobs
	var jobs []*Job
	env.jobs = &jobs
	bodyErr := fn()
	env.jobs = saved
	reportJobLeaks(env, jobs)
	return bodyErr
}

// reportJobLeaks walks the scope's registered jobs and invokes
// HandleJobLeak on any the script never marked Managed (via
// wait or kill). The handler owns the policy: a strict driver
// renders a diagnostic, kills the process, and records the
// leak on the session so the run exits non-zero; a friendly
// driver kills silently and leaves the session counter
// untouched. The shell layer takes no opinion -- a nil handler
// means "nothing to do; the leak passes silently".
func reportJobLeaks(env *Env, jobs []*Job) {
	if env.HandleJobLeak == nil {
		return
	}
	for _, j := range jobs {
		if j.IsManaged() {
			continue
		}
		env.HandleJobLeak(j)
	}
}

// evalBindStmt runs the command form on the right of a '<-' bind
// and assigns its result. For guard, a non-ok rc halts via
// GuardFailure with no bindings happening. For let, bindings
// happen regardless of rc.ok, with the rc carrying ok: false on
// failure so the consumer can inspect it.
//
// Single-name binding (Rc == "") binds Primary only. Tuple
// binding (Rc set) binds both names. "_" as a name discards that
// slot.
func evalBindStmt(s *BindStmt, env *Env) error {
	if env.ExecBind == nil {
		return spanErrorf(s.Span, "'<-' bind: command execution is not configured")
	}
	args, err := EvalArgs(s.Cmd.Args, env)
	if err != nil {
		return err
	}
	result, err := env.ExecBind(args, s.Span)
	if err != nil {
		return frameAtSpan(s.Span, err)
	}
	if s.Guard && !result.Rc.OK {
		return &GuardFailure{Span: s.Span, Primary: s.Primary, Args: args, Envelope: result.Rc}
	}
	if s.Rc != "" && s.Rc != "_" {
		env.Session.Set(s.Rc, ValueFromEnvelope(result.Rc))
	}
	if s.Primary != "" && s.Primary != "_" {
		env.Session.Set(s.Primary, result.Primary)
	}
	return nil
}

// GuardFailure is the error type a 'guard ... <- CMD' statement
// returns when the captured rc is not ok. The driver formats the
// failure through its renderer; the language layer carries the
// envelope so the renderer has the captured stdout, stderr, exit
// code, and the offending bind's source location, plus the
// resolved Args so the renderer can show the command line that
// failed and the Primary name (the bind target the user wrote)
// for the diagnostic.
type GuardFailure struct {
	Span
	Primary  string
	Args     []Arg
	Envelope Envelope
}

func (e *GuardFailure) Error() string {
	target := e.Primary
	if target == "" || target == "_" {
		target = "_"
	}
	if e.Envelope.Stderr != "" {
		return fmt.Sprintf("guard %s: command failed (exit %d): %s",
			target, e.Envelope.Code, e.Envelope.Stderr)
	}
	return fmt.Sprintf("guard %s: command failed (exit %d)", target, e.Envelope.Code)
}

func evalCommandStmt(s *CommandStmt, env *Env) error {
	args, err := EvalArgs(s.Args, env)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if name, ok := commandHeadName(args[0]); ok {
			if def, hit := env.Session.GetDef(name); hit {
				return callDef(def, args[1:], s.Pos, env)
			}
		}
	}
	if env.ExecCommand == nil {
		return spanErrorf(s.Span, "command execution is not configured")
	}
	_, err = env.ExecCommand(args, s.Span)
	return frameAtSpan(s.Span, err)
}

// commandHeadName extracts the command name from the first argument
// of a command call, but only when the first argument is a literal
// word (the syntactic shape that names a command). Quoted strings,
// resolved scalars, and structured values do not name commands and
// are not eligible to dispatch as defs.
func commandHeadName(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok {
		return "", false
	}
	return w.Text, true
}

// evalExprStmt evaluates a top-level expression statement and, when
// a PrintResult callback is wired, forwards the result to it.  This
// is how "$x" and "$x == 5" typed at the prompt get their
// auto-printed values: the parser wraps expression-led statements
// in ExprStmt, and the REPL's PrintResult handler renders the
// value through the same path print uses.  When PrintResult is nil
// (embedded use, tests) the value is evaluated for side effects
// and discarded, matching Python's script-mode semantics.
func evalExprStmt(s *ExprStmt, env *Env) error {
	v, err := EvalExpr(s.Expr, env)
	if err != nil {
		return err
	}
	if env.PrintResult == nil {
		return nil
	}
	return env.PrintResult(v)
}

// EvalExpr evaluates expr as a value-producing expression and
// returns its Value. Primary expressions (literals, variable
// references, command substitutions) produce a Value directly;
// binary and unary expressions combine their operands per their op
// or predicate. Adapter references are rejected — they only make
// sense as command arguments.
// EvalExpr is the public expression entry point. Each leaf
// evaluator is responsible for wrapping its own errors with a
// Span; callers above the evaluator (e.g. the chunk runner) add
// a final safety net that frames anything that escaped here
// without a Span.
func EvalExpr(expr Expr, env *Env) (Value, error) {
	switch e := expr.(type) {
	case *LiteralExpr:
		return literalValue(e), nil
	case *VarRefExpr:
		return resolveVarRefValue(e, env)
	case *AdapterExpr:
		return Value{}, spanErrorf(e.Span, "adapter %s:$%s cannot be used as an expression operand", e.Adapter, e.Name)
	case *InterpStringExpr:
		return evalInterpString(e, env)
	case *ThreadExpr:
		return dispatchThread(e, env)
	case *BinaryExpr:
		return evalBinary(e, env)
	case *UnaryExpr:
		return evalUnary(e, env)
	case *LogicalExpr:
		return evalLogical(e, env)
	case *NotExpr:
		return evalNot(e, env)
	case *NegateExpr:
		return evalNegate(e, env)
	case *TimeoutExpr:
		return evalTimeoutExpr(e, env)
	case *IterationExpr:
		return evalIterationExpr(e, env)
	default:
		return Value{}, fmt.Errorf("unhandled expression type %T", expr)
	}
}

// evalLogical evaluates 'and' / 'or' with short-circuit
// semantics.  Both operands must yield BoolValue; anything else
// is a type error cited at the operator's location.  For 'and',
// a false LHS returns false without evaluating RHS; for 'or', a
// true LHS returns true without evaluating RHS.  This matches
// every mainstream language's expectation for logical
// combinators.
func evalLogical(e *LogicalExpr, env *Env) (Value, error) {
	leftV, err := EvalExpr(e.Left, env)
	if err != nil {
		return Value{}, err
	}
	leftB, err := AsBool(leftV)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "%s: left: %v", e.Op, err)
	}
	switch e.Op {
	case "and":
		if !leftB {
			return BoolValue(false), nil
		}
	case "or":
		if leftB {
			return BoolValue(true), nil
		}
	default:
		return Value{}, spanErrorf(e.Span, "unknown logical operator %q", e.Op)
	}
	rightV, err := EvalExpr(e.Right, env)
	if err != nil {
		return Value{}, err
	}
	rightB, err := AsBool(rightV)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "%s: right: %v", e.Op, err)
	}
	return BoolValue(rightB), nil
}

// evalNot evaluates a boolean negation.  The operand must yield
// a BoolValue or the evaluator reports a type error at the
// 'not' location.
func evalNot(e *NotExpr, env *Env) (Value, error) {
	v, err := EvalExpr(e.Operand, env)
	if err != nil {
		return Value{}, err
	}
	b, err := AsBool(v)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "not: %v", err)
	}
	return BoolValue(!b), nil
}

// structuredShape returns a short description of a structured
// Value suitable for error messages.  The declared OriginKind is
// used when it is anything other than OriginUnknown (so "program"
// or "exec.result" read as such); otherwise the raw Go shape is
// inspected so an untagged record or array still reports
// meaningfully as "object" or "array" rather than the useless
// "unknown".
func structuredShape(v Value) string {
	if k := v.Kind(); k != OriginUnknown && k != OriginScalar {
		return k.String()
	}
	switch v.Raw().(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "structured"
	}
}

// evalInterpString walks an InterpStringExpr's segments,
// evaluates each expression segment, renders it to a string, and
// concatenates the pieces with the literal runs into a single
// StringValue.  Scalars render via Value.Scalar (plain text);
// structured values render as compact one-line JSON — "{"a":1}"
// or "[1,2,3]" — matching the "str(obj)"/"to_s" convention that
// Python, Ruby, and JavaScript use for interpolated containers.
// Nil renders as "null" so the output is always well-formed.
// Multi-line JSON is deliberately avoided: interpolation is a
// line-shaped output context (log lines, path construction,
// flag values) and an indented block in the middle of a line is
// actively harmful.  Users who want pretty formatting reach for
// "${[jq "." $r]}" or pipe through jq explicitly.
func evalInterpString(e *InterpStringExpr, env *Env) (Value, error) {
	var b strings.Builder
	for _, seg := range e.Segments {
		if seg.Expr == nil {
			b.WriteString(seg.Literal)
			continue
		}
		v, err := EvalExpr(seg.Expr, env)
		if err != nil {
			return Value{}, err
		}
		s, err := RenderCompact(v)
		if err != nil {
			return Value{}, spanErrorf(nodeSpan(seg.Expr), "interpolation: %v", err)
		}
		b.WriteString(s)
	}
	return StringValue(b.String()), nil
}

// RenderCompact renders a Value to a single-line string form.
// Scalars (including OriginNull, which renders as "null") use
// their text form; structured values marshal to compact JSON;
// an absent Value renders as "null" so a missing slot surfaces as
// visible "null" rather than silently vanishing or erroring.
// Used wherever a Value must flatten onto a single line — string
// interpolation and multi-argument print both feed through it
// so formatting stays consistent across those paths.
func RenderCompact(v Value) (string, error) {
	if v.IsNil() {
		return "null", nil
	}
	if v.IsStructured() {
		b, err := json.Marshal(v.Raw())
		if err != nil {
			return "", fmt.Errorf("render %s: %v", structuredShape(v), err)
		}
		return string(b), nil
	}
	return v.Scalar()
}

// EvalArgs evaluates each Expr in exprs as a command argument and
// returns the resulting []Arg, suitable for dispatch.
func EvalArgs(exprs []Expr, env *Env) ([]Arg, error) {
	out := make([]Arg, 0, len(exprs))
	for _, e := range exprs {
		a, err := evalArg(e, env)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func evalArg(expr Expr, env *Env) (Arg, error) {
	switch e := expr.(type) {
	case *LiteralExpr:
		if e.Quoted {
			return QuotedArg{Text: e.Text, Span: e.Span}, nil
		}
		return WordArg{Text: e.Text, Span: e.Span}, nil
	case *VarRefExpr:
		return resolveVarRefArg(e, env)
	case *AdapterExpr:
		return resolveAdapterArg(e, env)
	case *InterpStringExpr:
		val, err := evalInterpString(e, env)
		if err != nil {
			return nil, err
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, spanErrorf(e.Span, "interpolated string: %v", err)
		}
		return ScalarValueArg{Text: s, Span: e.Span}, nil
	case *ThreadExpr:
		val, err := dispatchThread(e, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, spanErrorf(e.Span, "thread produced no value")
		}
		if val.IsStructured() {
			return StructuredValueArg{Value: val, Span: e.Span}, nil
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, spanErrorf(e.Span, "thread: %v", err)
		}
		return ScalarValueArg{Text: s, Span: e.Span}, nil
	case *MatchesBlockExpr:
		return evalMatchesBlockArg(e, env)
	case *BinaryExpr, *UnaryExpr, *LogicalExpr, *NotExpr, *NegateExpr, *TimeoutExpr, *IterationExpr:
		return nil, locErrorf(exprLoc(expr), "boolean/comparison/arithmetic expression cannot be used as a command argument")
	default:
		return nil, locErrorf(exprLoc(expr), "cannot use %T as command argument", expr)
	}
}

func resolveVarRefValue(e *VarRefExpr, env *Env) (Value, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return Value{}, spanErrorf(e.Span, "undefined variable %q", e.Name)
	}
	if e.Path == "" {
		return v, nil
	}
	lv, err := v.LookupValue(e.Name, e.Path)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "%v", err)
	}
	return lv, nil
}

func resolveVarRefArg(e *VarRefExpr, env *Env) (Arg, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return nil, spanErrorf(e.Span, "undefined variable: %s", e.Name)
	}
	resolved := v
	if e.Path != "" {
		lv, err := v.LookupValue(e.Name, e.Path)
		if err != nil {
			return nil, spanErrorf(e.Span, "%v", err)
		}
		resolved = lv
	}
	if resolved.IsNil() {
		return nil, spanErrorf(e.Span, "variable %s is null", qualify(e.Name, e.Path))
	}
	if resolved.IsStructured() {
		return StructuredValueArg{Name: e.Name, Value: resolved, Span: e.Span}, nil
	}
	s, err := resolved.Scalar()
	if err != nil {
		return nil, spanErrorf(e.Span, "variable %s: %v", qualify(e.Name, e.Path), err)
	}
	return ScalarValueArg{Text: s, Span: e.Span}, nil
}

// qualify produces a "name.path" string for error messages, or
// just "name" when path is empty.
func qualify(name, path string) string {
	if path == "" {
		return name
	}
	return name + "." + path
}

func resolveAdapterArg(e *AdapterExpr, env *Env) (Arg, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return nil, spanErrorf(e.Span, "undefined variable: %s", e.Name)
	}
	resolved := v
	if e.Path != "" {
		lv, err := v.LookupValue(e.Name, e.Path)
		if err != nil {
			return nil, spanErrorf(e.Span, "%v", err)
		}
		resolved = lv
	}
	if resolved.IsNil() {
		return nil, spanErrorf(e.Span, "adapter %s: variable %s is null", e.Adapter, e.Name)
	}
	return AdapterArg{
		Adapter: e.Adapter,
		Name:    e.Name,
		Path:    e.Path,
		Value:   resolved,
		Span:    e.Span,
	}, nil
}

// dispatchThread evaluates a threading expression by threading the
// LHS's Value into the command described by Args. The LHS Value
// becomes the last element of the evaluated argument list so it
// matches the convention used by jq, file temp, and most
// shell-style "CMD ARGS VALUE" invocations. The thread errors
// loudly when the underlying command's result is not ok; in
// expression position there is no envelope slot to inspect, so
// failure must propagate.
func dispatchThread(e *ThreadExpr, env *Env) (Value, error) {
	if env.ExecBind == nil {
		return Value{}, spanErrorf(e.Span, "'|>' is only valid where commands can run; not available in this context")
	}
	lhsVal, err := EvalExpr(e.LHS, env)
	if err != nil {
		return Value{}, err
	}
	if lhsVal.IsNil() {
		return Value{}, spanErrorf(e.Span, "thread: left-hand side is null")
	}
	args, err := EvalArgs(e.Args, env)
	if err != nil {
		return Value{}, err
	}
	lhsArg, err := valueToArg(lhsVal, nodeSpan(e.LHS))
	if err != nil {
		return Value{}, spanErrorf(e.Span, "thread: %v", err)
	}
	result, err := env.ExecBind(append(args, lhsArg), e.Span)
	if err != nil {
		return Value{}, frameAtSpan(e.Span, err)
	}
	if !result.Rc.OK {
		if result.Rc.Stderr != "" {
			return Value{}, spanErrorf(e.Span, "thread: command failed (exit %d): %s", result.Rc.Code, result.Rc.Stderr)
		}
		return Value{}, spanErrorf(e.Span, "thread: command failed (exit %d)", result.Rc.Code)
	}
	return result.Primary, nil
}

// valueToArg wraps a Value in the most specific Arg variant for
// the dispatch boundary: structured values stay structured,
// scalars become ScalarValueArg, nil is a caller problem. span
// is attached to the resulting Arg so command-handler parsers
// can frame argument-position errors at the originating
// expression.
func valueToArg(v Value, span Span) (Arg, error) {
	if v.IsNil() {
		return nil, fmt.Errorf("value is null")
	}
	if v.IsStructured() {
		return StructuredValueArg{Value: v, Span: span}, nil
	}
	s, err := v.Scalar()
	if err != nil {
		return nil, err
	}
	return ScalarValueArg{Text: s, Span: span}, nil
}

func evalBinary(e *BinaryExpr, env *Env) (Value, error) {
	leftV, err := EvalExpr(e.Left, env)
	if err != nil {
		return Value{}, err
	}
	rightV, err := EvalExpr(e.Right, env)
	if err != nil {
		return Value{}, err
	}
	if isArithmeticOpText(e.Op) {
		left, err := leftV.Scalar()
		if err != nil {
			return Value{}, spanErrorf(e.Span, "binary %s: left: %v", e.Op, err)
		}
		right, err := rightV.Scalar()
		if err != nil {
			return Value{}, spanErrorf(e.Span, "binary %s: right: %v", e.Op, err)
		}
		v, err := evalArithmetic(e.Op, left, right)
		if err != nil {
			return Value{}, spanErrorf(e.Span, "%v", err)
		}
		return v, nil
	}
	return evalCompare(e.Op, leftV, rightV, e.Span)
}

// compareKind classifies a Value for the purpose of strict comparison
// dispatch. Numbers (json.Number, float64) are "number"; plain
// strings are "string"; booleans are "bool". Anything else (nil,
// map, slice, absent values) returns "" and is rejected by
// evalCompare with an error citing the actual underlying type so
// users see why the operands are incomparable.
func compareKind(v Value) string {
	switch v.Raw().(type) {
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "bool"
	}
	return ""
}

// evalCompare performs a strict, type-aware comparison. Both
// operands must classify as the same compareKind: number-vs-number
// compares as floats, string-vs-string as text, bool-vs-bool only
// supports == and != (booleans have no defined ordering). Cross-type
// comparisons are an error rather than a silent false, matching
// jq's strict equality and surfacing operator misuse loudly. To
// compare stringy numeric input (e.g. exec stdout) against a
// number, coerce explicitly via "$x |> jq tonumber" first.
func evalCompare(op string, l, r Value, span Span) (Value, error) {
	lk := compareKind(l)
	rk := compareKind(r)
	if lk == "" {
		return Value{}, spanErrorf(span, "%s: left side is a %s; only scalars (numbers, strings, booleans) can be compared with %s", op, l.Kind(), op)
	}
	if rk == "" {
		return Value{}, spanErrorf(span, "%s: right side is a %s; only scalars (numbers, strings, booleans) can be compared with %s", op, r.Kind(), op)
	}
	if lk != rk {
		return Value{}, spanErrorf(span, "binary %s: cannot compare %s to %s; coerce explicitly (e.g. \"$x |> jq tonumber\" for stringy numeric input)", op, lk, rk)
	}
	left, err := l.Scalar()
	if err != nil {
		return Value{}, spanErrorf(span, "binary %s: left: %v", op, err)
	}
	right, err := r.Scalar()
	if err != nil {
		return Value{}, spanErrorf(span, "binary %s: right: %v", op, err)
	}
	switch lk {
	case "number":
		v, err := evalNumericComparison(op, left, right)
		if err != nil {
			return Value{}, spanErrorf(span, "%v", err)
		}
		return v, nil
	case "bool":
		if op != "==" && op != "!=" {
			return Value{}, spanErrorf(span, "binary %s: booleans support only == and !=", op)
		}
		v, err := evalSymbolicTextComparison(op, left, right)
		if err != nil {
			return Value{}, spanErrorf(span, "%v", err)
		}
		return v, nil
	default:
		v, err := evalSymbolicTextComparison(op, left, right)
		if err != nil {
			return Value{}, spanErrorf(span, "%v", err)
		}
		return v, nil
	}
}

// isArithmeticOpText reports whether op is one of the five
// arithmetic operators.  Separate from isArithmeticOp (which
// operates on a Token) because the evaluator works with the
// already-extracted Op string on BinaryExpr.
func isArithmeticOpText(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%":
		return true
	}
	return false
}

// evalArithmetic parses both operands as float64 and performs
// the requested operation.  Division and modulo by zero are
// runtime errors; Go's math.Mod is used for '%' (which defines
// the result's sign to match the dividend, e.g. -7 % 3 = -1).
// Results are wrapped as numeric scalars via json.Number so
// the scalar-formatting path matches jq-sourced numbers:
// integer-valued floats render without a trailing ".0".
func evalArithmetic(op, left, right string) (Value, error) {
	a, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return Value{}, fmt.Errorf("arithmetic %s: left operand %q is not numeric", op, left)
	}
	b, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return Value{}, fmt.Errorf("arithmetic %s: right operand %q is not numeric", op, right)
	}
	var r float64
	switch op {
	case "+":
		r = a + b
	case "-":
		r = a - b
	case "*":
		r = a * b
	case "/":
		if b == 0 {
			return Value{}, fmt.Errorf("division by zero")
		}
		r = a / b
	case "%":
		if b == 0 {
			return Value{}, fmt.Errorf("division by zero")
		}
		r = math.Mod(a, b)
	default:
		return Value{}, fmt.Errorf("unknown arithmetic operator %q", op)
	}
	return numericValue(r), nil
}

// numericValue wraps a float64 result as a Value whose raw
// representation is a json.Number. That matches how jq-produced
// numbers land in the session and keeps Value.Scalar() on a common
// rendering path: integer-valued results print without a trailing
// ".0".
func numericValue(x float64) Value {
	text := strconv.FormatFloat(x, 'f', -1, 64)
	return Value{v: json.Number(text), kind: OriginScalar}
}

// literalValue classifies the text of a LiteralExpr into a typed
// Value. Quoted literals are always strings: the user opted into
// stringy interpretation by quoting. Unquoted literals are
// classified by shape: "true"/"false" become BoolValue, anything
// strconv.ParseFloat accepts becomes a numeric Value carrying the
// original text as a json.Number, and everything else stays a
// string. This is jq's literal model: bare 5 is the number 5, "5"
// is the string "5", and the comparison operator picks numeric or
// textual semantics from the operand types rather than from the
// operator's spelling.
func literalValue(e *LiteralExpr) Value {
	if e.Quoted {
		return StringValue(e.Text)
	}
	switch e.Text {
	case "true":
		return BoolValue(true)
	case "false":
		return BoolValue(false)
	}
	if _, err := strconv.ParseFloat(e.Text, 64); err == nil {
		return Value{v: json.Number(e.Text), kind: OriginScalar}
	}
	return StringValue(e.Text)
}

// evalNegate evaluates a unary '-' prefix.  The operand must
// reduce to a numeric scalar; anything else is a type error
// cited at the '-' token.
func evalNegate(e *NegateExpr, env *Env) (Value, error) {
	v, err := EvalExpr(e.Operand, env)
	if err != nil {
		return Value{}, err
	}
	s, err := v.Scalar()
	if err != nil {
		return Value{}, spanErrorf(e.Span, "negate: %v", err)
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return Value{}, spanErrorf(e.Span, "negate: operand %q is not numeric", s)
	}
	return numericValue(-x), nil
}

func evalUnary(e *UnaryExpr, env *Env) (Value, error) {
	operand, err := EvalExpr(e.Operand, env)
	if err != nil {
		return Value{}, err
	}
	switch e.Pred {
	case "not-empty":
		s, err := operand.Scalar()
		if err != nil {
			return Value{}, spanErrorf(e.Span, "not-empty: %v", err)
		}
		return BoolValue(s != ""), nil
	default:
		return Value{}, spanErrorf(e.Span, "unknown unary predicate %q", e.Pred)
	}
}

// evalSymbolicTextComparison compares two strings under the
// canonical symbol-form operator. Both operands have already been
// reduced to their scalar text by evalCompare; this function is the
// single textual-compare path for strings and (after the canonical
// "true"/"false" rendering) booleans.
func evalSymbolicTextComparison(op, left, right string) (Value, error) {
	var pass bool
	switch op {
	case "==":
		pass = left == right
	case "!=":
		pass = left != right
	case "<":
		pass = left < right
	case "<=":
		pass = left <= right
	case ">":
		pass = left > right
	case ">=":
		pass = left >= right
	default:
		return Value{}, fmt.Errorf("unknown textual operator %q", op)
	}
	return BoolValue(pass), nil
}

func evalNumericComparison(op, left, right string) (Value, error) {
	a, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return Value{}, fmt.Errorf("left operand %q is not numeric", left)
	}
	b, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return Value{}, fmt.Errorf("right operand %q is not numeric", right)
	}
	var pass bool
	switch op {
	case "==":
		pass = a == b
	case "!=":
		pass = a != b
	case "<":
		pass = a < b
	case "<=":
		pass = a <= b
	case ">":
		pass = a > b
	case ">=":
		pass = a >= b
	default:
		return Value{}, fmt.Errorf("unknown numeric operator %q", op)
	}
	return BoolValue(pass), nil
}

// ExprFromArgs rebuilds a primary/unary/binary expression from a
// list of already-evaluated arguments. The assertion layer calls
// this to re-interpret a command's evaluated args as a comparison
// or predicate expression and then evaluate the whole thing via
// EvalExpr. The returned expression has zero Pos on every node
// because the original token positions are not available at this
// point in the pipeline.
func ExprFromArgs(args []Arg) (Expr, error) {
	switch len(args) {
	case 0:
		return nil, fmt.Errorf("empty expression")
	case 1:
		return argToPrimary(args[0])
	case 2:
		pred, ok := argAsUnaryPred(args[0])
		if !ok {
			return nil, fmt.Errorf("expected a check like 'not-empty' as the first word, got %q", argDisplay(args[0]))
		}
		operand, err := argToPrimary(args[1])
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand}, nil
	case 3:
		op, ok := argAsBinaryOp(args[1])
		if !ok {
			return nil, fmt.Errorf("expected an operator (==, !=, <, <=, >, >=) between the two values, got %q", argDisplay(args[1]))
		}
		left, err := argToPrimary(args[0])
		if err != nil {
			return nil, err
		}
		right, err := argToPrimary(args[2])
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Left: left, Op: op, Right: right}, nil
	default:
		return nil, fmt.Errorf("got %d argument(s); expected one value, a unary check like 'not-empty $x', or a comparison like '$x == 5'", len(args))
	}
}

func argToPrimary(a Arg) (Expr, error) {
	switch v := a.(type) {
	case WordArg:
		return &LiteralExpr{Text: v.Text}, nil
	case QuotedArg:
		return &LiteralExpr{Text: v.Text, Quoted: true}, nil
	case ScalarValueArg:
		return &LiteralExpr{Text: v.Text}, nil
	case StructuredValueArg:
		return &VarRefExpr{Name: v.Name}, nil
	default:
		return nil, fmt.Errorf("cannot use %T as expression operand", a)
	}
}

func argAsUnaryPred(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok || !IsUnaryPred(w.Text) {
		return "", false
	}
	return w.Text, true
}

func argAsBinaryOp(a Arg) (string, bool) {
	w, ok := a.(WordArg)
	if !ok || !IsBinaryOp(w.Text) {
		return "", false
	}
	return w.Text, true
}

func argDisplay(a Arg) string {
	switch v := a.(type) {
	case WordArg:
		return v.Text
	case QuotedArg:
		return v.Text
	case ScalarValueArg:
		return v.Text
	case StructuredValueArg:
		return "$" + v.Name
	default:
		return fmt.Sprintf("%T", a)
	}
}

// AsBool extracts a boolean from a Value. It succeeds when the
// underlying raw value is a Go bool, regardless of the OriginKind
// tag: comparison results carry OriginBool explicitly, while a
// path lookup that lands on a JSON boolean field arrives with
// kind OriginUnknown but raw type bool. Both should drive
// if/assert truthiness without forcing the caller to add a
// redundant "== true". Anything else returns a type error.
func AsBool(v Value) (bool, error) {
	if b, ok := v.Raw().(bool); ok {
		return b, nil
	}
	if v.Kind() == OriginBool {
		return false, fmt.Errorf("condition has boolean origin but non-boolean value %T", v.Raw())
	}
	return false, fmt.Errorf("condition is a %s; use a comparison like '$x == 5' or a check like 'not-empty $x' to produce a boolean", v.Kind())
}

// exprLoc extracts the Pos embedded in any Expr variant. Used for
// error formatting where the caller has the Expr but not the
// concrete type.
func exprLoc(e Expr) Pos {
	switch v := e.(type) {
	case *LiteralExpr:
		return v.Pos
	case *VarRefExpr:
		return v.Pos
	case *AdapterExpr:
		return v.Pos
	case *InterpStringExpr:
		return v.Pos
	case *BinaryExpr:
		return v.Pos
	case *UnaryExpr:
		return v.Pos
	case *ThreadExpr:
		return v.Pos
	case *LogicalExpr:
		return v.Pos
	case *NotExpr:
		return v.Pos
	case *NegateExpr:
		return v.Pos
	case *TimeoutExpr:
		return v.Pos
	case *IterationExpr:
		return v.Pos
	case *MatchesBlockExpr:
		return v.Pos
	}
	return Pos{}
}
