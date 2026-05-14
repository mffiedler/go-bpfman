# bpfman-shell grammar

This document is the reference grammar for the language driven by
`cmd/bpfman-shell`. The parser source in `parse.go`, `token.go`,
`expr.go`, and `matches.go` is the load-bearing ground truth; this
document is the human-readable shape of what that code accepts.

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
`foreach ... in` operands, `retry ... until` operands, and
thread pipelines. Everywhere else, Word text stays opaque.

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

- `foreach a, b in xs { ... }` may lex as `Word("foreach")
  Word("a,") Word("b") Word("in")` rather than `foreach IDENT
  COMMA IDENT in`. The parser strips trailing commas glued to
  identifiers at every binding site (`parseBindTargetName`,
  `parseForEachNameToken`, `parseDefParams`). The grammar
  productions spell the surface syntax (Name `,` Name); the
  comma-gluing is a tokenisation detail to handle inside the
  binding-site rules.

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

    let guard foreach if retry defer def break continue
    assert require

Branch- and trailer-keyword words (tested in `parseIfStmt` and
`parseRetryStmt` after a block's closing `}`):

    elif else until

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

    timeout iteration not-empty true false

Assert-command predicate words (tested in `assertTakesExprForm`
to route into the command-form path):

    ok fail path contains nil

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

Branch continuations after a `}` are not separate statements
and are handled inside their owning statement productions:
`elif` and `else` are consumed by `IfStmt`, and `until` is
consumed by `RetryStmt`. Each of those productions skips any
number of Sep tokens between the closing `}` and the trailing
keyword, so the global SepSeq rule does not apply.

## Program

A program is a (possibly empty) sequence of statements with
optional Sep tokens between, before, and after them.

    Program        = { Sep } [ Statement { SepSeq Statement } { Sep } ] .
    Statement      = LetStmt           (* let X = EXPR *)
                   | BindStmt          (* let X <- CMD, guard X <- CMD *)
                   | ForEachStmt
                   | IfStmt
                   | RetryStmt
                   | DeferStmt
                   | DefStmt
                   | BreakStmt
                   | ContinueStmt
                   | AssertStmt
                   | AssertCommand
                   | ExprStmt
                   | CommandStmt
                   .

`LetStmt` covers only the `let X = EXPR` form; both `let X <-
CMD` and `guard X <- CMD` produce a `BindStmt` (distinguished by
the `Guard` flag). `AssertStmt` and `AssertCommand` share the
keywords `assert` / `require` and disambiguate at statement
dispatch; `AssertCommand` is structurally a `CommandStmt` whose
head is the keyword.

Statement dispatch is decided by the first token of the
statement. Reserved words `let`, `guard`, `foreach`, `if`,
`retry`, `defer`, `def`, `break`, `continue` route to their
named parsers. `assert` and `require` route to the assert
parser, which chooses between `AssertStmt`, `AssertCommand`
predicate form, and `AssertCommand` matches form using the
disambiguation rule in the AssertStmt section. A
statement-leading VarRef, Quoted, InterpString, AdapterRef, or
one of the Words `(`, `not`, `not-empty`, `true`, `false`
routes to ExprStmt. Anything else routes to CommandStmt.

A bare `{` or `}` at statement position is a parse error: the
parser has already routed block-introducing keywords elsewhere,
and a stray brace at the top level signals an incomplete
construct above.

## Statements

### Block

A block is a brace-delimited statement sequence used by `if`,
`elif`, `else`, `foreach`, `retry`, and `def`.

    Block          = '{' { Sep } [ Statement { SepSeq Statement } { Sep } ] '}' .

Statements inside the block follow the same dispatch rules as
top-level statements. An empty block `{ }` is allowed.

`matches { ... }` uses braces too, but its body is not a
statement block; it is parsed by the matcher-entry grammar in
`matches.go`.

### LetStmt

The pure-assignment form: `let X = EXPR` binds a name to an
expression value.

    LetStmt        = 'let' Identifier '=' Expression .

Tuple targets are not legal on `=`; only `<-` accepts them. In
this form `_` qualifies as an `Identifier` and binds an ordinary
name `_`; it is not a discard slot. Discard semantics for `_`
apply only at BindTarget and ForEach NameList positions.

Examples:

    let x = 5
    let m = $prog |> jq ".maps"

### BindStmt

`let X <- CMD` and `guard X <- CMD` both produce a `BindStmt`.
The keyword chooses whether a non-ok result envelope halts the
script (`guard`) or carries through normally (`let`).

    BindStmt       = ('let' | 'guard') BindTarget '<-' BindRHS .
    BindTarget     = Name
                   | '(' Name ',' Name [ ',' ] ')' .
    BindRHS        = CommandForm
                   | ForEachStmt           (* bind-collect *) .

`CommandForm` is the shared command-shape production; its
definition is in the CommandStmt section. `Name` is the shared
binding-site name production; its definition is in the Binding
sites section.

The tuple target accepts exactly two names. The first target
receives the result envelope; the second receives the primary
value. Arities other than two are rejected at parse time. `_` is
an accepted name at either slot, but `(_, _)` is rejected at
parse time as "tuple bind cannot discard both slots". A trailing
comma after the second name is accepted.

The `BindRHS` has two shapes:

1. **Command form.** The default path. `parseBindRHS` reads the
   command's tokens up to the next Sep or block marker via
   `takeBindRHSTokens` and produces a `CommandStmt`.

2. **Bind-collect form.** When the RHS begins with the keyword
   `foreach`, the parser delegates to `parseForEachStmt`. The
   foreach body must end with a CommandStmt; that command's
   primary value is collected into a list bound to the BindStmt
   target. An empty body or a non-CommandStmt last statement is
   rejected at parse time.

Examples:

    let pid <- bpfman program load file --path foo.o
    guard pid <- bpfman program load file --path foo.o
    let (rc, p) <- bpfman program get $pid
    let (_, p) <- bpfman program get $pid
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
                   | Name ',' Name { ',' Name } .

The Expression between `in` and the opening `{` is collected by
`takeUntilOpenBrace`, which skips Sep tokens and stops at the
next `{`; the collected token stream is then parsed as an
Expression. `_` is an accepted name at any slot. When
`len(NameList) >= 2`, an all-underscore name list is rejected as
"foreach: all loop variables are '_'; at least one must bind".
The single-name form `foreach _ in xs` is accepted.

Tokenisation note: in the current lexer, a comma may arrive
glued to the preceding identifier (`a,` lexes as one Word
token). The parser normalises by stripping the trailing comma
before validating the name. The same normalisation applies to
`BindTarget`'s tuple form and to `ParamList`. A tree-sitter
derivation should model the surface syntax (Name `,` Name) and
treat the comma-gluing as a tokenisation detail. A Tree-sitter
grammar can either split comma as punctuation globally and
preserve command-shaped arguments via a broader
command-argument token rule, or accept glued-comma identifiers
in the binding-site rules and normalise them semantically.

Examples:

    foreach prog in $progs { print $prog.name }
    foreach prio, po in (zip $priorities $proceed_ons) { ... }
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

### RetryStmt

`retry BLOCK until EXPR`. The body is a brace block; the
until-expression follows the closing brace, optionally separated
by newlines or `;` tokens.

    RetryStmt      = 'retry' Block 'until' Expression .

Example:

    retry {
        let (rc, _) <- bpfman program get $pid
    } until $rc.ok

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
    ParamList      = Identifier { ',' Identifier } [ ',' ]

Parameters are comma-separated. Duplicate parameter names are
rejected at parse time. A trailing comma after the last parameter
is permitted. Newlines and semicolons between parameters are
allowed so a long parameter list can wrap.

`_` qualifies as an Identifier so it may appear as a parameter
name, but unlike bind and foreach positions it is not treated as
a discard slot here: it is an ordinary parameter name, and the
duplicate-name rule rejects two `_` parameters in the same list.

Examples:

    def warn() {
        print "warning"
    }
    def attach(iface, prog) {
        bpfman link attach xdp $iface generic 50 $prog
    }

### BreakStmt and ContinueStmt

    BreakStmt      = 'break' .
    ContinueStmt   = 'continue' .

Both keywords take no arguments; trailing tokens before the next
separator are rejected at parse time.

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
    AssertCommandPredicate  = 'ok' | 'fail' | 'path'
                            | 'contains' | 'nil' .
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

Empty lists `[]` are rejected at parse time. A Word element
containing an unquoted comma is rejected with a hint pointing at
the whitespace-separated form. A bare binary, arithmetic,
thread, or logical operator between elements is rejected with a
hint to wrap the compound element in parens (`[($x + 1) $y]`).
Newlines between elements are transparent so a long list can
wrap across lines.

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

### TimeoutExpr and IterationExpr

Two unary expression forms parsed at the Term level:

    TimeoutExpr    = 'timeout' Term .
    IterationExpr  = 'iteration' Term .

Each takes one operand parsed at the Term layer. The
`'timeout'` and `'iteration'` keywords are recognised by name
inside the term parser; outside this position they parse as
bare-word literals.

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

Four surface forms introduce names: `let X = EXPR` (LetStmt),
`let X <- CMD` and `guard X <- CMD` (both BindStmt), `foreach
NAMES in LIST` (ForEachStmt), and `def f(PARAMS)` (DefStmt).
Each is documented in its own statement section above; this
section summarises the shared shape and the discard-slot rule.

The shared name production used by `BindTarget` and `NameList`:

    Name           = Identifier | '_' .

`def` parameters use `Identifier` directly; `_` is therefore
covered by the ordinary identifier and duplicate-name rules.

### Name list forms

    let x = EXPR
    let x <- CMD
    let (rc, x) <- CMD
    guard x <- CMD
    guard (rc, x) <- CMD

    foreach x in LIST { BODY }
    foreach a, b in LIST { BODY }

    def f() { BODY }
    def f(a) { BODY }
    def f(a, b, c) { BODY }

The bind tuple form (`let (rc, x) <-` and `guard (rc, x) <-`)
accepts exactly two names. The foreach multi-var form accepts
two or more names separated by commas. The def parameter list
accepts zero or more names separated by commas with an optional
trailing comma.

### Discard slot

A bare `_` accepts the value and binds nothing observable.
Accepted positions:

- Single bind target (`let _ <- cmd`, `guard _ <- cmd`). Used in
  the corpus when the command is run for its side effect and the
  primary value is not needed.
- Tuple-bind slots (`let (_, x) <- cmd`, `let (a, _) <- cmd`).
- ForEach name list (single-var `foreach _ in xs` and any slot
  in multi-var `foreach _, b in pairs`).

Rejected:

- Tuple bind where both slots are `_`: `let (_, _) <- cmd` is
  rejected as "tuple bind cannot discard both slots".
- Multi-var foreach where every name is `_`:
  "foreach: all loop variables are '_'; at least one must bind".
  Single-var `foreach _ in xs` is allowed (the gating is on
  `len(names) >= 2`).
- `def` accepts `_` as a parameter name (`_` qualifies as an
  identifier per `IsIdent`), but it is bound the same way any
  other parameter is. The duplicate-name rule applies: `def
  f(_, _)` is rejected as "duplicate parameter name '_'".

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
- Empty list literal `[]` is rejected at parse time.
