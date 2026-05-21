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

// PureCallExpr is an expression-position invocation of a pure
// builtin: a name registered in the pure-builtin registry plus
// exactly Arity primary expressions as arguments. The parser
// emits it when it sees a bare identifier in expression position
// whose text matches a registration; the eval path dispatches
// through ExecBind so the handler resolution is identical to the
// '<-' bind form, but the result envelope is discarded and only
// the primary Value flows back into the surrounding expression.
//
// Pure-builtin handlers are by contract side-effect-free and
// return a typed Value rather than an envelope, so the discarded
// envelope carries no information the script could observe. A
// handler failure is an evaluation error cited at PureCallExpr's
// Span, not a captured result the caller could inspect.
type PureCallExpr struct {
	Name string
	Args []Expr
	Span
}

// ListExpr is an inline list literal: [elem elem ...] with
// whitespace-separated elements. Each Elem is parsed as a
// primary expression (the parseTerm level), so a compound
// element must wrap in parens: [10 20 ($base + 30)]. Bare
// words become string literals via the usual literalValue
// rules, but unquoted identifiers that are not numeric or
// boolean still flow through as strings -- there is no
// bareword-disallow rule at the AST level; the grammar note
// just observes that the natural style is to quote string
// elements. Evaluation produces a Value whose underlying
// representation is []any, matching what the range pure
// builtin returns.
type ListExpr struct {
	Elems []Expr
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
func (*PureCallExpr) exprNode()     {}
func (*ListExpr) exprNode()         {}

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

	// RenderDeferOutput fires after every defer dispatch, win or
	// lose, so the driver can flush the deferred command's
	// captured stdout/stderr to its terminal. Defers go through
	// ExecBind, which captures output into the rc envelope (rc.
	// Stdout / rc.Stderr); without this hook the captured bytes
	// are dropped on the floor and `defer print "trace"` is
	// silent. A nil callback preserves the historical drop-the-
	// output behaviour for tests and embedders that do not want
	// side output during cleanup. The failure-path rendering in
	// RenderDeferFailure still shows the captured streams in
	// its labelled block, but the standalone success-output flow
	// is the job of this hook.
	RenderDeferOutput func(args []Arg, rc Envelope)

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

	// Trace, when non-nil, is invoked just before a statement
	// executes (and again when a deferred command fires at scope
	// exit). line is the statement's chunk-relative source line;
	// rendered is a one-line summary of the statement with
	// interpolations resolved, e.g. an argv with `$prog`
	// substituted by its compact-JSON form, or `let x = <value>`
	// after the RHS has evaluated. Drivers typically prepend
	// `file:line:` and write the result to stderr. shell/ never
	// decides whether to trace; it only emits when the callback
	// is non-nil, so policy (a `trace on` toggle, a CLI flag)
	// lives in the driver-side installer.
	Trace func(line int, rendered string)

	// RenderEventuallyFailure, when set, is invoked when an
	// `eventually` statement form runs out of retry budget. The
	// per-attempt diagnostic is suppressed during the construct
	// (retryable failures are polling state, not user-visible
	// failures) so the construct's overall outcome is the only
	// natural place to report. lastErr is the last retryable
	// failure returned by an attempt; the driver-side handler
	// renders a "file:line: eventually: timed out (...)" summary
	// to stderr. A nil callback discards the rendering; the
	// failure still propagates as an error.
	RenderEventuallyFailure func(span Span, attempts int, elapsedMs int64, lastErr error)

	// ChunkFile and ChunkStartLine identify the source file
	// and start line of the chunk currently being parsed and
	// evaluated. The chunk loop sets them from its loc; the
	// evaluator captures them when a construct (def) is
	// registered so a later error escaping its body can be
	// decorated with the correct absolute file+line, even when
	// the call happens from a different chunk or after a
	// sourced library has finished. An empty ChunkFile means
	// the caller did not install loc-aware framing; downstream
	// decoration paths skip the decoration in that case.
	ChunkFile      string
	ChunkStartLine int

	// currentDefRegStart is the RegStartLine of the def whose
	// body is currently being evaluated, or 0 at top level.
	// runDefCall pushes the called def's RegStartLine on entry
	// and restores the prior value on exit, so a nested call
	// inside a def body resolves "where am I in the file" by
	// looking at the current def's registration chunk rather
	// than at env.ChunkStartLine (which still names the
	// outermost user chunk that started the call). absoluteCallLoc
	// reads this so the "called at L:C" annotation in
	// decorateDefError reports the calling def's body line, not
	// the surrounding user chunk's start. The driver's trace
	// renderer in cmd/bpfman-shell/repl/loop.go consults the
	// same value via CurrentDefRegStart() so `--trace` lines
	// emitted from inside a def body cite the def body's own
	// file lines.
	currentDefRegStart int

	// defCallDepth counts the def-call frames currently active
	// on the evaluator's stack. runDefCall increments on entry
	// and decrements on exit; a value over MaxDefCallDepth is a
	// clean failure rather than a Go-runtime stack overflow.
	// Runaway recursion -- the natural shape of a value-returning
	// helper that forgets its base case -- otherwise dumps pages
	// of goroutine traces, which is unkind. The cap is far below
	// Go's stack limit so the diagnostic always wins.
	defCallDepth int
}

// CurrentDefRegStart returns the file line at which the def
// whose body is currently being evaluated was registered, or 0
// at top level. Drivers that translate chunk-relative Pos
// values to absolute file lines -- the diagnostic renderer in
// decorateDefError, the `--trace` line emitter -- consult this
// so positions captured during def-body parsing are shifted by
// the def's registration chunk rather than by whatever
// top-level chunk is currently driving execution.
func (e *Env) CurrentDefRegStart() int {
	return e.currentDefRegStart
}

// MaxDefCallDepth bounds how deep def calls can nest before
// the evaluator surfaces a clean recursion-limit diagnostic.
// The number is deliberately a few orders of magnitude smaller
// than Go's default per-goroutine stack ceiling so the
// diagnostic fires before the runtime panics. Real corpus
// patterns nest a handful of frames at most (a load_xxx helper
// calling guard_attach_yyy, say); 256 leaves abundant slack
// while still catching the textbook "forgot the base case"
// mistake within a fraction of a second.
const MaxDefCallDepth = 256

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
			// A returnSignal escaped without a callDef catching
			// it. The static checker rejects the visible shapes
			// (return at script top level, return in a block
			// outside a def); this is the runtime safety net for
			// anything the checker did not see.
			var ret *returnSignal
			if errors.As(err, &ret) {
				return spanErrorf(ret.Span, "return outside a def body")
			}
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
	case *LetDestructureStmt:
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
	case *EventuallyStmt:
		return v.Pos
	case *BreakStmt:
		return v.Pos
	case *ContinueStmt:
		return v.Pos
	case *DefStmt:
		return v.Pos
	case *ReturnStmt:
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

// returnSignal is the internal control-flow value raised when a
// def body executes `return EXPR`. The evaluator surfaces it as
// an error so it short-circuits the body via the normal error
// path and is caught by callDef / callDefAsBind. Outside a def
// call -- at the top level of a script, inside a sourced library
// top level, or via any other path where no enclosing call
// exists -- returnSignal escapes evalProgramBody and is converted
// into a user-facing "return outside a def body" diagnostic. The
// value itself, once evaluated, is independent of the call frame:
// it survives the frame pop intact and becomes the bind path's
// Primary.
type returnSignal struct {
	Span
	Value Value
}

func (e *returnSignal) Error() string {
	return "return outside a def body"
}

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
		if env.Trace != nil {
			rendered, rerr := RenderCompact(val)
			if rerr != nil {
				rendered = fmt.Sprintf("<unrenderable %s>", val.Kind())
			}
			env.Trace(s.Span.Pos.Line, fmt.Sprintf("let %s = %s", s.Name, rendered))
		}
		env.Session.Set(s.Name, val)
		return nil
	case *LetDestructureStmt:
		return evalLetDestructureStmt(s, env)
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
	case *EventuallyStmt:
		return evalEventuallyStmt(s, env)
	case *BreakStmt:
		return errBreak
	case *ContinueStmt:
		return errContinue
	case *DefStmt:
		return evalDefStmt(s, env)
	case *ReturnStmt:
		return evalReturnStmt(s, env)
	case *AssertStmt:
		if env.ExecAssertStmt == nil {
			return spanErrorf(s.Span, "assert/require has no executor on the env")
		}
		return env.ExecAssertStmt(s, env)
	default:
		return fmt.Errorf("unknown statement type %T", stmt)
	}
}

// evalLetDestructureStmt evaluates s.RHS, requires the value to be a
// list of length len(s.Names), and binds each non-'_' name to its
// positional element. Length mismatch or a non-list value is a
// runtime error cited at the let statement.
func evalLetDestructureStmt(s *LetDestructureStmt, env *Env) error {
	val, err := EvalExpr(s.RHS, env)
	if err != nil {
		return err
	}
	if val.IsNil() {
		return spanErrorf(s.Span, "let: destructure RHS produced no result")
	}
	sub, ok := val.Raw().([]any)
	if !ok {
		return spanErrorf(s.Span, "let: destructure RHS is not a list, cannot bind %d names", len(s.Names))
	}
	if len(sub) != len(s.Names) {
		return spanErrorf(s.Span, "let: destructure RHS has %d elements, cannot bind %d names", len(sub), len(s.Names))
	}
	if env.Trace != nil {
		parts := make([]string, 0, len(s.Names))
		for j, name := range s.Names {
			rendered, rerr := RenderCompact(val.IndexValue(j))
			if rerr != nil {
				rendered = fmt.Sprintf("<unrenderable %T>", sub[j])
			}
			parts = append(parts, fmt.Sprintf("%s=%s", name, rendered))
		}
		env.Trace(s.Span.Pos.Line, "let "+strings.Join(parts, " "))
	}
	for j, name := range s.Names {
		if name == "_" {
			continue
		}
		env.Session.Set(name, val.IndexValue(j))
	}
	return nil
}

// evalReturnStmt evaluates the return value and raises a
// returnSignal, which propagates as an error through evalStmt and
// the enclosing block loop until callDef / callDefAsBind catches
// it. The expression is evaluated in the current frame so the
// value uses the call frame's bindings; once stashed on the
// signal, the Value is independent of the frame and survives the
// frame pop.
//
// If no enclosing def call catches the signal -- a script-top-level
// `return`, a return inside an `if` or `foreach` that is not inside
// any def -- evalProgramBody surfaces it as a regular runtime
// error and the script halts. The static checker rejects the
// same shapes earlier; this is the safety net for paths the
// checker may not see (e.g. a `return` reached only via the
// runtime def-dispatch routing).
func evalReturnStmt(s *ReturnStmt, env *Env) error {
	v, err := EvalExpr(s.Expr, env)
	if err != nil {
		return err
	}
	if v.IsNil() {
		return spanErrorf(s.Span, "return: expression produced no result")
	}
	// Trace the resolved return value at the body's line so a
	// `--trace` session sees the moment the value crosses the
	// call boundary. Statement-level trace already covers
	// let / bind / command / defer; return is the only other
	// value-producing form whose evaluation matters for
	// debugging a script. Use the same RenderCompact path as
	// let-style tracing so structured values render as JSON
	// and scalars render as their underlying text.
	if env.Trace != nil {
		rendered, rerr := RenderCompact(v)
		if rerr != nil {
			rendered = fmt.Sprintf("<unrenderable %s>", v.Kind())
		}
		env.Trace(s.Span.Pos.Line, fmt.Sprintf("return %s", rendered))
	}
	return &returnSignal{Span: s.Span, Value: v}
}

// evalDefStmt registers s in the session's def table. Redefining an
// existing def replaces it silently; this matches let and alias.
// The current chunk's file and start line are captured on the
// DefValue so a later body error escaping the call frame can be
// decorated with the registration site's absolute file+line.
func evalDefStmt(s *DefStmt, env *Env) error {
	env.Session.SetDef(&DefValue{
		Name:         s.Name,
		Params:       s.Params,
		Body:         s.Body,
		RegFile:      env.ChunkFile,
		RegStartLine: env.ChunkStartLine,
		Span:         s.Span,
	})
	return nil
}

// callDef binds def parameters from args and runs def.Body in
// env. Each call runs in its own session frame: parameters bind
// into the call frame, body-level `let` lives there too, and
// everything disappears when the call returns. Recursion works
// naturally because each call gets its own frame. Arity is
// checked against len(def.Params) and a mismatch yields a
// runtime error citing both the call site and the def's
// declaration site.
//
// Defs do not capture variable frames: the body resolves
// references against the caller's frame stack at call time plus
// its own call frame. Definition-time bindings are not part of
// the closure. If lexical capture becomes a need, that is a
// separate design.
func callDef(def *DefValue, args []Arg, callLoc Pos, env *Env) error {
	_, _, _, err := runDefCall(def, args, callLoc, env)
	if err != nil {
		return decorateDefError(err, def, absoluteCallLoc(callLoc, env))
	}
	// A `return EXPR` inside the body short-circuits the body
	// loop via returnSignal. At command-form position the value
	// is discarded; the early exit itself is the only observable
	// effect. callDefAsBind handles the bind-position case and
	// keeps the Value. Defer failures still increment the
	// session counter via runDefers so the script's exit code
	// reflects the failure even when the call discards the
	// value -- that view is global by design.
	return nil
}

// callDefAsBind runs def in bind position and packages the
// outcome as a BindResult. The body's `return EXPR` becomes
// Primary; a body that runs to completion without `return`
// produces Primary = ValueFromEnvelope(Rc), matching the
// no-payload command-bind family (exec, bpftool, wait). The Rc
// is OK by default; a failure from a defer registered in THIS
// def's body flips Rc.OK to false so a `guard p <- f` halts and
// a tuple bind `let (rc p) <- f` lets the caller see the
// cleanup outcome.
//
// The local-cleanup view is load-bearing: a nested helper
// invoked at command form during the body has already run its
// own defers and left any failures on the session counter; that
// counter is the global exit-code view, not "did this def's
// cleanup fail". runDefCall threads runDefers's local return up
// to here so the flip reflects only defers belonging to this
// def's body, matching the def-local cleanup contract in
// SCOPE-DESIGN.md Section 9.
//
// A non-return error from the body (unbound variable, type
// error, guard halt inside the body, parse error from a
// dynamic source, etc.) propagates as a Go error; the bind
// path then frames it and the calling script halts. No
// bindings happen in that case.
func callDefAsBind(def *DefValue, args []Arg, callLoc Pos, env *Env) (BindResult, error) {
	returned, hasReturn, localDeferFailures, err := runDefCall(def, args, callLoc, env)
	if err != nil {
		return BindResult{}, decorateDefError(err, def, absoluteCallLoc(callLoc, env))
	}
	rc := Envelope{OK: true}
	if localDeferFailures > 0 {
		rc.OK = false
	}
	primary := returned
	if !hasReturn {
		primary = ValueFromEnvelope(rc)
	}
	return BindResult{Rc: rc, Primary: primary}, nil
}

// runDefCall is the shared body of callDef and callDefAsBind. It
// checks arity, opens a fresh session frame, binds parameters,
// opens a fresh defer scope, runs the body, and translates a
// returnSignal escape into (Value, hasReturn=true, ...). A
// non-return error escapes as (zero, false, ..., err) so the
// caller decides whether to frame/decorate it. The third return
// is the local defer-failure count for this call's defer scope;
// callDefAsBind uses it to flip the bind-position Rc.OK without
// leaking nested calls' cleanup failures into the local view.
// Defer unwinding happens before the frame pop, matching the
// spec's order: return value -> stash -> defers -> frame pop.
func runDefCall(def *DefValue, args []Arg, callLoc Pos, env *Env) (Value, bool, int, error) {
	if len(args) != len(def.Params) {
		return Value{}, false, 0, locErrorf(callLoc, "%s: expected %d argument(s), got %d (def declared at %d:%d)",
			def.Name, len(def.Params), len(args), def.Pos.Line, def.Pos.Col)
	}
	// Catch runaway recursion before Go's stack does. The cap
	// is far below Go's per-goroutine stack ceiling so the
	// clean diagnostic wins over a runtime panic; the count is
	// pushed / popped around the body so unrelated calls do not
	// accumulate against the limit and a backtrack out of a
	// recursive helper resumes with the right depth.
	if env.defCallDepth >= MaxDefCallDepth {
		return Value{}, false, 0, locErrorf(callLoc, "in def %s: recursion depth limit exceeded (%d)", def.Name, MaxDefCallDepth)
	}
	env.defCallDepth++
	defer func() { env.defCallDepth-- }()
	// Track the active def's RegStartLine so nested call sites
	// inside the body translate against the def's parsing chunk
	// rather than against whatever top-level chunk is currently
	// driving execution. Save / restore so recursive calls and
	// def-inside-def calls each get the right context.
	savedDefRegStart := env.currentDefRegStart
	env.currentDefRegStart = def.RegStartLine
	defer func() { env.currentDefRegStart = savedDefRegStart }()
	var localDeferFailures int
	err := env.Session.WithFrame(func() error {
		for i, p := range def.Params {
			env.Session.Set(p, argToValue(args[i]))
		}
		// Manage the defer scope inline so the local failure
		// count for this call's stack is observable. The shared
		// runWithDeferScope discards the count, which is fine
		// for callers whose contract is the session-wide view.
		saved := env.defers
		var stack []deferEntry
		env.defers = &stack
		bodyErr := func() error {
			for _, stmt := range def.Body {
				if err := evalStmt(stmt, env); err != nil {
					return err
				}
			}
			return nil
		}()
		env.defers = saved
		localDeferFailures = runDefers(env, stack)
		return bodyErr
	})
	if err == nil {
		return Value{}, false, localDeferFailures, nil
	}
	var ret *returnSignal
	if errors.As(err, &ret) {
		return ret.Value, true, localDeferFailures, nil
	}
	return Value{}, false, localDeferFailures, err
}

// absoluteCallLoc translates a chunk-relative Pos to the
// file-absolute coordinates the diagnostic renderer cites.
//
// Inside a def body, the BindStmt / CommandStmt whose evaluator
// triggered the call has positions relative to the chunk that
// PARSED that def, not relative to the chunk currently driving
// execution. runDefCall threads the active def's RegStartLine
// through env.currentDefRegStart; this helper prefers that
// value when non-zero so a nested call's "called at L:C"
// annotation cites the calling def's body line. At top level
// (outside any def body), currentDefRegStart is 0 and the
// chunk loop's env.ChunkStartLine is the right reference --
// the BindStmt's Pos is chunk-relative to the executing chunk
// in that case. Pos passes through unchanged when both fields
// are unset (embedded EvalProgram use, package tests): Pos
// values are already file-absolute there.
func absoluteCallLoc(callLoc Pos, env *Env) Pos {
	if callLoc.Line <= 0 {
		return callLoc
	}
	shift := env.ChunkStartLine
	if env.currentDefRegStart > 0 {
		shift = env.currentDefRegStart
	}
	if shift <= 0 {
		return callLoc
	}
	return Pos{Line: callLoc.Line + shift - 1, Col: callLoc.Col}
}

// decorateDefError attaches the def's registration site (file
// and absolute line offset) to a *SyntaxError escaping the body,
// so the diagnostic localises against the file the def was
// declared in rather than whatever chunk is currently running.
// Errors that already carry a File are left alone (an inner
// callDef has already decorated them with the inner def's
// registration site).
//
// Independently of the file decoration, the error's message
// gains a leading "in def NAME (called at L:C): " annotation
// so a runtime error escaping a value-returning helper is no
// longer ambiguous about which call site produced it -- a
// helper reused across several lines of script used to render
// only the body line. Decoration is suppressed when the error
// already carries an inner def's annotation (the innermost
// callDef has decorated first), so propagation preserves the
// closest-to-the-failure call rather than over-attributing to
// every wrapping caller.
func decorateDefError(err error, def *DefValue, callLoc Pos) error {
	if err == nil {
		return err
	}
	var se *SyntaxError
	if !errors.As(err, &se) {
		return err
	}
	if def.RegFile != "" && se.File == "" {
		se.File = def.RegFile
		if def.RegStartLine > 0 {
			shift := def.RegStartLine - 1
			se.Span.Pos.Line += shift
			if se.Span.End.Line > 0 {
				se.Span.End.Line += shift
			}
		}
	}
	// Annotate the message with the def name plus the caller's
	// line:col. An inner annotation -- recognised by the
	// "in def " prefix -- leaves the message alone so the
	// innermost def wins the attribution.
	if !strings.HasPrefix(se.Msg, "in def ") {
		callLine := callLoc.Line
		callCol := callLoc.Col
		if callLine > 0 {
			se.Msg = fmt.Sprintf("in def %s (called at %d:%d): %s", def.Name, callLine, callCol, se.Msg)
		} else {
			se.Msg = fmt.Sprintf("in def %s: %s", def.Name, se.Msg)
		}
	}
	return err
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
	// Each iteration runs in a fresh frame: the loop variables
	// and any body-level `let` live in that frame and disappear
	// at iteration end. State that needs to cross an iteration
	// boundary travels through bind-collect results, command
	// side effects, or defers captured while the frame was live;
	// the language has no outward-write primitive.
iter:
	for i := range list {
		elemVal := v.IndexValue(i)
		iterErr := env.Session.WithFrame(func() error {
			if err := bindForEachElement(env, s, elemVal, i); err != nil {
				return err
			}
			if env.Trace != nil {
				emitForEachTrace(env, s, elemVal, list[i])
			}
			for _, stmt := range s.Body {
				if err := evalStmt(stmt, env); err != nil {
					return err
				}
			}
			return nil
		})
		switch {
		case iterErr == nil:
			continue
		case errors.Is(iterErr, errBreak):
			break iter
		case errors.Is(iterErr, errContinue):
			continue iter
		default:
			return iterErr
		}
	}
	return nil
}

// bindForEachElement installs the current iteration's element
// into the loop variable bindings. Single-var form binds the
// element verbatim; multi-var form destructures the element as a
// list of matching length and binds each sub-element to its
// position. A "_" name discards its slot. Mismatched length or a
// non-list element under multi-var is a runtime error cited at
// the foreach statement, so the script sees the failure in
// context.
func bindForEachElement(env *Env, s *ForEachStmt, elem Value, idx int) error {
	if len(s.Names) == 1 {
		if s.Names[0] != "_" {
			env.Session.Set(s.Names[0], elem)
		}
		return nil
	}
	sub, ok := elem.Raw().([]any)
	if !ok {
		return spanErrorf(s.Span, "foreach: element %d is not a list, cannot destructure into %d names", idx, len(s.Names))
	}
	if len(sub) != len(s.Names) {
		return spanErrorf(s.Span, "foreach: element %d has %d sub-elements, cannot destructure into %d names", idx, len(sub), len(s.Names))
	}
	for j, name := range s.Names {
		if name == "_" {
			continue
		}
		env.Session.Set(name, elem.IndexValue(j))
	}
	return nil
}

// emitForEachTrace renders the iteration's binding(s) and feeds
// the trace callback the one-line summary it expects. Single-var
// renders the element directly; multi-var renders the
// "name1=val1 name2=val2" shape so the trace stays compact even
// when the body destructures.
func emitForEachTrace(env *Env, s *ForEachStmt, elem Value, raw any) {
	if len(s.Names) == 1 {
		rendered, err := RenderCompact(elem)
		if err != nil {
			rendered = fmt.Sprintf("<unrenderable %T>", raw)
		}
		env.Trace(s.Span.Pos.Line, fmt.Sprintf("foreach %s = %s", s.Names[0], rendered))
		return
	}
	parts := make([]string, 0, len(s.Names))
	for j, name := range s.Names {
		sub := elem.IndexValue(j)
		rendered, err := RenderCompact(sub)
		if err != nil {
			rendered = fmt.Sprintf("<unrenderable %T>", sub.Raw())
		}
		parts = append(parts, fmt.Sprintf("%s=%s", name, rendered))
	}
	env.Trace(s.Span.Pos.Line, "foreach "+strings.Join(parts, " "))
}

func evalIfStmt(s *IfStmt, env *Env) error {
	check := func(cond Expr) (bool, error) {
		// Conditions evaluate in the outer frame so let
		// bindings produced by a condition expression -- if any
		// future form ever introduces one -- would live where
		// the if statement does, not in a branch frame that is
		// already disappearing by the time we look at its
		// result.
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
	// Each branch body runs inside a fresh frame: a `let` in the
	// chosen branch lives there and disappears when the branch
	// ends. Sibling branches do not see each other's bindings
	// because each one runs in its own frame, and only the
	// selected branch runs at all.
	runBody := func(body []Stmt) error {
		return env.Session.WithFrame(func() error {
			for _, stmt := range body {
				if err := evalStmt(stmt, env); err != nil {
					return err
				}
			}
			return nil
		})
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

// defaultEventuallyInterval is the cadence between attempts
// when the user did not name an explicit `interval DUR`. Small
// enough to be responsive for sub-second polling, large enough
// not to peg the CPU on long retries.
const defaultEventuallyInterval = 100 * time.Millisecond

// eventuallyResult is the outcome of an eventually run. The
// bind form returns this packaged as a Value; the statement
// form consumes it directly and propagates the last retryable
// error on failure.
type eventuallyResult struct {
	ok        bool
	timedOut  bool
	attempts  int
	elapsedMs int64
	lastError error
}

// runEventually drives the retry loop for both the statement
// form and the bind form. Each attempt runs inside its own
// session frame and its own defer scope, snapshots the session
// assertion counter, executes the body, and classifies the
// outcome:
//
//   - Body completed cleanly AND no new assertion was recorded:
//     success.
//   - Body returned a retryable error, or recorded an assertion
//     failure: attempt failed; if Timeout has not elapsed, sleep
//     Interval and retry.
//   - Body returned a fatal error: propagate immediately, no
//     retry. The bind form does not catch fatal errors; only
//     retry timeout is caught.
//
// The assertion-counter snapshot/reset protocol compensates for
// the fact that the assert dispatcher mutates the counter as a
// side effect. After every attempt we reset to the snapshot so
// retries do not multi-count, and on overall failure we
// increment by one so the construct reports its outcome exactly
// once.
func runEventually(s *EventuallyStmt, env *Env) (eventuallyResult, error) {
	interval := s.Interval
	if interval == 0 {
		interval = defaultEventuallyInterval
	}
	startCounter := env.Session.AssertFailures()
	start := time.Now()
	result := eventuallyResult{}
	deadline := start.Add(s.Timeout)
	for {
		result.attempts++
		attemptCounter := env.Session.AssertFailures()
		env.Session.EnterEventuallyAttempt()
		bodyErr := env.Session.WithFrame(func() error {
			return WithDeferScope(env, func() error {
				for _, stmt := range s.Body {
					if err := evalStmt(stmt, env); err != nil {
						return err
					}
				}
				return nil
			})
		})
		env.Session.ExitEventuallyAttempt()
		// Did this attempt record a new assert failure? Reset
		// the counter to the pre-attempt snapshot so retries do
		// not accumulate; the overall outcome decides whether
		// to charge one failure at the end.
		assertDelta := env.Session.AssertFailures() - attemptCounter
		if assertDelta > 0 {
			resetAssertCounter(env.Session, attemptCounter)
		}
		// Success: no error and no new assertion failure.
		if bodyErr == nil && assertDelta == 0 {
			result.ok = true
			result.elapsedMs = time.Since(start).Milliseconds()
			// Counter has already been restored to the pre-
			// attempt value (which equals the pre-construct
			// value since earlier attempts also reset).
			_ = startCounter
			return result, nil
		}
		// Fatal: any non-retryable error halts immediately,
		// regardless of bind form.
		if bodyErr != nil {
			var re RetryableError
			if !errors.As(bodyErr, &re) || !re.Retryable() {
				return eventuallyResult{}, bodyErr
			}
			result.lastError = bodyErr
		} else {
			// Body returned nil but an assertion failed.
			// Record a synthetic last-error so the bind form
			// has something to surface and the statement form
			// has a reason to cite on overall timeout.
			result.lastError = &AssertFailure{Span: s.Span, Expr: "attempt assertion(s) failed"}
		}
		// Have we run out of budget?
		if !time.Now().Before(deadline) {
			result.timedOut = true
			result.elapsedMs = time.Since(start).Milliseconds()
			// Charge the construct's failure as exactly one
			// assertion against the session counter so the
			// run's exit code reflects the timeout outcome.
			env.Session.RecordAssertFailure()
			return result, nil
		}
		time.Sleep(interval)
	}
}

// resetAssertCounter rewinds the session's assertion failure
// counter to target by decrementing one at a time. The counter
// has no Set primitive on purpose -- mutation is meant to be
// monotonic; eventually's snapshot/reset is the documented
// exception bridge while the assert dispatcher still owns the
// counter mutation.
func resetAssertCounter(s *Session, target int) {
	for s.AssertFailures() > target {
		s.assertFailures--
	}
}

// evalEventuallyStmt runs the statement form: a successful run
// returns nil; an overall timeout propagates the last retryable
// failure so the script halts with the same shape the failing
// statement would have produced on its own. Per-attempt
// diagnostics are suppressed by the assertion dispatcher while
// retries are in flight; on overall timeout we ask the driver
// to render a single summary citing the eventually statement so
// the user sees what failed.
func evalEventuallyStmt(s *EventuallyStmt, env *Env) error {
	res, err := runEventually(s, env)
	if err != nil {
		return err
	}
	if res.ok {
		return nil
	}
	if env.RenderEventuallyFailure != nil {
		env.RenderEventuallyFailure(s.Span, res.attempts, res.elapsedMs, res.lastError)
	}
	if res.lastError != nil {
		return res.lastError
	}
	return spanErrorf(s.Span, "eventually: timed out after %s without satisfying the body", s.Timeout)
}

// evalEventuallyBind runs the bind form. Fatal errors still
// halt; overall retry timeout returns ok:false on the bound
// value rather than propagating. The result shape is the one
// SCOPE-DESIGN.md Section 3.4 documents:
//
//	{ ok, timed_out, attempts, elapsed_ms, last_error }
func evalEventuallyBind(s *BindStmt, env *Env) error {
	res, err := runEventually(s.Eventually, env)
	if err != nil {
		// Fatal: the bind catches retry timeout, not
		// programmer mistakes. No bindings happen.
		return err
	}
	value := buildEventuallyResultValue(res)
	rc := Envelope{OK: res.ok}
	if s.Rc != "" && s.Rc != "_" {
		env.Session.Set(s.Rc, ValueFromEnvelope(rc))
	}
	if s.Primary != "" && s.Primary != "_" {
		env.Session.Set(s.Primary, value)
	}
	return nil
}

// buildEventuallyResultValue packages an eventually outcome as a
// structured Value so the bind form's consumer can branch on
// .ok / .timed_out, read the human-readable .error message, and
// inspect the optional command envelope in .last_command. The
// result shape is deliberately small and stable:
//
//	{
//	  ok:           bool,
//	  timed_out:    bool,
//	  attempts:     int,
//	  elapsed_ms:   int,
//	  error:        string-or-nil,
//	  last_command: envelope-or-nil,
//	}
//
// `error` carries the rendered failure message for diagnostic
// printing; `last_command` is the captured command envelope
// when (and only when) the last retryable failure was
// command-shaped. The public value never names the internal
// RetryableError types -- assertion-shaped failures simply
// leave `last_command` nil rather than having a synthetic
// envelope manufactured for them. The field name carries its
// meaning so the sometimes-nil shape is stable rather than
// variant.
func buildEventuallyResultValue(res eventuallyResult) Value {
	errField, lastCommand := projectEventuallyLast(res.lastError)
	m := map[string]any{
		"ok":           res.ok,
		"timed_out":    res.timedOut,
		"attempts":     res.attempts,
		"elapsed_ms":   res.elapsedMs,
		"error":        errField,
		"last_command": lastCommand,
	}
	return ValueFromMap(m)
}

// projectEventuallyLast turns the last retryable failure into
// the (error, last_command) pair surfaced by the eventually
// bind result. A nil err -- the successful-attempt case --
// produces (nil, nil). For command-shaped failures (anything
// that implements FailureEnvelope: GuardFailure,
// CommandFailure, and the driver-side ExecFailure) the
// last_command field carries the captured envelope. For
// assertion-shaped failures and any other RetryableError we
// leave last_command nil and let `error` carry the rendered
// message.
func projectEventuallyLast(err error) (errMsg any, lastCommand any) {
	if err == nil {
		return nil, nil
	}
	msg := err.Error()
	var fe FailureEnvelope
	if errors.As(err, &fe) {
		return msg, map[string]any{
			"ok":     false,
			"code":   fe.FailureCode(),
			"stdout": fe.FailureStdout(),
			"stderr": fe.FailureStderr(),
		}
	}
	return msg, nil
}

// deferEntry is one captured invocation in a defer scope. Args
// are evaluated at register time and frozen onto the entry; Cmd
// holds the original command form so the diagnostic renderer can
// cite the source location of the defer statement.
type deferEntry struct {
	Span
	Args []Arg

	// trace is the Env.Trace callback captured at registration
	// time, or nil if tracing was not active when defer ran.
	// runDefers invokes this saved callback (not the current
	// env.Trace) so the fire trace cites the file:line of the
	// REGISTRATION site, not whatever chunk happens to be
	// unwinding. Without this, a script-scope defer registered
	// in chunk 5 but fired at scope exit in chunk 152 would
	// inherit chunk 152's shift and cite the wrong line.
	trace func(line int, rendered string)
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
	if env.Trace != nil {
		env.Trace(s.Span.Pos.Line, "defer "+renderArgvTrace(args))
	}
	*env.defers = append(*env.defers, deferEntry{
		Span:  s.Span,
		Args:  args,
		trace: env.Trace,
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
//
// The return value is the count of failures observed in THIS
// scope only -- the local-scope view a caller needs when it has
// to react to its own cleanup result. Nested scopes have already
// run their own defers by the time their stacks reach a caller
// of this function, so their failures land on the session
// counter (global view, used for exit-code accounting) but never
// in the local count returned here. callDefAsBind uses the local
// count to decide whether to flip the bind-position Rc.OK, so
// the contract documented at SCOPE-DESIGN.md Section 9 -- def-
// local cleanup -- does not silently broaden into "anything
// that failed during this call's dynamic extent".
func runDefers(env *Env, stack []deferEntry) int {
	if env.ExecBind == nil {
		return 0
	}
	failures := 0
	for i := len(stack) - 1; i >= 0; i-- {
		entry := stack[i]
		// Use the trace callback captured at registration so
		// the fire's file:line cites where defer was written,
		// not where the surrounding scope happens to be
		// unwinding. Fall back to the current env.Trace only
		// when tracing was off at registration time but is on
		// now -- still useful, even if the line is approximate.
		traceFn := entry.trace
		if traceFn == nil {
			traceFn = env.Trace
		}
		if traceFn != nil {
			traceFn(entry.Pos.Line, "defer fire: "+renderArgvTrace(entry.Args))
		}
		// Def dispatch precedes ExecBind for the same reason
		// it precedes ExecBind at the bind-statement and
		// command-statement positions: a def-named head must
		// resolve through callDefAsBind, not through the
		// external-subprocess fallback. Without this, a
		// `defer cleanup` against a user-defined cleanup helper
		// tried to exec a subprocess named "cleanup". The dispatch
		// returns a BindResult so the existing failure rendering
		// and counter accounting below stay verbatim.
		var result BindResult
		var err error
		if def, ok := lookupDefHead(entry.Args, env); ok {
			result, err = callDefAsBind(def, entry.Args[1:], entry.Pos, env)
		} else {
			result, err = env.ExecBind(entry.Args, entry.Span)
		}
		if err != nil {
			rc := Envelope{OK: false, Code: 1, Stderr: err.Error()}
			if env.RenderDeferFailure != nil {
				env.RenderDeferFailure(entry.Pos, entry.Args, rc)
			}
			env.Session.RecordDeferFailure()
			failures++
			continue
		}
		// Flush the captured stdout/stderr through the driver
		// before the failure-path branch decides whether to
		// also render a labelled block: a successful defer's
		// output would otherwise be dropped, and a failing
		// defer's output is included in the failure block below
		// so the success-output hook only carries the
		// non-failure case.
		if result.Rc.OK && env.RenderDeferOutput != nil {
			env.RenderDeferOutput(entry.Args, result.Rc)
		}
		if !result.Rc.OK {
			if env.RenderDeferFailure != nil {
				env.RenderDeferFailure(entry.Pos, entry.Args, result.Rc)
			}
			env.Session.RecordDeferFailure()
			failures++
		}
	}
	return failures
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
	// Most callers do not need the local failure count; the
	// session counter is the right view for them. callDefAsBind
	// reaches for the local count via its own scope wrapping in
	// runDefCall so cleanup observation stays def-local.
	_ = runDefers(env, stack)
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
	if s.Collect != nil {
		return evalBindCollect(s, env)
	}
	if s.Eventually != nil {
		return evalEventuallyBind(s, env)
	}
	args, err := EvalArgs(s.Cmd.Args, env)
	if err != nil {
		return err
	}
	if env.Trace != nil {
		header := bindTraceHeader(s)
		env.Trace(s.Span.Pos.Line, fmt.Sprintf("%s <- %s", header, renderArgvTrace(args)))
	}
	// Def dispatch precedes ExecBind: a def-named head must not
	// reach the external dispatch path, which would otherwise
	// either find a same-named builtin or try to exec the def
	// name as a subprocess. The same precedence applies at
	// command-statement position (see evalCommandStmt).
	if def, ok := lookupDefHead(args, env); ok {
		result, err := callDefAsBind(def, args[1:], s.Cmd.Span.Pos, env)
		if err != nil {
			return frameAtSpan(s.Span, err)
		}
		return applyBindResult(s, args, result, env)
	}
	result, err := env.ExecBind(args, s.Span)
	if err != nil {
		return frameAtSpan(s.Span, err)
	}
	return applyBindResult(s, args, result, env)
}

// lookupDefHead returns the def value when args[0] names a
// registered def, either directly or via a single-word alias.
// Used by every bind-dispatch path so the def lookup happens
// uniformly before ExecBind sees the args.
//
// Alias resolution: the driver-side ApplyAlias rewrites the
// args vector when it runs (replacing args[0] with the
// expansion's tokens), but the shell layer's def precedence
// has to fire BEFORE ApplyAlias, otherwise an aliased def name
// slips past def lookup and lands in the external-subprocess
// fallback. To keep the dispatch precedence honest, look the
// head up directly, then -- on a miss -- check whether the name
// is an alias whose expansion is a single bare word and try
// that name against the def table. Multi-word aliases ("alias
// mylist = bpfman program list") fall through to ExecBind,
// which does the full ApplyAlias and routes the resulting
// multi-token command to its proper handler; defs cannot
// usefully shadow multi-word aliases because the trailing
// expansion tokens have nowhere to bind as parameters.
func lookupDefHead(args []Arg, env *Env) (*DefValue, bool) {
	if len(args) == 0 {
		return nil, false
	}
	name, ok := commandHeadName(args[0])
	if !ok {
		return nil, false
	}
	if d, ok := env.Session.GetDef(name); ok {
		return d, true
	}
	expansion, ok := env.Session.GetAlias(name)
	if !ok {
		return nil, false
	}
	resolved, ok := singleWord(expansion)
	if !ok {
		return nil, false
	}
	return env.Session.GetDef(resolved)
}

// singleWord reports whether s is a single bare word (no
// internal whitespace, no quotes, no metacharacters that the
// shell tokeniser would split). The empty string and any
// string containing whitespace returns ("", false). Used by
// lookupDefHead to decide whether an alias expansion is
// simple enough for direct def-table resolution.
func singleWord(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", false
	}
	if strings.ContainsAny(trimmed, " \t\n\r;|<>&\"'$#") {
		return "", false
	}
	return trimmed, true
}

// applyBindResult installs result into the BindStmt's named slots
// or halts via GuardFailure on a non-ok envelope under the guard
// form. Shared between the def-dispatch and ExecBind paths so the
// caller-visible binding semantics stay identical no matter how
// the BindResult was produced.
func applyBindResult(s *BindStmt, args []Arg, result BindResult, env *Env) error {
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

// evalBindCollect runs the bind-collect form:
//
//	let RESULT <- foreach NAME in LIST { BODY ... PRODUCER }
//
// LIST is evaluated to a list. For each element:
//
//   - NAME is bound to the element in the session (body-scoped:
//     restored after the collect finishes, matching foreach).
//   - BODY's non-producer statements run in order.
//   - PRODUCER (the body's last statement, validated at parse time
//     to be a CommandStmt) is executed as a bind via ExecBind.
//     Its primary value accumulates into a list bound to s.Primary;
//     when s.Rc is set, the rc envelope accumulates into a parallel
//     list bound to s.Rc.
//
// break inside the body terminates iteration and binds the partial
// collection. continue skips that iteration's accumulation.
// Guard semantics carry: when s.Guard is set, the first non-ok
// rc halts via GuardFailure with no binding.
func evalBindCollect(s *BindStmt, env *Env) error {
	fe := s.Collect
	v, err := EvalExpr(fe.List, env)
	if err != nil {
		return err
	}
	if v.IsNil() {
		return spanErrorf(fe.Span, "bind-collect: list expression is null")
	}
	list, ok := v.Raw().([]any)
	if !ok {
		return spanErrorf(fe.Span, "bind-collect: expected a list, got %s", v.Kind())
	}

	prefix := fe.Body[:len(fe.Body)-1]
	producer := fe.Body[len(fe.Body)-1].(*CommandStmt)

	var rcAcc []any
	var priAcc []any
	var priOriginAcc []any
	priHasOrigin := false

iter:
	for i := range list {
		elemVal := v.IndexValue(i)
		iterErr := env.Session.WithFrame(func() error {
			if err := bindForEachElement(env, fe, elemVal, i); err != nil {
				return err
			}
			if env.Trace != nil {
				emitForEachTrace(env, fe, elemVal, list[i])
			}
			// Prefix statements may break or continue; both
			// propagate as errors so the outer loop can decide
			// whether to drop the producer (continue) or stop
			// iterating entirely (break). Either way, the
			// frame pops cleanly before the next iteration.
			for _, stmt := range prefix {
				if err := evalStmt(stmt, env); err != nil {
					return err
				}
			}
			args, err := EvalArgs(producer.Args, env)
			if err != nil {
				return err
			}
			if env.Trace != nil {
				header := bindTraceHeader(s)
				env.Trace(producer.Span.Pos.Line, fmt.Sprintf("%s <- %s", header, renderArgvTrace(args)))
			}
			var result BindResult
			if def, ok := lookupDefHead(args, env); ok {
				result, err = callDefAsBind(def, args[1:], producer.Span.Pos, env)
				if err != nil {
					return frameAtSpan(producer.Span, err)
				}
			} else {
				result, err = env.ExecBind(args, producer.Span)
				if err != nil {
					return frameAtSpan(producer.Span, err)
				}
			}
			if s.Guard && !result.Rc.OK {
				return &GuardFailure{Span: producer.Span, Primary: s.Primary, Args: args, Envelope: result.Rc}
			}
			if s.Rc != "" && s.Rc != "_" {
				rcAcc = append(rcAcc, ValueFromEnvelope(result.Rc).Raw())
			}
			if s.Primary != "" && s.Primary != "_" {
				priAcc = append(priAcc, result.Primary.Raw())
				origin := result.Primary.Origin()
				priOriginAcc = append(priOriginAcc, origin)
				if origin != nil {
					priHasOrigin = true
				}
			}
			return nil
		})
		switch {
		case iterErr == nil:
			continue
		case errors.Is(iterErr, errBreak):
			break iter
		case errors.Is(iterErr, errContinue):
			continue iter
		default:
			return iterErr
		}
	}

	if s.Rc != "" && s.Rc != "_" {
		env.Session.Set(s.Rc, ValueFromAny(rcAcc))
	}
	if s.Primary != "" && s.Primary != "_" {
		priVal := ValueFromAny(priAcc)
		// Attach the parallel origin slice when any element
		// carries an origin; this lets foreach iteration and path
		// indexing recover each element's typed Value via
		// IndexValue / LookupValue, so bpfman link detach $l
		// works after a bind-collect just as it does on a single
		// guard bind. The element kind is left as OriginUnknown
		// at the list level (lists aren't tagged per-element by
		// kind), but the per-element IndexValue derives kind from
		// the origin's Go type via kindForType.
		if priHasOrigin {
			priVal = priVal.withOrigin(priOriginAcc, OriginUnknown)
		}
		env.Session.Set(s.Primary, priVal)
	}
	return nil
}

// RetryableError is the interface a retrying construct (today,
// `eventually`) uses to classify a failed evaluator result.
// Concrete error types implement Retryable() to declare whether
// the construct should treat the failure as a candidate for
// retry (the world might change) or surface it as fatal (the
// programmer made a mistake; retrying delays diagnosis without
// changing the outcome).
//
// Retrying constructs consume the layer via `errors.As`:
//
//	var r RetryableError
//	if errors.As(err, &r) && r.Retryable() {
//	    lastError = err
//	    continue
//	}
//	return err  // fatal
//
// Wrappers in the evaluator (frameAtSpan, SyntaxError, and
// other SpanCarriers) preserve the chain via Unwrap so
// errors.As locates the typed value regardless of intermediate
// framing.
type RetryableError interface {
	error
	Retryable() bool
}

// FailureEnvelope marks errors that carry a command-result
// shape: an exit code plus captured stdout / stderr. The
// `eventually` bind form's last_command projection uses this
// interface to coalesce GuardFailure, CommandFailure, and the
// driver-side ExecFailure (a subprocess non-zero exit) into a
// single user-facing { ok, code, stdout, stderr } shape without
// leaking the internal error class hierarchy. AssertFailure
// and RequireFailure deliberately do not implement this
// interface -- their failure mode has no envelope -- so the
// projection leaves last_command nil for them.
type FailureEnvelope interface {
	error
	FailureCode() int
	FailureStdout() string
	FailureStderr() string
}

// ErrRequireFailed is the sentinel error chained under a
// *RequireFailure so existing `errors.Is(err, ErrRequireFailed)`
// checks at script-loop boundaries continue to recognise a
// failed `require` after the typed-error layer landed. The
// driver layer re-exports this value so callers reading repl
// import paths see the same sentinel.
var ErrRequireFailed = errors.New("require failed")

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

// Retryable reports that a guard failure is a candidate for
// retry under `eventually`: the command was successfully
// resolved and executed but produced a non-ok envelope, and the
// world might satisfy the guard on a later attempt.
func (e *GuardFailure) Retryable() bool { return true }

// FailureCode / FailureStdout / FailureStderr satisfy
// FailureEnvelope so the eventually bind form can project the
// guard's captured envelope into its `last_command` field
// without reaching into the typed error from outside the
// package.
func (e *GuardFailure) FailureCode() int      { return e.Envelope.Code }
func (e *GuardFailure) FailureStdout() string { return e.Envelope.Stdout }
func (e *GuardFailure) FailureStderr() string { return e.Envelope.Stderr }

// CommandFailure is the error type a CommandStmt produces when a
// command is successfully resolved and executed and returns a
// non-ok envelope. It explicitly does not cover unknown commands,
// launch failures, or argument resolution rejections -- those are
// environment-or-programmer mistakes that propagate as untyped
// errors and remain fatal under retrying constructs.
type CommandFailure struct {
	Span
	Args     []Arg
	Envelope Envelope
}

func (e *CommandFailure) Error() string {
	if e.Envelope.Stderr != "" {
		return fmt.Sprintf("command failed (exit %d): %s",
			e.Envelope.Code, e.Envelope.Stderr)
	}
	return fmt.Sprintf("command failed (exit %d)", e.Envelope.Code)
}

// Retryable reports that an ordinary non-ok envelope is a
// candidate for retry: the command itself ran, and the world
// might change in a way that flips the result.
func (e *CommandFailure) Retryable() bool { return true }

// FailureCode / FailureStdout / FailureStderr satisfy
// FailureEnvelope so the eventually bind form can surface the
// envelope in its `last_command` field.
func (e *CommandFailure) FailureCode() int      { return e.Envelope.Code }
func (e *CommandFailure) FailureStdout() string { return e.Envelope.Stdout }
func (e *CommandFailure) FailureStderr() string { return e.Envelope.Stderr }

// AssertFailure is the typed-error form of an `assert` predicate
// that did not hold. The dispatcher still records the failure
// into the session counter for the usual reporting path; the
// typed shape exists so retrying constructs can classify
// uniformly via `errors.As`. Expr is the rendered failure
// message produced by the dispatcher.
type AssertFailure struct {
	Span
	Expr string
}

func (e *AssertFailure) Error() string {
	if e.Expr == "" {
		return "assert failed"
	}
	return "assert failed: " + e.Expr
}

func (e *AssertFailure) Retryable() bool { return true }

// RequireFailure is the typed-error form of a `require`
// predicate that did not hold. Unwrapping yields
// ErrRequireFailed so existing `errors.Is(err, ErrRequireFailed)`
// halts at the same script-loop boundaries that already check
// for the sentinel.
type RequireFailure struct {
	Span
	Expr string
}

func (e *RequireFailure) Error() string {
	if e.Expr == "" {
		return "require failed"
	}
	return "require failed: " + e.Expr
}

func (e *RequireFailure) Retryable() bool { return true }

func (e *RequireFailure) Unwrap() error { return ErrRequireFailed }

func evalCommandStmt(s *CommandStmt, env *Env) error {
	args, err := EvalArgs(s.Args, env)
	if err != nil {
		return err
	}
	if env.Trace != nil {
		env.Trace(s.Span.Pos.Line, renderArgvTrace(args))
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
	case *PureCallExpr:
		return dispatchPureCall(e, env)
	case *ListExpr:
		return evalListExpr(e, env)
	default:
		return Value{}, fmt.Errorf("unhandled expression type %T", expr)
	}
}

// evalListExpr evaluates each element of a list literal and
// packs the resulting raw values into a []any, the same
// underlying representation foreach iterates and the range
// builtin produces. When any element carries an origin (typically
// from a $var that itself came from a structured bind), a
// parallel origin slice is attached to the result so foreach
// iteration and path indexing recover each element's typed Value
// via IndexValue / LookupValue.
func evalListExpr(e *ListExpr, env *Env) (Value, error) {
	out := make([]any, 0, len(e.Elems))
	origins := make([]any, 0, len(e.Elems))
	hasOrigin := false
	for _, elem := range e.Elems {
		v, err := EvalExpr(elem, env)
		if err != nil {
			return Value{}, err
		}
		out = append(out, v.Raw())
		o := v.Origin()
		origins = append(origins, o)
		if o != nil {
			hasOrigin = true
		}
	}
	list := ValueFromAny(out)
	if hasOrigin {
		list = list.withOrigin(origins, OriginUnknown)
	}
	return list, nil
}

// dispatchPureCall evaluates the pure-builtin call's arguments,
// hands them to ExecBind (the same dispatch path the '<-' form
// uses) with the builtin name prepended, then returns the
// primary Value. The result envelope is discarded because pure
// builtins are by contract side-effect-free and have no failure
// state worth capturing; a handler error halts the expression.
func dispatchPureCall(e *PureCallExpr, env *Env) (Value, error) {
	if env.ExecBind == nil {
		return Value{}, spanErrorf(e.Span, "%s: pure-builtin calls require an active command dispatcher", e.Name)
	}
	args := make([]Arg, 0, len(e.Args)+1)
	args = append(args, WordArg{Text: e.Name, Span: e.Span})
	// Route through evalArg rather than EvalExpr+valueToArg so the
	// literal-vs-resolved distinction reaches the handler. Literal
	// quoted/word args stay as QuotedArg/WordArg (jq treats them
	// as user-typed JSON-shaped input), variable references become
	// ScalarValueArg with HasValue=true carrying the source Value
	// (jq passes the typed value through), and structured args
	// remain structured. Going through EvalExpr+valueToArg would
	// flatten literals into untyped scalars and break the boundary
	// invariant the jq adapter relies on.
	for _, a := range e.Args {
		arg, err := evalArg(a, env)
		if err != nil {
			return Value{}, spanErrorf(nodeSpan(a), "%s: %v", e.Name, err)
		}
		args = append(args, arg)
	}
	result, err := env.ExecBind(args, e.Span)
	if err != nil {
		return Value{}, frameAtSpan(e.Span, err)
	}
	if !result.Rc.OK {
		if result.Rc.Stderr != "" {
			return Value{}, spanErrorf(e.Span, "%s: %s", e.Name, result.Rc.Stderr)
		}
		return Value{}, spanErrorf(e.Span, "%s: call failed (exit %d)", e.Name, result.Rc.Code)
	}
	return result.Primary, nil
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

// argTraceText renders a single Arg as text suitable for an
// execution trace. Scalars and word forms emit their resolved
// text; structured values render as compact JSON so the user can
// see the value that flowed into the call rather than the bare
// `$name` placeholder; adapter args keep their `adapter:$var.path`
// form because the temp-file backing path is uninteresting for
// debugging. Mirrors the cmd-side argText spelling, with the
// deliberate difference that StructuredValueArg yields the value
// not the variable name.
func argTraceText(a Arg) string {
	switch v := a.(type) {
	case WordArg:
		return v.Text
	case QuotedArg:
		return v.Text
	case ScalarValueArg:
		return v.Text
	case StructuredValueArg:
		s, err := RenderCompact(v.Value)
		if err != nil {
			return "$" + v.Name
		}
		return s
	case AdapterArg:
		if v.Path != "" {
			return fmt.Sprintf("%s:$%s.%s", v.Adapter, v.Name, v.Path)
		}
		return fmt.Sprintf("%s:$%s", v.Adapter, v.Name)
	default:
		return ""
	}
}

// renderArgvTrace joins the resolved Arg list as a single line for
// the trace hook. Whitespace inside a scalar is left as-is; the
// trace is for human reading, not for re-parsing, so re-quoting
// every value would obscure more than it clarifies.
func renderArgvTrace(args []Arg) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = argTraceText(a)
	}
	return strings.Join(parts, " ")
}

// bindTraceHeader formats the left-hand side of a BindStmt for the
// trace output: `let r`, `let (rc v)`, `guard r`, `guard (rc v)`,
// or `let _` when neither slot was named. The user typed this
// verbatim in source; reproducing it in the trace makes it easy to
// match an entry to the binding line.
func bindTraceHeader(s *BindStmt) string {
	verb := "let"
	if s.Guard {
		verb = "guard"
	}
	rc := s.Rc
	primary := s.Primary
	switch {
	case rc != "" && primary != "":
		return fmt.Sprintf("%s (%s %s)", verb, named(rc), named(primary))
	case primary != "":
		return fmt.Sprintf("%s %s", verb, named(primary))
	case rc != "":
		return fmt.Sprintf("%s %s", verb, named(rc))
	default:
		return verb + " _"
	}
}

// named keeps `_` for the discard slot and adds nothing else; the
// name is already a bare identifier in the source.
func named(s string) string {
	if s == "" {
		return "_"
	}
	return s
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
		return ScalarValueArg{Text: s, Value: val, HasValue: true, Span: e.Span}, nil
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
		return ScalarValueArg{Text: s, Value: val, HasValue: true, Span: e.Span}, nil
	case *MatchesBlockExpr:
		return evalMatchesBlockArg(e, env)
	default:
		// Any other Expr type reaches argument position only via
		// the '(EXPR)' parenthesised arg form. Evaluate it and
		// wrap the resulting Value via valueToArg so a thread, a
		// pure-builtin call, a list literal, or an arithmetic /
		// comparison expression all flow through as Scalar or
		// StructuredValueArg, matching the wart entry's
		// "(EXPR) in argument position" resolution.
		v, err := EvalExpr(expr, env)
		if err != nil {
			return nil, err
		}
		if v.IsNil() {
			return nil, spanErrorf(nodeSpan(expr), "parenthesised expression produced no value")
		}
		return valueToArg(v, nodeSpan(expr))
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
	path, err := resolveDynamicPath(e.Path, env, e.Span)
	if err != nil {
		return Value{}, err
	}
	lv, err := v.LookupValue(e.Name, path)
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
		path, err := resolveDynamicPath(e.Path, env, e.Span)
		if err != nil {
			return nil, err
		}
		// Soft lookup at the arg boundary: absent paths surface
		// as MissingArg so the shape-test predicates
		// (present / missing / strict nil) can distinguish
		// "field not in the value tree" from "field present and
		// null". Hard errors from the walker (malformed path,
		// non-traversable intermediate) still propagate.
		lv, class, err := v.LookupSoft(e.Name, path)
		if err != nil {
			return nil, spanErrorf(e.Span, "%v", err)
		}
		if class == LookupAbsent {
			return MissingArg{Name: e.Name, Path: path, Span: e.Span}, nil
		}
		resolved = lv
	}
	if resolved.IsNil() {
		// Terminal null is a value. Surface it as NilArg so
		// downstream consumers (print, jq, the nil/present
		// predicates) can decide how to handle it. Commands
		// that need a non-null arg surface their own clearer
		// diagnostic when they encounter NilArg.
		return NilArg{Span: e.Span}, nil
	}
	if resolved.IsStructured() {
		return StructuredValueArg{Name: e.Name, Value: resolved, Span: e.Span}, nil
	}
	s, err := resolved.Scalar()
	if err != nil {
		return nil, spanErrorf(e.Span, "variable %s: %v", qualify(e.Name, e.Path), err)
	}
	return ScalarValueArg{Text: s, Value: resolved, HasValue: true, Span: e.Span}, nil
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
	path := e.Path
	if path != "" {
		var err error
		path, err = resolveDynamicPath(path, env, e.Span)
		if err != nil {
			return nil, err
		}
		lv, err := v.LookupValue(e.Name, path)
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

// resolveDynamicPath rewrites "[$ident]" segments in path to "[N]"
// using the current session bindings. The tokeniser accepts the
// "[$ident]" form alongside literal "[digits]", deferring the
// integer resolution here so a single foreach can index parallel
// lists without round-tripping through jq. Segments that are not
// "[$ident]" pass through unchanged; downstream parsePath only ever
// sees the digit form.
//
// Errors cite the host span (the VarRefExpr's `$xs[$i]`), not the
// inner `[$i]` position, because the path text in VarRefExpr is
// stored without per-segment offsets. The index variable must
// resolve to a scalar integer; strings parsable as an integer are
// accepted (jq -r round-trips produce string scalars), booleans and
// nulls are rejected.
func resolveDynamicPath(path string, env *Env, span Span) (string, error) {
	if !strings.Contains(path, "[$") {
		return path, nil
	}
	var b strings.Builder
	b.Grow(len(path))
	i := 0
	for i < len(path) {
		if path[i] != '[' || i+1 >= len(path) || path[i+1] != '$' {
			b.WriteByte(path[i])
			i++
			continue
		}
		nameStart := i + 2
		j := nameStart
		for j < len(path) && (isIdentStart(path[j]) || (j > nameStart && isIdentContinue(path[j]))) {
			j++
		}
		if j == nameStart || j >= len(path) || path[j] != ']' {
			// Should not be reachable: lexPathIndex would have
			// rejected this at tokenisation time. Surface the
			// state defensively rather than silently writing
			// out malformed text.
			return "", spanErrorf(span, "malformed dynamic index in path %q", path)
		}
		name := path[nameStart:j]
		v, ok := env.Session.Get(name)
		if !ok {
			return "", spanErrorf(span, "index variable $%s is not defined", name)
		}
		n, err := valueToIndex(v)
		if err != nil {
			return "", spanErrorf(span, "index variable $%s: %v", name, err)
		}
		b.WriteByte('[')
		b.WriteString(strconv.Itoa(n))
		b.WriteByte(']')
		i = j + 1
	}
	return b.String(), nil
}

// valueToIndex converts a scalar Value to an int suitable for array
// indexing. Accepts json.Number (the shape `range N` produces),
// float64 (only if integral), and strings parseable as a base-10
// integer. Booleans, nulls, and structured values are rejected.
// Negative integers are returned as-is; range validation lives in
// walkPath's indexStep handler.
func valueToIndex(v Value) (int, error) {
	switch x := v.Raw().(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, fmt.Errorf("index must be an integer, got %q", x)
		}
		return int(n), nil
	case float64:
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("index must be an integer, got %v", x)
		}
		return int(x), nil
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 0, fmt.Errorf("index must be an integer, got %q", x)
		}
		return n, nil
	case bool:
		return 0, fmt.Errorf("index must be an integer, got bool")
	case nil:
		return 0, fmt.Errorf("index is null")
	default:
		return 0, fmt.Errorf("index must be a scalar integer, got %s", v.Kind())
	}
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
	args, err := EvalArgs(e.Args, env)
	if err != nil {
		return Value{}, err
	}
	// Route the LHS through evalArg rather than EvalExpr+valueToArg
	// so the literal-vs-resolved distinction reaches the receiving
	// command (same rationale as dispatchPureCall). A nil LHS
	// passes through as NilArg; jq treats it as JSON null and
	// filters like `length` work as expected.
	lhsArg, err := evalArg(e.LHS, env)
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
// scalars become ScalarValueArg, nil becomes NilArg so the
// receiving command can decide whether to accept null at its
// own input boundary (jq, print, the strict-nil and present
// predicates) rather than blanket-erroring at resolution time.
// span is attached to the resulting Arg so command-handler
// parsers can frame argument-position errors at the originating
// expression.
func valueToArg(v Value, span Span) (Arg, error) {
	if v.IsNil() {
		return NilArg{Span: span}, nil
	}
	if v.IsStructured() {
		return StructuredValueArg{Value: v, Span: span}, nil
	}
	s, err := v.Scalar()
	if err != nil {
		return nil, err
	}
	return ScalarValueArg{Text: s, Value: v, HasValue: true, Span: span}, nil
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
		// not-empty is content-presence under the Go zero-value
		// convention applied uniformly: nil is empty, "" is
		// empty, [] / nil-slice is empty, {} / nil-map is empty,
		// numeric 0 is empty, false is empty. This matches the
		// matches-block predicate semantics so `not-empty` reads
		// the same inline (`assert not-empty $xs`) as inside a
		// matches block (`field: not-empty`).
		if operand.IsNil() {
			return BoolValue(false), nil
		}
		switch x := operand.Raw().(type) {
		case string:
			return BoolValue(x != ""), nil
		case []any:
			return BoolValue(len(x) > 0), nil
		case map[string]any:
			return BoolValue(len(x) > 0), nil
		case json.Number:
			f, ferr := x.Float64()
			if ferr != nil {
				return Value{}, spanErrorf(e.Span, "not-empty: %v", ferr)
			}
			return BoolValue(f != 0), nil
		case float64:
			return BoolValue(x != 0), nil
		case bool:
			return BoolValue(x), nil
		default:
			// Any remaining carrier-type falls back to the
			// pre-existing scalar text check for consistency
			// with how Scalar() renders these types.
			s, err := operand.Scalar()
			if err != nil {
				return Value{}, spanErrorf(e.Span, "not-empty: %v", err)
			}
			return BoolValue(s != ""), nil
		}
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
