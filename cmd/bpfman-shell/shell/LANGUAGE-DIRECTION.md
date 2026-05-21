# bpfman-shell language direction

This document pins where the language is headed and where it
is deliberately not headed. The question it answers: can
bpfman-shell reach a place where higher-level behaviour
(functions, lifecycle helpers, DSL-shaped abstractions) is
defined in the language itself rather than baked into the
evaluator?

The short answer is yes at the semantic level, partially at
the syntactic level, and not all the way to Lisp. The longer
answer determines what to build next and what to leave alone.

Companion to `SCOPE-DESIGN.md` (the scope implementation
plan), `GRAMMAR.md` (the reference grammar), and
`GRAMMAR-FOLLOW-UP.md` (pending small language fixes). This
doc is purely forward-looking; nothing here is on the
ten-commit SCOPE-DESIGN sequence.

## Design discipline

Prefer features that compose with existing parser and value
primitives before adding new syntax. Parser-context novelty
is acceptable when it keeps command syntax boring; lexer
novelty is expensive and should need a stronger
justification. The language should grow by making existing
values, bindings, blocks, and command forms compose better,
not by adding unrelated sigils.

Two corollaries follow.

**Capabilities first, sugar later.** A new evaluator
capability (frames, typed errors, `return`) earns its place
on its own; surface-syntax sugar (record literals, the
hypothetical `with` block, `step "name" { ... }`) waits
until the capability is in use and the sugar's shape can be
chosen from real call sites rather than guessed. Bundling
capability with sugar in the same PR stretches the design
surface for the sugar before anything has shown it is needed.

**The corpus is the trigger.** Features earn their place
when a real script has worked around their absence in a way
the author would not have to. Speculative features are
deferred until the absence shows up as friction in real
call sites. This is why value-returning defs are next (the
corpus is full of helpers that would compose better with a
real `return`), and why structured assertions, table-driven
tests, and per-test scopes are not yet on the roadmap.

The recent record-literal discussion illustrates the rule:
`r#{...}` was rejected because it required a new
multi-character prefix-literal rule in the lexer (`#` is
already the comment introducer); `record(...)` reuses the
existing contextual-keyword parsing pattern (`range`,
`zip`, `not-empty`, `matches`) and needs no lexer changes
at all. Both shapes were syntactically plausible; the one
that fits the existing token stream is the one that wins.
See `SCOPE-DESIGN.md` Section 9 for the full record-literal
note.

## Two levels of "Lisp-like"

Lisp gets its compositional power from two distinct
properties. Conflating them gives a misleading answer to
whether bpfman-shell can "become Lisp".

**Semantic compositionality.** Lexical scope, first-class
local state, and explicit closures let you build big
programs from small functions whose private mechanics do
not leak. You do not need homoiconicity for this; you need
clean primitives with precise scope rules.

**Syntactic metaprogramming.** Macros let user code
construct or rewrite syntax before evaluation. This is the
distinctive Lisp move: code is data, and the surface
grammar is itself extensible. It requires homoiconicity (or
equivalent machinery like `quote` / `eval`) and a uniform
AST.

The two are independent. A language can be deeply
compositional at the semantic level without any
metaprogramming surface; conversely, a language with a
malformed scope model gains very little from macros.

## Where bpfman-shell is going

### Semantic level: yes

The SCOPE-DESIGN rework establishes the primitives that
make compositional library code possible:

    let x = EXPR
    let x <- COMMAND
    guard x <- COMMAND
    def f(args...) { BODY }
    foreach x in xs { BODY }
    if cond { BODY }
    defer COMMAND
    eventually timeout 5s { BODY }

Once those are stable -- once a def introduces locals that
do not leak, a sourced file publishes only defs, and defers
capture resolved values at registration -- the language
starts feeling compositional rather than "a script with
global side effects". A helper def can wrap lifecycle
behaviour. A library script can publish a vocabulary of
helpers without mutating caller state. A foreach iteration
can carry its own scratch state without bleeding into the
next.

This is the part bpfman-shell already commits to. The
ten-commit SCOPE-DESIGN sequence delivers it.

### Syntactic level: deliberately no

bpfman-shell is argv-first, block-oriented, and command-
biased. Pasted shell commands stay opaque Word tokens;
expression syntax is opt-in at specific parser sites. That
trade-off is the right one for the corpus -- e2e scripts
read like the CLI does -- but it means the language is not
shaped like Lisp.

To get Lisp-style syntactic abstraction you would need:

- a uniform AST that user code can construct and pass to
  the evaluator;
- a `quote` / `eval` pair, or macros that rewrite syntax at
  parse time;
- a token-level model that treats code and data the same.

bpfman-shell has none of these and gains little by adding
them. The cost is significant: a macro layer or `eval`
primitive immediately surfaces hygiene rules, expansion
order, and quoting subtleties. Those are not problems the
corpus has or wants.

So: no macros, no `eval`, no homoiconic AST. The
"definable in the language" answer is limited to what defs
plus existing primitives can express.

## The sweet spot: lifecycle primitives as library code

The valuable target is not "become Lisp". It is

    make common lifecycle patterns definable in the
    language, without adding evaluator special cases every
    time.

Today a multi-step lifecycle (load, attach, drive, detach,
unload) is written inline in every script that needs it,
or smeared across half-cooperative helpers. With lexical
frames plus value-returning defs the same sequence still
collapses, but the cleanup boundary belongs to the caller,
not to the helper. The shape that actually works:

    def load_xdp(path iface) {
        guard prog <- bpfman program load file \
            --path $path --type xdp
        guard link <- bpfman link attach xdp \
            -i $iface generic 50 $prog
        return [prog link]
    }

    guard pair <- load_xdp ./xdp.o eth0
    let (prog link) = $pair
    defer bpfman link detach $link
    defer bpfman program unload $prog

The cleanup `defer`s live at the call site, not inside
load_xdp. The reason is mechanical: a def opens its own
defer scope, so anything `defer`ed inside the body unwinds
when the def returns -- BEFORE the caller binds the result.
A helper that put `defer bpfman program unload $prog`
inside its body would unload the program at function return,
and the caller's `$prog` would name a freed resource. The
runtime is doing the right thing; the lifecycle metaphor
just has to put the cleanup with the consumer.

The honest trade-off: partial-cleanup-on-error inside the
helper is lost. If `bpfman link attach` fails after the
load succeeded, the prog is loaded but the helper never
ran an unload-on-error step before returning. For lifecycle
patterns where partial cleanup matters, write the helper
as two stages -- `load_then_attach`'s halves -- or have the
helper publish both the resource and a cleanup recipe the
caller installs. A "defer to caller scope" primitive (a
def-local `defer` that registers in the caller's defer
stack instead of the def's) would close the gap; it is not
on any roadmap today and the corpus has not yet shown the
shape is load-bearing.

Nothing else is magical. Just a def with locals that do not
leak, guards that halt on failure inside the helper, an
explicit return that crosses the call boundary, and
caller-side defers that name the resources the helper
returned. Every piece is a primitive the language already
has after SCOPE-DESIGN.

This is the 80% of practical benefit Lisp-style
abstraction would buy, achieved without a macro system.

## The first hard limit: control-flow abstractions

A def can wrap commands and expressions; it cannot wrap a
new body-taking form. This shape is not naturally
expressible today, even with value-returning defs:

    with_program ./xdp.o xdp {
        ...
    }

`with_program` would need to receive `{ ... }` as a value
-- a callable block -- and decide when to evaluate it. The
parser does not currently treat a trailing block as an
argument to a user-defined command; only built-in
constructs (`foreach`, `if`, `eventually`, `def`, `defer`,
`guard`) consume trailing blocks. A user-level `with_X`
form needs block-as-value support.

The shape such a feature would take, if added later:

    def with_program(path kind body) {
        guard prog <- bpfman program load file \
            --path $path --type $kind
        defer bpfman program unload $prog
        body $prog
    }

    with_program ./xdp.o xdp { prog
        require (some-check $prog)
    }

The block becomes a callable that the def invokes once
with the loaded program bound. The user-level command
gains a "consumes a trailing block" arity. This is
control-flow abstraction without macros: blocks are data,
but only by being passed, not by being constructed from
tokens.

This is the next serious primitive if the corpus ever
demands user-defined control forms. It is not on any
roadmap today.

## Deep modules, small primitive set

Ousterhout's framing maps cleanly onto this trajectory:
keep the evaluator's primitive set small, but make each
primitive deep enough that libraries compose them.

- Lexical scope is deep: it makes every other primitive
  composable.
- Value-returning defs are deep: they let library code
  express any sequence-of-commands abstraction.
- Block-as-value is between deep and cliff: it adds one
  evaluator concept (callable blocks) and unlocks a
  category of control abstraction. Worth considering if a
  use case appears; not worth speculatively adding.
- Macros are a cliff: they add a parallel evaluation
  model, a hygiene problem, and a debugging surface the
  language does not need.

The trade-off the language is making: prefer deep
primitives that compose, accept the loss of macro-level
expressiveness, and keep the evaluator simple enough that
script behaviour is predictable from primitives alone.

## On the keyword `def`

Is `def` the right word? Defensible, but it carries
baggage worth naming.

`def` is good if the construct should feel like "define a
shell command or helper" rather than "declare a function
in an expression language". It fits the current shape

    def expect_program_load(path kind) {
        ...
    }

and avoids overpromising expression-language semantics.
Today these are not closures, not values, not
expression-position callables, and not block-local
declarations. `def` is neutral enough to cover what is
here without committing to what is not.

If value-returning defs land later (`SCOPE-DESIGN.md`
Section 9), `def` still works. Python, Ruby, and
Clojure-ish languages all use `def` / `defn` for forms
that return values. Return values do not make the word
wrong.

The alternatives each imply something stronger:

- `function f(...) { ... }` -- shell/JavaScript-like, but
  longer and a bit too grand. Suggests function scope,
  expression-position calls, or first-class-ness.
- `fn f(...) { ... }` -- modern, but nudges towards
  Rust/Go-style "real function" semantics. If these are
  command helpers called in command position, `fn` may
  mislead.
- `proc f(...) { ... }` -- Tcl-ish and arguably accurate:
  a procedure with side effects. Less familiar; "proc"
  can feel old-fashioned.
- `command f(...) { ... }` -- very explicit, but verbose,
  and awkward once it returns values.
- `alias f(...) { ... }` -- wrong, because aliases already
  exist and are textually shallower than these.

Keep `def`. It says "bind this name to a user-defined
thing" without committing to whether that thing is a
command, a procedure, a function, or eventually a
value-returning helper. That flexibility matches where
the language is heading.

The tweak worth applying is terminology in the docs: call
them **user-defined commands** rather than "functions"
until `return` lands. So:

    def declares a user-defined command. A def runs in
    its own call frame; parameters and locals do not leak
    to the caller. Defs are session-level declarations
    and do not capture variable frames.

And later, if value-returning defs arrive:

    A def may optionally return EXPR when called in bind
    position.

That keeps the keyword stable and the semantics honest.

## Making tests easy to write

The biggest forward-looking move is to make the language
good at expressing test intent, not just shell
choreography. The pieces are already forming:

    guard p <- load_prog ./xdp.o xdp
    defer unload_prog $p
    eventually timeout 5s {
        require (ack_exists $ack 1)
    }

The directions worth biasing towards.

### Value-returning defs

Probably the biggest practical win. Instead of every test
spelling

    guard loaded <- bpfman program load file \
        --path $path --type xdp
    let pid = $loaded.record.id
    defer bpfman program unload $pid

the test eventually reads

    guard prog <- load_prog $path xdp
    defer unload_prog $prog

`SCOPE-DESIGN.md` Section 9 already pins the shape. The
important property is that returned values let helpers
become real test vocabulary: a script reads as
`load_prog` / `attach_xdp` / `send_ping` / `require
counter_equals` rather than as raw `bpfman ... |> jq ...`
plumbing.

### User-defined predicates for assert / require

Built-in predicate forms work today. The next step is for
defs to participate naturally:

    def program_loaded(pid) {
        guard got <- bpfman program get $pid
        return $got.ok
    }
    require (program_loaded $pid)

This falls out of value-returning defs plus bindable
commands; the happy path is already obvious. The bareword
form

    require program_loaded $pid

is tempting and should not be rushed -- it overlaps with
command-position resolution rules. Letting user-defined
helpers become assertion vocabulary is the goal
regardless.

### Better failure rendering

`eventually` as a test primitive is already the right
move. The forward-looking enhancement is observation, not
more retry syntax. A failed `eventually` should report:

- number of attempts;
- elapsed time;
- last failure value;
- source span of the failing assertion;
- last observed value, where applicable.

A failed test should say

    counter_equals failed after 51 attempts over 5.02s
        expected: 1
        actual:   0
        source:   TestXDP_LoadAttachDetachUnload.bpfman:42

not merely

    require failed

The same applies to plain `require` and `assert`.
Evidence beats pass/fail.

### Structured helper results

A Lisp-friendly and test-friendly pattern: helpers return
structured success/failure values rather than booleans:

    def counter_equals(prog name want) {
        guard got <- read_counter $prog $name
        if $got == $want {
            return { ok: true }
        }
        return {
            ok: false
            message: "counter mismatch"
            expected: $want
            actual: $got
        }
    }

    require (counter_equals $prog xdp_pass 1)

The renderer above then has structured evidence to
display, not just a name and a verdict. Depends on object
literals.

### Expand `matches { ... }` before general expression syntax

`matches { ... }` is already a major asset. Keep
investing there rather than growing arbitrary expression
complexity. Tests often want

    require $prog matches {
        .status.type == "xdp"
        .status.kernel.id present
        .status.maps contains {
            .name == "xdp_stats"
        }
    }

Worth considering: `contains-one`, `contains-all`, `not {
... }`, `where .field == value`. The sweet spot is jq
remains available, but common test assertions do not need
it.

### Object / record literals

Lists plus jq go a long way, but test ergonomics
eventually want small object literals:

    let expected = {
        id: $pid
        type: "xdp"
        attached: true
    }

or a constructor builtin:

    let expected = (object id $pid type xdp attached true)

Useful for golden-ish tests, table-driven tests, and
structured assertion output. Not urgent. Object literals
expand the expression language; add them only when
structured helper results become painful without them.

### Scoped resource helpers (`with`)

`def` plus `defer` already covers most of this. A future
primitive could make scope explicit:

    with prog <- load_prog ./xdp.o xdp {
        with link <- attach_xdp $iface $prog {
            drive_traffic
            require counter_equals $prog packets 1
        }
    }

Semantics:

- `with x <- CMD { BODY }` binds `x` inside the block.
- If the command publishes a cleanup action, the block
  registers it automatically.
- Cleanup runs at block exit, even on failure.
- Nested resources unwind in reverse order.

Tests would read as resource lifetimes rather than
imperative cleanup recipes. This is the test-DSL face of
block-as-value (the same primitive the control-flow
section holds back). Same bar: do not add until the
`guard` + `defer` pair becomes repetitive enough to
justify a new primitive.

### Table-driven tests

Once lexical scope and value-returning defs land, table
tests fall out:

    foreach (kind path attach) in [
        [xdp ./xdp.o attach_xdp]
        [tc  ./tc.o  attach_tc]
    ] {
        guard prog <- load_prog $path $kind
        defer unload_prog $prog
        guard link <- $attach $iface $prog
        defer detach_link $link
        require program_running $prog
    }

The `$attach` slot needs command values, which are not on
the roadmap. A pragmatic version dispatches through a
helper:

    foreach (kind path) in [
        [xdp ./xdp.o]
        [tc  ./tc.o]
    ] {
        run_attach_detach_case $kind $path
    }

Already good.

### `test "..." { ... }` blocks

If `.bpfman` files grow, a test-level structure may
become useful:

    test "xdp attach/detach unloads cleanly" {
        ...
    }

Potential benefits: named failure reports, per-test defer
scope, isolated variable root frame, table-driven
subtests later. Do not add until scripts are genuinely
large enough. The harness already treats each file as a
test; file-as-test is simple and good.

A lighter form might be `section "load program" { ... }`
purely for diagnostics, with no semantic weight. Same
rule: defer.

### Keep command-position boring

The most important negative recommendation. Do not make
command syntax clever. The best property of the language
is still

    guard p <- bpfman program load file \
        --path foo.o --type xdp

remaining pasteable and unsurprising. Put power at the
binding site, block site, matcher site, and assertion
site. Argv mode stays boring.

### North star

A test that reads like

    source ../lib.bpfman
    test "xdp program counts packets" {
        with prog <- load_prog ./xdp.o xdp {
            with link <- attach_xdp $iface $prog {
                ping_once $ns $target
                eventually timeout 5s {
                    require counter_equals $prog packets 1
                }
            }
        }
    }

is the kind of surface where bpfman-shell has become a
test DSL rather than a shell script with nicer variables.
The exact mix of primitives in that snippet is
aspirational -- `with` and `test "..."` blocks are both
held back until the corpus demands them -- but the shape
is the destination.

## What to build, in order

The merged priority across the compositional spine and
the test-DSL items:

1. Finish SCOPE-DESIGN. *(Landed.)* Lexical frames are
   the gate everything else passes through.
2. Land value-returning defs (SCOPE-DESIGN Section 9).
   *(Landed.)* The single biggest compositional unlock;
   the `load_xdp` helper is now idiomatic and tests gain
   a real vocabulary.
3. Let user-defined defs become assertion vocabulary
   (`require (program_loaded $pid)`). Falls out of step
   2 plus bindable commands; mostly a documentation and
   ergonomics pass.
4. Improve failure rendering for `require`, `assert`,
   and `eventually`. Evidence-bearing diagnostics, not
   more retry syntax.
5. Expand `matches { ... }` before growing general
   expression syntax. Structured assertions live in the
   matcher.
6. Add object / record literals when structured helper
   results become painful without them.
7. Add `with x <- CMD { BODY }` only after `guard` +
   `defer` patterns become repetitive enough to justify
   a new primitive. Same bar block-as-value is held to.
8. Add `test "..." { ... }` only when scripts grow large
   enough to need named test blocks.
9. Do not add macros, `quote`, or `eval`. The language
   does not need them and gains little from them.

The discipline is "deep but few". Every addition above
the line is a primitive the corpus genuinely uses; the
items below the line stay out unless they earn inclusion
through real use cases.
