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
