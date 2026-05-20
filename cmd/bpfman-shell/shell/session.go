package shell

import (
	"maps"
	"slices"
)

// Session holds variable bindings, aliases, and user-defined commands
// (defs) for the REPL. It is the runtime state that persists across
// commands within a session.
//
// Variables live on a stack of frames. There is always at least one
// frame: the root frame, established by NewSession and never popped
// while the session is alive. Block-shaped constructs (def calls, if
// branches, foreach iterations, eventually attempts) push a fresh
// frame for the duration of the body and pop it on exit. let writes
// to the innermost frame; reads walk outward; deleting from the
// innermost frame leaves outer bindings intact.
//
// Aliases and defs are session-level and are not part of the frame
// stack.
type Session struct {
	frames          []map[string]Value
	aliases         map[string]string
	defs            map[string]*DefValue
	assertFailures  int
	deferFailures   int
	jobLeaks        int
	traceEnabled    bool
	eventuallyDepth int
}

// DefValue is a user-defined command registered via the `def NAME(P1
// P2 ...) { BODY }` form. It holds the parameter list, the parsed
// body, and the source location of the declaration for diagnostics.
type DefValue struct {
	Name   string
	Params []string
	Body   []Stmt
	Span
}

// RecordAssertFailure increments the assertion failure counter.
func (s *Session) RecordAssertFailure() {
	s.assertFailures++
}

// AssertFailures returns the number of recorded assertion failures.
func (s *Session) AssertFailures() int {
	return s.assertFailures
}

// RecordDeferFailure increments the defer-failure counter.
func (s *Session) RecordDeferFailure() {
	s.deferFailures++
}

// DeferFailures returns the number of recorded defer failures.
// Drivers consult this after script completion to set the
// process exit code: any non-zero count means at least one
// defer reported a non-ok rc.
func (s *Session) DeferFailures() int {
	return s.deferFailures
}

// RecordJobLeak increments the unmanaged-job counter. The
// scope-exit leak check calls it for each started job that the
// script never waited or killed; drivers consult JobLeaks after
// script completion to fail the exit code.
func (s *Session) RecordJobLeak() {
	s.jobLeaks++
}

// JobLeaks returns the number of unmanaged jobs reported at
// scope exit. A non-zero count means at least one 'start' had
// no matching wait or kill before its enclosing defer scope
// unwound, and the script should fail.
func (s *Session) JobLeaks() int {
	return s.jobLeaks
}

// EnterEventuallyAttempt and ExitEventuallyAttempt bracket the
// body of one eventually attempt so the driver-side assert /
// require dispatchers can recognise retryable failures and
// suppress their "[assert]/[require] FAIL: ..." stderr line.
// The assertion counter still moves through its usual
// record-and-return path; eventually's snapshot/reset compensates
// for the counter side. The depth count permits nested eventually
// constructs without misclassifying outer scope as quiet.
//
// InEventuallyAttempt is the read side: drivers consult it from
// their assertion handlers before deciding whether to render the
// failure to stderr. Outside any attempt the depth is zero and
// reporting proceeds normally.
func (s *Session) EnterEventuallyAttempt() { s.eventuallyDepth++ }
func (s *Session) ExitEventuallyAttempt()  { s.eventuallyDepth-- }
func (s *Session) InEventuallyAttempt() bool {
	return s.eventuallyDepth > 0
}

// SetTrace enables or disables execution tracing. Drivers usually
// install an Env.Trace callback whose body consults this so the
// `trace on` / `trace off` builtin (and a startup CLI flag) can
// flip the state mid-session without swapping the Env hook itself.
func (s *Session) SetTrace(on bool) {
	s.traceEnabled = on
}

// TraceEnabled reports whether tracing is currently enabled.
func (s *Session) TraceEnabled() bool {
	return s.traceEnabled
}

// NewSession returns an empty session with a single root frame.
func NewSession() *Session {
	return &Session{
		frames:  []map[string]Value{make(map[string]Value)},
		aliases: make(map[string]string),
		defs:    make(map[string]*DefValue),
	}
}

// SetDef registers (or replaces) a user-defined command. The caller
// is responsible for validating the name and parameter list.
func (s *Session) SetDef(d *DefValue) {
	s.defs[d.Name] = d
}

// GetDef retrieves a user-defined command. The second return value
// indicates whether a def with that name exists.
func (s *Session) GetDef(name string) (*DefValue, bool) {
	d, ok := s.defs[name]
	return d, ok
}

// DeleteDef removes a user-defined command. Returns true if the def
// existed.
func (s *Session) DeleteDef(name string) bool {
	if _, ok := s.defs[name]; !ok {
		return false
	}
	delete(s.defs, name)
	return true
}

// DefNames returns the sorted list of registered def names.
func (s *Session) DefNames() []string {
	return slices.Sorted(maps.Keys(s.defs))
}

// Set binds a value to a variable name in the innermost frame,
// shadowing any same-named binding in an outer frame. Crossing a
// frame boundary creates a new shadowing binding rather than
// mutating an outer one.
func (s *Session) Set(name string, v Value) {
	s.frames[len(s.frames)-1][name] = v
}

// Get retrieves a variable's value. The lookup walks the frame
// stack from innermost to outermost and returns the first hit, so
// an inner binding shadows an outer one. The second return value
// indicates whether a binding exists in any frame.
func (s *Session) Get(name string) (Value, bool) {
	for i := len(s.frames) - 1; i >= 0; i-- {
		if v, ok := s.frames[i][name]; ok {
			return v, true
		}
	}
	return Value{}, false
}

// DeleteLocal removes a binding from the innermost frame only. A
// binding that lives further out is left intact: after
// DeleteLocal, Get may still return a value if an outer frame
// holds one. Callers that want to remove the visible binding
// wherever it lives should use DeleteVisible.
func (s *Session) DeleteLocal(name string) {
	delete(s.frames[len(s.frames)-1], name)
}

// DeleteVisible removes the first binding for name found while
// walking frames from innermost outward. Outer bindings are left
// intact even if multiple frames hold a binding of the same name
// -- only the visible shadowing binding is removed, and the next
// outer binding becomes visible.
//
// This is the operation a user-facing `unset NAME` builtin should
// call: the intuitive semantics of "remove the binding I can
// currently see" is delete-the-visible, not delete-from-innermost.
// The evaluator itself uses DeleteLocal; DeleteVisible is the
// escape hatch for explicit cleanup paths.
func (s *Session) DeleteVisible(name string) {
	for i := len(s.frames) - 1; i >= 0; i-- {
		if _, ok := s.frames[i][name]; ok {
			delete(s.frames[i], name)
			return
		}
	}
}

// Names returns the visible variable set as a sorted slice, with
// each name appearing exactly once. Inner bindings shadow outer
// ones, so a name present in multiple frames is reported once.
func (s *Session) Names() []string {
	seen := make(map[string]struct{})
	for _, f := range s.frames {
		for name := range f {
			seen[name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// PushFrame appends an empty frame to the stack. Subsequent Set
// calls bind into this frame; Get continues to walk outward and
// can see bindings that the new frame does not shadow.
func (s *Session) PushFrame() {
	s.frames = append(s.frames, make(map[string]Value))
}

// PopFrame removes the innermost frame. PopFrame panics if asked
// to pop the root frame: every Push must be paired with exactly
// one Pop, and an unbalanced Pop is an invariant violation in the
// evaluator. Callers that need exception-safe push/pop should use
// WithFrame.
func (s *Session) PopFrame() {
	if len(s.frames) <= 1 {
		panic("shell.Session.PopFrame: cannot pop root frame")
	}
	s.frames = s.frames[:len(s.frames)-1]
}

// WithFrame pushes a fresh frame, runs fn, and pops in a defer
// so the pop runs on every exit path (success, error, panic). The
// evaluator pushes frames only through WithFrame so block scope
// is symmetric with the body's lexical extent.
func (s *Session) WithFrame(fn func() error) error {
	s.PushFrame()
	defer s.PopFrame()
	return fn()
}

// ChildForSource returns a fresh sub-session for module-scoped
// evaluation per SCOPE-DESIGN.md Section 5. The child starts
// with a single empty root frame, no aliases, and zero counters,
// but inherits the parent's defs (shallow-cloned -- DefValue is
// immutable after construction so a map copy is sufficient) and
// the parent's traceEnabled flag (child-local: toggling tracing
// inside a sourced file does not propagate back).
//
// Counters do not inherit; they accumulate back into the parent
// via MergeChildSource regardless of evaluation outcome. Defs
// merge only on successful completion -- a failed source is
// transactional at the module boundary.
func (s *Session) ChildForSource() *Session {
	child := NewSession()
	maps.Copy(child.defs, s.defs)
	child.traceEnabled = s.traceEnabled
	return child
}

// MergeChildSource folds the child sub-session's effects back
// into s. Counter deltas (assert, defer, job-leak failures)
// always accumulate so the run's overall outcome reflects every
// failure that happened, including those inside a failed source.
// When commitDefs is true the child's defs are merged into the
// parent, making new defs and redefinitions visible to the
// importer; when false, the child's defs are discarded entirely.
//
// Aliases and variables are always discarded: those are private
// to the child session.
func (s *Session) MergeChildSource(child *Session, commitDefs bool) {
	s.assertFailures += child.assertFailures
	s.deferFailures += child.deferFailures
	s.jobLeaks += child.jobLeaks
	if !commitDefs {
		return
	}
	maps.Copy(s.defs, child.defs)
}

// SetAlias binds a first-token alias. The caller is responsible for
// validating that name does not collide with shell commands.
func (s *Session) SetAlias(name, expansion string) {
	s.aliases[name] = expansion
}

// GetAlias retrieves an alias expansion. The second return value
// indicates whether the alias exists.
func (s *Session) GetAlias(name string) (string, bool) {
	v, ok := s.aliases[name]
	return v, ok
}

// DeleteAlias removes an alias binding.
func (s *Session) DeleteAlias(name string) {
	delete(s.aliases, name)
}

// AliasNames returns the sorted list of defined alias names.
func (s *Session) AliasNames() []string {
	return slices.Sorted(maps.Keys(s.aliases))
}
