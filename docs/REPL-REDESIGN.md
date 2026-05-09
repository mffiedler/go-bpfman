# REPL DSL: language reference

This document is the canonical reference for the bpfman REPL/DSL.
It describes the language as implemented: tokens, statements,
expressions, lifecycle primitives, command capture, and the
result envelope. Earlier planning documents and the previous
language reference have been retired; this is the single
source of truth.

## 1. What this document is

The bpfman REPL is a lifecycle orchestration language for
bpfman: a small DSL for end-to-end test scripts and interactive
debugging that runs bpfman commands, captures their results,
asserts on observable state, and cleans up. It is not a general
shell, not a programming language, and not a data manipulation
tool. Each of those neighbouring concerns has an owner in the
ecosystem (bash, Go, jq) that this DSL deliberately does not
compete with.

The redesign collapses the current language's three command
delimiters (`[exec ...]`, `[bpfman ...]`, `[builtin ...]`) and
its expression-island form (`[[expr]]`) into a single
binding-with-execution sigil (`<-`) backed by a command
registry. It introduces a small set of lifecycle primitives
(`guard`, `defer`, `start`/`wait`/`kill`) that match the e2e
test shape directly. It pins jq as the sole rich-data engine
and treats every value the DSL receives from jq as a sequence,
not an array.

## 2. The language, in five lines

```
guard   required setup
defer   cleanup
jq      projection
assert  facts
exec    escape hatch
```

Everything beyond that risks becoming "jq, but worse" or
"shell, but worse". When evaluating any proposal, locate it on
those five rails first; if it does not fit, the question is
whether the rails are wrong (rare) or whether the proposal is
creep (usually).

## 3. The frame: three rails plus an escape hatch

Three rails, none of which carries the others' weight:

- **Orchestration**: the DSL itself. `let`, `guard`, `require`,
  `assert`, `defer`, `|>`, control-flow blocks.
- **Domain**: bpfman commands. Typed values, in-process
  function calls.
- **Projection**: embedded jq via `$value |> jq "..."`. Owns
  all structured-data manipulation.

The `exec` builtin is the escape hatch and lives outside the
three rails: subprocess wrapper returning
`{ok, code, stdout, stderr}`. Use when neither bpfman nor jq
covers the case.

The canonical pipeline shape that falls out of the split:

```
guard dump <- bpftool map dump id $mapID -j
let total = $dump.stdout |> jq                     \
    "[.[0].formatted.values[].value | tonumber] | add"
assert $total > 0
```

One line per rail: `bpftool` falls through to external exec and
produces JSON, jq normalises and aggregates it, the DSL asserts
on the final scalar. Each rail does what it is good at and the
failure messages are scoped to the layer that produced them.

**Convention: always reduce to a small scalar (or small object)
before asserting.** The DSL is good at comparing scalars
(`> 0`, `== $expected`, `not nil $value`); it should not be
asked to reason about deeply nested kernel-dump structures.
Keep the projection step in jq and pass a small named result
through to `assert`. A test that asserts directly against a
huge dump shape is doing jq's job in the wrong rail.

## 4. Execution model

### 4.1 The command registry

The RHS of `<-` is a command form. The first word is looked up
in a command registry; registered names dispatch to their
provider, and unregistered names fall through to external exec:

```
<- RHS is a command form.
registered name  -> in-process provider (bpfman, veth, netns, ...)
exec name ...    -> external process, forced
unknown name     -> external process fallback (bpftool, ip, awk, ...)
```

Examples:

```
guard prog <- bpfman program load file --path testdata/foo.o
guard pair <- veth create test-a test-b
guard dump <- bpftool map dump id $mapID -j
```

`bpfman` and `veth` are registered providers and dispatch to
the in-process implementation. `bpftool` is unregistered and
runs as an external process via the registry's fallback.

The `exec NAME ARGS...` form is the explicit "force external"
escape hatch for the rare case where a registered name
collides with a system binary the user actually wants:

```
let r <- exec bpfman version
```

This bypasses the registered `bpfman` provider and spawns the
system `bpfman` CLI instead.

`bpfman` is itself a registered builtin -- the shell binary
links against the bpfman library and registers it like any
other in-process provider. There is no special grammar for the
domain. `bpfman program get $pid` is one registered name
resolving to one provider, the same shape as `veth create v0 v1`
or any future addition.

### 4.2 Library / CLI parity test mode

The fact that `bpfman` has two legitimate implementations
(linked library vs system CLI) makes it a good candidate for a
parity test mode. A narrowly-scoped env var (or CLI flag)
selects which backend the registered `bpfman` name resolves to:

```
BPFMAN_SHELL_BPFMAN_PROVIDER=library   # default; linked provider
BPFMAN_SHELL_BPFMAN_PROVIDER=exec      # external bpfman CLI
```

The same DSL script, the same assertions, run against either
backend. The test value is that the suite stresses both
backends and catches divergence.

Two important constraints:

1. **Narrow scope.** Only `bpfman` gets this knob. Other
   registered providers (`veth`, `netns`, ...) have no external
   CLI to fall back to; generalising would invite "rewrite jq
   to exec jq" surprises.
2. **The forced escape hatch stays absolute.** `exec bpfman ...`
   always spawns the external CLI, regardless of the env var.
   The env var picks the resolver for the registered name; the
   explicit `exec NAME` form bypasses the registry entirely.

### 4.3 Result and primary

Every command form yields two pieces of data: a result envelope
and a primary result. The result envelope is execution
metadata; the primary is the provider's domain output.

The result envelope:

```
{
    ok:     bool       # true iff the command completed successfully
    code:   int        # exit code (subprocess) or 0/1 (in-process)
    stdout: string     # captured stdout, or in-process renderable
    stderr: string     # captured stderr, or in-process error message
    pid:    int        # set on async start; absent on synchronous capture
}
```

The primary varies by provider. Each provider declares which
shape it produces:

| Provider                                  | Primary          |
|-------------------------------------------|------------------|
| `bpfman load` / `get` / `list` / `attach` | typed payload    |
| `veth create`, `netns create`             | typed handle     |
| `start <cmd>`                             | job handle       |
| `wait $job`                               | result envelope  |
| `bpftool`, unknown external, `exec NAME`  | result envelope  |
| commands with no domain output            | result envelope  |

The result envelope is **not** a container for the primary.
It is execution metadata only. The primary lives in its own
slot, bound to its own name. There is no `$r.value`
indirection, no flattening, no reserved-key collisions.

Single-name binding captures the primary; tuple binding
captures both. See section 6.2.

### 4.4 The shared failure renderer

There is exactly one captured-result formatter. It is invoked
by every failure boundary in the language:

```
[guard]   FAIL at scripts/TestKprobe.bpfman:42
command:
  bpftool map dump id 123 -j
exit:
  255
stdout:
stderr:
  Error: map id 123 not found
```

The header changes to match the verb (`[guard]`, `[require]`,
`[assert]`, `[defer]` for cleanup failures), but the body
shape is identical: command, exit, stdout, stderr. No
construct in the language has bespoke failure-rendering code;
they all delegate to the same formatter.

The rule, in three lines:

```
<- captures.
guard / require / assert decide.
The shared renderer explains.
```

## 5. Syntactic surface

### 5.1 Statement forms

```
let NAME = EXPR                   # expression binding
let NAME <- COMMAND               # bind primary
let (RC, PRIM) <- COMMAND         # bind result and primary
guard NAME <- COMMAND             # bind primary, halt on failure
guard (RC, PRIM) <- COMMAND       # bind both, halt on failure
require EXPR                      # halt if EXPR is false
assert EXPR                       # record failure, continue
defer COMMAND                     # register cleanup
if EXPR { STMTS } [elif ...] [else ...]
foreach NAME in EXPR { STMTS }
retry { STMTS } until EXPR
def NAME(PARAMS) { STMTS }        # user-defined provider
break ; continue                  # within foreach
```

Tuple targets `(RC, PRIM)` are only legal on `<-`. Expression
binding (`=`) stays single-name. `_` discards a slot in tuple
form.

Bare-keyword forms (`break`, `continue`) take no arguments.
`source FILE` reads and evaluates a script file in the current
session.

### 5.2 Expression grammar

```
expr         := or
or           := and ('or' and)*
and          := not ('and' not)*
not          := 'not' not | comparison
comparison   := additive (BINOP additive)?
additive     := multiplicative (('+' | '-') multiplicative)*
multiplicative := predicate (('*' | '/' | '%') predicate)*
predicate    := UNARY-PRED term | negate
negate       := '-' negate | thread
thread       := term ('|>' command)*
term         := literal | varref | '(' expr ')'
              | 'timeout' DURATION | 'iteration' INTEGER
literal      := WORD | QUOTED
varref       := '$' IDENT path? | '${' IDENT path '}'
path         := ('.' IDENT | '[' DIGITS ']')+

BINOP        := '==' | '!=' | '<' | '<=' | '>' | '>='
UNARY-PRED   := 'not-empty'
```

Comparisons are type-aware: number-vs-number compares
numerically, string-vs-string textually, bool-vs-bool only
supports `==` and `!=`, mixed kinds error rather than silently
returning false. Coerce stringy numeric input via
`$x |> jq tonumber` before comparing.

Unquoted literals are typed by shape: `5` is a number, `true`
and `false` are booleans, `"5"` (quoted) is a string.

### 5.3 Bracket meanings

The redesign uses no DSL-level brackets at all. The current
language's three forms (`[cmd]`, `[[expr]]`, the auto-detect
single-bracket) are retired:

- Command capture moves to `<-` plus the registry.
- Expression-mode is the default at every RHS that follows `=`
  or `<-`; no explicit-island form is needed.
- `[...]` inside a quoted jq filter string remains jq's array
  constructor. The DSL has no array-literal syntax of its own;
  data construction is jq's job.

`{...}` remains a statement-block delimiter (`if`, `foreach`,
`def`). The DSL has no map-literal syntax for the same reason
it has no array literal: rich data lives in jq.

### 5.4 String interpolation

`"${EXPR}"` splices a value into a double-quoted string. Three
shapes inside the braces:

1. A bare variable reference: `${name}`, `${name.path}`,
   `${name[0]}`. No `$` prefix.
2. An expression: `${$n * 2}`, `${$count + 1}`. Anywhere a
   value is expected.
3. A command substitution: `${jq ".total" $data}`. The
   threading and capture forms compose inside `${...}` because
   it is an expression-mode context.

Single-quoted strings never interpolate. Double-quoted strings
support `\n`, `\t`, `\r`, `\\`, `\"`, `\$` escapes.

## 6. Bindings

### 6.1 `=` for expressions

`let NAME = EXPR` binds the result of an expression. The RHS
is always an expression; never a command form. Pure value
binding, no execution boundary crossed.

```
let delta = $after - $before
let kind = $prog.record.kind
let total = $a + $b
let ok = $count > 0
```

### 6.2 `<-` for command results

The RHS of `<-` is a command form. Every command yields a
result envelope (execution metadata) and a primary
(provider-defined). Single-name binding captures the primary;
tuple binding captures both.

#### Single-name binding

`let NAME <- COMMAND` binds NAME to the command's primary
result. For providers that produce a typed payload, NAME is
the payload directly:

```
let prog <- bpfman program get $pid
let pair <- veth create v0 v1
let mapID = $prog.record.maps[0].id
```

For providers that produce no typed payload, NAME is the
result envelope:

```
let r <- bpftool map dump id $mid -j
let r <- exec ping 8.8.8.8
require $r.code == 0
```

The provider declares which side it falls on (see section
4.3). There is no runtime check or magic; the script author
writes against the provider's declared shape.

#### Tuple binding

`let (RC, PRIM) <- COMMAND` binds RC to the result envelope
and PRIM to the primary. Use this when both pieces matter:

```
guard (rc, prog) <- bpfman program load file --path foo.o
assert $rc.code == 0
assert $rc.stderr == ""
assert $prog.record.kind == kprobe
```

`_` discards a slot:

```
let (rc, _) <- bpfman program get $pid    # rc only
let (_, prog) <- bpfman program get $pid  # primary only
```

Tuple binding is only legal on `<-`. There is no `let (a, b)
= expr`; expression binding stays single-name. This keeps
tuple syntax narrowly scoped to command capture and avoids
drifting into general destructuring.

#### Failure semantics

For `guard`, a non-ok result halts the script: the renderer
fires with the result envelope, no bindings happen, no
statements after the guard run.

For `let`, bindings happen regardless of ok. On failure the
result envelope carries `ok: false` and any captured
diagnostics; the primary is null when the provider produces
a typed payload and the command failed (no payload was
produced), or carries the result envelope when the provider's
primary is the envelope itself.

Anything could happen on the RHS of `<-` (process spawn,
in-process call, kernel side effects); that is what makes
`<-` distinct from `=`.

### 6.3 Names may be rebound; values are immutable

`let NAME = ...` and `let NAME <- ...` may rebind a name that
already exists in the current scope; this is shadowing, not
mutation:

```
let r <- bpftool map dump id $mid -j
require $r.code == 0
let r <- bpftool prog show -j
require $r.code == 0
```

Field mutation on values is rejected (`$r.stdout = "..."`,
`$r.record.program_id = 12` are not legal). Values are
immutable once bound. The line is: **names may be rebound,
values are immutable.** No explicit mutation verbs (`set`,
`var`, `+=`).

## 7. Lifecycle primitives

### 7.1 `guard`

`guard NAME <- COMMAND` and `guard (RC, NAME) <- COMMAND` run
the command and decide:

- if the result envelope is ok: bind the target(s) per the
  binding rules in section 6.2, and continue
- if the result envelope is not ok: render the failure
  through the shared formatter and halt; no bindings happen,
  no statements after the guard run

```
guard prog <- bpfman program load file --path testdata/foo.o
defer bpfman program unload $prog
guard link <- bpfman link attach kprobe --fn-name do_unlinkat $prog
defer bpfman link detach $link
```

`guard` is the workhorse for setup steps that must succeed
for the rest of the script to be meaningful. It collapses
the two-line `let r <- ...; require ok $r` idiom into one
statement and gives the renderer everything it needs.

### 7.2 `defer`

`defer COMMAND` registers a cleanup command in the current
defer scope. The top-level script is a defer scope; a `def`
body is a defer scope; plain blocks (`if`, `foreach`, `retry`)
are not.

Deferred commands run LIFO when the scope exits, on both
normal completion and `guard`-induced halt. A failing defer is
visible via the shared renderer (`[defer] cleanup failed at
...`), cleanup continues across the failure, and the body
failure (if any) remains the primary diagnostic. But any
defer failure does push the script's exit code non-zero, so
tear-down bugs cannot hide behind a successful body.

Exit-code precedence:

| Body          | Defers       | Exit     |
|---------------|--------------|----------|
| succeeds      | all succeed  | 0        |
| succeeds      | any fails    | non-zero |
| fails         | all succeed  | non-zero |
| fails         | any fails    | non-zero |

Failure-mode summary:

- `guard` fails loudly and halts.
- `defer` fails visibly, continues cleanup, contributes to exit code.

**Convention: prefer explicit idempotent cleanup over silent
best-effort.** `delete` providers should reject missing
resources by default; the script opts into idempotency at the
cleanup site:

```
defer veth delete --ignore-missing $pair
defer netns delete --ignore-missing $ns
```

Making `delete` silently tolerate absence everywhere would mask
real bugs in the test body. Making cleanup explicitly tolerant
keeps strict semantics for the active path while letting
tear-down be robust against partial setup.

### 7.3 `require` vs `assert`

The distinction is **fatal vs non-fatal**, not precondition vs
assertion in the abstract.

- `require EXPR` -- this must be true for the script to
  continue. On failure, render and halt immediately.
- `assert EXPR` -- record the failure on a per-session counter,
  render, continue. At end of script-mode execution the counter
  is checked, and a non-zero count produces a non-zero exit
  status.

The distinction matters most inside loops:

```
foreach prog in $programs {
    assert $prog.status.kernel_seen
    assert $prog.record.meta.name != ""
}
```

A failed `assert` records the diagnostic and the loop keeps
running; the script reports every program that violated the
expectation. With `require` in the same place, the first
failure would halt and the rest go unchecked.

Setup wants the opposite -- stop early before the test runs
against an inconsistent state -- and uses `guard`/`require`.

Parse, type, and unhandled runtime errors halt the script in
script mode the same way `require` failures do.

## 8. Async job control

### 8.1 `start` / `wait` / `kill`

`start COMMAND ARGS...` is a registered builtin that launches
a background process and returns a job handle. `wait $job`
blocks for completion and returns the same captured envelope as
a synchronous command, so the handle round-trips back into the
normal command-result path. `kill $job` is best-effort
termination; the standard pattern is `defer kill $job`.

```
let traffic <- start ip netns exec test-ns ping  \
    -i 0.05 -c 100 198.51.100.1
defer kill $traffic
guard dump <- bpftool map dump id $mapID -j
let total = $dump.stdout |> jq                  \
    "[.[].formatted.values[].value | tonumber] | add"
assert $total > 0
guard result <- wait $traffic
assert $result.code == 0
```

Why not a sigil (e.g. `COMMAND &`): a postfix `&` would burn
the obvious spelling for bitwise AND, which a kernel-adjacent
DSL might plausibly want later for mask manipulation
(`$flags & MASK`). `start` costs zero grammar -- it is a
regular registered command provider in the same registry as
`bpfman`, `veth`, `wait`, `kill` -- and reads cleanly without
operator-precedence questions.

Provisional rules:

- A waited job that exited non-zero contributes a script
  failure, same as a synchronous command failure.
- A killed job is a clean cleanup outcome (the user explicitly
  asked for it); cleanup-failure rules apply only if the kill
  itself fails.
- An **unmanaged** job (started, never waited on, never killed,
  still running at scope exit) is a script failure. The harness
  must make explicit lifecycle management the only way for a
  script to terminate cleanly with async work outstanding.
- v1 scope: `start` only launches external processes. Async
  invocation of in-process providers is out of scope until
  there is a concrete need.

### 8.2 Refinements that earn their place

Four additions to the bare three-verb vocabulary, scoped
narrowly so the surface stays small:

1. **`kill --signal=NAME $job`** -- default `kill` is
   SIGTERM-then-SIGKILL escalation suitable for cleanup. The
   flag form lets scripts test signal handlers.
2. **`wait --timeout=DURATION $job`** -- prevents
   `guard result <- wait $job` from hanging when a job never
   exits. Returns either the captured envelope (job finished)
   or a clear timeout failure.
3. **`$job.pid`** -- the handle exposes the process ID so
   scripts can correlate with `/proc/<pid>/...` lookups,
   `bpftool` PID filters, and any other tool that takes a PID.
4. **Process-group kill as the default for `start`** -- not a
   new primitive, an implementation rule. `start` puts the
   child in its own process group, and `kill $job` signals the
   whole group. Without this, jobs that `exec` a wrapper
   script leave orphans when killed.

### 8.3 Coordination is observation-driven

The general principle:

> Jobs run; the DSL polls.

The DSL does not give jobs a way to push events into the
script, and does not give the script callbacks or futures over
running jobs. Coordination across multiple concurrent jobs
follows the same pattern: start them, observe via
`retry { } until <condition observable on a side effect>`,
wait at the end:

```
let pinger <- start ip netns exec test-ns ping -i 0.1 198.51.100.1
defer kill $pinger
let mapper <- start exec watch -n 0.5 cat /sys/kernel/debug/...
defer kill $mapper

retry {
    guard dump <- bpftool map dump id $mapID -j
    let count = $dump.stdout |> jq                  \
        "[.[].formatted.values[].value | tonumber] | add"
    require $count >= 100
} until timeout 5s

guard pingResult <- wait --timeout=10s $pinger
assert $pingResult.code == 0
```

Inter-job channels, barriers, latches, job-to-script callbacks
are all rejected: each one is a runtime-model expansion
(futures, an event loop, message passing) that we do not need.

### 8.4 Exactness via bounded jobs

For exactness ("exactly N events caused exactly N counter
increments"), polling has timing variance. The DSL relies on a
different shape: **bounded job + wait-for-termination + sample**.
The signal that lets the script sample at the right moment is
the job's own exit, captured by `wait`:

```
let traffic <- start ip netns exec test-ns ping  \
    -c 100 198.51.100.1
guard pingResult <- wait $traffic
assert $pingResult.code == 0

guard dump <- bpftool map dump id $mapID -j
let count = $dump.stdout |> jq                  \
    "[.[].formatted.values[].value | tonumber] | add"
assert $count == 100   # exact, ping has fully terminated
```

The script never polls for "around 100"; it knows the producer
has finished and reads the counter once.

### 8.5 Signal-driven primitives (add on demand)

Three categories where polling is fundamentally inadequate:

1. **Ordering of transitions.** Polling samples state at
   intervals; if state went `A -> B -> A` between samples, the
   test sees `A` and concludes nothing happened.
2. **Sub-millisecond sampling tied to a kernel event.** "Snapshot
   map state the instant tracepoint X fires" cannot be expressed
   as `retry { }` because the latency between the event and the
   next sample is unbounded relative to event spacing.
3. **External producer with no shared bound.** A daemon or
   workload generator that runs until killed cannot use the
   bounded-job pattern; polling either undercounts or
   overcounts by an unbounded amount.

The narrow set of registered builtins that would extend the
DSL into signal-driven territory without growing the runtime
model:

- **`wait-for-line $file --regex "..."`** -- block until a new
  line matching the regex appears.
- **`wait-for-tracepoint <name>`** -- block until a kernel
  tracepoint fires.
- **`wait-for-fd $fd`** -- block on an eventfd or signalfd.

All three look identical from the script's perspective: a
registered builtin that blocks and produces a result envelope.
They compose with `start`/`wait`/`kill` and `defer` without
changing the runtime model; the DSL stays
synchronous-by-default.

**Discipline: add signal-driven primitives one at a time, when
a concrete test demands one.** Each carries real complexity
(regex semantics, perf-fd plumbing, timeout/cancellation
rules). The current e2e suite does not demand any of them.
v1 scope: bounded-job + `wait`, polling via `retry`, nothing
else.

## 9. jq integration

### 9.1 The threading operator `|>`

`|>` is value threading: it takes the LHS value and appends
it as the last argument of the RHS command call.

```
$value |> jq ".items"
$listed |> jq ".programs[] | .record.program_id"
$dump.stdout |> jq "[.[].formatted.values[].value | tonumber] | add"
```

`|>` is left-to-right composition: `$x |> jq ".a" |> jq ".b"`
reads as "take x, then jq .a on it, then jq .b on the result".
The thread RHS extends to the end of the current expression
(stops at the next `|>`, a binary operator, an arithmetic
operator, a logical `and`/`or`, a closing bracket
`)`/`]`/`}`, or end of input). Threads can sit inside
parens, command substitutions, and string interpolation.

### 9.2 jq returns sequences, not arrays

jq filters can emit multiple values. The right mental model is
that jq returns a `ValueSeq` -- zero or more values -- and the
DSL keeps that visible rather than hiding it behind silent
first/last/wrap rules.

The contexts that accept what:

| Context                                 | Multiplicity                  |
|-----------------------------------------|-------------------------------|
| `let NAME = ...`                        | one value or a sequence       |
| `foreach NAME in ...`                   | sequence (0 or more)          |
| `print ...`                             | sequence (one rendering each) |
| `assert`, `require`, comparison, arith  | exactly one (failure on N!=1) |

Why `let` accepts sequences: forcing `let` to demand exactly
one value makes scripts paper over jq's multi-output by adding
`[...]` brackets at every binding point, which hides intent.
Allowing the bind to carry a sequence pushes the failure to the
consumer that demands single values:

```
let xs = $v |> jq ".items[]"        # ok: xs is a ValueSeq
foreach x in $xs { assert $x > 0 }  # ok: foreach consumes sequences

assert ($v |> jq ".items[]") > 0    # error: assert needs one boolean
let m = $n + 1                      # error if $n is multi-valued
```

**Critical rule: no silent coercion of jq streams.** The DSL
must not auto-take-first, auto-take-last, or auto-wrap-as-array
on any boundary. Either coercion would silently mask bugs. The
user explicitly reduces with jq -- `jq "[.items[]]"` to wrap
as an array, `jq ".items[0]"` to take the first,
`jq ".items | length > 0"` to reduce -- so the intent stays
visible at the call site.

### 9.3 What jq does for the DSL

Because the DSL deliberately lacks arrays, maps, and rich
projection, jq earns its keep on every concrete pattern that
shows up in tests:

- **Normalise ugly output into stable facts.**
  `let ids = $listed |> jq ".programs[] | .record.program_id"`
- **Assert over sets, not ordering.**
  `let names = $listed |> jq "[.programs[].record.meta.name] | sort"`
- **Turn maps into loopable entries.**
  `foreach kv in ($obj |> jq ".labels | to_entries[]") { ... }`
- **Build compact expected objects.**
  `let expected = $prog |> jq '{id: .record.program_id, kind: .record.kind}'`
- **Compare projected actual vs expected.** Same projection on
  both sides; assert against the small shape.
- **Aggregate per-CPU or per-shard values.**
  `let total = $dump.stdout |> jq "[.[0].formatted.values[].value | tonumber] | add"`

The recurring pattern across all of these:

> big unstable JSON -> jq projection -> small stable value -> assert

The jq operations worth knowing for e2e: `select(...)` for
selection, `{id, kind, name}` for projection,
`sort`/`unique`/`tonumber` for normalisation,
`add`/`length`/`any`/`all` for aggregation, `to_entries[]` for
map iteration, `//` for defaults, `has("field")` for existence.

## 10. Test harness extensibility

When the test scaffolding needs new primitives (veth pair
management, netns creation, leases, lifecycle helpers), they
register as new entries in the command registry and are
indistinguishable from `bpfman` from the script's perspective:

```
guard pair <- veth create test-host test-peer --netns $ns
defer veth delete --ignore-missing $pair

guard ns <- netns create bpfman-tc
defer netns delete --ignore-missing $ns
```

Adding a new harness primitive does not touch the parser. It
adds an entry to the registry and an implementation behind it,
both in Go. The script-side surface is one new name.

Registered providers return the same result envelope as
everything else (`ok`, `code`, `stdout`, `stderr`, `pid`).
Their primary is the typed handle the test scripts read:

```
guard pair <- veth create test-a test-b
assert $pair.left == test-a
assert $pair.right == test-b
assert $pair.kind == veth
```

## 11. Conventions

### 11.1 `print` is basic display only

`print` produces a stable debug representation of a value.
Strings, numbers, and booleans print as text; arrays and
objects print as compact JSON; command-result envelopes print
as a structured summary. Anything that needs custom layout
goes through jq:

```
print $pid
print ($prog |> jq '"program \(.record.program_id): \(.record.meta.name)"')
print ($dump.stdout |> jq -r '"counter=\([.[0].formatted.values[].value | tonumber] | add)"')
```

`exec printf` is the last-resort escape hatch, not the normal
formatting story.

### 11.2 Reduce to scalar before asserting

Already covered in section 3. Restated: keep projection in jq,
pass a small named scalar (or small object) to `assert`. Do
not assert against deeply nested kernel-dump shapes directly.

### 11.3 Idempotent cleanup at the call site

Already covered in section 7.2. `delete` providers reject
missing resources by default; the script opts into idempotency
explicitly (`--ignore-missing` at each defer site).

## 12. Hard nos

Refuse on sight unless the user reverses the rule explicitly:

- Pattern matching, general destructuring, `match` blocks.
  The `<-` binder accepts a two-slot tuple target `(RC, PRIM)`
  for the narrow case of binding result and primary
  separately, with `_` to discard a slot; that is the only
  tuple form in the language. No nested patterns, no
  list/array destructuring, no destructuring on `=`.
- Monads, `Result`, `Option`, `Maybe` types in user-facing form.
- Anonymous functions, currying, partial application, operator
  overloading.
- Field mutation on values (`$r.stdout = "..."`). Names may be
  rebound via `let`; values are immutable. No `set`, `var`,
  `+=`.
- Optional chaining (`?.`/`?:`). jq covers structured selection.
- Type-inference syntax, generics, refinement types in
  user-facing form. Internal type discipline is fine; surface
  complexity is not.
- Actor systems, message passing, channels,
  recursion-as-control-flow.
- Auto-parsing JSON inside `<-`. Use `... |> jq "."` explicitly.
- Object/map literal syntax (`{key: value}`) in the DSL.
  `{...}` is a statement-block delimiter; map iteration goes
  through `jq "to_entries"`; map construction stays in
  `jq '{k: v}'`.
- Array literal syntax (`[1, 2, 3]`). Data construction is
  jq's job: `let xs = $listed |> jq "[.items[] | select(.v > 0)]"`.
- Higher-order programming over `|>`: keep it
  value-in-value-out, not function-composition.

## 13. Considered and rejected redesigns

### 13.1 Lexical dispatch (no capture sigil)

`let r = exec bpftool ...` and `let r = bpfman ...` would
dispatch on the leading keyword, with no `<-` and no command
delimiter. Rejected because user-defined commands (`def`)
cannot appear in this position: the parser would need to know
which words name registered commands at parse time, which
couples parsing to runtime def state and is fragile across
declaration order. `<-` avoids the question entirely: any
command form is legal after `<-` regardless of resolver.

### 13.2 Bash-style capture sigil `$(cmd)`

`let r = $(exec bpftool ...)` would borrow bash's visual form
but capture a structured result rather than stdout text.
Rejected because the visual familiarity is a footgun: bash
users would expect stdout-as-string semantics and be surprised
by `$r.stdout`/`$r.code` accessors. A non-bash sigil would
dodge that hazard but loses the visual familiarity. `<-` has a
weaker prior association (Go channels) and the DSL semantics
("value comes from the thing on the right") is close enough to
feel natural without strongly implying channels.

### 13.3 `&` for async job control

`COMMAND &` as a postfix sigil to start an async job. Rejected
because `&` is the obvious spelling for bitwise AND, which a
kernel-adjacent DSL might plausibly want later for mask
manipulation (`$flags & MASK`). `start` is a registered command
provider with zero grammar cost.

### 13.4 The current `[cmd]` form (TCL lineage)

The current language uses `[cmd args]` for command capture, a
choice with TCL ancestry: TCL uses square brackets for command
substitution (`set files [glob *.go]`). The visual precedent is
strong and the late-bound dispatch (any first token in the
brackets is parsed as a command name and resolved at eval time)
is exactly what the redesign preserves under `<-`. The
redesign retires `[cmd]` in favour of `<-` because the
registry-plus-binder shape is more uniform across synchronous
and asynchronous capture, and frees brackets to never be used
at the DSL level.

## 14. Migration from current to v2

The current language ships with `[cmd]`, `[[expr]]`, the
`bpfman` namespace prefix, the `[builtin ...]` discriminator,
auto-fail on non-zero exec exit, and the existing
`Test*.bpfman` script corpus. The migration is mechanical:

1. **Replace `let X = [bpfman ...]` with `guard X <- bpfman ...`**
   for the must-succeed case. Single-name bind captures the
   primary; for bpfman commands the primary is the typed
   payload, so `$X.record.program_id` continues to work
   without change. Use `let X <- ...` (no halt) only when the
   script wants to inspect the failure itself; in that case
   take the rc via tuple form: `let (rc, X) <- bpfman ...`.
2. **Add `defer` lines for every resource the script later
   relies on.** Most current scripts have implicit cleanup via
   per-test cleanup hooks; under the redesign, lifecycle
   management is explicit.
3. **Replace `let Y = [exec ...]` with `let Y <- ...` or
   `guard Y <- ...`.** `exec` is no longer required as a
   discriminator inside `<-` when the command is already an
   external binary; `bpftool`, `ip`, etc. fall through
   automatically. For external commands the primary is the
   result envelope, so `$Y.stdout`, `$Y.code`, `$Y.stderr`
   read directly off the bind.
4. **Remove the `[builtin ...]` discriminator** from veth /
   netns / leases / lifecycle helpers. They are now plain
   registered names.
5. **Drop the auto-fail on non-zero exec exit.** Scripts that
   relied on it must use `guard` for the strict path, or add
   `require $r.ok` (when the primary is the rc envelope) /
   `require $rc.ok` from a tuple bind (when the primary is a
   typed payload).
6. **Migrate `$r.exit_code` accesses to `$r.code`.**
7. **Wrap any threaded expression that crosses an arithmetic
   or comparison boundary in parens** so the thread RHS
   terminates cleanly. The closing-bracket-aware thread parser
   already supports this; the conversion is mechanical.

The migration is staged across multiple commits:

1. Land the parser changes (new `<-` token, registry-aware
   command-form parser, retire `[cmd]`/`[[expr]]` recognition).
2. Land the runtime changes (registry, captured-result
   envelope unification, shared renderer).
3. Migrate the `Test*.bpfman` corpus mechanically. Each script
   stands alone; migrate one test at a time and run it under
   the new parser to catch regressions.
4. Delete the old `[cmd]`/`[[expr]]` paths and tests.

The Go test suite is unaffected; it uses the bpfman library
directly and has no dependence on the DSL surface.

## 15. Open questions

The following are deliberately unresolved at the design-doc
stage; decisions will be made during implementation when there
is concrete code to push back against.

- **Does `[[expr]]` survive in any form?** The redesign
  largely retires it because expression-mode is the default at
  every RHS. The narrow case of expression-mode in command-arg
  position (`print [[$n + 1]]`) might justify keeping it as a
  rare escape hatch, or scripts could be required to bind via
  `let` first. Decide when implementing.
- **What does `start` do with in-process providers?** v1 says
  external processes only. Async invocation of `bpfman` or
  other registered providers would require designing
  cancellation, structured-value capture from a goroutine, and
  termination semantics. Defer until a real test demands it.
- **Does `wait $job` block forever, or have a default
  timeout?** The `--timeout=DURATION` flag is opt-in. Whether
  bare `wait $job` should default to a script-wide cap (e.g.
  the harness's overall test timeout) is a workflow question.
- **Library/CLI parity mode: env var or CLI flag or both?**
  Both are workable; `BPFMAN_SHELL_BPFMAN_PROVIDER` is the
  natural shape but a `--bpfman-provider=` flag on the shell
  binary may be more discoverable. Pick during implementation.
- **Does the parser need a strict mode that rejects bare
  expressions in command-arg position?** Today `print 1 + 1`
  parses as three command args; the redesign could reject
  unambiguous expression-shaped args in command position with
  a "did you mean `print [[1 + 1]]`?" error. Or leave the
  current behaviour. Decide based on script-corpus impact.

## Appendix A: Canonical bounded-job example

```
# TC kprobe-counter test under the redesigned syntax. Sets up a
# netns + veth pair, loads a TC program with a counter map,
# attaches it, generates ping traffic asynchronously, reads the
# map, asserts the counter incremented.

# ---- Setup --------------------------------------------------------

guard ns   <- netns create bpfman-tc
defer netns delete --ignore-missing $ns

guard pair <- veth create test-host test-peer --netns $ns
defer veth delete --ignore-missing $pair

guard up   <- exec ip -n bpfman-tc link set test-peer up
guard addr <- exec ip -n bpfman-tc addr add 198.51.100.2/24 dev test-peer
guard host <- exec ip link set test-host up
guard hadr <- exec ip addr add 198.51.100.1/24 dev test-host

# ---- Program load + attach ----------------------------------------

guard prog <- bpfman program load file              \
    --path testdata/bpf/tc_counter.bpf.o            \
    --programs tc:counter                           \
    -m owner=test-team
defer bpfman program unload $prog

guard link <- bpfman link attach tc                 \
    -i test-host --direction ingress $prog
defer bpfman link detach $link

let mapID = $prog.record.maps[0].id

assert $link.record.kind == tc
assert $link.record.program_id == $prog.record.program_id
assert $link.status.kernel_seen

# ---- Generate traffic asynchronously ------------------------------

let traffic <- start ip netns exec bpfman-tc ping  \
    -i 0.05 -c 100 198.51.100.1
defer kill --signal=SIGTERM $traffic

# ---- Read the counter map mid-flight ------------------------------

retry { } until iteration 1 or timeout 100ms

guard before <- bpftool map dump id $mapID -j
let baseline = $before.stdout |> jq                \
    "[.[0].formatted.values[].value | tonumber] | add"
assert $baseline > 0

retry { } until timeout 500ms

guard after <- bpftool map dump id $mapID -j
let delta = ($after.stdout |> jq                   \
    "[.[0].formatted.values[].value | tonumber] | add") - $baseline
assert $delta > 0

# ---- Wait for the ping job ----------------------------------------

guard pingResult <- wait --timeout=10s $traffic
assert $pingResult.code == 0
```

## Appendix B: Signal-driven sibling example

```
# A workload daemon handles SIGUSR1 by unlinking exactly one
# file. Verify per signal that do_unlinkat was called exactly
# once, with no cross-signal overlap. Demonstrates the
# wait-for-line signal-driven primitive (deferred from v1).

guard work <- start exec myworkload --log=/tmp/myworkload.log
defer kill $work

guard ready <- wait-for-line /tmp/myworkload.log --regex "READY"

guard prog <- bpfman program load file --path testdata/unlinkat_counter.bpf.o
defer bpfman program unload $prog
guard link <- bpfman link attach kprobe --fn-name do_unlinkat $prog
defer bpfman link detach $link

let mapID = $prog.record.maps[0].id

guard b <- bpftool map dump id $mapID -j
let baseline = $b.stdout |> jq "[.[].formatted.values[].value | tonumber] | add"

# Drive five SIGUSR1 events. Each handler emits "UNLINK_DONE i"
# to the log when complete, which the wait-for-line uses as the
# precise sampling boundary.
foreach i in ($null |> jq "range(1; 6)") {
    kill --signal=SIGUSR1 $work
    guard ack <- wait-for-line /tmp/myworkload.log --regex "UNLINK_DONE ${i}"

    guard sample <- bpftool map dump id $mapID -j
    let count = ($sample.stdout |> jq                      \
        "[.[].formatted.values[].value | tonumber] | add") - $baseline
    assert $count == $i
}

kill --signal=SIGTERM $work
guard result <- wait --timeout=5s $work
assert $result.code == 0
```

## Appendix C: e2e suite translation survey

Of the 59 top-level tests in `e2e/*_test.go`:

- **About 40 (~68%) translate directly** under the redesigned
  syntax. Load/Attach/Detach/Unload sweeps (24 tests),
  multi-program chain + traffic tests (about 8),
  pin/share/global-data tests (3), kmod-slot variants (2),
  multi-program lifecycle variants. These match the canonical
  example shape almost exactly.

- **About 8 (~14%) need cheap CLI/JSON additions** to bpfman.
  The dispatcher-invariant tests (`TestDispatcher_*`) currently
  use Go-only APIs (`env.GetDispatcherSnapshot`, `env.GC`,
  `env.scopeContainsLink`). Translation requires
  `bpfman dispatcher get -o json` exposing `members[]`,
  `bpfman gc -o json` returning `GCResult` shape, and per-test
  scope tracking via DSL state. None requires DSL grammar
  changes.

- **About 7 (~12%) should stay in Go.** Tests that probe
  implementation internals or harness internals, not
  user-visible bpfman behaviour:
  `TestPackageInitSurvivesAbsentProc`, `TestUniqueTestName`,
  `TestStaleTestIfaceRe`, `TestVethAddrsForIndex`,
  `TestDebug_DetachDeferral_Kretprobe`, `TestNetnsRootMount`,
  shared-runtime tests.

The split is principled: tests that probe what bpfman does for
its users live in the DSL; tests that probe how bpfman is built
live in Go.

The DSL has two acknowledged limitations:

1. **Tight feedback loops** where Go drives a function under
   test for thousands of iterations and inspects intermediate
   state via direct struct access. The DSL's loop primitives
   (`retry`, `foreach`) are coarser-grained.
2. **Type-checked invariants.** `dispatcher.Key{...}` binds at
   compile time; the Go form catches typos before the test
   runs. JSON + jq surfaces typos as runtime assertion failures.

These limitations are why the 7 Go-native tests stay in Go.
