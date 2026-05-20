# bpfman-shell scope design

This document is the implementation plan for lexical variable
scope in bpfman-shell. The language gets one variable-scope
primitive: a stack of frames on `Session`. `let` writes to
the innermost frame, lookup walks outward, and each
block-like construct chooses an explicit frame lifetime:
`if` branches and `def` calls run in a fresh frame;
`foreach` and `eventually` allocate one frame per iteration
/ attempt; `source` evaluates in a fresh sub-session.
Aliases and defs stay session-level; defs do not capture
variable frames.

Companion to `BINDING-DESIGN.md` (binding-site syntax) and
`GRAMMAR.md` (surface grammar). Load-bearing references are
the eval code in `expr.go` and the source builtin in
`repl/loop.go`.

## Context

bpfman-shell is the REPL and DSL implemented under
`cmd/bpfman-shell/`. The language is argv-first and
lifecycle-shaped: it drives `bpfman` operations
(load/attach/get/detach/unload, dispatcher and link
inspection) at an interactive prompt, and is also the
substrate for a growing share of the e2e test suite. A
`bpfman ...` command copied from shell history can usually
be pasted into a `.bpfman` script unchanged; expression
syntax is opt-in at specific binding sites (`let X = EXPR`,
`assert EXPR`, parenthesised command arguments,
interpolation bodies). See `GRAMMAR.md` for the surface
grammar.

The implementation is split:

- `cmd/bpfman-shell/shell/` -- pure language layer:
  tokeniser, parser, AST, expression evaluator, `Session`,
  `Value`, matchers. No I/O. This is the package the change
  touches.
- `cmd/bpfman-shell/repl/` and `cmd/bpfman-shell/*.go` --
  line editor, builtins, job control, command dispatch.
  Sees the language layer only through `Session` and `Env`.

The corpus this design targets is 66 `.bpfman` test files
under `e2e/scripts/` and `e2e/new/`, plus one shared library
file `e2e/lib.bpfman`. Tests run via the `test-e2e-scripts`
harness, which globs each file as a complete test. Typical
scripts are 20-80 statements -- load, attach, drive, assert,
detach, unload -- and bind 10-30 variables across that span;
names like `prog`, `pid`, `link`, `expected` recur in nearly
every file.

Three corpus properties drive the scope problem:

- **Library sourcing.** Most tests start with `source
  ../lib.bpfman`, which contributes assertion helpers like
  `expect_program_load` and `expect_program_round_trip`. The
  library and its consumers share one session today.
- **Repeated helper calls.** Within a single test, a helper
  is often called several times -- multi-program tests
  invoke `expect_program_load` once per loaded program. Each
  invocation evaluates the helper body against the same flat
  session.
- **Crowded names.** Scripts and helpers reach for the same
  identifiers (`prog`, `pid`, `link`) because the domain has
  a small vocabulary. There is no naming discipline that
  would route around collisions; the scripts read like the
  CLI does.

There are no external clients of bpfman-shell. The change is
breaking; the corpus migrates with the language layer.

## 1. The bug

Four constructs look lexically scoped (`def`, `foreach`,
`if`, `retry`) and one inclusion mechanism (`source`). The
implementation is a single flat namespace.

### 1.1 Storage

`Session` (`shell/session.go`) holds one `vars` map plus
session-level maps for aliases and defs:

    type Session struct {
        vars    map[string]Value
        aliases map[string]string
        defs    map[string]*DefValue
        ...
    }

`let x = ...` becomes `Session.Set("x", v)`. No frames, no
parent pointers.

### 1.2 The three partial mitigations

1. `callDef` (`expr.go:564-597`) snapshots prior values of
   the parameter names, sets them for the call, restores
   them on return.
2. `saveForEachNames` (`expr.go:792-815`) does the same for
   foreach loop variables.
3. `Source` (`repl/loop.go:674-723`) wraps the file in
   `WithDeferScope` so top-level `defer`s in the sourced
   file unwind at end-of-source.

These are the only scope-shaped behaviours. Everything else
writes directly to the parent session.

### 1.3 What leaks

- `let x = ...` in a def body persists in the caller's
  session after return.
- `let x = ...` in a foreach body persists past the loop
  and bleeds across iterations.
- `let x = ...` in `if`/`elif`/`else` has no save/restore;
  bindings persist unconditionally.
- `let x = ...` in `retry { ... }` persists past the loop.
- Top-level statements in a sourced file write through to
  the parent's vars, defs, and aliases.

### 1.4 Why it bites

The grammar promises scope (`{ ... }` blocks); the runtime
does not deliver it. The def-parameter shadow is doing
hidden work: `expect_program_load(prog ...)` in
`e2e/lib.bpfman` is correct only because the parameter
names shadow caller variables for the duration of the call.
The instant a helper introduces a scratch local, it
overwrites the caller's same-named binding and the
overwrite persists after return. The original is dropped,
not layered under a shadowing frame.

The retry/until pattern in nine `TestMultiProg*` scripts
under `e2e/new/` relies on the leak:

    retry {
        let probe <- test -f "${ack}.1"
    } until $probe.ok or timeout 5s
    require $probe.ok

`probe` is bound inside the retry body, then read by both
the `until` clause and the post-loop `require`. The script
only works because the body's `let` leaks out. The fix is
in Section 3.4: `retry ... until` is removed and replaced
by `eventually`, which makes the success boundary lexical
rather than ambient.

## 2. Scope contract

The implementation has one variable-scope primitive: a stack
of frames on `Session`.

    type Session struct {
        frames  []map[string]Value
        aliases map[string]string
        defs    map[string]*DefValue
        ...
    }

There is always at least one frame. For a script invocation,
the root frame is the script's top level and is discarded
when the script finishes. For the interactive REPL, the root
frame is the persistent prompt frame and survives across
inputs -- so `let x = 1` followed by `print $x` at the
prompt still works the way you expect.

### 2.1 Session API

- `Get(name)` walks frames from innermost to outermost and
  returns the first hit.
- `Set(name, value)` writes to the innermost frame only.
  Does not search outward to mutate an existing binding.
- `DeleteLocal(name)` deletes from the innermost frame only.
  Does not delete a binding that lives further out. This is
  the primitive `let`-shaped operations use; nothing in the
  evaluator's normal-path code calls `DeleteVisible`.
- `DeleteVisible(name)` walks frames from innermost outward
  and deletes the first matching binding wherever it lives.
  This is the operation a user-facing `unset NAME` builtin
  should call -- the intuitive semantics of "remove the
  binding I can currently see" is delete-the-visible, not
  delete-from-innermost. Both APIs ship in commit 1; the
  evaluator never calls `DeleteVisible` itself, but the
  `unset` builtin (and any future "global cleanup" path)
  has a single point to reach for.
- `Names()` returns the visible variable set as a sorted
  slice, with each name appearing exactly once. Inner
  bindings shadow outer ones, so a name that exists in
  multiple frames is reported once.
- `PushFrame()` appends an empty frame.
- `PopFrame()` removes the innermost frame; panics if asked
  to pop the root frame.
- `WithFrame(fn func() error) error` pushes a frame, runs
  `fn`, pops in a `defer`. The evaluator pushes frames only
  through `WithFrame` so `PopFrame` runs on every exit path
  (errors, panics, early returns).

Both `DeleteLocal` and `DeleteVisible` ship in commit 1 to
avoid retrofit pain at the `unset` builtin: the design
explicitly does not relax the local primitive to do
"visible delete" implicitly.

### 2.2 Variables only

Aliases and defs are not part of the frame stack. They
remain session-level declarations. A `let` writes to a
frame; an `alias` writes to `Session.aliases`; a `def`
writes to `Session.defs`. Different storage, different
lifetimes.

A consequence: a `def` or `alias` declared inside a block
updates the session-level table. After

    if cond {
        def helper() { ... }
    }
    helper

`helper` is callable if and only if the branch ran. This
mirrors today's declaration behaviour and is not changed by
this design. The style rule remains: declare helpers at the
top of the script. The same applies to in-block aliases.

### 2.3 Defs do not capture variable frames

A def body resolves variables against the caller's current
frame stack at call time plus its own call frame.
Definition-time variable bindings are not captured. The
following fails at the call site, not at the definition
site:

    if true {
        let x = 1
        def f() {
            print $x
        }
    }
    f
    # error: x is unbound

The design is lexical block lifetime for variables, not
Scheme-style closures over the variable environment. Command
and def lookup remain session-level. If real closures become
a need, that is a separate design.

### 2.4 Rust-style rebinding within a block

`let x = "a"; let x = "b"` in the same block rebinds in
place against the innermost frame. Crossing a block boundary
creates a fresh binding in the inner frame that shadows the
outer; on block exit the inner is gone and the outer is
reinstated. You do not have to invent `x2`.

A consequence: the following prints `outer`, not `inner`.

    let x = "outer"
    if cond {
        let x = "inner"
    }
    print $x

The inner `let` writes to the if-branch frame, which is
popped when the branch exits; the outer `x` is reinstated.

The proposal does not provide an assignment-to-outer-frame
form: `let` is always create-or-rebind in the innermost
frame, never mutate-outer. This is a deliberate sacrifice
of "definite assignment" patterns that conditionally
compute a value:

    let x = 0
    if cond {
        let x = 1
    }
    print $x

prints `0`, not "maybe 1". If branch-computed values become
useful, the right addition is a value-returning block or a
distinct assignment operator -- not a relaxation of `let`'s
contract. See Section 9.

### 2.5 Static checker mirrors the runtime model

The static checker in `check.go` currently keeps its own
scope state via ad-hoc save/restore (foreach loop variables,
def parameters; `if` is not yet handled). When the runtime
gains a frame stack, the checker has to gain one in step --
otherwise the two halves of the language disagree about
what is visible after a block, which is exactly the kind of
mismatch the redesign exists to remove.

The checker frame mirror:

    type checker struct {
        frames []checkFrame
        aliases map[string]string
        issues []Issue
    }
    type checkFrame struct {
        defined  map[string]bool
        shapes   map[string]Shape
        literals map[string]*LiteralExpr
    }

with the same three operations:

- `define(name, shape, lit)` writes to the innermost
  checker frame.
- `lookup(name)` walks frames from innermost outward.
- `withFrame(fn)` pushes a checker frame, runs `fn`, pops
  in a defer.

The checker's `withFrame` wrap-sites parallel the runtime's
but at definition-time rather than dynamic execution:

- def body checking, under a parameter frame.
- foreach body checking, with the loop variables defined
  in the frame (the checker has no notion of "iteration",
  so one frame for the body is enough).
- each `if` / `elif` / `else` branch body checked in its
  own frame; the checker does not know which branch will
  run, so all branches are checked separately and none of
  them contributes bindings to the post-`if` scope.
- `eventually` body checking, in its own frame.
- source: a fresh sub-checker with the parent's defs
  cloned in.

This pulls the existing `snapshotVar` mechanic into the
same primitive the runtime uses, so an `if`-branch `let`
becomes unreachable to the checker after the branch ends
-- mirroring the runtime semantics -- and the misalignment
risk disappears.

Lands in its own commit (commit 2) right after the
runtime `Session` frame stack, so any subsequent
construct-wiring commit can move runtime and checker in
lockstep.

## 3. Construct-by-construct semantics

Every block-like execution boundary chooses an explicit
variable-frame lifetime. Most bodies execute inside a fresh
frame allocated by `WithFrame`; `foreach` and `eventually`
allocate one frame per dynamic execution of the body
(per-iteration / per-attempt), not one frame for the
syntactic block. `source` is the only construct that does
not push a frame on the current session at all -- it
evaluates the file in a fresh sub-session (Section 5).
`eventually` additionally wraps each attempt in its own
defer scope alongside the variable frame (Section 3.4 and
Section 4). The precise binding behaviour per construct:

### 3.1 def call

At the call site: push a frame, bind parameters into it,
evaluate the body, pop on return. The existing parameter
save/restore in `callDef` is deleted. Recursion works
because each call gets its own frame.

### 3.2 foreach

One fresh frame per iteration. The loop variables bind into
that frame; body `let`s bind into that frame; the frame is
popped at iteration end. No frame for the loop as a whole.

A useful teaching model: a foreach iteration is an anonymous
def call. The loop variables are the parameters, the body is
the function body. The evaluator does not literally lower
foreach into a def -- dedicated evaluation gives better
diagnostics and source locations -- but the scope discipline
is identical.

Loop-wide accumulation is intentionally not supported. Any
state that crosses an iteration boundary must be explicit;
the language has no outward-write primitive. When
accumulation becomes a real need, the right shape is a
value-producing form added later, not leaked locals (see
Section 9).

The rule the corpus already follows: **foreach state does
not leak**. Values leave a foreach only through the
collect result (`guard refreshed <- foreach l in $links {
... }`), through command side effects, or through a defer
captured while the frame was live (Section 4). The
existing corpus is largely written in this style; the new
semantics formalises what is already idiomatic.

### 3.3 if / elif / else

One frame for the selected branch only. The chosen branch
body runs inside `WithFrame`; `let`s in that body bind into
the branch frame and disappear when the branch ends.
Branches do not see each other's bindings.

Condition expressions evaluate in the outer frame.

### 3.4 eventually (replaces retry)

`retry ... until` is removed. It is replaced by an
`eventually` construct whose success boundary is lexical:
the body either succeeds or fails as a unit, and there is
no ambient `success` value the loop tail can inspect.

The motivation is consistency with the rest of the scope
model. Under block scope, variables stop leaking out of
blocks; if `retry`'s success/failure remained an ambient
state the `until` clause could observe, the language would
fix variable leakage and keep control-outcome leakage,
which is a strange asymmetry. `eventually` removes the
asymmetry: outcome is a property of the body, captured by
the construct, not a magic value flowing alongside.

Grammar:

    EventuallyStmt =
        "eventually" "timeout" Duration
            [ "interval" Duration ] Block
    EventuallyCommand =
        "eventually" "timeout" Duration
            [ "interval" Duration ] Block
    Duration =
        # Go-style duration literal accepted by
        # time.ParseDuration: e.g. 100ms, 1s, 5s, 2m,
        # 1m30s. The lexer recognises Duration only at
        # the eventually grammar position, so command
        # arguments that look like 5s remain ordinary
        # Word tokens.

`timeout DUR` is **mandatory**. A test DSL should not have
unbounded waits by accident; `eventually { ... }` with no
timeout is a parse error. `interval DUR` is optional and
named; the default is `100ms`. Named fields rather than
positional duration arguments keep the surface readable and
leave room for future extensions (jitter, max-interval,
deadline grace) without ambiguity.

Examples:

    eventually timeout 5s {
        require (test -f "${ack}.1")
    }

    eventually timeout 10s interval 250ms {
        guard got <- bpfman program get $pid
        assert $got.status.kernel.id == $pid
    }

Bind form for non-fatal probing:

    let r <- eventually timeout 1s interval 50ms {
        test -f "${ack}.1"
    }
    # r is { ok, timed_out, attempts, elapsed_ms, last_error }

`eventually` is both a statement form and a bindable
command form. Internally, both reduce to one operation; the
statement form halts the enclosing scope on overall
failure, the bind form publishes the structured result.

Semantics:

- Each attempt runs the body inside `WithFrame` -- a fresh
  attempt frame. Variables bound inside the body do not
  escape the attempt. Each attempt also runs inside its own
  defer scope; defers registered during an attempt unwind
  at attempt end, before the next attempt or before the
  construct returns. Failed attempts clean up their own
  partial work before retry; successful attempts run their
  defers immediately at attempt end. Speculative attempts
  do not accumulate cleanup at outer-scope unwind.
- An attempt succeeds if the body completes without a
  retryable failure and no new `assert` failure was
  recorded during the attempt. Otherwise the attempt
  fails.
- Any retryable failure aborts the current attempt
  immediately. A multi-statement body is "all of these
  must succeed":

      eventually timeout 5s {
          test -f "${ack}.1"
          print "ack exists"
      }

  If `test -f` returns non-zero, the attempt aborts at
  that statement; `print` does not run; the retry loop
  schedules another attempt. This is deliberately stricter
  than ordinary command execution: inside `eventually`,
  commands are conditions unless their result is captured
  and handled by a nested construct.
- Retryable failures retry until `timeout` elapses; fatal
  failures halt the script immediately, regardless of
  whether the construct is bound. The bind form catches
  overall retry timeout, not programmer mistakes. So:

      let r <- eventually timeout 5s {
          $missing
      }

  halts with an unbound-variable error; `r` is never
  assigned.
- If an attempt-local defer fails while unwinding,
  `eventually` stops immediately and propagates that
  cleanup error as fatal. Cleanup failure means the
  environment is uncertain; retrying compounds leaks
  rather than reaching success.
- On overall failure (timeout reached without any
  successful attempt):
  - Unbound form: the last retryable error propagates as
    the construct's failure, halting the enclosing scope
    like `require`.
  - Bound form: `r.ok = false`, `r.timed_out = true`,
    `r.last_error` carries the last retryable failure;
    the script continues.

The result shape:

    {
      ok:         bool
      timed_out:  bool
      attempts:   int
      elapsed_ms: int
      last_error: envelope-or-error-or-nil
    }

`last_error` is nil on overall success. On timeout it
carries the last retryable failure value, using the same
envelope/error shape the failing statement would have
produced outside `eventually` (so callers can branch on
the same fields they would inspect from a guard or a
bound command).

`timed_out` is derivable from `ok == false` today
(timeout is the only terminal mode), but is named
explicitly so future terminal modes (max-attempts,
cancellation, fatal-as-value) can be distinguished
without breaking callers.

`elapsed_ms` is an integer count of elapsed milliseconds
to keep the Value system free of a duration scalar. Tests
that want a duration can construct one from the integer;
the bare integer compares cleanly in matchers without
needing parsing.

#### 3.4.1 Retryable vs fatal failures

`eventually` retries only on conditions the world might
satisfy later. Programmer mistakes propagate immediately:
retrying them for five seconds delays diagnosis without
changing the outcome.

The classification cannot be done robustly by pattern-
matching on error strings or `*SyntaxError`-type checks
spread across the evaluator. The implementation introduces
a typed-error layer first (commit 7) and `eventually`
consumes it in commit 8. The interface:

    type RetryableError interface {
        error
        Retryable() bool
    }

with concrete types covering the retryable cases. Reuse
the existing `GuardFailure` shape verbatim and follow its
field-naming for the new types:

    type GuardFailure struct {  // existing
        Span
        Primary  string
        Args     []Arg
        Envelope Envelope
    }
    type CommandFailure struct {
        Span
        Args     []Arg
        Envelope Envelope
    }
    type AssertFailure struct {
        Span
        Expr string
    }
    type RequireFailure struct {
        Span
        Expr string
    }

Each implements `Retryable() bool { return true }`. Add
the methods to `GuardFailure` without changing its
existing struct fields or `Error()` rendering, so
diagnostic output stays identical.

**`CommandFailure` means specifically: command was
successfully resolved and executed, and returned a non-ok
envelope.** It does not cover "unknown command", "command
launch failed", "argument parsing rejected the command
line", or other environment-or-programmer errors. Those
remain fatal even inside `eventually`; the classifier
must not wrap them as `CommandFailure` or the construct
will sit and retry a programmer mistake for the full
timeout.

Both expression-form and verb-form assertion failures
use the typed errors. `assert $x == 1` and the verb-form
`assert nil $x.field` both produce `AssertFailure`;
`require (test -f file)` and `require ok ...` both
produce `RequireFailure`. The command-side assert
dispatcher (`ExecAssertStmt`) must return the typed
variant, not just `error`, so `eventually` can classify
uniformly.

The typed assertion errors classify the failure; they do
not own counter mutation. Until assertion failure becomes
an evaluator result in its own right (the cleanup noted
above), the existing assertion dispatcher remains the only
code that increments the session's assert-failure counter,
and `eventually` compensates with the snapshot/reset
protocol described earlier in this section. Splitting
"classify" from "count" avoids double-accounting during
the transition.

Every retryable typed error embeds `Span` and must
survive `frameAtSpan` / `SyntaxError` / `SpanCarrier`
wrapping such that `errors.As` still locates the
interface. `SyntaxError.Unwrap()` already exposes `Cause`,
so wrapping a typed retryable as the `Cause` keeps
`errors.As` happy. Any new wrapping helper introduced in
this change must preserve the same property -- otherwise
`eventually` silently classifies a retryable failure as
fatal.

`eventually` consumes the layer via `errors.As`, not a
direct type assertion, because evaluator errors flow
through `frameAtSpan`, `SyntaxError`, and other wrapping
helpers:

    var r RetryableError
    if errors.As(err, &r) && r.Retryable() {
        lastError = err
        continue
    }
    return err   // fatal

No string parsing, no exhaustive type switch across
unrelated error shapes. Adding a new retryable case is a
matter of implementing the interface; nothing in
`eventually` needs to change.

| Cause                                       | Class      |
|---------------------------------------------|------------|
| `require` predicate false                   | retryable  |
| `assert` predicate false (recorded failure) | retryable  |
| `guard` command non-zero rc                 | retryable  |
| Ordinary command non-zero rc                | retryable  |
| Parse error                                 | fatal      |
| Unknown command, unknown def                | fatal      |
| Unbound variable, bad field path            | fatal      |
| Type error, bad coercion                    | fatal      |
| Pop-of-root or other invariant violations   | fatal      |

`assert` failures are recorded into the session counter
today. Inside `eventually`, an `assert` failure within an
attempt body marks the attempt as failed (retryable), but
it must not contribute directly to the final session
counter. The existing dispatcher may still increment the
counter during the attempt; `eventually` snapshots,
observes, and resets that mutation before deciding the
construct's final effect. The
counter reflects the construct's overall outcome: on
overall success it is unchanged; on overall failure it is
incremented exactly once. Implementation: snapshot the
counter at attempt start, observe the delta at attempt
end, reset to the snapshot before the next attempt; on
overall failure, increment the snapshot value by one and
restore.

The snapshot/reset protocol is deliberately local to this
change. It compensates for the fact that `assert` today
mutates session state as a side effect rather than
returning a structured `AssertionError` for the top-level
driver to count. A later cleanup should consider promoting
assertion failure to an evaluator result; if that lands,
`eventually` collapses to "retry on retryable evaluator
errors, count assertion errors only when the top-level
driver sees them". Not in scope for this change.

## 4. Defer capture

Variable scopes and defer scopes are separate stacks. They
interact at one point: defer registration time.

A `defer` statement evaluates its command's arguments
immediately, when the defer is registered, and stashes the
resolved argument vector in the defer entry. It does not
store an unevaluated AST that resolves variables at unwind
time. (This is already the implementation -- see
`evalDeferStmt` in `expr.go:941-958` and the existing
`TestEvalProgram_Defer_ArgsCapturedAtRegisterTime` and
`TestEvalProgram_Defer_ForEachRegistersInEnclosing`. The
new scope model makes the behaviour load-bearing rather
than merely tidy.)

This is what makes per-iteration foreach frames safe for
the corpus's lifecycle style:

    foreach l in $links {
        defer bpfman link detach $l
    }

Each iteration registers a concrete detach action with that
iteration's `$l` already resolved into the entry's `Args`.
When the iteration frame is popped, the deferred action
does not care: it no longer references `$l` as a variable.
At enclosing-scope unwind, the deferred actions fire in
LIFO order against their captured argument vectors.

The same shape holds for def calls:

    def use_program(prog) {
        defer bpfman program unload $prog
        ...
    }

`$prog` is resolved at defer registration; the captured
vector survives the def's call frame being popped on
return.

And for `eventually` attempts that register defers inside
their body, each attempt's deferred actions hold their
attempt-frame's resolved values. `eventually` opens a fresh
defer scope per attempt: defers registered during an
attempt unwind at attempt end, before the next attempt or
before the construct returns. Failed-attempt cleanup runs
immediately, so transient resources do not pile up across
retries. The variable-frame rule and the defer-scope rule
are aligned -- both are attempt-local.

Without this capture-at-registration rule, per-iteration
foreach frames would silently break a substantial fraction
of the corpus. With it, defer is the third documented
escape valve from a block frame (alongside the bind-collect
result and command side effects), and the corpus already
uses it correctly.

The implementation plan pins this behaviour as contract
(commit 3) before the frame wiring lands (commits 4 and 5),
so any regression there is caught immediately rather than
in the corpus migration.

## 5. Source semantics

`source` is module-scoped evaluation. It is not a textual
splice.

`Source` evaluates the file against a fresh sub-session
whose frame stack starts empty (one new top frame) and whose
`defs` map is seeded from a clone of the parent's. The
library can call sibling defs the importer has already
loaded; it cannot read or mutate the importer's variables
through the back door.

The boundary across the source call:

| Thing      | Reads from caller  | Exports to caller   |
|------------|--------------------|---------------------|
| `vars`     | no                 | no                  |
| `aliases`  | no                 | no                  |
| `defs`     | yes                | yes (on success)    |

Algorithm:

1. Clone (shallow-copy) the parent's `defs` map into the
   child sub-session. This relies on `DefValue` being
   immutable after registration -- `Name`, `Params`, `Body`,
   `Span` are all set at construction and never mutated by
   the evaluator. If a future change makes any of those
   fields mutable, clone the values as well as the map.
2. Evaluate the file against the child. Within the file, def
   calls resolve through the child's `defs`, so later
   redefinitions override earlier ones in the normal way.
3. On successful completion, merge the child's `defs` back
   into the parent. New defs become visible to the importer;
   redefinitions of inherited defs overwrite the importer's
   previous binding.
4. On failed evaluation, discard the child sub-session
   wholesale. The parent's `defs` are never partially
   mutated. `source` is transactional at the module
   boundary.

The child's variables and aliases are always discarded. The
existing `WithDeferScope` and Env-callback save/restore
around `Source` are preserved.

The source defer scope unwinds on both success and failure.
A library that registers a top-level `defer` runs that
cleanup at end-of-source regardless of whether evaluation
completed cleanly. The transactional rule applies to
exported `defs`, not to cleanup -- a failed source still
runs its registered defers, it just does not publish any
new defs to the importer.

### 5.1 Session counters and trace state

`Session` carries `assertFailures`, `deferFailures`,
`jobLeaks`, and `traceEnabled` outside the vars/aliases/
defs storage. The module boundary has to decide explicitly
what crosses it, because shallow-copying the parent
session would catch these by accident.

The policy:

| Field            | Inherits into child | Accumulates back |
|------------------|---------------------|------------------|
| `traceEnabled`   | yes (child-local)   | no               |
| `assertFailures` | no (starts at 0)    | yes (delta only) |
| `deferFailures`  | no (starts at 0)    | yes (delta only) |
| `jobLeaks`       | no (starts at 0)    | yes (delta only) |

`traceEnabled` is copied into the child at source start. A
`trace on` / `trace off` inside the sourced file affects
that child evaluation only and does not propagate back to
the importer's session. This keeps trace a per-script
visibility knob; a library that flips tracing for its own
diagnostics does not silently re-configure the importer.

Failure counters describe execution outcome, not exported
module state, so they belong to the run as a whole and
accumulate back into the parent regardless of whether
`defs` merge. The "delta only" rule keeps the counters
honest: if a sourced library logs three assertion failures,
the parent's counter goes up by three; the child does not
double-count what the parent already had.

This is orthogonal to the transactional defs rule. A
source that fails mid-evaluation still contributes its
asserted-failure count -- the failures happened, and the
test result needs to reflect them.

## 6. Non-goals

Explicitly out of scope for this change:

- No outward-assignment primitive. No `export`, no
  `nonlocal`, no `global`.
- No block-local aliases. In-block `alias` writes to the
  session alias table.
- No block-local defs. In-block `def` writes to the session
  def table.
- No closures. Defs do not capture variable frames.
- No value-returning defs. Defs remain side-effect helpers
  in this change. The shape for the future extension is
  pinned in Section 9 (explicit `return EXPR`, `BindResult`
  reuse, bind-position only).
- No value-returning blocks. `let ys = foreach x in xs {
  transform($x) }` is a clean future shape but not in this
  change.
- No `source --inline` for the REPL. `source` is uniformly
  module-scoped, including at the interactive prompt.
- No compatibility mode for the old flat namespace. The
  corpus migrates with the language layer.
- No `retry ... until` once `eventually` lands. The legacy
  construct is removed in commit 9, not deprecated and
  carried.
- No exponential or jittered backoff for `eventually`.
  Fixed interval only (default `100ms`). Backoff can be
  added later if a test legitimately needs it; the
  current corpus does not.

## 7. Implementation plan

Ten commits. The runtime and the checker move in step so
no intermediate commit lands with the two halves of the
language disagreeing about visibility.

### Commit 1: Session frames

- Replace `vars map[string]Value` with `frames []map[string]Value`.
- `NewSession` creates one root frame.
- `Set`, `Get`, `DeleteLocal`, `DeleteVisible`, `Names`
  operate over frames per the contract in Section 2.1.
- Add `PushFrame`, `PopFrame`, `WithFrame`.

Unit tests for the Session contract:

- `set-writes-innermost`
- `get-walks-outward`
- `deletelocal-removes-innermost-only`
- `deletevisible-walks-outward-and-deletes-first-hit`
- `names-returns-visible-names-deduped`
- `inner-name-shadows-outer`
- `pop-root-panics`
- `withframe-pops-on-error`

No language-level behaviour change yet.

### Commit 2: checker frames

- Restructure `check.go` around `[]checkFrame` mirroring
  the runtime model (Section 2.5). Replace the existing
  `snapshotVar` save/restore mechanic with `define` (write
  innermost), `lookup` (walk outward), and `withFrame`
  (push/run/pop).
- Wrap the existing checker call sites (foreach loop
  variables, def parameters) in `withFrame`. Do not add new
  wrap-sites yet; the runtime commits 4 and 5 introduce the
  matching `if`/`def` runtime frames and the checker will
  add its `withFrame` calls in the same commits.
- Add checker unit tests parallel to commit 1's Session
  tests so the two scope models stay aligned.

No language-level behaviour change yet; this commit makes
the checker capable of representing block scope so
subsequent construct commits can land it in both halves.

### Commit 3: pin defer capture as contract

Expected production-code change: none. The existing
`evalDeferStmt` (`expr.go:941-958`) already resolves command
arguments at defer registration time and stashes the
resolved `[]Arg` in the entry; this commit makes that
behaviour load-bearing by pinning it under the new scope
model. Minor test-helper plumbing may be needed to inspect
captured arg vectors.

Add contract tests covering the cases that the frame
wiring in commits 4 and 5 must not break:

- `defer-captures-args-at-registration-time` (extend the
  existing test if needed).
- `defer-inside-foreach-captures-iteration-variable` --
  each iteration registers a deferred command holding its
  own resolved `$x`, and LIFO unwind fires them with those
  values even though the iteration frames are gone.
- `defer-inside-def-captures-call-parameter` -- a deferred
  command registered in a def body survives the call
  frame's pop and runs with the parameter value the call
  saw.
- `defer-rebind-snapshot` -- rebinding a name between
  defer and unwind does not change the deferred call's
  argument (the existing
  `TestEvalProgram_Defer_ArgsCapturedAtRegisterTime`
  already covers the script-scope case; add the
  block-scope variants).

These tests are the safety net. Any regression in commits
4-6 trips them before it reaches the corpus.

### Commit 4: if, foreach frames (runtime + checker)

Runtime:

- `evalIfStmt`: the selected branch body runs inside
  `WithFrame`.
- `evalForEachStmt`: each iteration runs inside `WithFrame`;
  loop variables bind into the iteration frame.
  `saveForEachNames` is deleted.

Checker:

- Mirror with `withFrame` wrap-sites at the corresponding
  `check.go` positions.

`retry` is intentionally not touched here; it gets replaced
by `eventually` in commit 8 and removed in commit 9. Until
then, `retry ... until` does not introduce an additional
frame; writes land in whatever frame is current at the
retry statement (the script's root frame at top level, the
def's call frame if nested in a def body, and so on).

Contract tests:

- `if-branch-let-stays-local`
- `if-sibling-branches-do-not-share-locals`
- `foreach-loop-var-does-not-leak`
- `foreach-body-let-does-not-leak`
- `foreach-body-let-does-not-bleed-across-iterations`

### Commit 5: def calls (runtime + checker)

Runtime:

- `callDef` wraps body evaluation in `WithFrame`.
- Parameters bind into the call frame.
- Existing parameter save/restore is deleted.

Checker:

- Mirror with `withFrame` when checking the def body,
  binding parameters into that checker frame and replacing
  the existing param `snapshotVar` mechanism.

Contract tests:

- `def-parameter-shadows-caller-during-call`
- `caller-binding-restored-after-call`
- `def-body-let-does-not-leak`
- `recursive-def-call-gets-independent-frames`
- `def-does-not-capture-variable-frames`
- `def-with-defer-captures-job-handle` -- a `def f() {
  guard j <- start something; defer kill $j }` works
  correctly under the new model: the defer captures the
  resolved `$j` and fires when the def's call frame
  unwinds.

### Commit 6: source sub-session

- Build a child `Session`. Clone parent `defs` in (relying
  on `DefValue` immutability per Section 5); leave parent
  vars and aliases unread.
- Evaluate the file in the child.
- On success, merge child `defs` into parent. On failure,
  discard `defs` but still let the child's `defer`s unwind.
- Apply the counter inheritance policy from Section 5.1:
  child starts with zeroed assert/defer/leak counters but
  inherits `traceEnabled`; on completion (success or
  failure), accumulate the child's counter deltas into the
  parent.
- Preserve `WithDeferScope` and Env-callback save/restore.

Contract tests:

- `source-exports-defs`
- `source-does-not-export-vars`
- `source-does-not-export-aliases`
- `source-can-call-inherited-defs`
- `source-redefinition-visible-later-in-same-file`
- `source-redefinition-overwrites-parent-after-success`
- `source-failure-does-not-merge-partial-defs`
- `source-assert-failure-counts-against-parent`
- `source-defer-failure-counts-against-parent`
- `source-job-leak-counts-against-parent`
- `source-inherits-trace-setting`
- `source-defers-unwind-on-failure`

### Commit 7: typed retryable/fatal error classification

Production-code change before `eventually` lands. The
current evaluator returns plain `error` values; the new
construct cannot classify reliably by string-matching or
broad `*SyntaxError`-type checks. Introduce the typed-error
layer described in Section 3.4.1:

- `RetryableError` interface in the `shell` package.
- Concrete `CommandFailure`, `AssertFailure`,
  `RequireFailure` types implementing the interface.
  Reuse the existing `GuardFailure` shape verbatim
  (`Span`, `Primary string`, `Args []Arg`, `Envelope`);
  add the `Retryable() bool` method without changing the
  struct fields or the `Error()` rendering. The new types
  follow the same `Args` / `Envelope` field naming, not
  `Cmd` / `Rc`.
- `CommandFailure` is raised only when a command was
  successfully resolved and executed and produced a non-ok
  envelope. Programmer/environment errors -- unknown
  command, exec launch failure, argument parsing rejection
  -- continue to surface as untyped `error` and remain
  fatal under `eventually`.
- Both expression-form and verb-form assert/require paths
  produce the typed errors. The command-side
  `ExecAssertStmt` dispatcher returns
  `AssertFailure`/`RequireFailure` rather than opaque
  `error`; the expression-form `evalAssertStmt` /
  `evalRequireStmt` do the same.
- Top-level error rendering keeps working because the
  typed errors implement `error`. `eventually` consumes
  the layer via `errors.As` so wrapping by `frameAtSpan`
  and friends does not hide the retryability signal.

No language-level behaviour change. Tests:

- `assert-failure-is-retryable-typed`
- `require-failure-is-retryable-typed`
- `guard-failure-is-retryable-typed`
- `command-nonzero-rc-is-retryable-typed`
- `parse-error-is-not-retryable-typed`
- `unbound-variable-is-not-retryable-typed`

Adding a new retryable case in the future is a matter of
implementing `RetryableError`; nothing in the upcoming
`eventually` evaluator needs to know about each concrete
type.

### Commit 8: eventually construct

- Parse `eventually timeout DUR [interval DUR] { body }`
  and the bind form `let r <- eventually ...`. `timeout`
  is mandatory; `interval` is named, optional, defaults
  `100ms`. Internally one `Eventually` operation with two
  syntactic placements (statement form and bindable
  command form); the parser produces them as
  `EventuallyStmt` and `EventuallyCommand` AST nodes that
  share evaluator code. Both forms coexist with the legacy
  `RetryStmt` briefly; commit 9 removes the legacy one.
- `parseBindRHS` already has a special case for `foreach`
  because blocks do not fit the normal bind RHS token
  collector (`takeBindRHSTokens` stops at `{`). Add the
  same special case for `eventually`: when the bind RHS
  peeks at the `eventually` keyword, dispatch into
  `parseEventuallyBindRHS` rather than routing the
  block-bearing form through the generic command path.
- Evaluate per Section 3.4: each attempt runs the body
  inside `WithFrame` AND inside a fresh defer scope
  (attempt-local). Use the typed-error layer from commit 7
  via `errors.As(err, &r)` -- not a direct type assertion
  -- so wrapping by `frameAtSpan` / `SyntaxError` /
  `SpanCarrier` does not hide the retryability signal.
  Parser errors remain fatal because the program never
  reaches evaluation, so there is nothing to match at
  runtime. Any retryable failure aborts the current
  attempt immediately; subsequent body statements do not
  run. A failure during attempt-local defer unwinding is
  fatal to the construct, not retried.
- Implement the `assert`-counter snapshot/reset/increment
  protocol so a retried `assert` failure does not
  multi-count. Mark this in a code comment as bridge debt
  pending the cleanup noted in Section 3.4.1.
- The bind form returns a structured value
  `{ ok, timed_out, attempts, elapsed_ms, last_error }`.
  The unbound form propagates the last retryable error on
  overall failure. Fatal errors halt regardless of bind:
  the bind form catches timeout, not programmer mistakes.

Contract tests:

- `eventually-succeeds-on-first-attempt`
- `eventually-retries-on-require-failure-then-succeeds`
- `eventually-retries-on-assert-failure-then-succeeds`
- `eventually-retries-on-command-nonzero-then-succeeds`
- `eventually-times-out-and-propagates-last-error`
- `eventually-fatal-error-halts-without-retry`
- `eventually-body-let-does-not-leak`
- `eventually-bound-form-returns-result-on-overall-failure`
- `eventually-assert-counter-net-zero-on-success`
- `eventually-assert-counter-incremented-once-on-failure`
- `eventually-default-interval-is-100ms`
- `eventually-explicit-interval-honoured`
- `eventually-attempt-defer-runs-on-failed-attempt`
- `eventually-attempt-defer-failure-is-fatal`

### Commit 9: corpus migration to eventually + remove retry

- Rewrite the retry/until pattern in the nine `TestMultiProg*`
  scripts under `e2e/new/` (Section 8). Roughly 27
  mechanical rewrites (three waves per file).
- Delete the legacy `RetryStmt` AST node and evaluator.
  Keep `retry` and `until` as **reserved tombstone
  keywords** in the lexer so an old script lands a
  targeted parse error: "retry is removed; use
  eventually". Without the tombstone, an old `retry { ... }`
  parses as an ordinary command named `retry`, fails
  somewhere downstream, and produces a confusing
  diagnostic. The tombstone can be removed later once any
  private branches have caught up.
- Fix anything else the contract tests in commits 4-8
  surface. `e2e/lib.bpfman` is def-only and needs no
  changes.
- Run `make test-e2e-scripts` to verify.

### Commit 10: docs

- Update this document to reflect what landed.
- Update `GRAMMAR.md` for block-scope, the `eventually`
  grammar, and the retryable/fatal classification.
- Update `BINDING-DESIGN.md` if any binding-site language
  changed.

## 8. Corpus migration

The known migration is the retry/until pattern in nine
`TestMultiProg*` scripts under `e2e/new/`. Each script has
three retry blocks (one per wave); the pattern is uniform.
Roughly 27 mechanical rewrites.

Before:

    retry {
        let probe <- test -f "${ack}.1"
    } until $probe.ok or timeout 5s
    require $probe.ok

After:

    eventually timeout 5s {
        require (test -f "${ack}.1")
    }

The new shape is shorter, says what it means
("eventually the ack file must exist"), needs no `$probe`
variable, drops the post-loop `require`, and produces no
leaked names. The previous transitional form
(`retry { require ... } until success or timeout 5s`) is
not used -- `retry` is removed in commit 9, so the corpus
migrates directly to `eventually`.

Files touched:

- TestMultiProgFentry_LoadAttachDetachUnload.bpfman
- TestMultiProgFexit_LoadAttachDetachUnload.bpfman
- TestMultiProgKprobe_LoadAttachDetachUnload.bpfman
- TestMultiProgKprobe_LoadAttachDetachUnload_exec.bpfman
- TestMultiProgKretprobe_LoadAttachDetachUnload.bpfman
- TestMultiProgMixed_LoadAttachDetachUnload.bpfman
- TestMultiProgTracepoint_LoadAttachDetachUnload.bpfman
- TestMultiProgUprobe_LoadAttachDetachUnload.bpfman
- TestMultiProgUretprobe_LoadAttachDetachUnload.bpfman

`e2e/lib.bpfman` has no in-body lets and requires no
changes. Any other reliance on cross-block leakage is
caught by the contract tests in commits 4-8.

## 9. Future work

Deferred items, not in this change:

- **Returning a value from a def.** Asked during review --
  "with this, could a def return a new variable/value?" --
  and the answer is yes, and lexical frames make it a much
  cleaner future extension because a def can return a value
  without leaking locals. The shape is pinned, the work is
  deferred until the first call site asks.

  The natural shape is to make defs optionally participate
  in the existing bind path. A new `return EXPR` statement
  inside a def body publishes a primary value; outside a
  def, `return` is a static/runtime error. The def call's
  `BindResult` then carries `Rc` (envelope) and `Primary`
  (the returned value), exactly like any other bindable
  command:

      def load_prog(path kind) {
          guard prog <- bpfman program load file \
              --path $path --type $kind
          return $prog
      }
      guard p <- load_prog ./xdp.o xdp
      defer bpfman program unload $p

  The contrast that matters:

      def f() {
          let x = 1
      }
      f
      print $x          # still unbound -- nothing crosses

      def f() {
          let x = 1
          return $x
      }
      let y <- f
      print $y          # 1 -- explicit return crosses

  The call frame stays private; only the value named by
  `return` crosses the boundary. Ordinary `let` inside a
  def must not export implicitly -- that would reintroduce
  the flat-session problem through the back door. Explicit
  `return` is the only escape hatch.

  Bind forms reuse the existing family:

      let v <- my_def a b           # binds Primary
      let (rc v) <- my_def a b      # binds envelope + Primary
      guard v <- my_def a b         # halts if rc.ok false

  Multiple values fall out of the existing list/destructure
  story:

      def make_pair() {
          return [left right]
      }
      let pair <- make_pair
      let (a b) = $pair

  Start narrow: defs callable in bind position only, not in
  expression position. `let (a b) = make_pair()` is a
  natural next step but adds expression-position evaluation
  for def calls, which is a separate design surface.

  One ordering question pinned now: how `return` interacts
  with def-local defers. The sequence on a def call:

  1. evaluate `return EXPR` inside the call frame; obtain
     the resulting Value.
  2. stash the value (it is now independent of the frame).
  3. unwind def-local defers in LIFO order, in the call
     frame.
  4. pop the call frame.
  5. emit `BindResult{Rc, Primary: stashed}` to the
     caller.

  This means

      def f() {
          let x = 1
          defer print $x
          return $x
      }

  has well-defined behaviour: both `defer print $x` and
  `return $x` see the same `$x`, the deferred `print` runs
  while the call frame is still live, and the returned
  value survives the pop.

  Defer failure during step 3 flips `Rc.ok` to false, even
  if `return EXPR` itself evaluated cleanly. The bind path
  then makes its usual decisions: `guard p <- f` halts;
  `let (rc p) <- f` lets the caller inspect `rc`;
  `let p <- f` continues with `p` set to the returned
  primary and discards the envelope. Cleanup that failed
  is uncertain cleanup; the caller is the right place to
  decide whether to proceed.

  Worth being explicit: the single-name `let p <- f` form
  intentionally discards the rc, matching the existing
  command-bind family. Scripts that require successful
  cleanup should call the def via `guard` or via tuple
  bind and inspect `rc.ok` -- not via plain `let`. This is
  the same rule that already applies to bindable commands;
  value-returning defs do not get a special exemption.

- **Library-published constants or aliases.** Today
  libraries contribute defs only. If a real
  constant-sharing case emerges, design then. Once
  value-returning defs land, the natural shape is a
  nullary def -- a sourced library that wants to publish
  `DEFAULT_TIMEOUT = 5s` writes `def default_timeout() {
  return 5s }`, and the importer binds `let t <-
  default_timeout`. No new export mechanism is needed.
  This is an argument for keeping `source` vars private
  now: the future extension absorbs the use case cleanly
  without adding a parallel export channel.
- **Value-returning blocks.** `let ys = foreach x in xs {
  transform($x) }` would give clean accumulation without an
  outward-write primitive. Defer until the corpus needs it.
- **`source --inline` for the REPL prompt.** If interactive
  inspection of a sourced file's variables becomes
  load-bearing, add it as a REPL-only escape valve.
- **Closures.** Defs do not capture variable frames. If real
  closures become a need, that is a separate design with
  its own non-goals.

## Appendix: prior art

Where the design sits in the language neighbourhood. Variable
scope only:

| Language              | Default scope          | Write outward          | Block-scoped `{ }`         |
|-----------------------|------------------------|------------------------|----------------------------|
| POSIX sh              | global                 | always global          | no                         |
| bash / ksh / zsh      | global                 | always global          | no (function-scoped local) |
| Python                | local-to-function      | `nonlocal` / `global`  | no (function only)         |
| Tcl                   | local-to-proc          | `upvar` / `global`     | no                         |
| Rust                  | innermost block        | yes, via reassignment  | yes                        |
| JavaScript (`let`)    | innermost block        | yes, via assignment    | yes                        |
| Scheme                | innermost `let` frame  | yes, via `set!`        | yes                        |
| bpfman-shell now      | session-flat           | always flat            | no (despite the syntax)    |
| bpfman-shell after    | innermost block        | no outward write       | yes                        |

The design lands closer to Rust, JavaScript `let`, and
Scheme on the block-scope axis than to Python, Tcl, or bash
on the function-scope axis. The shell heritage of the
surface syntax (argv-first commands, `{ ... }` groups,
`def`) nudges readers toward the bash mental model;
variable semantics intentionally diverge while command and
def lookup remain session-level. Variables are the only
thing the frame stack scopes.
