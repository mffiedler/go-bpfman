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
//	primary := LiteralExpr | VarRefExpr | AdapterExpr | CmdSubExpr
//	unary   := UnaryExpr (pred operand)
//	binary  := BinaryExpr (left op right)
type Expr interface {
	exprNode()
}

// LiteralExpr wraps a word or quoted-string token. Quoted records
// whether the operand came from a quoted literal; callers use it
// to preserve quoting semantics when rebuilding arguments for
// dispatch.
type LiteralExpr struct {
	Text   string
	Quoted bool
	Loc
}

// VarRefExpr is a variable reference with an optional field/index
// path. The referenced [Value] is resolved at evaluation time
// against the Session on the Env.
type VarRefExpr struct {
	Name string
	Path string
	Loc
}

// AdapterExpr is an adapter-decorated variable reference such as
// file:$var.path. Adapters only make sense in command-argument
// position; using one as an expression operand is a runtime error.
type AdapterExpr struct {
	Adapter string
	Name    string
	Path    string
	Loc
}

// CmdSubExpr is a command substitution [cmd args...]. Inner is the
// parsed inner program; at evaluation time the evaluator dispatches
// its single CommandStmt via Env.ExecSubstitution and returns the
// resulting Value.
type CmdSubExpr struct {
	Inner *Program
	Loc
}

// ExprSubExpr is an expression substitution [[expr]].  The grammar
// inside double brackets is the same expression grammar used
// everywhere else, but the tokeniser runs in strict mode so '-'
// and '/' split as operators.  At evaluation time the node
// delegates to its Inner expression; the wrapper exists so source
// locations point at the '[[' rather than at whatever primary
// happens to sit at the head of the inner expression.
type ExprSubExpr struct {
	Inner Expr
	Loc
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
	Loc
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
	Loc
}

// UnaryExpr is a single-operand predicate. Pred is the only
// surviving unary predicate, "not-empty"; truthiness is read
// directly via AsBool on a single-arg expression assertion, and
// "true"/"false" are bare boolean literals (see literalValue).
// Evaluation produces a BoolValue.
type UnaryExpr struct {
	Pred    string
	Operand Expr
	Loc
}

// ThreadExpr is a value-threading composition: evaluate LHS to a
// Value, append that Value as the last argument of the command
// described by Args, and dispatch. The operator binds tighter than
// comparison operators but looser than the primary forms. Loc
// identifies the '|>' token itself.
type ThreadExpr struct {
	LHS  Expr
	Args []Expr
	Loc
}

// LogicalExpr is a short-circuit boolean combinator.  Op is
// either "and" or "or"; both operands must evaluate to a
// BoolValue at runtime.  Loc identifies the operator token.
type LogicalExpr struct {
	Op          string
	Left, Right Expr
	Loc
}

// NotExpr is boolean negation.  The operand must evaluate to a
// BoolValue at runtime.  Loc identifies the 'not' token.
type NotExpr struct {
	Operand Expr
	Loc
}

// NegateExpr is arithmetic negation.  The operand must evaluate
// to a numeric scalar at runtime; anything else is a type error
// cited at the '-' token.  Kept distinct from NotExpr so the
// two unary forms live in separate namespaces.
type NegateExpr struct {
	Operand Expr
	Loc
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
	Loc
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
	Loc
}

func (*LiteralExpr) exprNode()      {}
func (*VarRefExpr) exprNode()       {}
func (*AdapterExpr) exprNode()      {}
func (*CmdSubExpr) exprNode()       {}
func (*ExprSubExpr) exprNode()      {}
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
// the variable and alias store; ExecCommand and ExecSubstitution
// dispatch commands to the REPL's shell and domain pipelines,
// differing only in output visibility and return-value
// requirements.
//
// A nil ExecCommand makes any top-level CommandStmt a runtime
// error; a nil ExecSubstitution makes any CmdSubExpr a runtime
// error. Tests that only exercise expression evaluation can leave
// both unset.
type Env struct {
	Session *Session

	// ExecCommand runs a top-level CommandStmt. The returned
	// Value may be nil; any output is visible on the CLI.
	ExecCommand func(args []Arg) (Value, error)

	// ExecSubstitution runs a command inside a cmd-sub
	// expression. Output is suppressed; the returned Value must
	// be non-nil or the evaluator reports an error.
	ExecSubstitution func(args []Arg) (Value, error)

	// ExecBind runs a command form on the right of a '<-' bind.
	// The returned Envelope is always populated: command failure
	// (non-zero exit, in-process error) is encoded as OK: false
	// with code, stdout, and stderr set, not as a Go error. A Go
	// error is reserved for structural failures (empty argv,
	// malformed adapter, no provider for this hook) that the
	// language cannot recover from. Set by the REPL driver; nil
	// makes any BindStmt a runtime error.
	ExecBind func(args []Arg) (Envelope, error)

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
			return err
		}
	}
	return nil
}

// stmtLoc returns the Loc on any Stmt variant.  Used by
// EvalProgram to cite a source location when break / continue
// reach the top level without being caught by a loop.
func stmtLoc(s Stmt) Loc {
	switch v := s.(type) {
	case *LetStmt:
		return v.Loc
	case *BindStmt:
		return v.Loc
	case *IfStmt:
		return v.Loc
	case *CommandStmt:
		return v.Loc
	case *ExprStmt:
		return v.Loc
	case *ForEachStmt:
		return v.Loc
	case *RetryStmt:
		return v.Loc
	case *BreakStmt:
		return v.Loc
	case *ContinueStmt:
		return v.Loc
	case *DefStmt:
		return v.Loc
	case *AssertStmt:
		return v.Loc
	}
	return Loc{}
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
			return locErrorf(s.Loc, "expression produced no result to assign")
		}
		env.Session.Set(s.Name, val)
		return nil
	case *BindStmt:
		return evalBindStmt(s, env)
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
			return locErrorf(s.Loc, "assert/require has no executor on the env")
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
		Loc:    s.Loc,
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
func callDef(def *DefValue, args []Arg, callLoc Loc, env *Env) error {
	if len(args) != len(def.Params) {
		return locErrorf(callLoc, "%s: expected %d argument(s), got %d (def declared at %d:%d)",
			def.Name, len(def.Params), len(args), def.Loc.Line, def.Loc.Col)
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
	for _, stmt := range def.Body {
		if err := evalStmt(stmt, env); err != nil {
			return err
		}
	}
	return nil
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
// enough to avoid pegging the CPU for long-running checks.  A
// future enhancement could expose this via a CLI flag or
// environment variable.
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
			return locErrorf(s.Loc, "retry: until expression: %v", err)
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
		return Value{}, locErrorf(e.Loc, "timeout expression is only valid inside a retry body or its until clause")
	}
	v, err := EvalExpr(e.Arg, env)
	if err != nil {
		return Value{}, err
	}
	s, err := v.Scalar()
	if err != nil {
		return Value{}, locErrorf(e.Loc, "timeout: argument must be a scalar duration: %v", err)
	}
	d, err := parseDurationLiteral(s)
	if err != nil {
		return Value{}, locErrorf(e.Loc, "timeout: %v", err)
	}
	return BoolValue(time.Since(env.retryStart) >= d), nil
}

// evalIterationExpr returns true when the current retry has
// executed at least the iteration count produced by e.Arg.
// Outside a retry context (Env.retryIter zero) it errors.
func evalIterationExpr(e *IterationExpr, env *Env) (Value, error) {
	if env.retryIter == 0 {
		return Value{}, locErrorf(e.Loc, "iteration expression is only valid inside a retry body or its until clause")
	}
	v, err := EvalExpr(e.Arg, env)
	if err != nil {
		return Value{}, err
	}
	s, err := v.Scalar()
	if err != nil {
		return Value{}, locErrorf(e.Loc, "iteration: argument must be a scalar count: %v", err)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return Value{}, locErrorf(e.Loc, "iteration: %q is not a valid integer count", s)
	}
	if n < 0 {
		return Value{}, locErrorf(e.Loc, "iteration: count %d is negative", n)
	}
	return BoolValue(env.retryIter >= n), nil
}

func evalForEachStmt(s *ForEachStmt, env *Env) error {
	v, err := EvalExpr(s.List, env)
	if err != nil {
		return err
	}
	if v.IsNil() {
		return locErrorf(s.Loc, "foreach: list expression is null")
	}
	list, ok := v.Raw().([]any)
	if !ok {
		return locErrorf(s.Loc, "foreach: expected a list, got %s", v.Kind())
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
		return AsBool(v)
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

// evalBindStmt runs the command form on the right of a '<-' bind,
// captures the result as an Envelope, and binds it to s.Name. When
// s.Guard is true a not-ok envelope halts the script via
// HaltOnGuardFailure; when s.Guard is false the bind always
// succeeds at the language level and the consumer inspects
// $name.ok / $name.code itself.
func evalBindStmt(s *BindStmt, env *Env) error {
	if env.ExecBind == nil {
		return locErrorf(s.Loc, "'<-' bind: command execution is not configured")
	}
	args, err := EvalArgs(s.Cmd.Args, env)
	if err != nil {
		return err
	}
	envelope, err := env.ExecBind(args)
	if err != nil {
		return err
	}
	env.Session.Set(s.Name, ValueFromEnvelope(envelope))
	if s.Guard && !envelope.OK {
		return &GuardFailure{Loc: s.Loc, Name: s.Name, Args: args, Envelope: envelope}
	}
	return nil
}

// GuardFailure is the error type a 'guard NAME <- CMD' statement
// returns when the captured envelope is not ok. The driver formats
// the failure through its renderer; the language layer carries the
// envelope so the renderer has the captured stdout, stderr, exit
// code, and the offending bind's source location, plus the
// resolved Args so the renderer can show the command line that
// failed.
type GuardFailure struct {
	Loc      Loc
	Name     string
	Args     []Arg
	Envelope Envelope
}

func (e *GuardFailure) Error() string {
	if e.Envelope.Stderr != "" {
		return fmt.Sprintf("guard %s: command failed (exit %d): %s",
			e.Name, e.Envelope.Code, e.Envelope.Stderr)
	}
	return fmt.Sprintf("guard %s: command failed (exit %d)", e.Name, e.Envelope.Code)
}

func evalCommandStmt(s *CommandStmt, env *Env) error {
	args, err := EvalArgs(s.Args, env)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if name, ok := commandHeadName(args[0]); ok {
			if def, hit := env.Session.GetDef(name); hit {
				return callDef(def, args[1:], s.Loc, env)
			}
		}
	}
	if env.ExecCommand == nil {
		return locErrorf(s.Loc, "command execution is not configured")
	}
	_, err = env.ExecCommand(args)
	return err
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
func EvalExpr(expr Expr, env *Env) (Value, error) {
	switch e := expr.(type) {
	case *LiteralExpr:
		return literalValue(e), nil
	case *VarRefExpr:
		return resolveVarRefValue(e, env)
	case *AdapterExpr:
		return Value{}, locErrorf(e.Loc, "adapter %s:$%s cannot be used as an expression operand", e.Adapter, e.Name)
	case *CmdSubExpr:
		return dispatchCmdSub(e, env)
	case *ExprSubExpr:
		return EvalExpr(e.Inner, env)
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
		return Value{}, locErrorf(e.Loc, "%s: left: %v", e.Op, err)
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
		return Value{}, locErrorf(e.Loc, "unknown logical operator %q", e.Op)
	}
	rightV, err := EvalExpr(e.Right, env)
	if err != nil {
		return Value{}, err
	}
	rightB, err := AsBool(rightV)
	if err != nil {
		return Value{}, locErrorf(e.Loc, "%s: right: %v", e.Op, err)
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
		return Value{}, locErrorf(e.Loc, "not: %v", err)
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
			return Value{}, locErrorf(exprLoc(seg.Expr), "interpolation: %v", err)
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
// returns the resulting []Arg, suitable for dispatch. Command
// substitutions nested inside the list are evaluated via
// Env.ExecSubstitution, with their results wrapped as
// ScalarValueArg/StructuredValueArg according to their shape.
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
			return QuotedArg{Text: e.Text}, nil
		}
		return WordArg{Text: e.Text}, nil
	case *VarRefExpr:
		return resolveVarRefArg(e, env)
	case *AdapterExpr:
		return resolveAdapterArg(e, env)
	case *CmdSubExpr:
		val, err := dispatchCmdSub(e, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, locErrorf(e.Loc, "nested command substitution produced no value")
		}
		if val.IsStructured() {
			return StructuredValueArg{Value: val}, nil
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "nested command substitution: %v", err)
		}
		return ScalarValueArg{Text: s}, nil
	case *ExprSubExpr:
		val, err := EvalExpr(e.Inner, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, locErrorf(e.Loc, "expression substitution produced no value")
		}
		if val.IsStructured() {
			return StructuredValueArg{Value: val}, nil
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "expression substitution: %v", err)
		}
		return ScalarValueArg{Text: s}, nil
	case *InterpStringExpr:
		val, err := evalInterpString(e, env)
		if err != nil {
			return nil, err
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "interpolated string: %v", err)
		}
		return ScalarValueArg{Text: s}, nil
	case *ThreadExpr:
		val, err := dispatchThread(e, env)
		if err != nil {
			return nil, err
		}
		if val.IsNil() {
			return nil, locErrorf(e.Loc, "thread produced no value")
		}
		if val.IsStructured() {
			return StructuredValueArg{Value: val}, nil
		}
		s, err := val.Scalar()
		if err != nil {
			return nil, locErrorf(e.Loc, "thread: %v", err)
		}
		return ScalarValueArg{Text: s}, nil
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
		return Value{}, locErrorf(e.Loc, "undefined variable %q", e.Name)
	}
	if e.Path == "" {
		return v, nil
	}
	return v.LookupValue(e.Name, e.Path)
}

func resolveVarRefArg(e *VarRefExpr, env *Env) (Arg, error) {
	v, ok := env.Session.Get(e.Name)
	if !ok {
		return nil, locErrorf(e.Loc, "undefined variable: %s", e.Name)
	}
	resolved := v
	if e.Path != "" {
		lv, err := v.LookupValue(e.Name, e.Path)
		if err != nil {
			return nil, err
		}
		resolved = lv
	}
	if resolved.IsNil() {
		return nil, locErrorf(e.Loc, "variable %s is null", qualify(e.Name, e.Path))
	}
	if resolved.IsStructured() {
		return StructuredValueArg{Name: e.Name, Value: resolved}, nil
	}
	s, err := resolved.Scalar()
	if err != nil {
		return nil, locErrorf(e.Loc, "variable %s: %v", qualify(e.Name, e.Path), err)
	}
	return ScalarValueArg{Text: s}, nil
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
		return nil, locErrorf(e.Loc, "undefined variable: %s", e.Name)
	}
	resolved := v
	if e.Path != "" {
		lv, err := v.LookupValue(e.Name, e.Path)
		if err != nil {
			return nil, err
		}
		resolved = lv
	}
	if resolved.IsNil() {
		return nil, locErrorf(e.Loc, "adapter %s: variable %s is null", e.Adapter, e.Name)
	}
	return AdapterArg{
		Adapter: e.Adapter,
		Name:    e.Name,
		Path:    e.Path,
		Value:   resolved,
	}, nil
}

// dispatchThread evaluates a threading expression by threading the LHS's
// Value into the command described by Args.  The LHS Value
// becomes the last element of the evaluated argument list so it
// matches the convention used by jq, file temp, and most
// shell-style "CMD ARGS VALUE" invocations.
func dispatchThread(e *ThreadExpr, env *Env) (Value, error) {
	if env.ExecSubstitution == nil {
		return Value{}, locErrorf(e.Loc, "thread requires a substitution runner; none configured")
	}
	lhsVal, err := EvalExpr(e.LHS, env)
	if err != nil {
		return Value{}, err
	}
	if lhsVal.IsNil() {
		return Value{}, locErrorf(e.Loc, "thread: left-hand side is null")
	}
	args, err := EvalArgs(e.Args, env)
	if err != nil {
		return Value{}, err
	}
	lhsArg, err := valueToArg(lhsVal)
	if err != nil {
		return Value{}, locErrorf(e.Loc, "thread: %v", err)
	}
	return env.ExecSubstitution(append(args, lhsArg))
}

// valueToArg wraps a Value in the most specific Arg variant for
// the dispatch boundary: structured values stay structured,
// scalars become ScalarValueArg, nil is a caller problem.
func valueToArg(v Value) (Arg, error) {
	if v.IsNil() {
		return nil, fmt.Errorf("value is null")
	}
	if v.IsStructured() {
		return StructuredValueArg{Value: v}, nil
	}
	s, err := v.Scalar()
	if err != nil {
		return nil, err
	}
	return ScalarValueArg{Text: s}, nil
}

// dispatchCmdSub evaluates a command-substitution expression and
// returns its value.  The inner program must contain exactly one
// statement.  An ExprStmt is a bracketed expression ("[1 == 1]",
// "[$x == $y]") and is evaluated directly.  A CommandStmt is a
// command invocation ("[bpfman ...]", "[jq ...]") and is dispatched
// via env.ExecSubstitution.  Any other statement form — let, if,
// foreach, retry, break, continue — is rejected.
func dispatchCmdSub(e *CmdSubExpr, env *Env) (Value, error) {
	if e.Inner == nil || len(e.Inner.Stmts) == 0 {
		return Value{}, locErrorf(e.Loc, "empty command substitution")
	}
	if len(e.Inner.Stmts) != 1 {
		return Value{}, locErrorf(e.Loc, "command substitution must contain a single command or expression")
	}
	switch stmt := e.Inner.Stmts[0].(type) {
	case *ExprStmt:
		return EvalExpr(stmt.Expr, env)
	case *CommandStmt:
		if env.ExecSubstitution == nil {
			return Value{}, locErrorf(e.Loc, "command substitution is not permitted in this context")
		}
		args, err := EvalArgs(stmt.Args, env)
		if err != nil {
			return Value{}, err
		}
		return env.ExecSubstitution(args)
	default:
		return Value{}, locErrorf(e.Loc, "command substitution must contain a command or expression, got %T", stmt)
	}
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
			return Value{}, locErrorf(e.Loc, "binary %s: left: %v", e.Op, err)
		}
		right, err := rightV.Scalar()
		if err != nil {
			return Value{}, locErrorf(e.Loc, "binary %s: right: %v", e.Op, err)
		}
		v, err := evalArithmetic(e.Op, left, right)
		if err != nil {
			return Value{}, locErrorf(e.Loc, "%v", err)
		}
		return v, nil
	}
	return evalCompare(e.Op, leftV, rightV, e.Loc)
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
func evalCompare(op string, l, r Value, loc Loc) (Value, error) {
	lk := compareKind(l)
	rk := compareKind(r)
	if lk == "" {
		return Value{}, locErrorf(loc, "binary %s: left operand is not a comparable scalar (got %T)", op, l.Raw())
	}
	if rk == "" {
		return Value{}, locErrorf(loc, "binary %s: right operand is not a comparable scalar (got %T)", op, r.Raw())
	}
	if lk != rk {
		return Value{}, locErrorf(loc, "binary %s: cannot compare %s to %s; coerce explicitly (e.g. \"$x |> jq tonumber\" for stringy numeric input)", op, lk, rk)
	}
	left, err := l.Scalar()
	if err != nil {
		return Value{}, locErrorf(loc, "binary %s: left: %v", op, err)
	}
	right, err := r.Scalar()
	if err != nil {
		return Value{}, locErrorf(loc, "binary %s: right: %v", op, err)
	}
	switch lk {
	case "number":
		return evalNumericComparison(op, left, right)
	case "bool":
		if op != "==" && op != "!=" {
			return Value{}, locErrorf(loc, "binary %s: booleans support only == and !=", op)
		}
		return evalSymbolicTextComparison(op, left, right)
	default:
		return evalSymbolicTextComparison(op, left, right)
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
		return Value{}, locErrorf(e.Loc, "negate: %v", err)
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return Value{}, locErrorf(e.Loc, "negate: operand %q is not numeric", s)
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
			return Value{}, fmt.Errorf("not-empty: %w", err)
		}
		return BoolValue(s != ""), nil
	default:
		return Value{}, fmt.Errorf("unknown unary predicate %q", e.Pred)
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
// EvalExpr. The returned expression has zero Loc on every node
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
			return nil, fmt.Errorf("expected unary predicate as first operand, got %q", argDisplay(args[0]))
		}
		operand, err := argToPrimary(args[1])
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Pred: pred, Operand: operand}, nil
	case 3:
		op, ok := argAsBinaryOp(args[1])
		if !ok {
			return nil, fmt.Errorf("expected binary operator as middle operand, got %q", argDisplay(args[1]))
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
		return nil, fmt.Errorf("expression has %d operands; expected 1 (primary), 2 (unary) or 3 (binary)", len(args))
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
	return false, fmt.Errorf("condition is not a boolean (got %s); use a comparison or unary predicate", v.Kind())
}

// exprLoc extracts the Loc embedded in any Expr variant. Used for
// error formatting where the caller has the Expr but not the
// concrete type.
func exprLoc(e Expr) Loc {
	switch v := e.(type) {
	case *LiteralExpr:
		return v.Loc
	case *VarRefExpr:
		return v.Loc
	case *AdapterExpr:
		return v.Loc
	case *CmdSubExpr:
		return v.Loc
	case *ExprSubExpr:
		return v.Loc
	case *InterpStringExpr:
		return v.Loc
	case *BinaryExpr:
		return v.Loc
	case *UnaryExpr:
		return v.Loc
	case *ThreadExpr:
		return v.Loc
	case *LogicalExpr:
		return v.Loc
	case *NotExpr:
		return v.Loc
	case *NegateExpr:
		return v.Loc
	case *TimeoutExpr:
		return v.Loc
	case *IterationExpr:
		return v.Loc
	case *MatchesBlockExpr:
		return v.Loc
	}
	return Loc{}
}
