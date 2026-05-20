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
frames plus value-returning defs (deferred to
SCOPE-DESIGN Section 9, shape pinned), the same sequence
collapses to one library def:

    def load_xdp(path iface) {
        guard prog <- bpfman program load file \
            --path $path --type xdp
        defer bpfman program unload $prog
        guard link <- bpfman link attach xdp \
            -i $iface generic 50 $prog
        defer bpfman link detach $link
        return [prog link]
    }

    guard pair <- load_xdp ./xdp.o eth0
    let (prog link) = $pair

Nothing magical. No new evaluator construct. Just a def
with locals that do not leak, defers captured at
registration, guards that halt on failure, and an explicit
return that crosses the call boundary. Every piece is a
primitive the language already has or will have after
SCOPE-DESIGN.

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

## What to build, in order

1. Finish SCOPE-DESIGN. Lexical frames are the gate
   everything else passes through.
2. Land value-returning defs (SCOPE-DESIGN Section 9).
   The single biggest compositional unlock; the
   `load_xdp` example above becomes idiomatic.
3. Wait for block-as-value to be asked for. Until a
   concrete `with_X` shape is missed by the corpus, do
   not build it speculatively.
4. Do not add macros, `quote`, or `eval`. The language
   does not need them and gains little from them.

The discipline is "deep but few". Every addition above
the line is a primitive the corpus genuinely uses; the
items below the line stay out unless they earn inclusion
through real use cases.
