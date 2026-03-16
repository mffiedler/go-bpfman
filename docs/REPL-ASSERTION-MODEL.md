# REPL assertion comparison semantics and infix notation

## Summary

The REPL's current assertion system is workable, but it conflates two
different concerns:

* **comparison semantics**
* **assertion syntax**

These should be separated.

Phase 1 is the current model, with one important clarification:
comparison verbs already fall into two semantic families:

* textual equality / inequality
* numeric ordering

That split is correct and should be preserved.

Phase 2 improves only the **surface syntax** for binary comparisons by
adding infix operators. Unary assertions remain prefix.

The intended end state is:

* prefix assertions for unary predicates and command-status checks
* infix assertions for binary comparisons
* numeric operators for numeric comparison
* explicit textual verbs for scalar textual comparison
* no generic truthiness model

Examples:

```bpfman
# Unary predicates remain prefix.
assert true $prog.status.kernel_seen
assert false $something
assert nil maybe_value
assert not-empty $data[0].name
assert ok exec ip link show
assert fail exec diff file:$x file:$y

# Textual equality remains explicit.
assert eq $prog.record.name xdp_stats
assert ne $raw1.stdout $raw2.stdout

# Numeric comparison becomes infix.
assert $d.exit_code == 1
assert $count > 0
assert $a <= $b
```

This keeps the language small while making the common comparison forms
read more naturally.

## Motivation

The current assertion syntax is prefix-verb based:

```bpfman
assert equal 3 3
assert gt $count 0
assert not-empty $name
assert ok exec ip link show
```

This has some advantages:

* easy to parse
* uniform dispatch model
* naturally fits unary predicates such as `nil`, `true`, `false`,
  `not-empty`, `ok`, and `fail`

However, the prefix form is awkward for binary comparison.

Users naturally expect:

```bpfman
assert $count > 0
assert $d.exit_code == 1
```

rather than:

```bpfman
assert gt $count 0
assert equal $d.exit_code 1
```

There is also a deeper semantic issue. Comparisons are not all of one
kind:

* equality over strings is different from equality over numbers
* ordering only makes sense for numbers
* command success/failure is neither string nor numeric comparison
* truthiness is vague and should be avoided

The REPL should therefore make the semantic split explicit rather than
pretending all assertions are just variants of one generic comparison
mechanism.

## Goals

### 1. Preserve the current semantic split

The REPL should continue to distinguish between:

* textual equality / inequality
* numeric comparison
* unary predicates
* command outcome assertions

### 2. Add infix syntax for binary comparisons

Binary comparisons should be expressible in a more natural form:

```bpfman
assert $count > 0
assert $d.exit_code == 1
```

### 3. Keep unary assertions prefix

Assertions such as these should remain prefix:

```bpfman
assert nil x
assert not-empty $name
assert true $flag
assert ok exec ...
assert fail exec ...
```

### 4. Avoid broad truthiness rules

The REPL should not introduce generic truthy/falsy semantics. Boolean,
nil, emptiness, and command outcome should remain explicit.

### 5. Keep the language small

This should not become a general expression language. Infix support is
for a small set of comparison operators only.

## Non-goals

This feature does not provide:

* a general expression evaluator
* arithmetic expressions
* precedence rules beyond a single binary comparison
* chained comparisons such as `a < b < c`
* boolean composition (`&&`, `||`)
* implicit truthiness
* automatic type coercion between text and numbers
* user-defined operators
* `not` negation over infix assertions (use the direct complementary
  operator instead: `!=` for not-equal, `<=` for not-greater-than)

## Current model

Today, assertions are dispatched by a prefix verb:

```bpfman
assert equal <a> <b>
assert ne <a> <b>
assert lt <a> <b>
assert le <a> <b>
assert gt <a> <b>
assert ge <a> <b>
assert true <x>
assert false <x>
assert nil <x>
assert not-empty <x>
assert ok <command...>
assert fail <command...>
```

This already implies a useful semantic split:

* `equal` / `ne` are textual comparisons
* `lt` / `le` / `gt` / `ge` are numeric comparisons
* `true`, `false`, `nil`, `not-empty` are unary predicates
* `ok`, `fail` are command-outcome assertions

That split is good.

What is awkward is not the semantics, but the syntax for binary
comparison.

## Core design question

The main question is:

**should the REPL change comparison semantics, or only comparison
notation?**

The answer is: **primarily notation**.

Phase 1 semantics are already largely right:

* textual equality should remain explicit
* numeric ordering should remain numeric-only
* unary predicates should remain explicit
* broad truthiness should be avoided

Phase 2 should therefore focus on improving the syntax for binary
comparisons without destabilising the semantic model.

## Proposed model

The assertion language has two families.

### 1. Prefix assertions

Prefix assertions remain the model for:

* unary predicates
* command outcome checks
* explicit textual equality / inequality

Examples:

```bpfman
assert eq $prog.record.name xdp_stats
assert ne $raw1.stdout $raw2.stdout
assert true $flag
assert false $flag
assert nil maybe_value
assert not-empty $name
assert ok exec ip link show
assert fail exec grep -q needle file:$haystack
```

### 2. Infix assertions

Infix assertions are added for binary numeric comparison only:

```bpfman
assert $d.exit_code == 1
assert $count != 0
assert $count > 0
assert $a >= $b
assert $x < $y
assert $x <= $y
```

These operators all have numeric semantics.

That means:

* both operands are parsed as numbers
* comparison fails if either side is not numeric

This gives one coherent numeric comparison family:

* `==`
* `!=`
* `<`
* `<=`
* `>`
* `>=`

**Important**: `!=` is the numeric inequality operator. It is **not**
the infix form of `ne`. A user who writes `assert $x != $y` is
asserting that two numbers are unequal, not that two strings differ.
For textual inequality, the correct form is `assert ne $x $y`. This
is the one place where the split between textual and numeric families
is least obvious, and users should be aware of the distinction.

## Why symbolic operators are numeric-only

A tempting alternative is to make `==` and `!=` generic scalar
equality operators that try numeric comparison when both sides parse
as numbers and fall back to textual comparison otherwise.

That would be a mistake, for three reasons.

### The comparison mode would depend on data, not intent

A "try numeric, fall back to textual" rule means the user writes
`assert $x == $y` and whether that performs a numeric or textual
comparison depends on what `$x` and `$y` happen to contain at
runtime. The form of the assertion no longer tells you the semantics.
This is the JavaScript `==` problem.

### `eq` and `ne` become vestigial

If `==` / `!=` are generic, there is no reason to use `eq` / `ne`.
The generic operators do everything the textual operators do, plus
handle numbers. The result is two ways to do the same thing, one
strictly more powerful, the other existing only because the design
could not commit.

### The split between families becomes unmemorable

With numeric-only symbolic operators, the rule is simple:

* symbols (`==`, `!=`, `<`, `<=`, `>`, `>=`) -- numeric, all of them
* words (`eq`, `ne`) -- textual, both of them

Two families. No overlap. No coercion. The form tells you the
semantics.

### The `==` ambiguity in detail

```bpfman
assert 03 == 3
```

Should that pass?

* textual equality says no
* numeric equality says yes

If `==` is numeric, the answer is clear.

If users want textual equality, they should say so explicitly:

```bpfman
assert eq 03 3
```

That will fail, which is correct for text.

The same logic applies to `!=` and `ne`:

* `assert $x != $y` -- numeric: are these different numbers?
* `assert ne $x $y` -- textual: are these different strings?

These are different questions. The REPL makes the distinction
explicit rather than guessing which one the user meant.

## Why not generic truthiness

Broad truthiness sounds convenient, but it quickly becomes vague.

Questions arise immediately:

* is `"0"` truthy?
* is `""` false?
* is `null` false?
* is `"false"` false?
* is `[]` false?
* is `{}` false?

The REPL should avoid this entire category of ambiguity.

Instead, keep these checks explicit:

* `true`
* `false`
* `nil`
* `not-empty`
* `ok`
* `fail`

That is more precise and easier to reason about.

## Phase 1

Phase 1 is the current assertion model, with clarified semantics.

### Textual equality

Use explicit verbs:

```bpfman
assert eq <left> <right>
assert ne <left> <right>
```

These compare their operands as scalar textual equality. The operands
are not necessarily strings in origin -- they may be numbers, booleans,
or any scalar value rendered textually by the expansion pipeline. The
comparison is over the textual representation, regardless of the
original type.

Examples:

```bpfman
assert eq $prog.record.name xdp_stats
assert ne $raw1.stdout $raw2.stdout
assert eq $flag true
```

### Numeric ordering

Use numeric verbs:

```bpfman
assert lt <left> <right>
assert le <left> <right>
assert gt <left> <right>
assert ge <left> <right>
```

These parse both operands as numbers. Numeric operators use Go's
`strconv.ParseFloat` semantics, accepting ordinary integer and
floating-point forms including negative numbers and scientific
notation (e.g., `42`, `-3`, `3.14`, `1e2`). Octal or hex prefixes
are not recognised; `03` parses as the number 3.

Examples:

```bpfman
assert gt $count 0
assert le $d.exit_code 1
```

### Unary predicates

Remain prefix:

```bpfman
assert true <value>
assert false <value>
assert nil <value>
assert not-empty <value>
```

### Command outcome

Remain prefix:

```bpfman
assert ok <command...>
assert fail <command...>
require ok <command...>
require fail <command...>
```

### Phase 1 canonical naming

The canonical comparison verb names are:

* `eq`
* `ne`
* `lt`
* `le`
* `gt`
* `ge`

This makes the family visually coherent. `equal` remains as a
compatibility alias only; new code and documentation should use `eq`.

## Phase 2

Phase 2 adds infix notation for numeric comparisons.

### Supported infix operators

The following operators are recognised in `assert` and `require`:

* `==`
* `!=`
* `<`
* `<=`
* `>`
* `>=`

Examples:

```bpfman
assert $d.exit_code == 1
assert $count != 0
assert $count > 0
require $a <= $b
```

### Semantics

All infix operators are numeric. Both operands must be present, and
both must parse as numbers using the same rules as the prefix numeric
verbs (`strconv.ParseFloat`). Non-numeric operands cause the assertion
itself to error.

**`!=` is not the infix form of `ne`.** It is numeric inequality.
String inequality must use `ne`:

```bpfman
assert $x != $y       # numeric: are these different numbers?
assert ne $x $y       # textual: are these different strings?
```

### Parsing model

The grammar is intentionally narrow:

```text
assert <left> <op> <right>
require <left> <op> <right>
```

where `<op>` is one of:

* `==`
* `!=`
* `<`
* `<=`
* `>`
* `>=`

No other infix forms are recognised.

There is no general expression parsing.

The operands remain ordinary REPL argument forms, including variable
expansion:

```bpfman
assert $count > 0
assert $d.exit_code == 1
```

### Prefix and infix coexistence

Phase 2 does not remove prefix assertions. Prefix numeric verbs
(`lt`, `le`, `gt`, `ge`) remain supported. Infix becomes the
preferred spelling in documentation and examples, but prefix verbs
are not deprecated.

So these remain valid:

```bpfman
assert eq $prog.record.name xdp_stats
assert ne $raw1.stdout $raw2.stdout
assert gt $count 0
assert true $flag
assert fail exec diff file:$x file:$y
```

while these become newly valid:

```bpfman
assert $d.exit_code == 1
assert $count > 0
assert $a <= $b
```

### `not` is not supported with infix assertions

Phase 2 does not support `not` over infix form:

```bpfman
# This is NOT valid:
assert not $count > 0
```

Permitting `not` before an infix comparison introduces parsing
ambiguity (is `not` the negation keyword or the left operand?) and
moves towards general expression parsing. Infix operators already
provide direct complementary forms:

* `!=` instead of `not ==`
* `<=` instead of `not >`
* `>=` instead of `not <`

`not` remains available for prefix verbs only.

## Tokenisation implications

The tokeniser currently recognises standalone `=` as `TokenAssign`
when it appears at a token boundary after at least one preceding
token. This supports the `let` and `set` assignment syntax.

Phase 2 extends this area of lexical behaviour. The tokeniser must
additionally recognise `==`, `!=`, `<`, `<=`, `>`, and `>=` as
comparison operator tokens rather than splitting them into
assignment-like pieces or absorbing them as plain word text.

In particular, `==` cannot simply be left as a word because the
existing `=` recognition fires first and would split it into two
`TokenAssign` tokens. The tokeniser must peek ahead when it
encounters `=` to distinguish `=` (assignment) from `==` (comparison).
Similarly, `!=` must be recognised as a single token rather than `!`
followed by `=`.

`<`, `<=`, `>`, and `>=` do not conflict with existing token kinds
because they are not currently special characters. They would be
consumed by `lexWord` today, which is acceptable for Phase 2 parsing
as long as the assert parser can recognise them as operators.

This is a local extension to the tokeniser, not a move towards general
expression syntax. The tokeniser does not need operator precedence or
expression support. It only needs to preserve these operator tokens so
the assert parser can recognise the simple binary-comparison shape.

## Error model

Phase 1 and Phase 2 should have direct, type-oriented errors.

### Textual equality

`eq` / `ne` do not parse numbers. They compare the textual
representation directly.

### Numeric operators

If either operand is non-numeric, the assertion errors:

```text
assert foo > 3
```

should fail with an error such as:

```text
assert: left operand "foo" is not numeric
```

Similarly for numeric equality:

```text
assert foo == bar
```

should fail with:

```text
assert: left operand "foo" is not numeric
```

### Unsupported infix shapes

These should error:

```bpfman
assert $a + $b
assert $a > $b > $c
assert == 3 3
```

The error should make clear that only a single binary comparison is
supported.

## Examples

### Phase 1 text equality

```bpfman
assert eq $prog.record.name xdp_stats
assert ne $raw1.stdout $raw2.stdout
```

### Phase 1 numeric ordering

```bpfman
assert gt $count 0
assert le $d.exit_code 1
```

### Phase 1 unary predicates

```bpfman
assert true $prog.status.kernel_seen
assert false $something
assert nil maybe_value
assert not-empty $data[0].name
```

### Phase 1 command outcome

```bpfman
require ok exec ip netns add bpfman-test
assert fail exec grep -q needle file:$haystack
```

### Phase 2 numeric infix

```bpfman
assert $d.exit_code == 1
assert $count != 0
assert $count > 0
assert $a <= $b
```

### Mixed use

```bpfman
let d = exec status diff file:$raw1.stdout file:$raw2.stdout
assert $d.exit_code == 1
assert ne $raw1.stdout $raw2.stdout
assert not-empty $d.stdout
```

## Alternatives considered

### 1. Keep everything prefix forever

For example:

```bpfman
assert eq $x 3
assert gt $x 0
```

This is simple to parse, but reads less naturally for binary numeric
comparison.

### 2. Make `==` and `!=` generic over strings and numbers

This looks attractive, but creates ambiguity between textual and
numeric equality. Keeping string equality explicit avoids that
confusion.

### 3. Add generic truthiness

For example:

```bpfman
assert $x
```

This makes the language less precise and introduces ambiguity around
empty strings, zero, null, and false-like values.

### 4. Add a full expression language

For example:

```bpfman
assert ($a + $b) > 3
```

This is much larger in scope than needed. The design here is
deliberately narrow: one binary comparison, no arithmetic, no boolean
composition.

## Open questions

### 1. Should `equal` remain as an alias?

If existing scripts use `equal`, it may be worth keeping as an alias
for `eq`. The preferred spelling should still become `eq`.

### 2. Should `require` support the same infix syntax?

It should. The syntax change is orthogonal to the assert-vs-require
control-flow distinction.

## Recommendation

Proceed in two phases.

### Phase 1

Keep the current model, with clarified semantics:

* `eq` / `ne` for textual equality (scalar textual comparison)
* `lt` / `le` / `gt` / `ge` for numeric ordering
* `true`, `false`, `nil`, `not-empty` for explicit unary predicates
* `ok`, `fail` for command outcome
* no generic truthiness
* `eq` as the canonical name; `equal` as compatibility alias only

### Phase 2

Add infix notation for numeric comparison only:

* `==`, `!=` for numeric equality / inequality
* `<`, `<=`, `>`, `>=` for numeric ordering

Prefix numeric verbs remain supported but infix becomes the preferred
form in documentation and examples. Unary assertions remain prefix.
Textual equality remains explicit via `eq` / `ne`. `not` negation is
not supported with infix form.

That gives the REPL a cleaner and more natural assertion surface
without turning it into a general expression language.
