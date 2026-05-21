# bpfman-shell grammar

This document is the reference grammar for the language driven by
`cmd/bpfman-shell`. The parser source in `parse.go`, `token.go`,
`expr.go`, and `matches.go` is the load-bearing ground truth; this
document is the human-readable shape of what that code accepts.

**Implementation status.** The SCOPE-DESIGN.md ten-commit plan
has landed: `EventuallyStmt` / `EventuallyCommand` are live,
`retry` / `until` are reserved tombstone keywords that emit
targeted diagnostics, the runtime and the checker both push a
fresh frame on each block-shaped construct, and the typed
retryable/fatal error layer classifies attempt failures
uniformly via `errors.As`. `return` is no longer a tombstone:
SCOPE-DESIGN.md Section 9's value-returning def form has landed,
so `ReturnStmt` is a real statement (see the production below).
The single-name `let _ = EXPR` rejection and the restriction of
`break` / `continue` to `foreach` bodies are still tracked in
`GRAMMAR-FOLLOW-UP.md` as small follow-on items.

## Scope

The doc describes parsing: what tokens the lexer emits, what
productions the parser recognises, where the binding sites live,
and how operator precedence is layered. Evaluation semantics and
runtime errors are mentioned only where they affect what is
parseable; the full runtime semantics live in `expr.go`'s
evaluator.

## Notation

Productions use W3C-style EBNF:

    NonTerminal  = expansion .
    'literal'         terminal token
    [ X ]             X is optional
    { X }             X repeats zero or more times
    X | Y             alternation
    ( X Y )           grouping

Terminal tokens are written `'as'` (single-quoted) when they are
literal source text, or `TokenName` (CamelCase, no quotes) when
they refer to a token kind the lexer emits. Recursive
productions are spelled directly; productions in this document
favour readability over conformance to any specific
recursive-descent style.

Some productions constrain a Word token by its text -- "`Word`
with text in `S`" or "`Word` with text not in `S`" where `S` is
a named set. These constraints are not directly executable EBNF;
a Tree-sitter derivation should encode them as either separate
keyword tokens or as Word with a text-equality check at the
matching production.

## Lexer design notes

**Design principle.** This is an argv-first shell language with
expression islands, not an expression language with command
escape hatches. The lexer is command-biased: it preserves most
CLI-shaped text as single Word tokens so paths, flags, key=value
pairs, comma-separated program specs, and colon-qualified
arguments can be written without quoting. Expression syntax is
opt-in at specific parser sites: `let X = EXPR`, `assert` /
`require` expression form, parenthesised command arguments,
interpolation bodies, list literals, `if` / `elif` conditions,
`foreach ... in` operands, and thread pipelines. Everywhere
else, Word text stays opaque.

**Ergonomic goal.** A `bpfman ...` command copied from ordinary
shell history can usually be pasted after `let NAME <-`,
`guard NAME <-`, `defer`, or inside a block with little or no
rewriting. Worked example from the e2e corpus:

    guard loaded <- bpfman program load file \
        --path testdata/bpf/multi_prog_tcx_counter.bpf.o \
        --programs tcx:mtcx_a,tcx:mtcx_b,tcx:mtcx_c \
        -g "weight_a=0x${u64le $weight_a}" \
        -g "weight_b=0x${u64le $weight_b}" \
        -g "weight_c=0x${u64le $weight_c}"

At command position these arguments lex as boring Word tokens:
`--path`, the bytecode path, `--programs`, the comma-separated
program-spec list, `-g`, then an InterpString containing
`weight_a=0x${u64le $weight_a}`. A "clean" lexer that split `:`,
`,`, `/`, `.`, `=`, `-` everywhere would force every CLI-shaped
argument to be quoted; that is the wrong direction for an
argv-first language and would break the paste-from-shell
property the corpus relies on.

**Whitespace-is-significant for operators is deliberate.** Where
the language needs an operator that is also common in command
arguments (`-`, `/`, `<`, `>`, `=`, the comparison ops), the
operator is recognised only at documented parser sites, and
usually only when it appears as a standalone Word. This keeps
copied command lines stable and pushes the small amount of
extra ceremony onto expression-heavy code rather than onto
command invocations.

**Tree-sitter guidance.** Preserve the same surface convenience
in command positions while treating the documented parser sites
(expression operators, binding sigils, assignment) as contextual
reinterpretations of Word tokens. Concrete consequences worth
knowing:

- Commas are not a separator at any binding site. The grammar
  uses whitespace between names (`def f(a b)`, `let (rc x) <-`,
  `foreach (a b) in ...`). Because the lexer does not split on
  `,`, an old-style `def f(a, b)` would lex as `Word("a,")
  Word("b")`; every binding-site parser (`parseBindTargetName`,
  `parseForEachNameToken`, `parseDefParams`) rejects any token
  whose text contains a comma with an explicit "comma is not a
  separator" diagnostic so the migration from the previous
  comma-separated spelling fails loudly.

- `+`, `*`, and `%` are Delimiter Words emitted as single-char
  tokens regardless of surrounding whitespace. `-` and `/` are
  Compound Word constituents, so `1+2` and `1 * 2` parse as
  arithmetic but `1/2` lexes as a single Word and does not.
  Operators that need whitespace to emit standalone are called
  out at the relevant production.

- Assignment, bind, and thread sigils (`=`, `<-`, `|>`) only
  emit as their own tokens at top-level dispatch. `let x=1` and
  `let x<-cmd` (bareword LHS) lex `x=1` / `x<-cmd` as single
  Compound Words and do not parse as their intended forms. A
  VarRef LHS does not have this problem: `$x|>jq` correctly
  splits because the VarRef lexer stops at the sigil character.

The grammar productions in this document describe the intended
surface syntax; the prose notes call out the places where the
current parser is more permissive or more
tokenisation-dependent. A Tree-sitter implementer should model
that surface syntax and surface the "needs whitespace"
constraints to the user via syntax-error highlighting, rather
than mirror the parser's post-tokenisation normalisation.

## Tokens

The lexer produces the following token kinds. Bracket and brace
characters (`(`, `)`, `[`, `]`, `{`, `}`) are emitted as
single-character `Word` tokens; their meaning is determined by the
parsing context.

  Word           Bare identifier, command name, flag, path,
                 operator character, or any contiguous run of
                 non-boundary bytes. Two shapes emit at the lexer
                 level:
                  - **Delimiter Word**: a single-character token
                    whose text is one of `(`, `)`, `[`, `]`,
                    `{`, `}`, `+`, `*`, `%`. The lexer emits
                    each of these as its own Word token
                    regardless of surrounding whitespace.
                  - **Compound Word**: a multi-character token
                    produced by the word lexer. A Compound Word
                    extends until the next boundary character.
                    In shell mode (the default) the boundary
                    set is: whitespace (space, tab, CR),
                    newline, `;`, `$`, `"`, `'`, `#`, the nine
                    Delimiter Word characters listed above.
                    `-`, `/`, `.`, letters, digits, `_`, `,`,
                    `:`, `=`, `<`, `>`, `|`, `!`, and other
                    punctuation are Compound Word constituents
                    in shell mode. (See TokeniseStrict below.)

  Assign         A standalone `=` token. The lexer emits Assign
                 when the main lexer dispatch loop reaches `=`
                 directly; if `=` is encountered while scanning
                 a Compound Word, it remains part of that Word.
                 `==` at the same dispatch-loop position lexes
                 as a single Compound Word with text `==`, not
                 as Assign followed by Assign.

                 In practice this means `let x = 1` emits
                 Assign, while `let x=1` lexes `x=1` as a
                 single Compound Word and does not parse as a
                 let assignment.

  VarRef         `$name` or `$name.field.path[index]`. The
                 variable name and the optional path are recorded
                 on the token. Index forms `[N]` and `[$ident]`
                 are part of the path syntax.

  Quoted         A single- or double-quoted string with the
                 delimiters stripped. `$` inside single quotes is
                 literal; double-quoted strings with no `${...}`
                 interpolation also tokenise as Quoted.

  AdapterRef    `adapter:$var` or `adapter:$var.path`, where
                 `adapter` is one of a registered set (currently
                 just `file`). Carries the adapter name, the
                 variable name, and the optional path.

  Sep            Newline or `;`. The lexer emits these
                 unconditionally; parser-level token collectors
                 decide where a Sep terminates a statement and
                 where it is transparent, such as list literals,
                 def parameter lists, and list literals appearing
                 in a bind RHS. Any number of Sep tokens may
                 appear between two statements.

  Thread         `|>` at a top-level token boundary. The lexer
                 emits Thread when `|>` is encountered from
                 top-level dispatch, i.e. not while scanning a
                 Compound Word. This includes the position
                 immediately after a non-Word token has ended,
                 because the main loop returns to top-level
                 dispatch there. `$x|>jq` therefore emits
                 VarRef, Thread, Word (three tokens) even
                 though no whitespace separates them. Inside a
                 Compound Word, `|` and `>` remain
                 constituents, so `a|>b` (bareword LHS) lexes
                 as a single Word.

  Bind           `<-` at a top-level token boundary. The lexer
                 emits Bind under the same rule as Thread:
                 encountered from top-level dispatch, not from
                 inside a Compound Word. `<-` after a bareword
                 stays glued (`let x<-cmd` lexes `x<-cmd` as
                 one Word), but `<-` after a closing token
                 such as `}` or after whitespace emits as Bind.

  InterpString   A double-quoted string containing one or more
                 `${...}` interpolation segments. The token
                 carries the alternation of literal text segments
                 and raw expression text; `parsePrimary` tokenises
                 and parses each interpolation body when
                 constructing the `InterpStringExpr`.

Comments are lexer-stripped before tokenisation: `#` outside a
quoted string starts a comment that runs to end of line. The
strip preserves source offsets so error spans remain accurate.

A backslash immediately followed by a newline (or `\r\n`) is a
line continuation: the backslash and line ending are consumed by
the lexer, the continuation does not emit a Sep token, and the
two adjacent lines tokenise as one logical line. The
continuation is recognised only at top-level positions outside
quoted strings; inside quoted strings backslash handling is
governed by the quoted-string lexer rather than by the line
continuation rule.

Lexical predicates applied to Compound Word text:

  Identifier     A Compound Word whose first rune is `_` or a
                 `unicode.IsLetter`, and whose remaining runes
                 are `_`, `unicode.IsLetter`, or
                 `unicode.IsDigit`. In ASCII terms this is
                 roughly `[A-Za-z_][A-Za-z0-9_]*`, but the
                 matcher is unicode-aware in full. Implemented
                 by `IsIdent` in `token.go`.

  IntegerLiteral A Compound Word whose text matches a base-10
                 integer (digits only). The VarRef path
                 `[index]` slot accepts IntegerLiteral or
                 `$Identifier` and rejects everything else at
                 the lexer level via `lexPathIndex`.

A second lexing mode, `TokeniseStrict`, treats `-` and `/` as
single-character operators in addition to the shared splits.
Strict mode is callable as `TokeniseStrict` and exercised by
tests; the rest of the parser uses `Tokenise`. Paths and flags
appearing inside strict mode must be quoted.

## Contextual keywords

The grammar has no hard lexical keywords. Every reserved word is
lexed as a Word token; a word becomes a keyword only at parser
sites that explicitly test the Word's text. Outside those sites,
the same byte sequence remains an ordinary Compound Word (and so
may appear as a bareword command name, argument, or string
content). The categories below enumerate every text the parser
tests by name.

Statement-leading reserved words (tested in `parseStmt`):

    let guard foreach if eventually defer def return break
    continue assert require

Branch-keyword words (tested in `parseIfStmt` after a block's
closing `}`):

    elif else

Removed tombstone words (reserved only to produce a targeted
diagnostic for old scripts; never valid as statement forms):

    retry until

`retry` and `until` are reserved tombstones after the
SCOPE-DESIGN rework removes the construct; the parser emits
"retry is removed; use eventually" rather than letting the
words parse as ordinary command names.

`return` was a tombstone until the value-returning def form
landed; it is now a real statement keyword tested at
`parseStmt` and routed to `parseReturnStmt`. See the
`ReturnStmt` section below for the grammar and SCOPE-DESIGN.md
Section 9 for the semantics.

Inside `eventually`, the keyword words `timeout` and
`interval` are recognised at the leading statement form
only (between `eventually` and the block). Outside that
position they remain ordinary Words. `timeout` was
previously a unary expression form attached to the old
`retry ... until` clause; that role is gone.

ForEach separator word (tested in `parseForEachStmt` between the
name list and the iteration expression):

    in

Expression operator words (tested in the precedence ladder):

    or and not

Expression-leading words (recognised by `leadsExpression` so the
statement dispatcher routes to `ExprStmt`):

    ( not not-empty true false

Expression primary words (tested in `parseTerm` and
`parsePredicate`):

    not-empty true false

Assert-command predicate words (tested in `assertTakesExprForm`
to route into the command-form path):

    ok fail path-exists contains nil present missing empty

Predicate semantics:

  - `ok COMMAND`             command exits with ok envelope
  - `fail COMMAND`           command exits with non-ok envelope
  - `path-exists FILE`       filesystem path exists (renamed from
                             the previous `path exists FILE`
                             two-arg form; reserves the word
                             "path" so it never carries both
                             filesystem and object-path meaning)
  - `contains HAYSTACK NEEDLE`
                             HAYSTACK string contains NEEDLE
  - `nil $X.field`           path resolves and terminal value is
                             JSON null (strict; a missing path
                             fails)
  - `present $X.field`       path resolves; terminal value may
                             be anything including JSON null
  - `missing $X.field`       path does not resolve in the value
                             tree
  - `empty $X.field`         path resolves and terminal value is
                             "" / [] / {} (an empty string, list,
                             or map; a missing or null terminal
                             fails)

`present`, `missing`, `nil`, and `empty` all accept either a
$-prefixed variable expression or a bareword variable-name with
optional dotted path; the verb dispatch reads the underlying
LookupClass from the runtime path walker so the three states
(absent / null / value) are distinguishable.

Match-tail word (tested by `parseCommandStmt`):

    matches

Adapter prefix words (registered in `token.go`'s
`adapterPrefixes`):

    file

Pure-builtin names (registered via `RegisterPureBuiltin`):

    jq                           (registered by the shell package)
    range zip u32le u64le        (registered by cmd/bpfman-shell)

Anywhere outside the contexts above, these texts are ordinary
Words. A Tree-sitter derivation should model them as Word with a
text-equality check at the relevant production, not as separate
token kinds.

## Statement separators

    SepSeq           = Sep { Sep } .

SepSeq (one-or-more Sep) is the separator between two
independent statements. Any number of Sep tokens may appear at
the start of a Program or Block; any number may appear at the
end; two consecutive statements must be separated by at least
one Sep.

Branch continuations after a `}` are not separate statements.
`elif` and `else` are consumed by `IfStmt`. `IfStmt` skips
any number of Sep tokens between the closing `}` and the
trailing keyword, so the global SepSeq rule does not apply.
`eventually` has no trailing control clause: its `timeout`
and `interval` keywords appear in the leading statement
form, before the block.

## Program

A program is a (possibly empty) sequence of statements with
optional Sep tokens between, before, and after them.

    Program        = { Sep } [ Statement { SepSeq Statement } { Sep } ] .
    Statement      = LetStmt              (* let X = EXPR *)
                   | LetDestructureStmt   (* let (a b) = EXPR *)
                   | BindStmt             (* let X <- CMD, guard X <- CMD *)
                   | ForEachStmt
                   | IfStmt
                   | EventuallyStmt
                   | DeferStmt
                   | DefStmt
                   | ReturnStmt
                   | BreakStmt
                   | ContinueStmt
                   | AssertStmt
                   | AssertCommand
                   | ExprStmt
                   | CommandStmt
                   .

`LetStmt` covers only the single-name `let X = EXPR` form;
`let (NAMES) = EXPR` produces a `LetDestructureStmt`. Both
`let X <- CMD` and `guard X <- CMD` produce a `BindStmt`
(distinguished by the `Guard` flag). `AssertStmt` and `AssertCommand` share the
keywords `assert` / `require` and disambiguate at statement
dispatch; `AssertCommand` is structurally a `CommandStmt` whose
head is the keyword.

Statement dispatch is decided by the first token of the
statement. Reserved words `let`, `guard`, `foreach`, `if`,
`eventually`, `defer`, `def`, `return`, `break`, `continue`
route to their named parsers. `assert` and `require` route
to the assert parser, which chooses between `AssertStmt`,
`AssertCommand` predicate form, and `AssertCommand` matches
form using the disambiguation rule in the AssertStmt
section. Tombstone words `retry` and `until` at statement
position raise targeted parse errors rather than routing as
command names. A statement-leading VarRef, Quoted,
InterpString, AdapterRef, or one of the Words `(`, `not`,
`not-empty`, `true`, `false` routes to ExprStmt. Anything
else routes to CommandStmt. `source` has no dedicated
production; it parses as a CommandStmt and is recognised by
the command dispatcher in `repl/loop.go`, which evaluates
the referenced file under the module-scope rules in
SCOPE-DESIGN.md Section 5.

A bare `{` or `}` at statement position is a parse error: the
parser has already routed block-introducing keywords elsewhere,
and a stray brace at the top level signals an incomplete
construct above.

## Statements

### Block

A block is a brace-delimited statement sequence used by `if`,
`elif`, `else`, `foreach`, `eventually`, and `def`.

    Block          = '{' { Sep } [ Statement { SepSeq Statement } { Sep } ] '}' .

Statements inside the block follow the same dispatch rules as
top-level statements. An empty block `{ }` is allowed.

`matches { ... }` uses braces too, but its body is not a
statement block; it is parsed by the matcher-entry grammar in
`matches.go`.

### LetStmt

Two surface forms parse here. The single-name `let X = EXPR`
binds a name to an expression value. The destructure form
`let (NAMES) = EXPR` evaluates EXPR to a list and binds each
positional element to its name in the list.

    LetStmt        = 'let' Identifier '=' Expression
                   | 'let' DestructureTarget '=' Expression .
    DestructureTarget
                   = '(' Name Name { Name } ')' .

In the single-name form, `_` as the bound name is **rejected**
at parse time with "single-name let cannot bind '_'; use a
real name". `_` is consistently a discard slot at every other
binding site (bind, destructure, foreach); the single-name
let is the only place that historically bound `_` as an
ordinary name, and that asymmetry is removed. Force-evaluation
for side effects belongs in bind / guard / a bare command,
not in `let _ = ...`. The destructure form requires at least
two names; single-name parens (`let (x) = EXPR`) are rejected
because the binding design refuses implicit single-name parens
at non-def sites. Names are whitespace-separated; tokens whose
text contains `,` are rejected explicitly. Duplicate real names
are rejected; `_` is exempt. An all-underscore destructure list
is rejected at parse time.

Runtime behaviour of the destructure form: EXPR must evaluate to
a list of length `len(NAMES)`. Each non-`_` name binds to the
positional element. A list of the wrong length or a non-list
value is a runtime error cited at the let statement.

Examples:

    let x = 5
    let m = $prog |> jq ".maps"
    let (a b) = [$foo.a $bar.b]
    let (a _ c) = $triple

### BindStmt

`let X <- CMD` and `guard X <- CMD` both produce a `BindStmt`.
The keyword chooses whether a non-ok result envelope halts the
script (`guard`) or carries through normally (`let`).

    BindStmt       = ('let' | 'guard') BindTarget '<-' BindRHS .
    BindTarget     = Name
                   | '(' Name Name ')' .
    BindRHS        = CommandForm
                   | ForEachStmt           (* bind-collect *)
                   | EventuallyCommand .

`CommandForm` is the shared command-shape production; its
definition is in the CommandStmt section. `Name` is the shared
binding-site name production; its definition is in the Binding
sites section.

The tuple target accepts exactly two names, separated by
whitespace; there is no comma form. The first target receives the
result envelope; the second receives the primary value. Arities
other than two are rejected at parse time. `_` is an accepted
name at either slot, but `(_ _)` is rejected at parse time as
"tuple bind cannot discard both slots". A token whose text
contains `,` (which the lexer does not split on its own) is
rejected explicitly with "comma is not a separator".

The `BindRHS` has three shapes:

1. **Command form.** The default path. `parseBindRHS` reads the
   command's tokens up to the next Sep or block marker via
   `takeBindRHSTokens` and produces a `CommandStmt`. At evaluation
   time, the command head is looked up against the session's def
   table before any external dispatch: a def-named head routes
   through `callDefAsBind` so the def's `return EXPR` value
   becomes the bind primary; the same precedence applies at
   command-statement position so a def name never reaches the
   external dispatch path. The same def precedence applies inside
   the bind-collect form's producer.

2. **Bind-collect form.** When the RHS begins with the keyword
   `foreach`, the parser delegates to `parseForEachStmt`. The
   foreach body must end with a CommandStmt; that command's
   primary value is collected into a list bound to the BindStmt
   target. An empty body or a non-CommandStmt last statement is
   rejected at parse time.

3. **Eventually form.** When the RHS begins with the keyword
   `eventually`, the parser delegates to a dedicated
   `parseEventuallyBindRHS`. The generic `takeBindRHSTokens`
   path cannot handle the trailing block (it stops at `{`),
   so `eventually` requires the same special-case routing as
   `foreach`. The construct's structured result becomes the
   bound primary value; see `EventuallyCommand` below and
   SCOPE-DESIGN.md Section 3.4 for the result shape.

Examples:

    let pid <- bpfman program load file --path foo.o
    guard pid <- bpfman program load file --path foo.o
    let (rc p) <- bpfman program get $pid
    let (_ p) <- bpfman program get $pid
    guard _ <- touch "${sentinel}.1"

    guard links <- foreach prio in $priorities {
        bpfman link attach tc -i $iface -d ingress -p $prio $pid
    }
    guard links <- foreach _ in (range 10) {
        bpfman link attach xdp -i $iface generic 50 $pid
    }

### ForEachStmt

    ForEachStmt    = 'foreach' NameList 'in' Expression Block .
    NameList       = Name
                   | '(' Name Name { Name } ')' .

The Expression between `in` and the opening `{` is collected by
`takeUntilOpenBrace`, which skips Sep tokens and stops at the
next `{`; the collected token stream is then parsed as an
Expression. `_` is an accepted name at any slot in either form.

The single-var form `foreach NAME in LIST` carries the loop
element through to NAME verbatim. The parenthesised multi-var
form destructures each list element as a sub-list of length
`len(NameList)`. Parens are required for multi-var because a
bare `foreach a b in xs` reads as a command-shaped name list
rather than a binding; a single-name parenthesised form `foreach
(x) in xs` is rejected to keep the spelling unambiguous (the
single-var form omits the parens). Duplicate real names are
rejected; `_` is exempt. An all-underscore parenthesised list is
rejected as "foreach: all loop variables are '_'; at least one
must bind"; the single-var `foreach _ in xs` is the
iterate-for-side-effects idiom and is accepted.

Newlines and semicolons inside the parens are transparent so a
long destructure list can wrap.

Examples:

    foreach prog in $progs { print $prog.name }
    foreach (prio po) in (zip $priorities $proceed_ons) { ... }
    foreach _ in (range 10) { print "tick" }

### IfStmt

    IfStmt         = 'if' Expression Block
                     { 'elif' Expression Block }
                     [ 'else' Block ] .

The Expression between the keyword and the opening `{` is
collected by `parseCondition`, which skips Sep tokens and stops
at the next `{`. Sep tokens between the closing `}` of one
branch and the next `elif` or `else` keyword are skipped.

Example:

    if $count > 10 {
        print "many"
    } elif $count == 0 {
        print "none"
    } else {
        print "some"
    }

### EventuallyStmt and EventuallyCommand

`eventually` retries a body until it succeeds or until a
mandatory timeout elapses. Two syntactic placements share one
operation: a statement form and a bindable command form. See
SCOPE-DESIGN.md Section 3.4 for the full semantics (attempt
frames, retryable/fatal classification, result shape).

    EventuallyStmt = 'eventually' 'timeout' DurationWord
                         [ 'interval' DurationWord ] Block .
    EventuallyCommand
                   = 'eventually' 'timeout' DurationWord
                         [ 'interval' DurationWord ] Block .
    DurationWord   = Word whose text is accepted by
                     time.ParseDuration .

The lexer does not emit a distinct Duration token. The parser
expects a Word at each duration slot and validates its text
with `time.ParseDuration`. Elsewhere, the same text is just
an ordinary Word.

`timeout DUR` is mandatory; an `eventually` without it is a
parse error. `interval DUR` is optional and named; the
default interval is `100ms`. The keywords `timeout` and
`interval` are recognised at this position only.

Examples:

    eventually timeout 5s {
        require path-exists "${ack}.1"
    }

    eventually timeout 10s interval 250ms {
        guard got <- bpfman program get $pid
        assert $got.status.kernel.id == $pid
    }

    let r <- eventually timeout 1s interval 50ms {
        test -f "${ack}.1"
    }
    # r is { ok, timed_out, attempts, elapsed_ms, error,
    #        last_command }

**Bind form result.** The bindable form publishes a structured
value summarising the run:

    {
      ok:           bool
      timed_out:    bool
      attempts:     int
      elapsed_ms:   int
      error:        string-or-nil
      last_command: envelope-or-nil
    }

`error` is the rendered failure message for the last
retryable failure, nil on success. `last_command` is the
captured command envelope `{ ok, code, stdout, stderr }` when
the last retryable failure was command-shaped (ordinary
command, guard, or subprocess exit); nil otherwise. Assertion-
shaped failures (`assert`, `require`) set `error` and leave
`last_command` nil rather than manufacturing a synthetic
envelope. The internal RetryableError taxonomy stays internal
-- callers branch on `ok` and on `last_command`'s presence,
not on which evaluator error class fired. See
SCOPE-DESIGN.md Section 3.4 for the full contract.

**Commands as conditions.** Inside an `eventually` block, an
uncaptured command statement whose envelope is non-ok is a
retryable attempt failure. This is stricter than ordinary
block execution, where a command may run for side effects
and the caller can ignore its envelope. Use
`let rc <- CMD` or `let (rc value) <- CMD` inside the block
when the script wants to inspect and handle the result
itself rather than have it count against the attempt.

**Loop control.** `break` and `continue` are not valid
inside an `eventually` body. `eventually` is not an
iteration construct in the user-language sense; it is a
retrying assertion block whose success boundary is the body
as a whole. See `BreakStmt and ContinueStmt` below.

### DeferStmt

`defer CMD`. The RHS is parsed as a command form; the parser
emits a `DeferStmt` whose child is a `CommandStmt`.

    DeferStmt      = 'defer' CommandForm .

Example:

    defer bpfman program unload $pid

### DefStmt

`def` declares a user command. The parameter list is always
parenthesised, even for zero-arity or single-arity declarations,
because a bare `def f a { ... }` would collide with command
syntax.

    DefStmt        = 'def' Identifier '(' [ ParamList ] ')' Block .
    ParamList      = Identifier { Identifier } .

Parameters are whitespace-separated identifiers; there is no
comma form. Duplicate parameter names are rejected at parse time.
Newlines and semicolons between parameters are allowed so a long
parameter list can wrap. A token whose text contains `,` (which
the lexer does not split on its own) is rejected explicitly with
"comma is not a parameter separator".

`_` qualifies as an Identifier so it may appear as a parameter
name, but unlike bind and foreach positions it is not treated as
a discard slot here: it is an ordinary parameter name, and the
duplicate-name rule rejects two `_` parameters in the same list.

Examples:

    def warn() {
        print "warning"
    }
    def attach(iface prog) {
        bpfman link attach xdp $iface generic 50 $prog
    }

### ReturnStmt

`return EXPR` is the value-publishing exit from a def body. The
expression is mandatory; bare `return` is rejected at parse time
so the construct stays uniformly value-publishing. A bareword
early-exit form earns its own keyword if it is ever wanted.

    ReturnStmt     = 'return' Expression .

`return` is only meaningful inside a def body. The static checker
rejects a `return` reached without an enclosing def with "return
outside a def body"; the runtime carries a safety net at
evalProgramBody for any path the checker does not see (dynamic
source, future evaluator paths).

At a def's call site:

- **Command-form** invocation (`my_def args`) -- the expression is
  evaluated for its side effects and the value is discarded. The
  early exit itself is the only observable effect:

      def f() {
          if $skip { return 0 }
          do-the-work
      }

- **Bind-position** invocation (`let p <- my_def args`,
  `guard p <- my_def args`, `let (rc p) <- my_def args`) -- the
  evaluated value becomes the BindResult's Primary. A def that
  runs to completion without `return` produces Primary equal to
  `ValueFromEnvelope(Rc)`, matching the no-payload command-bind
  family (exec, bpftool, wait).

Defer interaction (SCOPE-DESIGN.md Section 9):

1. evaluate `return EXPR` in the call frame; obtain the Value;
2. stash the Value -- it is now independent of the call frame;
3. unwind def-local defers in LIFO order while the call frame
   is still live so deferred `print $captured` and similar
   commands resolve against the body's bindings;
4. pop the call frame;
5. emit `BindResult{Rc, Primary: stashed}` to the bind-position
   caller, or discard at command-form position.

A defer that fires during step 3 and reports a non-ok envelope
flips Rc.OK to false on the bind-position path even when
`return EXPR` itself evaluated cleanly. The single-name bind
family (`let p <- f`) discards the envelope; `let (rc p) <- f`
exposes the cleanup outcome through `$rc.ok`; `guard p <- f`
halts via GuardFailure when cleanup failed. Defs are not a
special case; this is the existing bind family contract.

The call sites for value-returning defs uniformly compose with
the existing bind-target shapes. Multiple values fall out of
the existing list / destructure story:

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

The single-name bind form intentionally discards the envelope,
matching the existing command-bind family. Scripts that require
successful cleanup should call the def via `guard` or via the
tuple bind and inspect `$rc.ok`; value-returning defs get no
special exemption.

### BreakStmt and ContinueStmt

    BreakStmt      = 'break' .
    ContinueStmt   = 'continue' .

Both keywords take no arguments; trailing tokens before the next
separator are rejected at parse time.

`break` and `continue` are valid only inside `foreach`. Using
either inside `eventually`, `if`, `def`, a sourced file, or
at top level is a static/runtime error. `eventually`
deliberately does not let `continue` mean "next attempt": the
attempt boundary is the body succeeding or producing a
retryable failure, not a user-driven control transfer.

### AssertStmt and AssertCommand

`assert` and `require` have two surface forms: an expression
form that asserts the truth of an expression, and a command form
that asserts a property of a command invocation or a value's
structure.

    AssertStmt              = AssertKeyword Expression .
    AssertCommand           = AssertCommandPredicateForm
                            | AssertCommandMatchesForm .
    AssertCommandPredicateForm
                            = AssertKeyword [ 'not' ]
                              AssertCommandPredicate { CommandArg } .
    AssertCommandMatchesForm
                            = AssertKeyword { CommandArg } MatchesBlock .
    AssertCommandPredicate  = 'ok' | 'fail' | 'path-exists'
                            | 'contains' | 'nil' | 'present'
                            | 'missing' | 'empty' .
    AssertKeyword           = 'assert' | 'require' .

Disambiguation at statement dispatch: a statement that begins
with `assert` or `require` parses as `AssertCommandPredicateForm`
when the next non-Sep token (after a skippable `not`) is one of
the `AssertCommandPredicate` words. Otherwise, if the statement
has a trailing `MatchesBlock` (the bare word `matches`
immediately before a top-level `{`), it parses as
`AssertCommandMatchesForm`. Otherwise it parses as `AssertStmt`.

The `{ CommandArg }` repetition in `AssertCommandPredicateForm`
is unconstrained at parse time: `assert ok` with no trailing
command is syntactically valid but is rejected (or no-op'd) at
evaluation time. Each predicate (`ok`, `fail`, `path`,
`contains`, `nil`) imposes its own arity expectation on the
following arguments at runtime rather than in this grammar.

The `{ CommandArg }` in `AssertCommandMatchesForm` is also
unconstrained at parse time: `assert matches { ... }` with no
intervening CommandArg parses (the matches block carries the
whole shape). This is deliberate; runtime semantics decide
whether the LHS-less form is meaningful.

Examples:

    assert $count > 0                           # AssertStmt
    require not-empty $links                    # AssertStmt
    assert ok bpfman program get $pid           # AssertCommandPredicateForm
    require not ok bpfman program get $pid      # AssertCommandPredicateForm
    assert $output matches { ... }              # AssertCommandMatchesForm

### ExprStmt

A statement whose first significant token unambiguously starts an
expression: a VarRef, Quoted, InterpString, AdapterRef, `(`,
`not`, `not-empty`, `true`, or `false`. Everything not routed to a
keyword statement and not unambiguously expression-leading falls
through to CommandStmt.

    ExprStmt       = Expression .

Examples:

    $x > 10
    $count == 5

`takeStmtTokens` stops at the next Sep or `{`, so a trailing
`matches { ... }` does not reach the expression statement path;
it only attaches via the CommandStmt grammar (see below).

### CommandStmt

Anything that does not match a keyword statement and does not
unambiguously start an expression is parsed as a command
invocation.

    CommandStmt     = CommandForm .
    CommandForm     = CommandHead { CommandArg } [ MatchesBlock ] .
    CommandHead     = Word .
    CommandArg      = Word
                    | Quoted
                    | VarRef
                    | AdapterRef
                    | InterpString
                    | CommandParenArg
                    | CommandListArg .
    CommandParenArg = '(' Expression ')' .
    CommandListArg  = ListExpr .

`CommandParenArg` and `CommandListArg` are named separately from
`ParenExpr` and `ListExpr` because command argument parsing is
not the general expression entry point; the dedicated arg
parser (`parseCommandArgs`) recognises these grouped forms
explicitly via `findMatchingParen` and `findMatchingBracket`
before falling back to plain primary tokens.
`CommandParenArg`'s inner Expression is parsed at the OrExpr
level. `CommandListArg`'s body is the same `ListExpr`
production used in expression position.

**Termination.** A `CommandForm` consumes tokens until one of
the following:

- A `Sep` token at top-level depth.
- A `}` token at top-level depth (closing an enclosing Block).
- A `{` token at top-level depth, unless the immediately
  preceding token is the bare word `matches`, in which case the
  `{` opens a trailing `MatchesBlock` and is part of the same
  CommandForm.

"Top-level depth" here means: outside any open
`CommandParenArg` and outside any open `CommandListArg`. The
matched-paren and matched-bracket consumption inside those
forms handles their own balancing.

`CommandForm` is also the production used on bind RHS and defer
RHS; see `BindStmt` and `DeferStmt`. On those sites the
termination rule is the same.

**Language rule versus parser accident: command head.**

- *Language rule.* `CommandHead = Word`. A command statement
  begins with a bare word naming the command.

- *Parser accident.* `parseCommandArgs` builds `CommandStmt.Args`
  from any sequence of CommandArg forms, so a leading `[` at
  statement position (which is not in the expression-leading
  set) routes to `CommandStmt` and produces a single-element
  `Args` whose only element is a `ListExpr`. That construct
  parses but has no useful runtime dispatch.

For Tree-sitter, follow the language rule and treat leading-`[`
statements as a parse error. The parser's permissiveness here is
a side effect of `parseCommandArgs` operating on a slice without
a separate head check; do not model it.

The command head is the first Word token; subsequent tokens are
arguments. Arguments accept the argv-style primaries plus the two
expression-bearing forms `(EXPR)` and `[EXPR...]`. `[...]` runs
through `parseListLiteral` and produces a `ListExpr`; the parser
has no separate command-substitution form.

A trailing `matches { ... }` tail, where the token immediately
before `{` is the bare word `matches`, attaches as the command's
last argument; see the `MatchesBlock` production.

Examples:

    print "hello world"
    bpfman program list
    print ($snap |> jq ".id")
    print [1 2 3]

Errors at parse time:

- Bare stray `)` in argument position: "unmatched ')' in command
  argument".
- Bare stray `]` in argument position: "unmatched ']' in command
  argument".
- Unmatched `(` or `[` that reaches end of statement: matching
  "unmatched '('" or "unmatched '['" diagnostic at the opening
  token.

## Expressions

The expression grammar is parsed by precedence-climbing recursive
descent. Each layer calls the next-tighter layer for its operands
and loops for its own operator. From loosest to tightest binding:

    Expression     = OrExpr .
    OrExpr         = AndExpr        { 'or'  AndExpr } .
    AndExpr        = NotExpr        { 'and' NotExpr } .
    NotExpr        = 'not' NotExpr
                   | ComparisonExpr .
    ComparisonExpr = AdditiveExpr   [ CompareOp AdditiveExpr ] .
    CompareOp      = '==' | '!=' | '<' | '<=' | '>' | '>=' .
    AdditiveExpr   = MultExpr       { ('+' | '-') MultExpr } .
    MultExpr       = PredicateExpr  { ('*' | '/' | '%') PredicateExpr } .
    PredicateExpr  = UnaryPred Term
                   | NegateExpr .
    NegateExpr     = '-' NegateExpr
                   | ThreadExpr .
    ThreadExpr     = Term           { '|>' ThreadRHS } .
    ThreadRHS      = ThreadAtom { ThreadAtom } .
    ThreadAtom     = ThreadWord
                   | Quoted
                   | VarRef
                   | AdapterRef
                   | InterpString .
    ThreadWord     = Word with text not in ThreadTerminators .
    ThreadTerminators = '(' | ')' | '[' | ']' | '{' | '}'
                      | '+' | '-' | '*' | '/' | '%'
                      | 'and' | 'or'
                      | '==' | '!=' | '<' | '<=' | '>' | '>=' .
    Term           = Primary .

The terminator set applies by Word text, not by token character
class. `-`, `/`, and the comparison operator characters `<`,
`>`, `!`, `=` are not Delimiter Words in shell mode; they are
Compound Word constituents. Each terminator listed in
`ThreadTerminators` only fires when it appears as a standalone
Compound Word with exactly that text.

`$x-3` lexes as VarRef plus the Word `-3`, so the `-` is not a
thread terminator here; `$x - 3` lexes as VarRef, Word `-`,
Word `3` and the standalone `-` does terminate. The same shape
governs the comparison ops `!=`, `<`, `<=`, `>`, `>=`: each
becomes standalone only when separated from adjacent tokens by
whitespace or a delimiter. (`==` has its own lexer dispatcher
that emits it as a standalone Word in some contexts even
without whitespace, but the production rule above does not
depend on that exception.)

`or` and `and` are left-associative. `not` is recursive on
itself, so `not not $x` is two stacked `NotExpr` wrappers.

Comparison is non-chaining: at most one `CompareOp` between two
`AdditiveExpr` operands. A second comparison operator after a
complete comparison is reported as "unexpected token after
expression".

Arithmetic `+` `-` `*` `/` `%` are left-associative. `+`, `*`,
and `%` are Delimiter Words: the lexer emits each as its own
single-character Word regardless of surrounding whitespace, so
`1+2`, `1 + 2`, and `1+ 2` all tokenise the operator the same
way. `-` and `/` are Compound Word constituents in shell mode,
so they are recognised as operators only when they appear as
standalone Compound Words whose text is exactly `-` or `/`
(surrounded by token boundaries on both sides).

Unary `-` lives below the predicate layer, right-associative
via direct recursion. Numeric literals tokenise with a leading
`-` attached (`-3` is one Word), so the unary operator only
appears at whitespace-bounded positions where the `-` is a
standalone token rather than part of an adjacent literal.

`UnaryPred` covers exactly one word: `not-empty`. (`true` and
`false` are bare-word literals at the term level, not
predicates.) The predicate parser only consumes the next operand
when `operandFollowsPred` returns true: the immediately
following token is not `|>`, not a binary or arithmetic operator,
not `and`/`or`, not `)`, and not end of input. Otherwise the
predicate word falls through to a literal at the tightest level,
so `not-empty` at end of input or followed by an operator parses
as a literal rather than erroring.

`|>` parses an LHS at the Term level, then a non-empty sequence
of `ThreadAtom` tokens. A `ThreadAtom` is a Word whose text is not in
`ThreadTerminators`, or any of Quoted, VarRef, AdapterRef,
InterpString. The exclusion set names exactly the Word texts
that terminate the RHS: the six bracket/brace/paren Delimiter
Words, the five arithmetic operator Words, the two
logical-keyword Words, and the six binary-comparison Words.
TokenThread (the next `|>`) and TokenBind (`<-`) are not Words
and so are naturally excluded.

Grouped expressions and list literals are not part of the
`ThreadRHS` grammar; `CommandParenArg` and `CommandListArg` are
not reachable here. An empty RHS (no tokens before the
terminator) is rejected as "thread requires a command on the
right-hand side".

**Language rule versus parser accident.** Two things to keep
distinct:

- *Language rule.* Thread RHS accepts only simple atoms:
  ThreadWord, Quoted, VarRef, AdapterRef, InterpString. Grouped
  forms like `$x |> foo ($a + 1)` and `$x |> foo [1 2 3]` are
  not supported, even though `(EXPR)` and `[ ... ]` are valid
  command arguments elsewhere.

- *Parser accident.* The current recursive-descent parser
  implements thread RHS by feeding isolated tokens to
  `parsePrimary`. Because `(` and `[` are lexed as Word tokens,
  the RHS parser sees them as solitary word-like tokens with no
  view of the matching closer; `(` becomes a `LiteralExpr("(")`
  added to the thread args, and whether the RHS continues or
  terminates after that depends on the very next token. That is
  not a clean grammar rule; it is a side effect of
  token-at-a-time parsing.

For Tree-sitter, follow the language rule. Model `ThreadRHS` as
a sequence of `ThreadAtom` tokens with delimiter Words excluded
via `ThreadTerminators`, and do not try to reproduce the
"parsePrimary sees a lonely `(`" behaviour.

## Primary expressions

A primary is one of:

    Primary        = LiteralExpr
                   | VarRefExpr
                   | AdapterExpr
                   | InterpStringExpr
                   | ParenExpr
                   | ListExpr
                   | PureCallExpr
                   | TimeoutExpr
                   | IterationExpr
                   .

`MatchesBlock` is not in this alternation. Although it produces
a `MatchesBlockExpr` node that implements the `Expr` interface,
the general expression parser does not accept it; it is attached
only by `parseCommandStmt` and `AssertCommandMatchesForm` as a
trailing tail (see CommandStmt / AssertCommand).

### LiteralExpr

A bare Word or Quoted token. Word literals carry their source
text verbatim; quoted literals carry the inner text with the
delimiting quotes stripped. The lexer distinguishes the two
because `$` inside single-quoted strings stays literal whereas in
unquoted text it would start a VarRef.

    LiteralExpr    = Word | Quoted .

### VarRefExpr

A variable reference, optionally with a dotted field path and
indexed accesses.

    VarRefExpr     = '$' Identifier { '.' Identifier | '[' IndexKey ']' } .
    IndexKey       = IntegerLiteral | '$' Identifier .

Index keys are integer literals or variable references; arbitrary
expressions inside `[...]` are not accepted by the path
tokeniser. The `${name.path}` form is the same production
tokenised inside curly braces.

### AdapterExpr

An adapter-prefixed variable reference. The set of accepted
adapter names is registered in `token.go`'s `adapterPrefixes`
list. Currently the only entry is `file`.

    AdapterExpr    = AdapterName ':' '$' Identifier
                                     { '.' Identifier | '[' IndexKey ']' } .
    AdapterName    = 'file' .

### InterpStringExpr

A double-quoted string containing one or more `${...}`
interpolation segments. Literal segments and expression segments
alternate; `parsePrimary` parses each expression segment when
constructing the `InterpStringExpr`, via the InterpBody grammar
described below.

    InterpStringExpr = '"' { LiteralSegment | '${' InterpBody '}' } '"' .

A double-quoted string with no `${...}` segments tokenises as
Quoted, not InterpString.

### ParenExpr

A parenthesised sub-expression resets the precedence ladder. The
inner expression parses at the OrExpr level.

    ParenExpr      = '(' Expression ')' .

Empty parens are rejected with "empty parenthesised expression".
The same production reaches argument position via
parseCommandArgs (see CommandStmt), so `print ($x + 1)` is well
formed.

### ListExpr

A bracket-delimited whitespace-separated list literal. Each
element is parsed by `parseTerm`, which accepts any Term:
ordinary primaries plus parenthesised expressions, nested list
literals, pure-builtin calls, and `timeout` / `iteration` forms.

    ListExpr       = '[' Term { Term } ']' .

So these are all valid:

    [1 2 3]
    [(range 10) [1 2] (1 + 2)]
    ["a" $x file:$path]

An empty list `[]` is accepted; it evaluates to a list Value of
length zero. A Word element containing an unquoted comma is
rejected with a hint pointing at the whitespace-separated form.
A bare binary, arithmetic, thread, or logical operator between
elements is rejected with a hint to wrap the compound element in
parens (`[($x + 1) $y]`). Newlines between elements are
transparent so a long list can wrap across lines.

### PureCallExpr

A name registered via `RegisterPureBuiltin` invoked at expression
position. The arity is fixed at registration time; each argument
is parsed by `parsePureCallArg`.

EBNF cannot express the per-name fixed arity directly; the
production is:

    PureCallExpr   = BuiltinName followed by exactly arity(BuiltinName) CallArg operands .
    CallArg        = Primary .

`parsePureCallArg` has explicit paths for `(EXPR)` and `[ ... ]`
before falling back to ordinary primary parsing; the explicit
paths are not separate grammar alternatives, since both forms are
already Primary, but they let the argument parser dispatch
directly to the right sub-parser.

Pure-builtin dispatch is by name lookup against the registry at
parse time. The shell package itself registers `jq` (arity 2,
filter and input). Additional names may be registered by the
hosting binary at startup; `cmd/bpfman-shell` adds `range`,
`zip`, `u32le`, and `u64le` via `kindshapes.go`'s init.

Tree-sitter note: a grammar derived from this document cannot
discover runtime registrations. A Tree-sitter implementation for
the `cmd/bpfman-shell` binary should treat `jq`, `range`, `zip`,
`u32le`, and `u64le` as the concrete `BuiltinName` set with the
arities listed below. Alternatively, parse pure-builtin calls as
ordinary word-headed forms (no distinct production) and leave
builtin recognition to semantic tooling. Either choice is
correct; the first gives better highlighting.

Known builtin arities for `cmd/bpfman-shell`:

    jq      arity 2
    range   arity 1
    zip     arity 2
    u32le   arity 1
    u64le   arity 1

### MatchesBlock

A `matches { ... }` tail attached as the last argument of a
CommandStmt or AssertCommand. The parser recognises this shape
by sniffing the token immediately before `{`: when it is the
bare word `matches`, the brace block following is parsed as a
MatchesBlock and attached as the host command's last argument.

    MatchesBlock   = 'matches' '{' MatcherBody '}' .

`MatcherBody`'s detailed grammar is defined in `matches.go`; it
is parsed as a sequence of matcher entries against the LHS
value.

In the AST this production produces a `MatchesBlockExpr` node;
it implements the `Expr` interface but is not reachable from the
general expression parser, only from the command-tail dispatch
in `parseCommandStmt`.

## Binding sites

Five surface forms introduce names: `let X = EXPR` (LetStmt),
`let (NAMES) = EXPR` (LetDestructureStmt), `let X <- CMD` and
`guard X <- CMD` (both BindStmt), `foreach NAMES in LIST`
(ForEachStmt), and `def f(PARAMS)` (DefStmt). Each is documented
in its own statement section above; this section summarises the
shared shape and the discard-slot rule.

The shared name production used by `BindTarget`, `NameList`, and
`DestructureTarget`:

    Name           = Identifier | '_' .

`def` parameters use `Identifier` directly; `_` is therefore
covered by the ordinary identifier and duplicate-name rules.

### Name list forms

    let x = EXPR
    let (a b) = EXPR
    let (a _ c) = EXPR
    let x <- CMD
    let (rc x) <- CMD
    guard x <- CMD
    guard (rc x) <- CMD

    foreach x in LIST { BODY }
    foreach (a b) in LIST { BODY }

    def f() { BODY }
    def f(a) { BODY }
    def f(a b c) { BODY }

The bind tuple form (`let (rc x) <-` and `guard (rc x) <-`)
accepts exactly two names, whitespace-separated. The
let-destructure form (`let (a b ...) = EXPR`) accepts two or
more whitespace-separated names. The foreach multi-var form
(`foreach (a b) in xs`) accepts two or more whitespace-separated
names; the parens are required so that `foreach a b in xs` does
not read as a command-shaped name list. The def parameter list
accepts zero or more whitespace-separated names. None of these
sites accepts a comma separator.

### Discard slot

A bare `_` accepts the value and binds nothing observable.
Accepted positions:

- Single bind target (`let _ <- cmd`, `guard _ <- cmd`). Used in
  the corpus when the command is run for its side effect and the
  primary value is not needed.
- Tuple-bind slots (`let (_ x) <- cmd`, `let (a _) <- cmd`).
- Let-destructure slots (`let (_ b) = $pair`, `let (a _ c) = $triple`).
- ForEach name list (single-var `foreach _ in xs` and any slot
  in multi-var `foreach (_ b) in pairs`).

Rejected:

- Tuple bind where both slots are `_`: `let (_ _) <- cmd` is
  rejected as "tuple bind cannot discard both slots".
- Let-destructure where every slot is `_`: `let (_ _) = $pair`
  is rejected as "all destructure slots are '_'; at least one
  must bind".
- Multi-var foreach where every name is `_`:
  "foreach: all loop variables are '_'; at least one must bind".
  Single-var `foreach _ in xs` is allowed (the gating is on
  `len(names) >= 2`).
- `def` accepts `_` as a parameter name (`_` qualifies as an
  identifier per `IsIdent`), but it is bound the same way any
  other parameter is. The duplicate-name rule applies: `def
  f(_ _)` is rejected as "duplicate parameter name '_'".

## Quoting

Single-quoted strings are fully literal: no `${...}` interpolation,
no escape decoding (`\n`, `\t`, `\r`, `\\`, `\"`, `\$` are
preserved verbatim), and no way to embed a literal `'` because
the closing delimiter cannot be escaped.

Double-quoted strings recognise `${...}` interpolation and
decode the escape set `\n`, `\t`, `\r`, `\\`, `\"`, `\$`. A bare
`$` not followed by `{...}` is a lex-time error rather than a
literal `$`.

When the embedded content already uses double quotes -- jq
filters, JSON fragments, regular expressions with character
classes -- prefer single quotes for the outer string so the
interior reads as written:

    jq 'split("/fs/")[0]'
    jq '[.status.maps[] | select(.name == "kp_count") | .id][0]'

rather than backslash-escaping each interior `"`:

    jq "split(\"/fs/\")[0]"
    jq "[.status.maps[] | select(.name == \"kp_count\") | .id][0]"

Single quotes also disable `${...}` interpolation, so reach for
double quotes whenever you need to splice a variable into the
string.

## Interpolation

A double-quoted string with no `${...}` content tokenises as
Quoted (LiteralExpr at the expression layer). A double-quoted
string with at least one `${...}` tokenises as InterpString and
produces an InterpStringExpr whose Segments alternate literal
text and parsed expressions.

    InterpBody     = '$' Identifier { '.' Identifier | '[' IndexKey ']' }
                   | Expression .

`parseInterpBody` tries the bare-name shortcut first: if the body
parses (after a synthesised `$` prefix) as a single VarRef token,
it returns a VarRefExpr directly. If the bare-name match fails,
the body is tokenised and parsed at the full Expression level:

    "${4 * 2}"
    "${$prog |> jq '.maps[0]'}"
    "${jq '.id' $obj}"

Empty bodies (`${}` or whitespace-only `${ }`) are rejected as
"empty interpolation". Position information from the inner parse
is translated back to original-source coordinates so error spans
point at the offending byte inside the original source.

Statements (let, if, foreach, etc) are not reachable inside
`${...}` because the inner parse uses the Expression entry point,
not the Statement entry point.

## Command substitution

The current parser has no command-substitution form. `[...]` at
any position is a list literal. In command argument position
`parseCommandArgs` finds the matching bracket via
`findMatchingBracket`, then parses the bracketed token slice as
an expression; syntactically, that expression must be the
`ListExpr` production. The only way to capture a command's
output into a name is the bind form `let x <- CMD`.

## Parser limitations

- Multi-line `(EXPR)` is not supported by the statement-token
  collectors (`parseCommandStmt`, `takeStmtTokens`,
  `takeBindRHSTokens`): they track `[` / `]` depth but not `(`
  / `)`, so a parenthesised expression that spans lines is cut
  at the newline. `def` parameter lists and `[ ... ]` list
  literals have dedicated handling and do support wrapping
  across lines.
- VarRef index keys (`$x[K]`) accept only an integer literal or
  a `$ident` token; arbitrary expressions inside `[...]` in
  index position are not accepted by the path tokeniser.
