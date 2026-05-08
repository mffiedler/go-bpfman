# REPL Arithmetic: design and implementation plan

Status: **plan of record**.  Target: land as a single PR that is
legible end-to-end.  The shell language currently has no
arithmetic — any derived metric requires shelling out to `jq`.
After recent work on expression-statements and bracketed
expression evaluation, the gap is visible at the prompt
(`$count + 1` is a parse error today).  This plan adds a minimal,
principled set of arithmetic operators and stops.

## Scope

### In scope

- Five binary operators: `+`, `-`, `*`, `/`, `%`.
- Unary `-` (negation) on expressions (`-$x`, `-(a + b)`,
  `-[jq ...]`).  Negative literals (`-3`, `-3.14`) already parse
  as single WORD tokens and need no grammar change.
- Float64 semantics throughout, routed through the same
  `strconv.ParseFloat` helper the comparison evaluator uses.
- Runtime diagnostics for division by zero and non-numeric
  operands.
- Two new precedence levels (multiplicative above additive,
  both above comparison) in the recursive-descent parser.
- Grammar, parser, evaluator, tests, docs, Emacs mode updates.

### Out of scope — explicit boundary

The following are **deliberately excluded**.  They should not
grow into the same PR; if a future change adds them, it should
be its own deliberate commit with its own motivation.

- String concatenation via `+` (Python style).  `"a" + "b"` is a
  runtime error.  Concat stays via jq or future interpolation.
- Integer division (`//`) or power (`**`).
- Bitwise operators (`&`, `|`, `^`, `~`, `<<`, `>>`).
- In-place forms (`+=`, `-=`, etc.).  The language has no
  mutation story beyond `let`.
- Builtins like `min`, `max`, `abs`, `floor`, `ceil`.  Covered
  by `jq` for now.
- Comparison chains (`1 < $x < 10`).  Stays binary.
- Integer-result preservation.  `5 * 2` formats as `10` (no
  trailing `.0`) thanks to `strconv.FormatFloat(..., -1, 64)`,
  but internally everything is float64.

The boundary statement should appear verbatim in
`docs/repl/language.md` under the new Arithmetic section so
users know where the line is.

## Design decisions (rationale)

### Numeric only — no string concat

Python overloads `+` for strings; shell does not.  We are
neither: we are a command-shaped DSL.  Users coming from Python
will be surprised that `"a" + "b"` errors — document that
surprise explicitly rather than committing to ad-hoc operator
overloading.  If and when string interpolation arrives, it will
be the explicit concat story.

### Float64 throughout

The comparison evaluator already parses both operands via
`strconv.ParseFloat`.  Keeping arithmetic on the same path is
consistent and avoids a second numeric tower.  `5 / 2` is
`2.5`, not `2`.  Users who want integer division can pipe
through jq (`[jq "(.a / .b) | floor" ...]`).

### Float formatting

Results are rendered via `strconv.FormatFloat(x, 'f', -1, 64)`,
same as `Value.Scalar()` does today for float scalars.  `10.0`
prints as `10`; `2.5` prints as `2.5`.  No surprising trailing
zeros for integer-valued results.

### Whitespace-sensitive

The tokeniser treats `-3` as a single WORD.  This means
`$x - 3` (with spaces) is subtraction but `$x -3` (no space
after `-`) tokenises as two WORDs (`$x`, `-3`) and parses as
two adjacent primaries with no binop between them — a parse
error.  We accept this quirk rather than retokenising.  The
Arithmetic section of the docs should state: **whitespace is
required around binary operators**.

### Precedence

Standard arithmetic: multiplicative binds tighter than
additive, both bind tighter than comparison.  Unary negation
binds tightest among non-primary forms.  Unary predicates
(`not-empty`, `true`, `false`) stay at their current tight
level, so `not-empty $x + 1` parses as
`(not-empty $x) + 1`, which is a type error at eval time —
acceptable, because any mix of booleans and arithmetic is
nonsense anyway.

### Unary minus vs binary subtraction

At additive level, `-` is always binary.  At the start of an
expression (or after another operator), `-` is unary.  The
parser distinguishes by position: additive's left operand is
parsed at the tighter `multiplicative` level which chains
through `unary → negate`; additive's loop then only looks for
`-` and `+` as infix operators.  No ambiguity.

## Grammar

Change in `docs/repl/language.md` (full block, showing what
moves where):

```text
program      := { stmt (SEP stmt)* }
stmt         := let-stmt | if-stmt | foreach-stmt | retry-stmt
              | expr-stmt | command-stmt | 'break' | 'continue'

expr         := or
or           := and ('or' and)*
and          := not ('and' not)*
not          := 'not' not | comparison
comparison   := additive (BINOP additive)?
additive     := multiplicative (('+' | '-') multiplicative)*   (* NEW *)
multiplicative := predicate (('*' | '/' | '%') predicate)*     (* NEW *)
predicate    := UNARY-PRED term | negate                       (* renamed from unary *)
negate       := '-' negate | thread                            (* NEW *)
thread       := term ('|>' command)*
term         := literal | varref | cmdsub | adapter | '(' expr ')'
              | 'timeout' DURATION | 'iteration' INTEGER

BINOP        := '==' | '!=' | '<' | '<=' | '>' | '>='
UNARY-PRED   := 'true' | 'false' | 'not-empty'
```

`unary` in the old grammar is renamed to `predicate` to avoid
collision with the new `negate` level (which is morally also a
unary form).  The rename is local to the grammar description;
the Go function currently called `parseUnaryOr` stays named or
is renamed to `parsePredicate` at the same time — pick one at
implementation and use consistently.

## Parser changes (shell/parse.go)

### New functions

```go
func (p *exprParser) parseAdditive() (Expr, error)
func (p *exprParser) parseMultiplicative() (Expr, error)
func (p *exprParser) parseNegate() (Expr, error)
```

### Call chain update

`parseComparison` currently calls `parseUnaryOr` for its
operands.  Change it to call `parseAdditive`.
`parseMultiplicative` calls the current predicate parser (renamed
if applicable).  `parseNegate` sits between predicate's
fall-through and thread: predicate's non-UNARY-PRED path goes to
`parseNegate` instead of `parseThread`.

### Binary-op recognition

`binaryOpFromToken` currently returns only the comparison ops.
Keep it that way — arithmetic operators are recognised
positionally in the new levels, not by consulting a shared
helper.  The `isBinaryOp` check used by `operandFollowsPred`
should include `+`, `-`, `*`, `/`, `%` so that `true $x + 1`
still parses `true` as a unary predicate rather than as a bare
literal followed by arithmetic.  Add the arithmetic tokens to
the existing `binaryOpFromToken` set, or extend the lookahead
function to include them explicitly.

### AST nodes

A single `BinaryExpr` already covers comparison ops with an `Op`
field.  Reuse it for arithmetic — same struct, different Op
strings (`"+"`, `"-"`, `"*"`, `"/"`, `"%"`).  Add `NegateExpr`
(or reuse `NotExpr` with a different discriminator — up to
taste, but a separate node is clearer).

### Tokeniser

No change expected.  `+`, `*`, `/`, `%` are not currently
reserved characters in the tokeniser's word-terminator set, but
they are also not currently emitted as distinct tokens.  Verify:
if the tokeniser treats `+` as a word character, `$x + 1` splits
correctly on whitespace, but `$x+1` would tokenise as a single
WORD `$x+1` which is wrong.  **Action item**: confirm at
implementation time whether `+`, `*`, `/`, `%` terminate WORDs;
if they do not, either add them to the terminator set (and rely
on parser-level assembly) or document the whitespace
requirement.  The lowest-disruption choice is: require
whitespace, don't touch the tokeniser.

## Evaluator changes (shell/expr.go)

### New function

```go
func evalArithmetic(op string, left, right string) (Value, error)
```

Mirrors `evalNumericComparison`: parses both operands via
`strconv.ParseFloat`, dispatches on `op`, returns a numeric
`Value`.  Division by zero returns a `locErrorf` with a clear
message ("division by zero in EXPR").

### Dispatch

`evalBinary` type-switches on `e.Op`.  Add cases for `"+"`,
`"-"`, `"*"`, `"/"`, `"%"` that call `evalArithmetic`.  Keep
the existing comparison-op cases unchanged.

### Unary negate

New `evalNegate(*NegateExpr, *Env) (Value, error)`: evaluate
operand, parse scalar as float, return `-x` as a numeric Value.
Wire into the main EvalExpr switch.

### Value construction

Arithmetic produces numeric Values.  Use `json.Number` (matching
how jq-sourced numbers are represented) or float64 — pick one.
The existing `StringValue` of a numeric string plus scalar
parsing at every comparison site suggests `StringValue` with
numeric text would be the least-intrusive.  Go with
`json.Number(strconv.FormatFloat(result, 'f', -1, 64))`
wrapped in a `Value` to match how jq-emitted numbers arrive.

## Tests

### shell/parse_test.go

- Precedence: `1 + 2 * 3` → AST has `+` at root, `*` nested.
- Precedence: `(1 + 2) * 3` → `*` at root, parens contain `+`.
- Mixed with comparison: `$x + 1 == 5` -> `==` at root,
  additive as left operand.
- Unary negate: `-$x`, `-(1 + 2)`, `- -3` (double negation).
- Negative literal: `let x = -3` (unchanged behaviour —
  regression test).
- Error: `$x + ` (trailing operator) surfaces a locErrorf.

### shell/expr_test.go

- Each binop on two integer operands, two float operands, and
  mixed.
- Modulo: `7 % 3` = `1`, `7.5 % 2.0` = `1.5` (Go's math.Mod
  semantics — confirm at implementation time or use a simpler
  float-mod helper).
- Division by zero: `1 / 0` and `1 % 0` both error.
- Non-numeric operand: `"abc" + 1` errors with a clear message.
- Unary negate: `-5` = `-5`, `-(-5)` = `5`, `-$x` where `$x`
  is a structured value errors.
- Arithmetic in comparison position: `3 + 4 > 5` → `true`.
- Arithmetic in let RHS: `let n = $count + 1`.

### cmd/bpfman/repl_test.go

- End-to-end at the REPL: `let x = 5; $x + 1` auto-prints `6`.
- `print [[$count * 2]]` prints the doubled value.

## Documentation

### docs/repl/language.md

- Update the grammar block (shown above).
- Add an "Arithmetic" subsection after "Comparisons" with:
  - One line per operator.
  - Float-semantics note.
  - Division-by-zero behaviour.
  - Whitespace-required note.
  - "What is not included" list (verbatim from the Scope →
    Out of scope boundary above).

### docs/repl/builtins.md

No change required unless we want to add an "arithmetic vs jq"
guidance note alongside the existing "when to use jq" section.
Low priority.

### emacs/bpfman-mode.el

Fontify `+`, `-`, `*`, `/`, `%` as builtin-faces when they
appear in expression position.  The current mode machinery
treats subcommand words as builtins; arithmetic operators
should get the same face for consistency.  Add them to the
subcommand/operator hash table.

### emacs/syntax-gallery.bpfman

New section after the threading example:

```bpfman
# ---- Arithmetic ----

let count    = 10
let doubled  = $count * 2
let doubled2 = $count + $count
let avg      = ($a + $b) / 2
let mod      = 7 % 3
let neg      = -$count

$count + 1
$count > $doubled / 3
```

Verify the gallery still parses with `bpfman-shell repl --check -f`
after the update.

## Implementation order

1. Plumb grammar changes through the parser (recursive-descent
   levels + AST node + lookahead update).  Parser tests green.
2. Implement evaluator (`evalArithmetic`, negate, dispatch).
   Evaluator tests green.
3. End-to-end REPL test.
4. Docs updates (language.md grammar, new subsection).
5. Emacs mode and gallery updates.  Gallery parses.
6. Final `make test` and `make` sanity.

Commit as a single PR titled along the lines of
`shell: add arithmetic operators (+ - * / %) with unary negation`.
The commit message should restate the scope boundary so readers
reviewing the PR know what was left out on purpose.

## Risks and things to reconfirm at implementation time

- **Tokeniser word-terminator set.**  Confirm whether `+`, `*`,
  `/`, `%` currently terminate WORDs.  If they do, user forms
  like `$x+1` will tokenise as three tokens and the
  whitespace-required rule softens; if they don't, the rule is
  hard.  Plan works either way; the doc sentence changes.
- **Modulo on floats.**  Go's `math.Mod` handles negative
  operands in a way users might not expect
  (`math.Mod(-7, 3) = -1`, not `2`).  Document the chosen
  semantics.  Simpler: forward to `math.Mod` and note it.
- **Negation of a structured value.**  `-$xs` where `$xs` is
  an array.  Must error cleanly at eval time, not panic.
  Test covers it.
- **Integer overflow.**  Not a concern — float64.  Just note it
  if anyone asks: very large arithmetic silently loses
  precision.

## Not-a-design-corner check

Before merging, re-read this plan's "Out of scope" section and
confirm that nothing in the implementation accidentally opens a
door we wanted closed.  Specifically:

- No code path lets `"a" + "b"` succeed.
- No code path treats integers differently from floats in a
  user-visible way.
- No code path adds operators outside the five listed.

If any of these slipped in, either pull them or file a
follow-up plan document.  Don't let the PR grow.
