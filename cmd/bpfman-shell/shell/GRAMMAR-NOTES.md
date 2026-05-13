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
let priorities = [100, 200, 300, 400, 500, 600, 700, 800, 900, 1000]
let links <- foreach prio in $priorities {
    bpfman link attach tc -i $iface -d ingress -p $prio $pid
}
foreach l in $links {
    defer bpfman link detach $l
}
```

### What pushes back

Two pieces are missing.

**List literal syntax.** There is no `[a, b, c]` form in the
grammar. The only way to construct a list value in a script
today is to invoke `range N` (which produces `[0, 1, ..., N-1]`)
or to round-trip through `jq -n "[1, 2, 3]"`. Neither is a
literal -- both are calls -- and neither is great for the
mixed-shape lists scripts actually want (priorities, names,
flags).

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

1. **List literal**: `[expr, expr, ...]`. Construct a list
   value inline. Elements can be scalars, structured values,
   or further lists. Path traversal and `jq` piping already
   accept lists, so once the literal exists every downstream
   form just works.

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

### What is out of scope even with the fix

The harness-style `TestTC_DispatcherFillDrainRefill` test
maintains a fixed-size mutable slot array indexed by
position, with phases that drain ranges (`slots[3..10)`) and
fill the first N empty slots. Even with list literals and
foreach-collect, the slot bookkeeping does not fall out:
list slicing, indexed mutation, and "first N where
predicate" are separate primitives. That test is the
canonical "stay in Go" case from the original e2e survey
and remains so.
