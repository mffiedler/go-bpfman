# Binding-site syntax

The shell language binds names to values at four sites that share a
structural shape: a name list paired with a value source. This
document fixes the surface syntax across all four so the language
has one binding convention rather than four near-misses.

## Convention

Binding sites use one spelling rule: one name is bare; two or
more names use a parenthesised, whitespace-separated name list.
`def` keeps the parentheses at every arity because they delimit
the command declaration itself, not because the inner rule
changes.

    let x = EXPR                            # single-name bind
    let (a b) = LIST_EXPR                   # destructure into N >= 2 names

    let x <- CMD                            # single-name bind to primary
    let (rc x) <- CMD                       # tuple-bind: envelope + primary
    guard x <- CMD                          # single-name halt-on-fail
    guard (rc x) <- CMD                     # tuple-bind halt-on-fail

    foreach x in LIST { BODY }              # single-var iteration
    foreach (a b) in LIST { BODY }          # destructuring iteration

    def f() { BODY }                        # zero-arity command
    def f(a) { BODY }                       # one-argument command
    def f(a b c) { BODY }                   # multi-argument command

At bind and foreach sites, a name is an identifier accepted by
`IsIdent` (see GRAMMAR.md for the exact predicate), or `_` for a
discard slot. In `def` parameter lists, `_` is just an ordinary
identifier (the `def` parameter list uses `Identifier`, not
`Name`). The parens are load-bearing: they delimit the name list
from the surrounding grammar. Inside the parens, names are
separated by whitespace; the language has no comma form at
binding sites.

## Grammar

    Name              = Identifier | '_' .
    DestructureTarget = '(' Name Name { Name } ')' .
    NameList          = Name | DestructureTarget .
    BindTarget        = Name | '(' Name Name ')' .
    DefParamList      = '(' [ Identifier { Identifier } ] ')' .

    LetStmt           = 'let' Identifier '=' Expression
                      | 'let' DestructureTarget '=' Expression .
    BindStmt          = ('let' | 'guard') BindTarget '<-' BindRHS .
    ForEachStmt       = 'foreach' NameList 'in' Expression Block .
    DefStmt           = 'def' Identifier DefParamList Block .

`DestructureTarget` is the named N >= 2 form shared between let-
destructure and foreach destructuring. `BindTarget`'s tuple shape
is kept inline because it is structurally fixed at two slots and
does not share the variable-length pattern.

Structural check baked into the grammar:

- bind tuple arity is fixed at two by the `BindTarget`
  production.

Semantic checks applied at parse time, on top of the grammar:

- all-underscore multi-name groups are rejected;
- duplicate real names in a single group are rejected (`_` is
  exempt because it does not bind);
- `def` parameters use `Identifier`, not `Name`, so `_` in
  `def f(_)` is an ordinary parameter name and the duplicate-name
  rule applies if it appears twice.

## Per-site contract

`let (NAMES) = EXPR`, where `len(NAMES) >= 2`, destructures
EXPR's value. EXPR is a single Expression and must evaluate to
a list of length `len(NAMES)`; each name binds to its positional
element. Mismatched length or a non-list value is a runtime
error cited at the let statement.

The RHS uses the existing expression grammar without any new
list-construction shape. Parens are grouping (one Expression
inside); list literals `[a b c]` are list construction. The
natural form for a literal list RHS is therefore:

    let (a b) = [$foo.a $bar.b]

Not:

    let (a b) = ($foo.a $bar.b)

The second form fails to parse because the inner content of the
parens is two adjacent VarRefs, which the expression parser
rejects as "unexpected token after expression". This asymmetry
is deliberate: `(EXPR)` stays grouping, `[ELEM ELEM ...]` stays
list construction, and parens are not overloaded with a
tuple-constructor meaning.

`let (RC PRIM) <- CMD` and `guard (RC PRIM) <- CMD` run CMD and
bind two slots: RC receives the result envelope, PRIM receives
the primary value. The tuple form has exactly two slots by
grammar (`BindTarget = Name | '(' Name Name ')'`). This is a
special binding form for "envelope plus primary", not general
N-ary destructuring. The guard form halts the script on a non-ok
envelope, the let form does not.

`foreach (NAMES) in LIST { BODY }`, where `len(NAMES) >= 2`,
iterates LIST and destructures each element. The current element
must be a list of length `len(NAMES)` and each name binds to its
position for the body's duration. Mismatched length or a
non-list element is a runtime error cited at the foreach
statement so the failure points at the consumer, not at the
data. Single-var foreach (`foreach x in LIST`) never
destructures: the loop variable carries the element through,
list or not. This is the deliberate guard against a single
variable accidentally destructuring because the value happened
to be list-shaped.

`def f(PARAMS) { BODY }` declares a user command. The arity of the
parameter list is the function's arity; call sites must pass
exactly that many arguments. Zero-arity `def f() { BODY }` is
allowed and is the common case for utilities that work from
session state without arguments.

## Discard slot

A single underscore `_` in any slot accepts the value and binds
nothing observable. Multiple underscores in one multi-name binding
are allowed:

    let (_ x) <- cmd                        # keep primary, drop envelope
    foreach (_ b) in pairs { print $b }     # iterate, ignore first element
    let (a _ c) = triple                    # drop the middle

A multi-name binding consisting entirely of underscores is
rejected at parse time: at least one slot must establish a real
binding. The rejected forms add nothing that dropping the binding
entirely would not express.

    let (_ _) = pair                        # rejected
    guard (_ _) <- cmd                      # rejected
    foreach (_ _) in pairs { print "tick" } # rejected

Single-name `foreach _ in LIST { ... }` is exempt: it is the
iterate-for-side-effects idiom that other languages (Python `for
_ in range(N)`, Go `for range xs`, Rust `for _ in 0..n`) codify,
and the body runs once per element regardless of binding.
Single-name `let _ <- CMD` is allowed for the same side-effect
case (run CMD, ignore the primary value). Single-name `let _ =
EXPR` is also allowed for uniformity: `_` is a discard target at
binding sites. It is usually stylistically redundant, but not a
parse error.

## Duplicate names

Duplicate real names in a single binding group are rejected at
parse time. `_` is exempt from this rule because it does not bind.

    let (a a) = pair                        # rejected: duplicate "a"
    foreach (x x) in pairs { ... }          # rejected: duplicate "x"
    def f(a a) { ... }                      # rejected: duplicate "a"
    let (_ _ x) = triple                    # allowed: only "x" binds

The rule prevents binding syntax from quietly becoming a way to
assert shape rather than introduce names. If shape assertions are
ever wanted, they should be explicit rather than smuggled through
a destructuring bind.

## Why words inside the parens

The shell's command-argument grammar is whitespace-separated. List
literals (`[100 200 300]`) follow the same rule. Inside parens at
a binding site, the same rule applies: tokens are separated by
whitespace and there is no comma form to strip from glued
identifiers.

The shell already wants a "name group inside parens" shape for
tuple-bind and def-params. Earlier sketches used commas here,
mostly by reflex from Algol-family languages, but that never
aligned with the shell's own argument grammar. Words inside the
parens unifies the language around a single convention:
whitespace separates names; parens delimit groups where the
surrounding grammar would otherwise be ambiguous.

## Why parens around multi-name groups

`let rc x <- cmd` is ambiguous: which is rc, which is the primary?
`def f a b c { ... }` reads as a command line rather than a
parameter declaration. `foreach a b in xs { ... }` is unambiguous
on its own (the keyword `in` terminates the name list), but a
grammar that introduces parens only when N >= 2 in some sites and
never in others has two shapes for the same idea. The
parenthesised form is uniform and costs one token pair per N >= 2
binding; the single-name form omits parens for the common case.
`def` keeps the parens at every arity because the surrounding
command grammar otherwise consumes the parameters as command
arguments.

## Non-goals

- Implicit single-name parens at non-def sites. `let (x) = EXPR`
  is rejected; the unparenthesised `let x = EXPR` is the canonical
  single-name spelling. `def f(a)` is the only single-name form
  that keeps parens.
- Nested destructuring. `let ((a b) c) = LIST_OF_PAIR_AND_SCALAR`
  is not in scope. Add when a consumer appears.
- Variable-arity patterns. `let (a *rest) = LIST` is not in scope.
  Add when a consumer appears.
- Trailing-comma tolerance. There is no comma form at all, so
  there is nothing to trail.
- Parens-as-list-constructor on the RHS. `let (a b) = ($x $y)`
  is not the spelling; use `let (a b) = [$x $y]`. Parens stay
  grouping (one Expression inside); list literals stay list
  construction. The let-destructure LHS uses parens because they
  delimit a name list, not because they introduce a matching
  parens-as-tuple form on the RHS.

## Migration from comma-separated binding sites

The current parser accepts comma-separated multi-name binding
sites (`let (rc, x) <- cmd`, `foreach a, b in xs`, `def f(a,
b)`). The redesign deliberately does not preserve those forms;
the new grammar has no comma form at any binding site and no
trailing-comma tolerance.

    let (rc, x) <- cmd        # current        # rejected after
    let (rc x) <- cmd         #                # new

    foreach a, b in xs { }    # current        # rejected after
    foreach (a b) in xs { }   #                # new

    def f(a, b) { }           # current        # rejected after
    def f(a b) { }            #                # new

The migration is mechanical and will land alongside the parser
change as a single corpus sweep; there is no period during which
both spellings parse.
