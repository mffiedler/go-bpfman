package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/ir"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/semantics"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/source"
	"github.com/frobware/go-bpfman/cmd/bpfman-shell/shell/syntax"
)

// Env is the execution environment for the evaluator. Session is
// the variable and def store; ExecCommand dispatches top-level
// commands to the shell's command and domain pipelines; ExecBind
// dispatches command forms on the right of a '<-' bind.
//
// A nil ExecCommand makes any top-level syntax.CommandStmt a runtime
// error; a nil ExecBind makes any syntax.BindStmt a runtime error.
// Tests that only exercise expression evaluation can leave both
// unset.
type Env struct {
	Session *Session

	// ExecCommand runs a top-level syntax.CommandStmt. span is the
	// originating statement's source extent so handlers (and any
	// errors they emit) can frame diagnostics at the failing
	// command. The returned Value may be nil; any output is
	// visible on the CLI.
	ExecCommand func(args []Arg, span source.Span) (Value, error)

	// ExecBind runs a command form on the right of a '<-' bind.
	// span is the bind statement's source extent. The returned
	// BindResult carries the result envelope (Rc) and the
	// provider's primary result (Primary). Command failure
	// (non-zero exit, in-process error) is encoded on Rc as OK:
	// false with code, stdout, and stderr set, not as a Go
	// error. A Go error is reserved for structural failures
	// (empty argv, malformed adapter, no provider for this
	// hook). Set by the shell runner; nil makes any syntax.BindStmt a
	// runtime error.
	ExecBind func(args []Arg, span source.Span) (BindResult, error)

	// ExecAssertIR runs a lowered Assert instruction. Set by the
	// shell runner; nil makes any lowered Assert a runtime error.
	ExecAssertIR func(*ir.Assert, *Env) error

	// PrintResult is called when a top-level syntax.ExprStmt produces a
	// value. It is the shell auto-print hook: a top-level "$x"
	// or "$x == 5" lands here. A nil callback
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
	RenderDeferFailure func(stmtLoc source.Pos, args []Arg, rc Envelope)

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
	// exit). pos identifies the statement's source position;
	// rendered is a one-line summary of the statement with
	// interpolations resolved, e.g. an argv with `$prog`
	// substituted by its compact-JSON form, or `let x = <value>`
	// after the RHS has evaluated. Drivers typically prepend
	// `file:line:` and write the result to stderr. shell/ never
	// decides whether to trace; it only emits when the callback
	// is non-nil, so policy (a `trace on` toggle, a CLI flag)
	// lives in the driver-side installer.
	Trace func(pos source.Pos, rendered string)

	// RenderPollFailure, when set, is invoked when a poll runs
	// out of retry budget. The per-attempt retry reasons are
	// suppressed during the construct, so this is the single
	// place the user sees the timeout summary.
	RenderPollFailure func(span source.Span, timeout, every time.Duration, attempts int, lastRetry string)

	// Now and Sleep are the clock hooks the retrying constructs
	// consult. Drivers leave them nil to use time.Now and
	// time.Sleep; tests override them to make timeout boundaries
	// deterministic without package-global state.
	Now   func() time.Time
	Sleep func(time.Duration)

	// defCallDepth counts the def-call frames currently active
	// on the evaluator's stack. runDefCall increments on entry
	// and decrements on exit; a value over MaxDefCallDepth is a
	// clean failure rather than a Go-runtime stack overflow.
	// Runaway recursion -- the natural shape of a value-returning
	// helper that forgets its base case -- otherwise dumps pages
	// of goroutine traces, which is unkind. The cap is far below
	// Go's stack limit so the diagnostic always wins.
	defCallDepth int

	// activePolls counts the poll constructs currently executing.
	// Helper bodies can observe this even when the helper text is
	// not itself nested directly inside a poll block.
	activePolls int
}

func (e *Env) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) sleep(d time.Duration) {
	if e != nil && e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (e *Env) enterPoll() {
	if e != nil {
		e.activePolls++
	}
}

func (e *Env) exitPoll() {
	if e != nil && e.activePolls > 0 {
		e.activePolls--
	}
}

func (e *Env) InPoll() bool {
	return e != nil && e.activePolls > 0
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

// withDeferScope runs fn inside a fresh defer scope, restoring the outer
// scope on return and executing every registered deferred statement in LIFO
// order regardless of fn's outcome.
func withDeferScope(env *Env, fn func() error) error {
	return runWithDeferScope(env, fn)
}

// callDef binds def parameters from args and runs the def's
// lowered entry block in env. Each call runs in its own session frame: parameters bind
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
func callDef(def *defValue, args []Arg, callLoc source.Pos, env *Env) error {
	_, _, _, err := runDefCall(def, args, callLoc, env)
	if err != nil {
		return decorateDefError(err, def, callLoc)
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
// def's body, matching the def-local cleanup contract.
//
// A non-return error from the body (unbound variable, type
// error, guard halt inside the body, parse error from a
// dynamic source, etc.) propagates as a Go error; the bind
// path then frames it and the calling script halts. No
// bindings happen in that case.
func callDefAsBind(def *defValue, args []Arg, callLoc source.Pos, env *Env) (BindResult, error) {
	returned, hasReturn, localDeferFailures, err := runDefCall(def, args, callLoc, env)
	if err != nil {
		return BindResult{}, decorateDefError(err, def, callLoc)
	}
	rc := OkEnvelope()
	if localDeferFailures > 0 {
		rc = FailEnvelope()
	}
	primary := returned
	if !hasReturn {
		primary = ValueFromEnvelope(rc)
	}
	return BindResult{Rc: rc, Primary: primary}, nil
}

// runDefCall is the shared body of callDef and callDefAsBind. It
// checks arity, enforces the recursion limit, and dispatches into
// the lowered def body. The returned tuple matches callDefAsBind's
// needs: returned Value, hasReturn flag, def-local defer failure
// count, and any escaping runtime error.
func runDefCall(def *defValue, args []Arg, callLoc source.Pos, env *Env) (Value, bool, int, error) {
	if len(args) != len(def.Params) {
		return Value{}, false, 0, syntax.LocErrorf(callLoc, "%s: expected %d argument(s), got %d (def declared at %d:%d)",
			def.Name, len(def.Params), len(args), def.Pos.Line, def.Pos.Col)
	}
	// Catch runaway recursion before Go's stack does. The cap
	// is far below Go's per-goroutine stack ceiling so the
	// clean diagnostic wins over a runtime panic; the count is
	// pushed / popped around the body so unrelated calls do not
	// accumulate against the limit and a backtrack out of a
	// recursive helper resumes with the right depth.
	if env.defCallDepth >= MaxDefCallDepth {
		return Value{}, false, 0, syntax.LocErrorf(callLoc, "in def %s: recursion depth limit exceeded (%d)", def.Name, MaxDefCallDepth)
	}
	env.defCallDepth++
	defer func() { env.defCallDepth-- }()
	if def.Entry == nil {
		return Value{}, false, 0, syntax.LocErrorf(callLoc, "def %s has no IR body", def.Name)
	}
	return runLoweredDefCall(def, args, env)
}

// decorateDefError annotates a *syntax.SyntaxError escaping a def body
// with the def name and the caller's line/col. Positions are
// already absolute, so the helper only needs to preserve the
// innermost source span and add call-site context to the message.
//
// Independently of the source coordinates, the error's message
// gains a leading "in def NAME (called at L:C): " annotation
// so a runtime error escaping a value-returning helper is no
// longer ambiguous about which call site produced it -- a
// helper reused across several lines of script used to render
// only the body line. Decoration is suppressed when the error
// already carries an inner def's annotation (the innermost
// callDef has decorated first), so propagation preserves the
// closest-to-the-failure call rather than over-attributing to
// every wrapping caller.
func decorateDefError(err error, def *defValue, callLoc source.Pos) error {
	if err == nil {
		return err
	}
	var se *syntax.SyntaxError
	if !errors.As(err, &se) {
		return err
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
// adapter args carry their already-resolved Value through.
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

// deferEntry is one captured invocation in a defer scope. Args
// are evaluated at register time and frozen onto the entry; Cmd
// holds the original command form so the diagnostic renderer can
// cite the source location of the defer statement.
type deferEntry struct {
	source.Span
	Args   []Arg
	policy ir.DispatchPolicy

	// trace is the Env.Trace callback captured at registration
	// time, or nil if tracing was not active when defer ran.
	// runDefers invokes this saved callback (not the current
	// env.Trace) so the fire trace cites the file:line of the
	// REGISTRATION site, not whatever source happens to be
	// unwinding. Without this, a script-scope defer registered
	// in one source unit but fired much later would inherit the
	// later source's shift and cite the wrong line.
	trace func(pos source.Pos, rendered string)
}

// evalDeferStmt evaluates the deferred command's arguments now
// (so values are captured at register time, not at scope exit)
// and appends the entry to the active defer scope's stack. A
// missing scope is a runtime error; the parser does not enforce
// that defer is reachable, so a malformed driver could trip this
// check.
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
// the def-local cleanup contract does not silently broaden into "anything
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
			traceFn(entry.Pos, "defer fire: "+renderArgvTrace(entry.Args))
		}
		// Defer dispatches through the shared bind-style
		// helper so the def-vs-external precedence matches the
		// other bind-position sites. Failure rendering and
		// session counter accounting stay in this loop because
		// they are defer-specific (RenderDeferFailure block,
		// RecordDeferFailure tick); the helper only handles
		// the head resolution.
		result, err := dispatchBindByPolicy(entry.policy, entry.Args, entry.Pos, entry.Span, env)
		if err != nil {
			rc := FailEnvelopeFromError(err)
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
// withJobScope) the call is a no-op: there is nothing to leak
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
// Defer scopes are independent of job scopes (see withJobScope)
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

// withJobScope establishes a job scope around fn: any Job that
// fn registers via Env.RegisterJob is tracked in this scope's
// registry, and on exit each unmanaged entry is reported through
// HandleJobLeak and counted on the session.
//
// Drivers open exactly one job scope per session unit: the whole
// script in script mode, the whole imported file in import mode,
// the whole interactive session in interactive mode. Inner
// blocks (def bodies, foreach, retry) deliberately do not open
// new job scopes, so a job started inside a def joins the
// caller's registry and survives the def's return without
// being flagged.
//
// Job leak reporting runs after fn returns. When the driver
// composes withJobScope around an outer withDeferScope (the
// usual shape: defers nest inside jobs), the outer defers run
// before the leak walk, so 'defer kill $job' marks a job
// Managed before the leak walk sees it.
func withJobScope(env *Env, fn func() error) error {
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

// dispatchBindHead resolves args[0] to either a def call or
// the external bind-dispatch path. Used by every bind-position
// site (evalBindStmt, bind-collect producer, runDefers) so the
// def-vs-external precedence is one rule applied at one site
// rather than three. Without this helper, each site
// independently consulted lookupDefHead before falling through
// to env.ExecBind, and asymmetries crept in -- the W11 / W23
// fixes were each "rule applied at site A but not B".
//
// The helper returns the BindResult and error verbatim. source.Span
// framing is left to the caller because the relevant span
// differs per site (the bind statement, the producer command,
// the defer entry); a helper-side syntax.FrameAt would either
// over-fit one site or lose context.
// dispatchBindByPolicy runs the shell-level head-resolution rule
// named by policy. Today the only bind-position policy is
// def-first fallback to ExecBind, but keeping the policy
// explicit lets the lowered IR expose the rule and keeps future
// dispatch growth honest.
func dispatchBindByPolicy(policy ir.DispatchPolicy, args []Arg, callLoc source.Pos, span source.Span, env *Env) (BindResult, error) {
	switch policy {
	case ir.DispatchPolicyDefThenExecBind:
		if def, ok := lookupDefHead(args, env); ok {
			return callDefAsBind(def, args[1:], callLoc, env)
		}
		return env.ExecBind(args, span)
	default:
		return BindResult{}, syntax.SpanErrorf(span, "unsupported bind dispatch policy %s", dispatchPolicyName(policy))
	}
}

// dispatchCommandHead is the command-style sibling: defs first,
// then env.ExecCommand.
func dispatchCommandByPolicy(policy ir.DispatchPolicy, args []Arg, callLoc source.Pos, span source.Span, env *Env) error {
	switch policy {
	case ir.DispatchPolicyDefThenExecCommand:
		if def, ok := lookupDefHead(args, env); ok {
			return callDef(def, args[1:], callLoc, env)
		}
		if env.ExecCommand == nil {
			return syntax.SpanErrorf(span, "command execution is not configured")
		}
		_, err := env.ExecCommand(args, span)
		return syntax.FrameAt(span, err)
	default:
		return syntax.SpanErrorf(span, "unsupported command dispatch policy %s", dispatchPolicyName(policy))
	}
}

func dispatchPolicyName(policy ir.DispatchPolicy) string {
	switch policy {
	case ir.DispatchPolicyDefThenExecBind:
		return "def-then-exec-bind"
	case ir.DispatchPolicyDefThenExecCommand:
		return "def-then-exec-command"
	default:
		return fmt.Sprintf("<unknown:%d>", int(policy))
	}
}

// lookupDefHead returns the def value when args[0] names a
// registered def. Used by both dispatch helpers above.
func lookupDefHead(args []Arg, env *Env) (*defValue, bool) {
	if len(args) == 0 {
		return nil, false
	}
	name, ok := commandHeadName(args[0])
	if !ok {
		return nil, false
	}
	return env.Session.getDef(name)
}

// applyBindResult installs result into the syntax.BindStmt's named slots
// or halts via GuardFailure on a non-ok envelope under the guard
// form. Shared between the def-dispatch and ExecBind paths so the
// caller-visible binding semantics stay identical no matter how
// the BindResult was produced.

// ErrRequireFailed is the sentinel error chained under a
// *RequireFailure so existing `errors.Is(err, ErrRequireFailed)`
// checks at script-loop boundaries continue to recognise a
// failed `require` after the typed-error layer landed. The
// driver layer re-exports this value so callers reading driver
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
	source.Span
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

// CommandFailure is the error type a syntax.CommandStmt produces when a
// command is successfully resolved and executed and returns a
// non-ok envelope. It explicitly does not cover unknown commands,
// launch failures, or argument resolution rejections -- those are
// environment-or-programmer mistakes that propagate as untyped
// errors and remain fatal under retrying constructs.
type CommandFailure struct {
	source.Span
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

// AssertFailure is the typed-error form of an assertion whose
// condition did not hold. Tests and helper hooks use it directly
// when they need a concrete assertion-failure value.
type AssertFailure struct {
	source.Span
	Expr string
}

func (e *AssertFailure) Error() string {
	if e.Expr == "" {
		return "assert failed"
	}
	return "assert failed: " + e.Expr
}

// RequireFailure is the typed-error form of a `require`
// predicate that did not hold. Unwrapping yields
// ErrRequireFailed so existing `errors.Is(err, ErrRequireFailed)`
// halts at the same script-loop boundaries that already check
// for the sentinel.
type RequireFailure struct {
	source.Span
	Expr string
}

func (e *RequireFailure) Error() string {
	if e.Expr == "" {
		return "require failed"
	}
	return "require failed: " + e.Expr
}

func (e *RequireFailure) Unwrap() error { return ErrRequireFailed }

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

// structuredShape returns a short description of a structured
// Value suitable for error messages.  The declared semantics.OriginKind is
// used when it is anything other than semantics.OriginUnknown (so "program"
// or "exec.result" read as such); otherwise the raw Go shape is
// inspected so an untagged record or array still reports
// meaningfully as "object" or "array" rather than the useless
// "unknown".
func structuredShape(v Value) string {
	if k := v.Kind(); k != semantics.OriginUnknown && k != semantics.OriginScalar {
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

// RenderCompact renders a Value to a single-line string form.
// Scalars (including semantics.OriginNull, which renders as "null") use
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

func resolveVarRefValueParts(name, path string, span source.Span, env *Env) (Value, error) {
	v, ok := env.Session.Get(name)
	if !ok {
		return Value{}, syntax.SpanErrorf(span, "undefined variable %q", name)
	}
	if path == "" {
		return v, nil
	}
	path, err := resolveDynamicPath(path, env, span)
	if err != nil {
		return Value{}, err
	}
	lv, err := v.LookupValue(name, path)
	if err != nil {
		return Value{}, syntax.SpanErrorf(span, "%v", err)
	}
	return lv, nil
}

func resolveVarRefArgParts(name, path string, span source.Span, env *Env) (Arg, error) {
	v, ok := env.Session.Get(name)
	if !ok {
		return nil, syntax.SpanErrorf(span, "undefined variable: %s", name)
	}
	resolved := v
	if path != "" {
		path, err := resolveDynamicPath(path, env, span)
		if err != nil {
			return nil, err
		}
		// Soft lookup at the arg boundary: absent paths surface
		// as MissingArg so the shape-test predicates
		// (present / missing / strict null) can distinguish
		// "field not in the value tree" from "field present and
		// null". Hard lookup errors (malformed path,
		// non-traversable intermediate) still propagate.
		presence, err := v.LookupPresence(name, path)
		if err != nil {
			return nil, syntax.SpanErrorf(span, "%v", err)
		}
		if presence.IsMissing() {
			return MissingArg{Name: name, Path: path, Span: span}, nil
		}
		resolved = presence.Value()
	}
	if resolved.IsNil() || resolved.IsNull() {
		// Terminal null is a value. Surface it as NilArg so
		// downstream consumers (print, jq, the null/present
		// predicates) can decide how to handle it. Commands
		// that need a non-null arg surface their own clearer
		// diagnostic when they encounter NilArg.
		return NilArg{Span: span}, nil
	}
	if resolved.IsStructured() {
		return StructuredValueArg{Name: name, Value: resolved, Span: span}, nil
	}
	s, err := resolved.Scalar()
	if err != nil {
		return nil, syntax.SpanErrorf(span, "variable %s: %v", qualify(name, path), err)
	}
	return ScalarValueArg{Text: s, Value: resolved, HasValue: true, Span: span}, nil
}

// qualify produces a "name.path" string for error messages, or
// just "name" when path is empty.
func qualify(name, path string) string {
	if path == "" {
		return name
	}
	return name + "." + path
}

func resolveAdapterArgParts(adapter, name, path string, span source.Span, env *Env) (Arg, error) {
	v, ok := env.Session.Get(name)
	if !ok {
		return nil, syntax.SpanErrorf(span, "undefined variable: %s", name)
	}
	resolved := v
	if path != "" {
		var err error
		path, err = resolveDynamicPath(path, env, span)
		if err != nil {
			return nil, err
		}
		lv, err := v.LookupValue(name, path)
		if err != nil {
			return nil, syntax.SpanErrorf(span, "%v", err)
		}
		resolved = lv
	}
	if resolved.IsNil() || resolved.IsNull() {
		return nil, syntax.SpanErrorf(span, "adapter %s: variable %s is null", adapter, name)
	}
	return AdapterArg{
		Adapter: adapter,
		Name:    name,
		Path:    path,
		Value:   resolved,
		Span:    span,
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
// Errors cite the host span (the syntax.VarRefExpr's `$xs[$i]`), not the
// inner `[$i]` position, because the path text in syntax.VarRefExpr is
// stored without per-segment offsets. The index variable must
// resolve to a scalar integer; strings parsable as an integer are
// accepted (jq -r round-trips produce string scalars), booleans and
// nulls are rejected.
func resolveDynamicPath(path string, env *Env, span source.Span) (string, error) {
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
		for j < len(path) && (isIdentStartByte(path[j]) || (j > nameStart && isIdentContinueByte(path[j]))) {
			j++
		}
		if j == nameStart || j >= len(path) || path[j] != ']' {
			// Should not be reachable: lexPathIndex would have
			// rejected this at tokenisation time. Surface the
			// state defensively rather than silently writing
			// out malformed text.
			return "", syntax.SpanErrorf(span, "malformed dynamic index in path %q", path)
		}
		name := path[nameStart:j]
		v, ok := env.Session.Get(name)
		if !ok {
			return "", syntax.SpanErrorf(span, "index variable $%s is not defined", name)
		}
		n, err := valueToIndex(v)
		if err != nil {
			return "", syntax.SpanErrorf(span, "index variable $%s: %v", name, err)
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

// valueToArg wraps a Value in the most specific Arg variant for
// the dispatch boundary: structured values stay structured,
// scalars become ScalarValueArg, nil becomes NilArg so the
// receiving command can decide whether to accept null at its
// own input boundary (jq, print, the strict-null and present
// predicates) rather than blanket-erroring at resolution time.
// span is attached to the resulting Arg so command-handler
// parsers can frame argument-position errors at the originating
// expression.
func valueToArg(v Value, span source.Span) (Arg, error) {
	if v.IsNil() || v.IsNull() {
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

// compareKind classifies a Value for the purpose of strict
// comparison dispatch. Numbers (json.Number, float64) are
// "number"; plain strings are "string"; booleans are "bool";
// explicit JSON null is "null" -- a first-class comparable
// value with the equality rules `null == null` true and `null
// == X` false for any non-null X (no ordering). Anything else
// (map, slice, absent values) returns "" and is rejected by
// evalCompare with an error citing the actual underlying type
// so users see why the operands are incomparable.
func compareKind(v Value) string {
	if v.IsNull() {
		return "null"
	}
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
func evalCompare(op string, l, r Value, span source.Span) (Value, error) {
	lk := compareKind(l)
	rk := compareKind(r)
	if lk == "" {
		return Value{}, syntax.SpanErrorf(span, "%s: left side is a %s; only scalars (numbers, strings, booleans) can be compared with %s", op, l.Kind(), op)
	}
	if rk == "" {
		return Value{}, syntax.SpanErrorf(span, "%s: right side is a %s; only scalars (numbers, strings, booleans) can be compared with %s", op, r.Kind(), op)
	}
	// Null comparisons are special: `null == null` is true,
	// `null == X` (X non-null) is false, and the cross-kind
	// case is well-defined rather than an error. The language
	// supports null as a first-class comparable value, so any
	// op that does not need ordering (== and !=) bypasses the
	// strict same-kind rule when at least one operand is null.
	// Ordering operators (<, <=, >, >=) are not defined for
	// null and surface as an explicit error.
	if lk == "null" || rk == "null" {
		if op != "==" && op != "!=" {
			return Value{}, syntax.SpanErrorf(span, "binary %s: null supports only == and != (no ordering)", op)
		}
		bothNull := lk == "null" && rk == "null"
		pass := (op == "==") == bothNull
		return BoolValue(pass), nil
	}
	if lk != rk {
		return Value{}, syntax.SpanErrorf(span, "binary %s: cannot compare %s to %s; coerce explicitly (e.g. \"$x |> jq tonumber\" for stringy numeric input)", op, lk, rk)
	}
	left, err := l.Scalar()
	if err != nil {
		return Value{}, syntax.SpanErrorf(span, "binary %s: left: %v", op, err)
	}
	right, err := r.Scalar()
	if err != nil {
		return Value{}, syntax.SpanErrorf(span, "binary %s: right: %v", op, err)
	}
	switch lk {
	case "number":
		v, err := evalNumericComparison(op, left, right)
		if err != nil {
			return Value{}, syntax.SpanErrorf(span, "%v", err)
		}
		return v, nil
	case "bool":
		if op != "==" && op != "!=" {
			return Value{}, syntax.SpanErrorf(span, "binary %s: booleans support only == and !=", op)
		}
		v, err := evalSymbolicTextComparison(op, left, right)
		if err != nil {
			return Value{}, syntax.SpanErrorf(span, "%v", err)
		}
		return v, nil
	default:
		v, err := evalSymbolicTextComparison(op, left, right)
		if err != nil {
			return Value{}, syntax.SpanErrorf(span, "%v", err)
		}
		return v, nil
	}
}

// isArithmeticOpText reports whether op is one of the five
// arithmetic operators.  Separate from isArithmeticOp (which
// operates on a syntax.Token) because the evaluator works with the
// already-extracted Op string on syntax.BinaryExpr.
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
	return Value{v: json.Number(text), kind: semantics.OriginScalar}
}

func literalValueParts(text string, quoted bool) Value {
	if quoted {
		return StringValue(text)
	}
	switch text {
	case "true":
		return BoolValue(true)
	case "false":
		return BoolValue(false)
	case "null":
		return NullValue()
	}
	if _, err := strconv.ParseFloat(text, 64); err == nil {
		return Value{v: json.Number(text), kind: semantics.OriginScalar}
	}
	return StringValue(text)
}

// evalNotEmpty implements the not-empty unary predicate. "Empty"
// is applied uniformly under the Go zero-value convention -- null
// is empty, "" is empty, [] / nil-slice is empty, {} / nil-map is
// empty, numeric 0 is empty, false is empty -- so the predicate
// reads the same inline (`assert not-empty $xs`) and inside a
// matches block (`field: not-empty`).
func evalNotEmpty(operand Value, span source.Span) (Value, error) {
	if operand.IsNil() || operand.IsNull() {
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
			return Value{}, syntax.SpanErrorf(span, "not-empty: %v", ferr)
		}
		return BoolValue(f != 0), nil
	case float64:
		return BoolValue(x != 0), nil
	case bool:
		return BoolValue(x), nil
	default:
		// Any carrier outside the documented vocabulary
		// (string / []any / map[string]any / json.Number /
		// float64 / bool / nil) is a misuse of the Value
		// API: ValueFromAny's doc lists those types, and
		// anything else is a programmer error. Fall back to
		// the scalar conversion so the diagnostic identifies
		// the unsupported carrier rather than silently
		// declaring it truthy.
		s, err := operand.Scalar()
		if err != nil {
			return Value{}, syntax.SpanErrorf(span, "not-empty: %v", err)
		}
		return BoolValue(s != ""), nil
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

func isIdentStartByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentContinueByte(b byte) bool {
	return isIdentStartByte(b) || (b >= '0' && b <= '9')
}

// AsBool extracts a boolean from a Value. It succeeds when the
// underlying raw value is a Go bool, regardless of the semantics.OriginKind
// tag: comparison results carry semantics.OriginBool explicitly, while a
// path lookup that lands on a JSON boolean field arrives with
// kind semantics.OriginUnknown but raw type bool. Both should drive
// if/assert truthiness without forcing the caller to add a
// redundant "== true". Anything else returns a type error.
func AsBool(v Value) (bool, error) {
	if b, ok := v.Raw().(bool); ok {
		return b, nil
	}
	if v.Kind() == semantics.OriginBool {
		return false, fmt.Errorf("condition has boolean origin but non-boolean value %T", v.Raw())
	}
	return false, fmt.Errorf("condition is a %s; use a comparison like '$x == 5' or a check like 'not-empty $x' to produce a boolean", v.Kind())
}
