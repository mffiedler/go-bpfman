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
