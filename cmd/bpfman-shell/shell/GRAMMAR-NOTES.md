# Grammar notes

Living notes on shell-language warts and oddities that surfaced
while writing scripts. Each entry records what was tried, what
pushed back, the workaround in use, and a sketch of how the
underlying grammar would need to change to remove the wart.

## `|>` is allowed in expression position but not in argument position

### What was tried

```
print ($prog |> jq ".status.maps")
```

### What pushed back

The parser errors out at `|>` with `unexpected "|>"`. The
underlying cause: `|>` is recognised by the **expression**
parser (let RHS, assert operand, if condition), not by the
**argument** parser that handles command operands. A command's
arguments are read as argv-style tokens -- bare words, quoted
strings, varrefs (`$name`, `$name.path`), `${...}`
interpolation, adapter invocations -- not as general
expressions. The parenthesised form `(EXPR)` in argument
position is parsed as a literal `(` token rather than an
expression group, so `|>` inside never gets its turn.

The grammar makes the difference visible by what does work:

```
assert ($snap |> jq ".members | length") == 10    # expression
let count = $snap |> jq ".members | length"        # expression
print "${snap}"                                    # arg with interpolation
```

But:

```
print ($snap |> jq ".members | length")            # arg, fails
bpfman link attach tc ... ($snap |> jq ".id")      # arg, fails
```

### Workaround

Bind the piped value via `let`, reference the binding:

```
let count = $snap |> jq ".members | length"
print $count
```

The corpus already follows this pattern broadly (`let mapID =
$prog |> jq "..."` then `... $mapID ...`), so the workaround is
local and reads no worse than the inline form. Often the
intermediate name reads better, since it names what the value
*is* rather than how it was derived.

### How a fix could look

Two paths, both structural rather than one-line tweaks:

1. **Promote `(EXPR)` in argument position.** Extend
   `parseCommandArgs` to recognise a leading `(` as the start
   of a parenthesised expression, parsed via the same path
   that handles let RHSes and assert operands. The resulting
   value flows through as a structured arg (Scalar /
   StructuredValueArg as appropriate). This is the most
   localised change: the args grammar gains one new arg-form,
   no other grammar productions move.

2. **Unify argument and expression parsing.** Drop the
   distinction and treat all command operands as expressions
   that happen to evaluate to scalars or structured values.
   Larger refactor, gives `|>` everywhere by construction,
   but risks ambiguity around argument-position constructs
   that are not valid expressions (matches blocks, adapter
   syntax) and inflates the parser's responsibilities.

Path 1 is the surgical option if the corpus accumulates enough
sites that the let-binding workaround feels load-bearing
rather than aesthetic. Until then, the workaround is the right
shape and this entry exists so the limitation is visible.

### When to revisit

Three or more script sites where the let-binding form
demonstrably hurts readability or invites name-collision bugs.
The campaign has roughly 25 dispatcher-port scripts using the
pattern today and no complaints; the threshold has not been
crossed.

## No list literals; `foreach` does not accumulate bind results

### What keeps coming up

Repetitive attaches across the dispatcher port corpus
collapse to ten plus ten plus ten of the same shape:

```
guard l_100  <- bpfman link attach tc -i $iface -d ingress -p 100  $pid
guard l_200  <- bpfman link attach tc -i $iface -d ingress -p 200  $pid
guard l_300  <- bpfman link attach tc -i $iface -d ingress -p 300  $pid
... (seven more lines that differ only in the priority and the bind name)

defer bpfman link detach $l_100
defer bpfman link detach $l_200
... (eight more)

guard g_100 <- bpfman link get $l_100
guard g_200 <- bpfman link get $l_200
... (eight more)
```

The natural way to express it is iteration over a list of
parameters with per-iteration result accumulation:

```
let priorities = [100 200 300 400 500 600 700 800 900 1000]
let links <- foreach prio in $priorities {
    bpfman link attach tc -i $iface -d ingress -p $prio $pid
}
foreach l in $links {
    defer bpfman link detach $l
}
```

### What pushes back

Two pieces are missing.

**List literal syntax.** There is no `[a b c]` form in the
grammar. The only way to construct a list value in a script
today is to invoke `range N` (which produces a list of
integers `0` to `N-1`) or to round-trip through `jq -n "[1,
2, 3]"`. Neither is a literal -- both are calls -- and
neither is great for the mixed-shape lists scripts actually
want (priorities, names, flags).

**foreach has no accumulator.** `foreach NAME in LIST { BODY }`
runs BODY for each element; the loop variable holds the last
value after the loop, but every binding produced inside the
body is local to its iteration. A `guard l <- bpfman link attach
...` inside the body is forgotten when the iteration ends.
There is no syntax for "run this body and collect the bind
result of each iteration into an outer list".

### Workaround

Manually unroll. Every dispatcher-port script in `e2e/new/`
does this for any N greater than two or three: separate
guard-bind lines, separate defer-detach lines, separate
re-query lines. The unroll grows linearly in N -- the
PriorityOrdering pair has 30 lines of pure attach + defer
+ re-query, the AttachExceedsMaxPrograms pair has 21, the
SlotReusedAfterDetach pair has 20, and so on. The pattern
has surfaced in well over five scripts.

### How a fix could look

A pair of related additions that together close the gap:

1. **List literal**: `[expr expr ...]`. Construct a list
   value inline. Elements are whitespace-separated, matching
   the rest of the shell's argument grammar; compound
   expressions wrap in parens (`[10 20 ($base + 30)]`) the
   same way they do in every other expression position.
   Elements parse as expressions: variables are bare
   (`[$x $y]`), strings need quoting (`["foo" "bar"]`),
   barewords are not list-element strings (so a typo'd name
   errors rather than silently becoming a literal). Path
   traversal and `jq` piping already accept lists, so once
   the literal exists every downstream form just works.

   Comma-separation was the obvious other choice; the survey
   splits clean along the language family line. Algol/C
   descendants (Python, Ruby, JS, Rust, Haskell, Elixir,
   PowerShell) use commas; shell and S-expression languages
   (bash, fish, tcl, Lisp/Scheme/Clojure, Elvish, Nushell,
   Janet) use whitespace. bpfman-shell is a shell, its
   argument grammar is already whitespace-separated, and the
   paren-wrap convention for compound expressions already
   exists in every expression context. Commas would have
   been the inconsistent choice.

2. **`foreach` bind-collect form**: `let RESULT <- foreach
   NAME in LIST { BODY }`. The body's last bind result is
   collected into RESULT for each iteration, producing a list
   of the same length as LIST. Existing `foreach` semantics
   (per-iteration scope, last-value-on-exit) carry over for
   the non-bind form.

3. Possibly **`foreach` defer-each shape**, but this might be
   subsumed by `foreach L in $links { defer bpfman link
   detach $L }` once the result of (2) is the iterable for
   (3). Worth verifying that the defer registration inside a
   foreach body fires at script exit (not iteration exit);
   if it already does, no new construct needed here.

This is a structural change to the parser (new token for
`[`, new statement form for `let X <- foreach`), but the
runtime impact is minor: foreach already drives a body
evaluation per element; the bind-collect variant just
captures the body's primary slot value into an accumulating
slice.

### When to revisit

Threshold met. The pattern has appeared in roughly fifteen
dispatcher-port scripts. Retrofit potential after the
addition lands: every `bpfman link attach` set in
`e2e/new/Test*.bpfman` collapses from 3N to ~5 lines.

### How far the fix gets `TestTC_DispatcherFillDrainRefill`

The harness-style FillDrainRefill test is ported in
`e2e/new/TestTC_DispatcherFillDrainRefill.bpfman` -- 563
lines of unrolled program loads, attaches, defers, and
traffic verification phases. It is the largest consumer of
the gap this entry describes: a substantial fraction of
those 563 lines would collapse under the proposed addition.

Per-peak load + attach today (one of four peaks):

```
guard ld_0   <- bpfman program load file ... --programs tc:stats
guard ld_100 <- bpfman program load file ... --programs tc:stats
... eight more loads
let p_0   = $ld_0.programs[0]
let p_100 = $ld_100.programs[0]
... eight more lets
defer bpfman program unload $p_0
... nine more defers
let m_0 = $p_0 |> jq "[.status.maps[] | select(.name == \"tc_stats_map\") | .id][0]"
... nine more lets
guard l_0   <- bpfman link attach tc -i $iface -d ingress -p 0   --proceed-on ok,pipe,dispatcher_return $p_0
guard l_100 <- bpfman link attach tc -i $iface -d ingress -p 100 --proceed-on ok,pipe,dispatcher_return $p_100
... eight more attaches
```

Roughly 50 lines per peak, four peaks plus three drains.

What the proposed addition (list literal + foreach-collect)
gives directly: collection of bind results into a list,
iteration for the defer-each shape. What it does **not**
give directly: paired iteration over a priority list and a
load list, indexed lookup into a list by a foreach variable,
or list slicing for drain ranges. Without those, the port
would still need parallel lists named per-phase (one for
priorities, one for programs, one for maps, one for links)
and either a `zip` helper or a `foreach i in (range N) { use
$prio[$i], $prog[$i], ... }` pattern.

So FillDrainRefill is a real consumer of the proposed
addition (the load/attach blocks shrink meaningfully) but
also the test that benefits from the additional primitives
that would naturally follow. Sequence:

1. List literal + foreach-collect: simpler dispatcher ports
   (PriorityOrdering, AttachExceedsMaxPrograms, SlotReused,
   ConfigAfterDetach, MultipleInterfacesIndependent) shrink
   from 3N to ~5 lines per attach set. FillDrainRefill shrinks
   too but still carries parallel-list bookkeeping.
2. List indexing by variable (`$xs[$i]`): drops the
   parallel-list pattern in favour of a single foreach over
   `(range N)` projecting into multiple slot-aligned values.
3. List slicing (`$xs[lo:hi]`): expresses drain ranges
   directly and replaces the run of explicit detaches in
   each trough.

(1) is the universal win; (2) and (3) are FillDrainRefill-
specific elaborations that may or may not earn their keep
from a single consumer. The corpus answer for now is "port
(1) when ready and live with FillDrainRefill's residual
verbosity"; whether to go further depends on whether other
harness-style tests show up.

## List-to-CLI-argument bridge: deferred design pressure

### What this anticipates

The whitespace-separated list literal (entry above) makes
`[100 200 300]` a first-class language value -- structured,
iterable, indexable. The CLIs the shell drives consume the
opposite shape: a single comma-payload argument
(`--proceed-on ok,pipe,dispatcher_return`) or a repeated
flag set (`--port 80 --port 443 --port 8080`). Neither is
reachable from a language list without explicit conversion.
Choosing whitespace-separated structured lists over a
textual list-of-strings defers this bridge question; it
does not eliminate it.

### Workaround today

```
let policies = ["ok" "pipe" "dispatcher_return"]
let joined = $policies |> jq -r 'join(",")'
bpfman link attach tc ... --proceed-on $joined ...
```

Two lines, mechanical. The dispatcher-port retrofit lists
feed `foreach`, not CLI flags, so the workaround does not
bite the immediate use case; the pressure shows up only
when someone wants to build a flag value from a list.

### Candidate primitives, when pressure builds

Three shapes, each with a different cost in the language's
syntactic namespace:

1. **`join` pure builtin.** `let joined = join "," $policies`.
   Adds one identifier to the pure-builtin registry. Cheap,
   narrow, covers the comma-payload case. No sigil consumed.
   Composable with `|>` since pure builtins already accept
   thread input.

2. **Splat sigil into command args.** `bpfman foo @$policies`
   expands a list into N positional args. Eats `@` from the
   sigil space. The cost is real: `@` is currently free for
   bareword use, and a future grammar wanting `@`-prefixed
   tokens (think `git log @{1.day.ago}` or YAML-style refs)
   would be blocked. The benefit is real too: repeated-flag
   CLIs become expressible without a per-call helper. Note
   that the same silent-absorption failure mode the comma
   check above caught would re-emerge: `@policies` lexes as
   a bareword today, so adding splat under retrofit pressure
   without first reserving `@` at the tokeniser would silently
   accept the wrong shape.

3. **Structured-arg pass-through.** The `evalArg` path
   already produces `StructuredValueArg` for some shapes; a
   list could flow through unchanged to a builtin that knows
   how to consume it. Most ergonomic for builtins; useless
   for external CLIs that read argv. Zero syntactic cost,
   but invisible to script authors -- the bridging happens
   in Go, not the shell. Only useful in combination with one
   of the above for the external-CLI case.

### When to revisit

When a concrete consumer surfaces. Pick the cheapest
candidate that fits; do not add all three speculatively. A
`join` builtin is the lowest commitment and covers the most
common shape (comma-payload). Splat is a sigil-namespace
commitment that should wait until a repeated-flag CLI
appears in the retrofit set. Structured pass-through is
worth considering whenever a builtin (not an external CLI)
needs to consume a list; the immediate consumer is the
builtin's signature, not the language.

### Why this lives here

The whitespace choice in the entry above is sound for the
shell language family it sits in, but it lands the
language with a list value whose default rendering matches
no CLI convention. Without this note, the next person
typing `bpfman foo @$xs` would expect splat semantics and
quietly get bareword absorption -- the same failure mode
the unquoted-comma check now catches inside `[...]`.
Recording the trade-off so future grammar work picks one
candidate deliberately rather than absorbing a sigil by
accident.

## Defer-each is the single most-repeated cleanup pattern

### What keeps coming up

Every dispatcher-port script that uses the list-literal + foreach
bind-collect pair ends up with the same three-line cleanup
block:

```
foreach l in $links {
    defer bpfman link detach $l
}
```

Sometimes for programs:

```
foreach p in $progs {
    defer bpfman program unload $p
}
```

Always the same shape: iterate a freshly-collected list,
register one defer per element, do nothing else with the
iteration variable. The block has appeared verbatim in nine
out of nine retrofitted dispatcher scripts so far. It is the
most-repeated three-line pattern in the corpus.

### What pushes back

There is no shorthand. `defer` registers a single command
against the enclosing scope; there is no batch form that
takes a list and an action template.

### Workaround

Write the three lines. Cost is mechanical, but the
boilerplate is real -- six lines per script when defers are
needed for both links and programs.

### How a fix could look

Two candidate shapes, both narrow:

1. **`defer-each LIST CMD ...`**: a batch defer that
   registers `CMD ... ELEM` for each element of LIST.

   ```
   defer-each $links bpfman link detach
   defer-each $progs bpfman program unload
   ```

   The element is appended as the last arg per
   registration. Works when the cleanup command takes one
   trailing arg in the per-element slot. Doesn't generalise
   to commands with positional placeholders elsewhere.

2. **`foreach-defer LIST { defer ... $it }`**: a one-line
   form with an implicit iteration variable (sigil to be
   bikeshed; `$it` is a candidate). More general than
   (1) -- the body is full DSL -- but it eats more syntactic
   space (an implicit-variable name, a new keyword).

(1) covers every retrofit so far. (2) is more flexible but
adds a sigil-namespace cost. The corpus answer right now
prefers (1).

### When to revisit

Threshold met. Nine retrofits, all using the same shape.
Worth implementing when the next pass of test ports happens
or when a new test author asks "why three lines for this".

## No index-by-variable in path lookup

### What was tried

Parallel-list iteration:

```
let priorities = [100 200 300]
let progs      = [$p1 $p2 $p3]
foreach i in (range 3) {
    bpfman link attach ... -p $priorities[$i] $progs[$i]
}
```

### What pushed back

`$priorities[$i]` is a tokenise error -- the varref grammar
allows `[digits]` inside the path, not `[$var]`. The tokeniser
fails before parsing reaches the access form.

### Workaround

Use `jq` to do the index lookup:

```
foreach i in (range 3) {
    let prio = $priorities |> jq ".[${i}]"
    let prog = $progs      |> jq ".[${i}]"
    bpfman link attach ... -p $prio $prog
}
```

Works for scalars. Loses kind/origin metadata for structured
values, so a `$progs` of typed program records would yield
untyped maps -- forces a fallback to `.record.program_id`
(a scalar) for the structured-arg path. Used in
TestTC_DispatcherChainProceedOn, where heterogeneous
attaches force the parallel-list iteration shape.

### How a fix could look

Two paths, both small:

1. **Allow `$var` inside the path index**: extend `lexVarRef`
   and `lexBracedVarRef` to accept `[$ident]` in addition to
   `[digits]`. Evaluation resolves the inner varref to a
   scalar integer at lookup time. Localised tokeniser /
   evaluator change.

2. **Add `zip`**: combine N lists into a list of N-element
   sub-lists, then iterate the zipped list. More functional
   but requires a pair/sub-list convention the language
   doesn't otherwise have, and the iteration body would do
   literal-index access (`$pair[0]`, `$pair[1]`) anyway.

(1) is the cheaper option and addresses every site where the
jq-indexed workaround appears today. (2) only earns its keep
if a pair representation has other uses, which it doesn't.

### When to revisit

When a retrofit needs parallel-list iteration with
metadata-preserving access (i.e. when the workaround's
metadata loss bites). TestTC_DispatcherChainProceedOn hits
the workaround but only needs scalar arguments, so the
metadata loss is invisible there; a future script that
wants `$progs[$i]` to round-trip as a typed Program would
force the fix.

## `jq` is a pure builtin, not a bind-collect producer

### What was tried

Per-element transformation via jq inside a foreach bind-collect:

```
let mapIDs <- foreach p in $progs {
    jq "[.status.maps[] | select(.name == \"tc_stats_map\") | .id][0]" $p
}
```

### What pushed back

`jq` is registered as a pure builtin (see
RegisterPureBuiltin in cmd/bpfman-shell/kindshapes.go). Pure
builtins return Values directly; they do not produce a
result envelope and so do not go through ExecBind. Bind-
collect requires the body's last statement to be a
CommandStmt whose execution flows through ExecBind --
otherwise there is no primary slot to accumulate.

When the parser sees `jq ...` as the last statement of a
bind-collect body, it routes through the *external command*
path (looking for a shell `jq` binary) rather than the pure-
builtin dispatcher. The external jq then receives the
element as stdin, which is usually not what the user wrote
the body to do.

### Workaround

Compound list literal with line continuation:

```
let mapFilter = "[.status.maps[] | select(.name == \"tc_stats_map\") | .id][0]"
let mapIDs = [ \
    ($progs[0] |> jq $mapFilter) \
    ($progs[1] |> jq $mapFilter) \
    ($progs[2] |> jq $mapFilter) \
    ($progs[3] |> jq $mapFilter) \
    ($progs[4] |> jq $mapFilter) \
]
```

One element per line, parenthesised so the `|>` thread
operator works inside the list-element position
(parens-for-compound rule from the list-literal entry).
The `\` newline continuation keeps the list literal on a
single logical line (see the "multi-line list literals"
entry below for why this is required).

Works but reads worse than a 5-line foreach body would have.

### How a fix could look

Two candidate paths:

1. **Pure builtins as bind-collect producers.** Special-case
   the bind-collect body's last statement: if it is a
   PureCallExpr or evaluates to one, run it directly through
   the pure-builtin dispatcher and use its returned Value as
   the iteration's primary. No envelope -- the bind-collect's
   tuple-bind form would be invalid (rc slot has nothing to
   fill). Reject `let (rc, X) <- foreach { jq ... }` at parse
   time.

2. **A wrapper command that promotes a pure-builtin call to
   a CommandStmt result.** Something like `value $x` or
   `return $x` whose primary slot is its argument. Plumbing
   cost: the new command must look like an external command
   to the parser but evaluate to its arg's Value at runtime.
   Feels indirect.

(1) is the cleaner shape. It also unblocks any pure builtin
in this position (range, jq, anything future), not just
the jq case.

### When to revisit

When a script wants per-element jq (or any pure builtin)
collected into a list. Has appeared three times so far
(TestTC_DispatcherChainExecution, the XDP sibling,
ChainProceedOn). The workaround is annoying but mechanical;
the fix is worth it when a fourth site shows up or when an
author complains about the line-continuation density.

## Multi-line list literals require `\` line continuation

### What was tried

Wrapping a long list literal across lines for readability:

```
let priorities = [
    100
    200
    300
    400
    500
]
```

### What pushed back

`takeStmtTokens` collects the let RHS tokens until a
`TokenSep` (newline or `;`). The newline after `[` is a
`TokenSep`, so the RHS truncates to `[` and the parser sees
a malformed expression. Subsequent lines reparse as
separate statements ("100" as a command name, etc.).

### Workaround

Use `\` newline continuation, which the tokeniser absorbs
before emitting any token:

```
let priorities = [ \
    100 \
    200 \
    300 \
    400 \
    500 \
]
```

Each `\<newline>` is consumed as whitespace; the whole let
RHS stays on one logical line. Inside the brackets,
`parseListLiteral` already skips `TokenSep` tokens, so an
alternative fix would be to push that knowledge up into
`takeStmtTokens` -- but that requires bracket-depth tracking
across the whole statement-token collector.

### How a fix could look

**Bracket-aware `takeStmtTokens` / `takeBindRHSTokens`**:
track `[`/`]` depth (and probably `{`/`}` and `(`/`)` for
symmetry). Newlines inside open brackets are skipped; only
top-level newlines terminate the statement.

Cost: small. The collectors already handle `{`/`}` as
statement terminators; bracket depth-tracking is one extra
counter. The change is localised; expression parsing already
strips seps inside parseListLiteral so once the seps reach
the parser the rest works.

### When to revisit

When a script's list literal grows past about five
elements and the `\<newline>` continuation density starts
making the script noisy. The chain-execution retrofits
have 5-10 element lists with `\`-continuation; readable but
visibly load-bearing on the backslashes. A bracket-aware
collector would let those lists wrap naturally.
