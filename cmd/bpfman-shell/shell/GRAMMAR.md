# bpfman-shell grammar

This document is the reference grammar for the language driven by
`cmd/bpfman-shell`. The parser and AST in `shell/syntax` are the
load-bearing ground truth; this document is the human-readable shape
of what that code accepts.

**Implementation status.** The current language includes
statement-only `poll timeout DUR every DUR { ... }`, explicit
`retry` inside `poll`, value-returning `def`s with `return`, and
`matches` as a first-class expression operator.

## Scope

The doc describes parsing: what tokens the lexer emits, what
productions the parser recognises, where the binding sites live,
and how operator precedence is layered. Evaluation semantics and
runtime errors are mentioned only where they affect what is
parseable; the full runtime semantics live in `shell/runtime`.

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
`require` clauses, parenthesised command arguments,
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
  uses whitespace between names (`def f(a b)`, `let (a b) =`,
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

The primary continuation model is structural: a Sep token does
not terminate a statement while the parser is inside an open
syntactic form. The statement-token collectors treat newlines
and `{` as inert at positive paren / bracket depth, so an
expression spread across lines reads naturally when it is
inside `(...)`, `[...]`, a matches block, a `def` parameter
list, an `if` / `elif` condition before its block, or any
other context that opens a balanced pair. Inside `{ ... }`
block bodies, newlines are ordinary statement separators.

A backslash immediately followed by a newline (or `\r\n`) is
also accepted as a lexer-level line continuation: the backslash
and line ending are consumed, the continuation does not emit a
Sep token, and the two adjacent lines tokenise as one logical
line. The continuation is recognised only at top-level
positions outside quoted strings; inside quoted strings
backslash handling is governed by the quoted-string lexer
rather than by the line continuation rule. The backslash form
is intended for paste-from-shell compatibility on a plain argv
command line that has no surrounding syntactic form; reach for
it only when the structural continuation does not apply.

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

    let guard foreach if poll retry defer def return break
    continue assert require

Branch-keyword words (tested in `parseIfStmt` after a block's
closing `}`):

    elif else

`return` was a tombstone until the value-returning def form
landed; it is now a real statement keyword tested at
`parseStmt` and routed to `parseReturnStmt`. See the
`ReturnStmt` section below for the grammar and the later def
discussion for the bind-position semantics.

Inside `poll`, the keyword words `timeout` and `every` are
recognised at the leading statement form only (between
`poll` and the block). Outside that position they remain
ordinary Words.

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

Assert command-head words (tested in `isAssertCommandHead`
inside the assert parser to route into the transitional
command-shaped clause):

    ok fail

Predicate semantics:

  - `ok COMMAND`             command exits with ok envelope
  - `fail COMMAND`           command exits with non-ok envelope
Named assertion predicates now live on the expression lane as
pure calls:

  - `path-exists FILE`       filesystem path exists
  - `contains HAYSTACK NEEDLE`
                             HAYSTACK string contains NEEDLE
  - `null $X.field`          path resolves and terminal value is
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

`present`, `missing`, `null`, and `empty` all accept either a
$-prefixed variable expression or a bareword variable-name with
optional dotted path; the predicate evaluator reads the
underlying presence state from the runtime path walker so the
three states (absent / null / value) are distinguishable.

Expression operator words (recognised at the matching position
in the expression grammar):

    matches exhaustive

`matches` is the postfix comparison-level operator described in
the Expressions section; `exhaustive` is recognised only when it
appears immediately after `matches`. Outside that position both
words are ordinary Word tokens.

Adapter prefix words (registered in `token.go`'s
`adapterPrefixes`):

    file

Pure-builtin names (owned by `shell/semantics` and mirrored by
`shell/syntax`'s parser-visible table):

    jq range zip u32le u64le     (registered by the shell package)

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
`poll` has no trailing control clause: its `timeout`
and `every` keywords appear in the leading statement
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
                   | PollStmt
                   | DeferStmt
                   | DefStmt
                   | ReturnStmt
                   | BreakStmt
                   | ContinueStmt
                   | AssertStmt
                   | ExprStmt
                   | CommandStmt
                   .

`LetStmt` covers only the single-name `let X = EXPR` form;
`let (NAMES) = EXPR` produces a `LetDestructureStmt`. Both
`let X <- CMD` and `guard X <- CMD` produce a `BindStmt`
(distinguished by the `Guard` flag). `AssertStmt` always owns
`assert` / `require`; the assertion clause inside the statement
then distinguishes ordinary expression forms and transitional
command-shaped forms. `matches` is now an expression operator,
not a dedicated assertion-clause shape.

Statement dispatch is decided by the first token of the
statement. Reserved words `let`, `guard`, `foreach`, `if`,
`poll`, `retry`, `defer`, `def`, `return`, `break`, `continue`
route to their named parsers. `assert` and `require` route
to the assert parser, which always produces `AssertStmt` and
then chooses the clause form using the disambiguation rule in
the AssertStmt section. A statement-leading VarRef, Quoted,
InterpString, AdapterRef, or one of the Words `(`, `not`,
`not-empty`, `true`, `false` routes to ExprStmt. Anything
else routes to CommandStmt. `import` has no dedicated
production; it parses as a CommandStmt and is recognised by
the command dispatcher in the shell runner, which loads a
def-only library file.

A bare `{` or `}` at statement position is a parse error: the
parser has already routed block-introducing keywords elsewhere,
and a stray brace at the top level signals an incomplete
construct above.

## Statements

### Block

A block is a brace-delimited statement sequence used by `if`,
`elif`, `else`, `foreach`, `poll`, and `def`.

    Block          = '{' { Sep } [ Statement { SepSeq Statement } { Sep } ] '}' .

Statements inside the block follow the same dispatch rules as
top-level statements. An empty block `{ }` is allowed.

`matches { ... }` uses braces too, but its body is not a
statement block; it is parsed by the matcher-entry grammar in
`syntax/matches.go`.

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
On the success path, `guard` has already checked the envelope's
`ok` field; scripts usually bind the envelope only to inspect
post-success data such as `stdout`, `stderr`, or the primary
value, not to repeat `assert $rc.ok`.

    BindStmt       = ('let' | 'guard') BindTarget '<-' BindRHS .
    BindTarget     = Name
                   | '(' Name Name ')' .
    BindRHS        = CommandForm
                   | ForEachStmt .         (* bind-collect *)

`CommandForm` is the shared command-shape production; its
definition is in the CommandStmt section. `Name` is the shared
binding-site name production; its definition is in the Binding
sites section.

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
   primary value is collected through the same result model:
   `guard xs <- foreach ...` unwraps the successful value list,
   while `let r <- foreach ...` binds an aggregate outcome with
   per-iteration results and filtered successful values. An empty
   body or a non-CommandStmt last statement is rejected at parse
   time.

Examples:

    let result <- bpfman program load file --path foo.o
    require $result.ok
    let loaded = $result.value
    guard pid <- bpfman program load file --path foo.o
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

### PollStmt and RetryStmt

`poll` is the retrying control-flow construct. It is
statement-only. `retry` is the only recoverable "not ready
yet" signal. It appears directly inside `poll`, and helper
defs may execute it when they are called from an active
`poll`.

    PollStmt       = 'poll' 'timeout' DurationWord
                         'every' DurationWord Block .
    RetryStmt      = 'retry'
                   | 'retry' StringLike
                   | 'retry' 'unless' Expression
                   | 'retry' StringLike 'unless' Expression .
    DurationWord   = Word whose text is accepted by
                     time.ParseDuration .

The lexer does not emit a distinct Duration token. The parser
expects a Word at each duration slot and validates its text
with `time.ParseDuration`. Elsewhere, the same text is just
an ordinary Word.

`timeout DUR every DUR` is mandatory; there is no default
cadence and no bindable poll result object.

Examples:

    poll timeout 5s every 100ms {
        retry "waiting for ack" unless path-exists "${ack}.1"
    }

    poll timeout 30s every 250ms {
        let rc <- exec test -f $ack
        retry "ack file absent" unless $rc.ok
    }

**Polling semantics.**

- One attempt runs the body from top to bottom.
- `retry` means "this attempt is not ready; start another one if
  the deadline allows".
- `retry MSG` records `MSG` as the last retry reason shown on
  timeout.
- `retry unless EXPR` retries when `EXPR` is false.
- Helpers called from `poll` may execute `retry`; the retry
  still targets the caller's active poll attempt.
- A helper that executes `retry` outside an active `poll`
  fails at runtime with `retry: outside any poll`.
- `require` is fatal immediately, including inside `poll`.
- `assert` is not valid inside `poll`, including helper calls
  reached from `poll`; use `retry unless ...` for recoverable
  waiting and `require ...` for fail-now invariants.
- Ordinary command, guard, and expression failures inside `poll`
  are fatal unless the script handles them and turns the state
  into an explicit `retry`.

### DeferStmt

`defer CMD`. The RHS is parsed as a command form; the parser
emits a `DeferStmt` whose child is a `CommandStmt`.

    DeferStmt      = 'defer' CommandForm .

Example:

    defer bpfman program unload $pid

A possible future local-cleanup block form such as
`defer { CMD ... }` is not part of the grammar today. If it
lands, it should be treated as sugar over multiple ordinary
deferred commands rather than as a different cleanup model.
The intended ordering is source-order execution of the block
body: lowering would reverse registration so ordinary LIFO
unwind still fires the captured commands in the order they
were written.

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

`_` is rejected in a def parameter list. Def parameters must use
real names; there is no discard-slot meaning for call arguments,
so the parser reports "def parameters cannot bind '_'" rather
than letting `_` through as a one-off ordinary name.

`def` is a top-level declaration. The static checker rejects a
`def` nested inside if/elif/else, foreach, poll, or
another def body with `def "NAME" must be declared at top
level`.

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

- **Bind-position** invocation (`let r <- my_def args`,
  `guard p <- my_def args`) -- the evaluated value becomes the
  BindResult's Primary. A def that runs to completion without
  `return` produces Primary equal to `ValueFromEnvelope(Rc)`,
  matching the no-payload command-bind family (exec, bpftool,
  wait). `let` binds the inspectable outcome; `guard` requires
  success and binds the primary directly.

Defer interaction:

1. evaluate `return EXPR` in the call frame; obtain the Value;
2. stash the Value -- it is now independent of the call frame;
3. unwind def-local defers in LIFO order while the call frame
   is still live so deferred `print $captured` and similar
   commands resolve against the body's bindings;
4. pop the call frame;
5. emit `BindResult{Rc, Primary: stashed}` to the bind-position
   caller, or discard at command-form position.

A defer that fires during step 3 and reports a non-ok envelope
marks Rc failed on the bind-position path even when
`return EXPR` itself evaluated cleanly. `let r <- f` exposes that
cleanup outcome through `$r.ok`; `guard p <- f` halts via
GuardFailure when cleanup failed. Defs are not a special case;
this is the existing bind family contract.

The call sites for value-returning defs uniformly compose with
the existing bind-target shapes. Multiple values fall out of
the existing list / destructure story:

    def load_xdp(path iface) {
        guard prog <- bpfman program load file \
            --path $path --type xdp
        guard link <- bpfman link attach xdp \
            -i $iface generic 50 $prog
        return [prog link]
    }

    guard pair <- load_xdp ./xdp.o eth0
    let (prog link) = $pair
    defer bpfman program unload $prog
    defer bpfman link detach $link

The cleanup `defer`s live at the call site, not inside the
helper. A def opens its own defer scope on entry, so anything
`defer`ed inside the body unwinds when the def returns -- BEFORE
the caller binds the result. Putting `defer bpfman program
unload $prog` inside `load_xdp` would unload the program at
function return and leave the caller's `$prog` naming a freed
resource. The runtime is doing exactly what the spec says.
Today that means the caller must register cleanup itself.
Registration order matters: unwind is LIFO, so the example
registers unload first and detach second so detach runs
before unload at scope exit.
Explicit caller-side `defer` remains the baseline.

`guard x <- load_xdp ...` is the success path: it propagates a
non-ok outcome and binds the returned value directly. `let r <-
load_xdp ...` is the inspection path: it binds the outcome so the
caller can test `$r.ok` and, on success, read the conditional
payload through `$r.value`.

### BreakStmt and ContinueStmt

    BreakStmt      = 'break' .
    ContinueStmt   = 'continue' .

Both keywords take no arguments; trailing tokens before the next
separator are rejected at parse time.

`break` and `continue` are valid only inside `foreach`. Using
either inside `poll`, `if`, `def`, an imported file, or
at top level is a static/runtime error. `poll`
deliberately does not let `continue` mean "next attempt": the
attempt boundary is explicit `retry`, not a user-driven
control transfer.

### AssertStmt

`assert` and `require` always parse as `AssertStmt`. The
statement carries one assertion clause chosen by the parser:
an expression clause or a transitional command-shaped clause.

    AssertStmt                = AssertKeyword AssertClause .
    AssertClause              = AssertCommandClause
                              | AssertExprClause .
    AssertExprClause          = Expression .
    AssertCommandClause       = [ 'not' ]
                                AssertCommandHead { CommandArg } .
    AssertCommandHead         = 'ok' | 'fail' .
    AssertKeyword             = 'assert' | 'require' .

Disambiguation inside the assertion parser:

- If the next non-`Sep` token (after an optional leading `not`)
  is one of the `AssertCommandHead` words, the clause is an
  `AssertCommandClause`.
- Otherwise the clause is an `AssertExprClause`.

`AssertCommandClause` is the one deliberate transitional bucket.
It now preserves only the command-status forms (`assert ok
exec ...`, `assert fail exec ...`). Named predicates have
already moved to the expression lane. The keyword remains
syntax-owned even when the clause payload looks command-like.

Examples:

    assert $count > 0                           # AssertExprClause
    require not-empty $links                    # AssertExprClause
    assert ok bpfman program get $pid           # AssertCommandClause
    require not ok bpfman program get $pid      # AssertCommandClause
    assert $output matches { ... }              # AssertExprClause

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

`matches { ... }` is part of the expression grammar, so
`assert $x matches { ... }` and `if $x matches { ... } { ... }`
both flow through the ordinary expression parser.

### CommandStmt

Anything that does not match a keyword statement and does not
unambiguously start an expression is parsed as a command
invocation.

    CommandStmt     = CommandHead { CommandArg } .
    CommandForm     = CommandHead { CommandArg } .
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

`CommandForm` is the production reused on bind RHS, defer RHS,
let-`<-` RHS, and assert/require clauses; the structure is the
same as the bare `CommandStmt` plus the surrounding parser's
own framing. There is no trailing `MatchesBlock` in either
production: `matches` is not a command-tail. It is a postfix
expression operator at the comparison level (see Expressions
below); when a command argument needs to use it, the argument
enters expression syntax explicitly via `CommandParenArg`
(`print ($x matches { ... })`).

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
- A `{` token at top-level depth.

"Top-level depth" here means: outside any open
`CommandParenArg` and outside any open `CommandListArg`. The
matched-paren and matched-bracket consumption inside those
forms handles their own balancing.

The parser's RHS token collectors for `assert`/`require`, `let`,
and a few similar contexts perform a small lookahead trick: when
the buffer's tail is the bare word `matches` (optionally
followed by `exhaustive`), an immediately following `{` is
absorbed into the buffer rather than treated as a block
terminator. This lookahead exists so that an expression-position
parser called on the buffer (`parseExpression` for the
let-expression and assert-expression clauses) can see the
complete `matches { ... }` tail of its expression. It is a
buffer-collection convenience, not a feature of CommandForm:
the resulting tokens are still parsed as a single Expression
where `matches` reduces through `ComparisonExpr`.

**Language rule versus parser accident: command head.**

- *Language rule.* `CommandHead = Word`. A command statement
  begins with a bare word naming the command.

- *Parser rule.* A leading `[` at statement position is rejected
  with "list literal at statement position is not allowed".
  List literals remain valid in expression position and in
  command argument position, but not as standalone statements.

For Tree-sitter, follow the language rule and treat leading-`[`
statements as a parse error.

The command head is the first Word token; subsequent tokens are
arguments. Arguments accept the argv-style primaries plus the two
expression-bearing forms `(EXPR)` and `[EXPR...]`. `[...]` runs
through `parseListLiteral` and produces a `ListExpr`; the parser
has no separate command-substitution form.

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
    ComparisonExpr = AdditiveExpr
                     [ CompareOp AdditiveExpr
                     | 'matches' [ 'exhaustive' ] MatchesBlockBody ] .
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
                   .

`MatchesBlockExpr` is not in this alternation: it is the
right-hand side of the `matches` postfix operator at the
`ComparisonExpr` level, not a primary in its own right.
`parseMatchesBlock` is only reached by `parseMatches` when the
expression grammar has already consumed a primary on the left
and the next token is the bare word `matches`.

Expression parsing is entered at the following sites; anywhere
in that subtree, `EXPR matches { ... }` works because matches
is just another expression-level operator:

- The `=` form of `let`: `let r = $x matches { ... }`.
- The expression clause of `assert` and `require`:
  `require $x matches { ... }`.
- Condition expressions of `if` and `elif`.
- Parenthesised command arguments: `print ($x matches { ... })`.
- Parenthesised list literal elements:
  `[($a matches { ... }) $b]`. List elements are
  whitespace-separated and each element parses as a Term, so a
  compound expression has to wrap in `(...)` to enter the
  expression grammar.
- The expression body inside an interpolation segment of a
  double-quoted string: `"${$x matches { ... }}"`.

Sites that are command-position (the `<-` form of `let`, `defer`,
the assert-clause command shape `assert ok ...`, bare
`CommandStmt`) do not own a `matches { ... }` tail; to use a
matches expression there, wrap it in `(EXPR)` to enter the
expression grammar through `CommandParenArg`.

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

A name from the pure-builtin table invoked at expression position.
Its arity is fixed by the language's semantic table; each argument
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
parse time. The shell package registers the language-level set:
`jq` (arity 2, filter and input), plus `range`, `zip`, `u32le`,
and `u64le`.

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

`matches` owns a structural block on the right-hand side of the
expression operator:

    MatchesExpr        = Expression 'matches'
                         [ 'exhaustive' ] MatchesBlockBody .
    MatchesBlockBody   = '{' MatcherBody '}' .

`MatcherBody`'s detailed grammar is defined in `syntax/matches.go`; it
is parsed as a sequence of matcher entries against the LHS
value. In the AST the overall expression is `MatchesExpr`, which
owns a `MatchesBlockExpr` payload for the structural matcher
body.

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
    guard x <- CMD

    foreach x in LIST { BODY }
    foreach (a b) in LIST { BODY }

    def f() { BODY }
    def f(a) { BODY }
    def f(a b c) { BODY }

The let-destructure form (`let (a b ...) = EXPR`) accepts two or
more whitespace-separated names. Parenthesised names after `<-`
are rejected; command capture binds a single name and exposes
named fields on the outcome. The foreach multi-var form
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
- Let-destructure slots (`let (_ b) = $pair`, `let (a _ c) = $triple`).
- ForEach name list (single-var `foreach _ in xs` and any slot
  in multi-var `foreach (_ b) in pairs`).

Rejected:

- Tuple bind after `<-`: `let (_ x) <- cmd` is rejected as
  "tuple bind after '<-' is no longer supported".
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
is stamped directly with the surrounding source file and absolute
line/column, so error spans point at the offending byte inside the
original source without later rebasing.

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

- VarRef index keys (`$x[K]`) accept only an integer literal or
  a `$ident` token; arbitrary expressions inside `[...]` in
  index position are not accepted by the path tokeniser.
