# REPL assertion comparison semantics and infix notation

## Summary

The REPL's current assertion system is workable, but it conflates two
different concerns:

* **comparison semantics**
* **assertion syntax**

These should be separated.

Phase 1 is the current model, with one important clarification:
comparison verbs fall into two semantic families:

* textual (lexicographic) comparison
* numeric comparison

That split is correct and should be preserved.  Phase 2 extends it
to a fully parallel set of operators: every word operator has a
symbolic counterpart.

Phase 2 improves only the **surface syntax** for binary comparisons by
adding infix operators.  The organising principle is **arity**:

* binary comparisons are infix (operator between operands)
* unary predicates are prefix (predicate before operand)
* command-status checks are prefix (predicate before command)

The intended end state is:

* infix assertions for all binary comparisons, textual and numeric
* prefix assertions for unary predicates and command-status checks
* word operators (`eq`, `ne`, `lt`, `le`, `gt`, `ge`) for textual
  (lexicographic) comparison
* symbol operators (`==`, `!=`, `<`, `<=`, `>`, `>=`) for numeric
  comparison
* no generic truthiness model

Examples:

```bpfman
# Binary comparisons are infix.
# Word operators are textual (lexicographic).
assert $prog.record.name eq xdp_stats
assert $raw1.stdout ne $raw2.stdout
assert $name lt $other_name
# Symbol operators are numeric.
assert $d.exit_code == 1
assert $count > 0
assert $a <= $b

# Unary predicates remain prefix.
assert true $prog.status.kernel_seen
assert false $something
assert nil maybe_value
assert not-empty $data[0].name
assert ok exec ip link show
assert fail exec diff file:$x file:$y
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
assert $name eq xdp_stats
```

rather than:

```bpfman
assert gt $count 0
assert equal $d.exit_code 1
assert eq $name xdp_stats
```

The reason is arity.  When a comparison takes two operands, users
expect the operator between them.  When it takes one operand, they
expect the predicate first.  This is how natural language works:
"assert count greater than zero", not "assert greater-than count
zero".

There is also a deeper semantic issue.  Comparisons are not all of one
kind:

* equality over strings is different from equality over numbers
* ordering over strings (lexicographic) is different from ordering
  over numbers
* command success/failure is neither string nor numeric comparison
* truthiness is vague and should be avoided

The REPL should therefore make the semantic split explicit rather than
pretending all assertions are just variants of one generic comparison
mechanism.

## Goals

### 1. Preserve the current semantic split

The REPL should continue to distinguish between:

* textual (lexicographic) comparison
* numeric comparison
* unary predicates
* command outcome assertions

### 2. Add infix syntax for all binary comparisons

All binary comparisons should be expressible in infix form:

```bpfman
assert $name eq xdp_stats
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

The REPL should not introduce generic truthy/falsy semantics.  Boolean,
nil, emptiness, and command outcome should remain explicit.

### 5. Keep the language small

This should not become a general expression language.  Infix support is
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
  operator instead: `ne` for textual not-equal, `le` for textual
  not-greater-than, `!=` for numeric not-equal, `<=` for numeric
  not-greater-than)

## Current model

Today, assertions are dispatched by a prefix verb:

```bpfman
assert eq <a> <b>
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

* `eq` / `ne` / `lt` / `le` / `gt` / `ge` are textual comparisons
* `true`, `false`, `nil`, `not-empty` are unary predicates
* `ok`, `fail` are command-outcome assertions

In Phase 1, `lt` / `le` / `gt` / `ge` had numeric semantics.  Phase 2
changes them to textual (lexicographic) semantics, pairing them with
their symbolic numeric counterparts.

That split is good.

What is awkward is not the semantics, but the syntax for binary
comparison.

## Core design question

The main question is:

**should the REPL change comparison semantics, or only comparison
notation?**

The answer is: **primarily notation**.

Phase 1 semantics are already largely right:

* textual comparison should remain explicit
* numeric comparison should remain explicit
* unary predicates should remain explicit
* broad truthiness should be avoided

Phase 2 should therefore focus on improving the syntax for binary
comparisons without destabilising the semantic model.

## Why arity, not implementation category

An earlier version of this design kept `eq` / `ne` as prefix verbs
and made only the symbolic operators (`==`, `!=`, `<`, `<=`, `>`,
`>=`) infix.  The organising principle was "symbols are numeric,
words are textual".

That is the wrong axis.  The right axis is **arity**:

* two operands -- operator in the middle
* one operand -- predicate first

This is what users feel immediately.  `assert $a eq $b` reads as
"assert a equals b" -- natural English.  `assert eq $a $b` reads as
"assert equals a b" -- Polish notation.  For any binary comparison,
regardless of whether the operator is a symbol or a word, infix is
more natural.

The implementation category (textual vs numeric) still matters for
semantics, but it should not drive syntax.  The syntax rule is:

**binary in the middle, unary in front.**

## Proposed model

The assertion language has two syntactic families, determined by
arity.

### 1. Prefix assertions (unary)

Prefix assertions remain the model for:

* unary predicates
* command outcome checks

Examples:

```bpfman
assert true $flag
assert false $flag
assert nil maybe_value
assert not-empty $name
assert ok exec ip link show
assert fail exec grep -q needle file:$haystack
```

### 2. Infix assertions (binary)

All binary comparisons use infix notation:

```bpfman
assert $prog.record.name eq xdp_stats
assert $raw1.stdout ne $raw2.stdout
assert $name lt $other_name
assert $d.exit_code == 1
assert $count != 0
assert $count > 0
assert $a >= $b
assert $x < $y
assert $x <= $y
```

The full set of infix operators is:

**Textual (lexicographic):**

* `eq` -- textual equality
* `ne` -- textual inequality
* `lt` -- textual less than
* `le` -- textual less than or equal
* `gt` -- textual greater than
* `ge` -- textual greater than or equal

**Numeric:**

* `==` -- numeric equality
* `!=` -- numeric inequality
* `<` -- numeric less than
* `<=` -- numeric less than or equal
* `>` -- numeric greater than
* `>=` -- numeric greater than or equal

Every word operator has a symbolic counterpart.  The form tells you
the semantics: words compare text, symbols compare numbers.

The textual operators compare the scalar textual representation of
their operands using Go's native string comparison (byte-level
lexicographic ordering).  The numeric operators parse both operands
as numbers using Go's `strconv.ParseFloat` semantics, accepting
ordinary integer and floating-point forms including negative numbers
and scientific notation (e.g., `42`, `-3`, `3.14`, `1e2`).  Octal
or hex prefixes are not recognised; `03` parses as the number 3.

**Important**: `!=` is the numeric inequality operator.  It is **not**
the infix form of `ne`.  A user who writes `assert $x != $y` is
asserting that two numbers are unequal, not that two strings differ.
For textual inequality, the correct form is `assert $x ne $y`.  This
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
runtime.  The form of the assertion no longer tells you the semantics.
This is the JavaScript `==` problem.

### `eq` and `ne` become vestigial

If `==` / `!=` are generic, there is no reason to use `eq` / `ne`.
The generic operators do everything the textual operators do, plus
handle numbers.  The result is two ways to do the same thing, one
strictly more powerful, the other existing only because the design
could not commit.

### The split between families becomes unmemorable

With the word/symbol split, the rule is simple:

* words (`eq`, `ne`, `lt`, `le`, `gt`, `ge`) -- textual, all of them
* symbols (`==`, `!=`, `<`, `<=`, `>`, `>=`) -- numeric, all of them

Two families.  Every word has a symbolic counterpart.  No overlap.
No coercion.  The form tells you the semantics.

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
assert 03 eq 3
```

That will fail, which is correct for text.

The same logic applies to `!=` and `ne`:

* `assert $x != $y` -- numeric: are these different numbers?
* `assert $x ne $y` -- textual: are these different strings?

These are different questions.  The REPL makes the distinction
explicit rather than guessing which one the user meant.

The same applies to ordering:

* `assert $x < $y` -- numeric: is this number smaller?
* `assert $x lt $y` -- textual: does this string sort before?

`assert 9 lt 10` is false (lexicographically `"9"` sorts after `"1"`).
`assert 9 < 10` is true (numerically 9 is less than 10).

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

All assertions use prefix notation.

### Textual equality

Use explicit verbs:

```bpfman
assert eq <left> <right>
assert ne <left> <right>
```

These compare their operands as scalar textual equality.  The operands
are not necessarily strings in origin -- they may be numbers, booleans,
or any scalar value rendered textually by the expansion pipeline.  The
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

These parse both operands as numbers.

Note: in Phase 1, `lt` / `le` / `gt` / `ge` have numeric semantics.
Phase 2 changes them to textual (lexicographic) semantics, pairing
each word operator with a symbolic numeric counterpart.

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

This makes the family visually coherent.  The old `equal` verb is
removed, not aliased.

## Phase 2

Phase 2 introduces infix notation for all binary comparisons.  The
organising principle is arity: binary comparisons go in the middle,
unary predicates go in front.

### Supported infix operators

The following operators are recognised in `assert` and `require`:

**Textual (lexicographic):**

* `eq`
* `ne`
* `lt`
* `le`
* `gt`
* `ge`

**Numeric:**

* `==`
* `!=`
* `<`
* `<=`
* `>`
* `>=`

Examples:

```bpfman
assert $prog.record.name eq xdp_stats
assert $raw1.stdout ne $raw2.stdout
assert $name lt $other_name
assert $d.exit_code == 1
assert $count != 0
assert $count > 0
require $a <= $b
```

### Semantics

Infix operators preserve the semantic split between textual and
numeric comparison:

* `eq`, `ne`, `lt`, `le`, `gt`, `ge` compare scalar values textually
  (lexicographic byte ordering).
* `==`, `!=`, `<`, `<=`, `>`, `>=` compare numerically.  Both
  operands must parse as numbers using `strconv.ParseFloat`.
  Non-numeric operands cause the assertion itself to error.

**`!=` is not the infix form of `ne`.** It is numeric inequality.
String inequality must use `ne`:

```bpfman
assert $x != $y       # numeric: are these different numbers?
assert $x ne $y       # textual: are these different strings?
```

### Parsing model

The grammar is intentionally narrow:

```text
assert <left> <op> <right>
require <left> <op> <right>
```

where `<op>` is one of: `eq`, `ne`, `lt`, `le`, `gt`, `ge`,
`==`, `!=`, `<`, `<=`, `>`, `>=`.

No other infix forms are recognised.

There is no general expression parsing.

The operands remain ordinary REPL argument forms, including variable
expansion:

```bpfman
assert $count > 0
assert $d.exit_code == 1
assert $name eq xdp_stats
```

### Disambiguation

When the assert parser sees three or more expanded arguments, it
checks whether the second argument is a recognised infix operator.
If so, the assertion is infix binary.  If not, the first argument is
treated as a prefix unary verb.

This means a value that happens to equal `eq`, `ne`, `==`, etc. in
the first operand position would be misinterpreted as an infix
operator.  In practice this is not a concern: the first argument after
`assert` is either a known verb or a variable reference, and variable
references do not resolve to operator strings in normal use.

### Prefix binary verbs are removed

Phase 2 replaces prefix binary verbs entirely.  The prefix forms
`eq <a> <b>`, `ne <a> <b>`, `lt <a> <b>`, `le <a> <b>`,
`gt <a> <b>`, `ge <a> <b>` are no longer recognised.

The canonical forms are:

```bpfman
assert $prog.record.name eq xdp_stats
assert $raw1.stdout ne $raw2.stdout
assert $count > 0
assert $d.exit_code == 1
```

Unary and command assertions are unaffected:

```bpfman
assert true $flag
assert not-empty $name
assert ok exec ip link show
assert fail exec diff file:$x file:$y
```

### `not` is not supported with infix assertions

Phase 2 does not support `not` over infix form:

```bpfman
# This is NOT valid:
assert not $count > 0
assert not $a eq $b
```

Permitting `not` before an infix comparison introduces parsing
ambiguity (is `not` the negation keyword or the left operand?) and
moves towards general expression parsing.  Infix operators already
provide direct complementary forms:

* `ne` instead of `not eq`
* `le` instead of `not gt`
* `ge` instead of `not lt`
* `!=` instead of `not ==`
* `<=` instead of `not >`
* `>=` instead of `not <`

`not` remains available for prefix unary verbs only:

```bpfman
assert not true $flag
assert not nil $x
assert not ok exec ...
```

## Tokenisation implications

The tokeniser currently recognises standalone `=` as `TokenAssign`
when it appears at a token boundary after at least one preceding
token.  This supports the `let` and `set` assignment syntax.

Phase 2 extends this area of lexical behaviour.  The tokeniser must
additionally recognise `==`, `!=`, `<`, `<=`, `>`, and `>=` as
comparison operator tokens rather than splitting them into
assignment-like pieces or absorbing them as plain word text.

In particular, `==` cannot simply be left as a word because the
existing `=` recognition fires first and would split it into two
`TokenAssign` tokens.  The tokeniser must peek ahead when it
encounters `=` to distinguish `=` (assignment) from `==` (comparison).
Similarly, `!=` must be recognised as a single token rather than `!`
followed by `=`.

`<`, `<=`, `>`, and `>=` do not conflict with existing token kinds
because they are not currently special characters.  They would be
consumed by `lexWord` today, which is acceptable for Phase 2 parsing
as long as the assert parser can recognise them as operators.

The word operators (`eq`, `ne`, `lt`, `le`, `gt`, `ge`) do not require
tokeniser changes.  They are already consumed as `TokenWord` tokens.
The assert parser recognises them as infix operators by checking the
second argument position, not by relying on a distinct token kind.

This is a local extension to the tokeniser, not a move towards general
expression syntax.  The tokeniser does not need operator precedence or
expression support.  It only needs to preserve the symbolic operator
tokens so the assert parser can recognise the simple binary-comparison
shape.

## Error model

Phase 1 and Phase 2 should have direct, type-oriented errors.

### Textual operators

`eq`, `ne`, `lt`, `le`, `gt`, `ge` do not parse numbers.  They
compare the textual representation directly using lexicographic byte
ordering.

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

### Textual comparison (infix)

```bpfman
assert $prog.record.name eq xdp_stats
assert $raw1.stdout ne $raw2.stdout
assert $name lt $other_name
assert $version ge 1.2.0
```

### Numeric comparison (infix)

```bpfman
assert $count > 0
assert $d.exit_code == 1
assert $a <= $b
```

### Unary predicates

```bpfman
assert true $prog.status.kernel_seen
assert false $something
assert nil maybe_value
assert not-empty $data[0].name
```

### Command outcome

```bpfman
require ok exec ip netns add bpfman-test
assert fail exec grep -q needle file:$haystack
```

### Mixed use

```bpfman
let d = exec status diff file:$raw1.stdout file:$raw2.stdout
assert $d.exit_code == 1
assert $raw1.stdout ne $raw2.stdout
assert not-empty $d.stdout
```

## Alternatives considered

### 1. Keep everything prefix forever

For example:

```bpfman
assert eq $x 3
assert gt $x 0
```

This is simple to parse, but reads less naturally for binary
comparison.

### 2. Make `==` and `!=` generic over strings and numbers

This looks attractive, but creates ambiguity between textual and
numeric equality.  Keeping string equality explicit avoids that
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

This is much larger in scope than needed.  The design here is
deliberately narrow: one binary comparison, no arithmetic, no boolean
composition.

### 5. Keep `eq` / `ne` prefix, make only symbols infix

An earlier version of this design kept `eq` / `ne` as prefix verbs
and made only the symbolic operators infix.  The organising principle
was "symbols are numeric, words are textual".

That is the wrong axis.  The right axis is arity.  `assert $a eq $b`
reads as "assert a equals b".  `assert eq $a $b` reads as "assert
equals a b".  For any binary comparison, regardless of whether the
operator is a symbol or a word, infix is more natural.  Organising
around arity ("binary in the middle, unary in front") is what users
feel immediately.

## Open questions

### 1. Should `require` support the same infix syntax?

It should.  The syntax change is orthogonal to the assert-vs-require
control-flow distinction.

## Recommendation

Proceed in two phases.

### Phase 1

Keep the current prefix model, with clarified semantics:

* `eq` / `ne` for textual equality (scalar textual comparison)
* `lt` / `le` / `gt` / `ge` for numeric ordering (changed to textual
  in Phase 2)
* `true`, `false`, `nil`, `not-empty` for explicit unary predicates
* `ok`, `fail` for command outcome
* no generic truthiness

### Phase 2

Introduce infix notation for all binary comparisons:

* `eq`, `ne`, `lt`, `le`, `gt`, `ge` for textual (lexicographic)
  comparison
* `==`, `!=`, `<`, `<=`, `>`, `>=` for numeric comparison

The organising principle is arity:

* **binary in the middle** -- all comparisons with two operands
* **unary in front** -- all predicates with one operand

Prefix binary verbs are removed, not kept as aliases.  Unary
and command assertions remain prefix.  `not` negation is not supported
with infix form; use the direct complementary operator instead.

That gives the REPL a cleaner and more natural assertion surface
without turning it into a general expression language.
