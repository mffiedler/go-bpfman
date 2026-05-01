# REPL `def name(params) { ... }` — Design Note

Status: proposal, not implemented.  Captured for later
implementation.  Companion to REPL-MATCHES-ASSERTION.md (now
shipped); the two features were discussed together but split
because their implementations are independent and `def` is much
larger.

## Motivation

The matches block shrank each per-record assertion from eight or
more lines to one block.  What it did not address is **the same
block being written many times**.  The lifecycle scripts under
`e2e/scripts/Test*_LoadAttachDetachUnload.bpfman` each carry
near-identical "expect tracepoint program" / "expect tracepoint
link" blocks; the only thing that varies between scripts is the
program type, the program/link names, and the test label.

Without `def`, the only DRY mechanism in the language is `alias`,
which is a textual-prefix expansion with no parameters and no
locals.  Aliases cannot capture "given prog and name, assert this
contract on them" because there is no parameter binding.  The
result is the same block copy-pasted across every script that
exercises a program type.

## Proposal

Add a new statement form to the shell parser:

```
def NAME(PARAM1, PARAM2, ...) {
    BODY
}
```

When evaluated, `def` registers a callable named `NAME` in the
session.  Invoking `NAME ARG1 ARG2 ...` later evaluates BODY with
each parameter bound to the corresponding argument and the
parameters out of scope after the body returns.

### Surface syntax

* `def NAME(P1, P2, ...) { ... }` — declaration.  Parameters are
  comma-separated; trailing comma allowed.  An empty parameter
  list is `def NAME() { ... }`.
* `NAME ARG1 ARG2 ...` — invocation.  Space-separated arguments,
  exactly the same shape as any other command call.  Arity is
  checked at call time; mismatched arity is a runtime error
  citing the def's source location.
* `def NAME` (no body) — error.  No forward declarations; no
  abstract bodies.

Decisions deliberately kept narrow:

* **No default parameters.**  Optional arguments would force a
  decision about how `expect_tracepoint $prog tp_a` differs from
  `expect_tracepoint $prog tp_a "" ""`; not worth the complexity.
* **No variadic parameters.**  `foreach` over a list inside the
  body covers the cases that would otherwise reach for varargs.
* **No type annotations.**  Arguments arrive as the same Args the
  rest of the language uses; existing pattern matching (Word,
  Quoted, Scalar, Structured) carries over.
* **No keyword arguments.**  Positional only, matching the rest
  of the command grammar.

### Scoping

Parameter binding is **shadow-and-restore**: at call time, each
parameter's name is set in the session to the corresponding
argument's value, *replacing* any outer binding by the same name;
when the body returns, the original outer value is restored (or
the name is unset if no outer value existed).  This matches how a
reader thinks about parameters — "inside the body, `$prog` means
the argument" — without introducing a separate scope stack.

```
let prog = "outer"
def f(prog) { print $prog }   # prints the argument
f "inner"                      # outputs: inner
print $prog                    # outputs: outer
```

Variables introduced inside a body via `let` are *not* automatic
locals: `let` writes to the same session.  This is the
shell-script reading: a body that wants a temporary uses a fresh
name, or unsets the name on the way out.  The alternative — true
lexical scoping for `let` inside a body — was rejected because
it would diverge from how `let` already works at top level
(persistent through the rest of the session) and produce two
different `let` semantics depending on syntactic position.

### Return value

For the first cut, `def` is **statement-only**: the body runs for
its effects (asserts, prints, side-effecting commands) and the
invocation produces no value.  A def cannot be used inside `[...]`
as a command substitution.

This matches the dominant use case (capturing a block of
assertions or a setup/teardown sequence).  If a need for "compute
and return a value" surfaces later, it can be added under a
`return EXPR` form without changing what already works — adding
a return path is non-breaking; removing the absence of one is.

### Errors

* **Arity mismatch** at call site: error message names the def,
  cites both the call and the def site, and says how many
  parameters were expected vs supplied.
* **Body parse error**: caught at `def` evaluation time, before
  the def is registered.  A def whose body fails to parse is not
  installed.
* **Body runtime error**: surfaces as the def's error.  The
  failure message includes the call site so a deep stack does not
  hide which top-level invocation triggered it.
* **Redefinition**: an existing def with the same name is
  replaced.  No warning.  This matches `let` and `alias`; users
  who want diagnostic strictness can wrap their lib in a guard.
* **Calling a def that does not exist**: routes to the normal
  "unknown command" diagnostic, no special case needed.

### Composition with `matches`

The motivating shape, taken from the matches design note:

```
def expect_tracepoint(prog, name, run_label) {
    assert $prog matches {
        record.meta.name:           $name,
        record.load.program_type:   tracepoint,
        status.kernel.id:           $prog.record.program_id,
        record.meta.metadata.test:  $run_label,
        status.kernel.tag:          not-empty,
        record.handles.pin_path:    not-empty,
    }
}

expect_tracepoint $prog_a tp_a $run_label
expect_tracepoint $prog_b tp_b $run_label
```

Each lifecycle script today carries one such block per program
type per record (load response, get round-trip, list round-trip,
attach response, get-link round-trip, list-link round-trip).
With `def`, the block lives once in a shared file (see below)
and the script becomes a sequence of named calls.

## Sharing across scripts via lib.bpfman

This is the question the user raised: can `def`s be dropped into
a `lib.bpfman` and `source`d by individual scripts?

**Yes.**  No new mechanism is required because the language
already has `source`, and the def installation goes through the
same session the sourcing script uses:

```
# lib.bpfman
def expect_tracepoint(prog, name, run_label) { ... }
def expect_kprobe(prog, name, run_label) { ... }
def expect_tracepoint_link(link, prog, group, name) { ... }
...
```

```
# scripts/TestTracepoint_LoadAttachDetachUnload.bpfman
source ../lib.bpfman
let prog = [bpfman program load file ...]
expect_tracepoint $prog tracepoint_kill_recorder $run_label
let link = [bpfman link attach tracepoint $prog ...]
expect_tracepoint_link $link $prog syscalls sys_enter_kill
...
```

The `source` command already runs the sourced file's statements
in the caller's session; `def` registrations therefore persist
into the caller after sourcing returns.  Existing source rules
apply unchanged: nested source is rejected, the sourced file
shares the caller's variables, etc.

A few practical points fall out of this:

* **`source` is idempotent in effect, not in execution.**
  Sourcing `lib.bpfman` twice re-runs every `def` and re-installs
  it (last write wins, same as `let`).  No harm, but if the lib
  grows expensive bodies, callers should source it once at the
  top of the script.
* **Lib path conventions.**  `e2e/scripts/lib.bpfman` is the
  natural home for the lifecycle test helpers; `examples/lib.bpfman`
  for shapes that examples want to share.  No mechanism change —
  the relative-path resolution `source` already does is enough.
* **Versioning.**  A lib that changes the parameter list of a
  shared def silently breaks every script that calls it.  This is
  the same caveat any shared library has; the runtime arity check
  surfaces it as a clear error rather than a silent misparse.

The design therefore has no new "library" concept.  `def` is the
mechanism; `source` is the import; convention picks the path.

## Scope of the change

* **Lexer**: no changes.  `def` is a TokenWord; `(` and `)` are
  already token-boundary characters; the parser routes them.
* **Parser**:
  * New AST node `DefStmt { Name, Params, Body, Loc }`.
  * In `parseStmt`, recognise the `def` keyword and route to a
    new `parseDefStmt`.
  * `parseDefStmt` parses the name, the parenthesised parameter
    list, and the brace-delimited body via the existing
    `parseBlock`.
  * `parseStmt` learns to recognise a CommandStmt whose first
    word is a registered def and treat it as a call.  Two
    options:
    1. Defer detection to evaluation time — at runtime, when a
       CommandStmt's first arg names a def, dispatch to the def
       call path before falling through to the domain command
       layer.  No parser change.
    2. Detect at parse time via a session-aware lookahead.  More
       precise diagnostics ("call to def f", "def f does not
       exist at parse time") but threads parser state through
       parts of the grammar that do not need it today.  Reject;
       option 1 is good enough.
* **Evaluator**:
  * `Session` gains `Defs map[string]*DefValue` (or similar).
  * `DefValue` carries the parsed body and parameter list, plus
    a source location for diagnostics.
  * Command dispatch grows a "is this a def?" check that, on
    hit, binds parameters, evaluates the body in the same Env,
    and restores prior bindings on the way out.
* **Completion and inspection**: `defs` lister builtin, completion
  hooks for def names alongside command and alias names.
* **Tests**: parameter binding and shadow/restore semantics, body
  evaluation order, arity errors, redefinition replacing prior
  body, source location in errors, `source`-then-call across
  files, def calling another def.

Estimated size: 600-1000 LOC including completion, tests, and the
`defs` builtin.  The bulk is parser plumbing and evaluator
dispatch; the body itself reuses existing `Stmt` evaluation.

## Migration

No migration needed for existing scripts: defs and aliases coexist
trivially and aliases keep working.  Per-script adoption is
opportunistic — a script that opens a `.bpfman` file for any reason
can collapse its repeated blocks into shared defs in one diff.

## Non-goals

* **Closures over outer mutable state.**  A def captures nothing
  beyond its parameters; it reads the session at call time, just
  like any other command.
* **First-class def values.**  Defs are not values that can be
  passed as arguments or stored in variables.  This rules out
  higher-order patterns; if a user reaches for one, the shape they
  want is probably a `foreach` body or a domain-level command, not
  a function value.
* **Anonymous defs / lambdas.**  Same reason as above.
* **Generics over program type / link kind.**  A def is a
  parameterised body, not a generic.  If two defs differ only in
  one constant, the user writes two defs.

## Why this and not a host-language plugin

The REPL deliberately keeps a small primitive set; `def` is the
mechanism that lets users grow vocabulary inside the language
rather than adding a host-language extension surface (Go plugins,
Lua bindings, etc.) that would balloon the trust and review
surface for every script.  It costs one parser keyword and one
session map; in exchange, the test scripts stop carrying their
boilerplate.
