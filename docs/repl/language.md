# REPL language reference

The bpfman REPL is a small domain-specific language for lifecycle
scripting and interactive debugging of bpfman state. It is
deliberately small: line-oriented where possible, block-aware where
useful (for `if`), with explicit data flow through typed variable
bindings.

This document is the authoritative reference for the shipped
language. For the host-integration builtins (`exec`, `json`, `file
temp`), see `builtins.md`. For implementation notes and deferred
work, see `implementation-notes.md`.

## Overview

### What the REPL is

- A CLI with a persistent session. Domain commands use the `bpfman`
  prefix; shell-language commands (`let`, `if`, `assert`, `exec`,
  `json`, ...) are bare.
- A debugger over bpfman state, with structured values, typed
  origins, and cross-layer correlation via the `inspect` and
  `coherency` packages.
- A thin scripting layer: `let` bindings, `if`/`elif`/`else` blocks,
  command substitution via `[cmd]`, and assertions.

### What the REPL is not

- A general-purpose shell. No pipelines, redirects, globbing, or job
  control.
- A full programming language. No user-defined functions, no
  closures, no modules.  `foreach` iterates a block over an
  already-materialised list (typically produced by `jq`); `retry
  { } until EXPR` handles open-ended polling with
  `timeout DURATION` and `iteration N` as retry-scoped primaries.
- A foreign runtime (Lua, TCL, Python, Starlark).

## Grammar

```
program      := { stmt (SEP stmt)* }
stmt         := let-stmt | if-stmt | foreach-stmt | retry-stmt
              | command-stmt | 'break' | 'continue'
let-stmt     := 'let' IDENT '=' expr
if-stmt      := 'if' expr block { 'elif' expr block } [ 'else' block ]
foreach-stmt := 'foreach' IDENT 'in' expr block
retry-stmt   := 'retry' block 'until' expr
block        := '{' { stmt (SEP stmt)* } '}'
command      := IDENT arg*
arg          := WORD | QUOTED | varref | cmdsub | adapter

expr         := or
or           := and ('or' and)*
and          := not ('and' not)*
not          := 'not' not | comparison
comparison   := unary (BINOP unary)?
unary        := UNARY-PRED term | thread
thread       := term ('|>' command)*
term         := literal | varref | cmdsub | adapter | '(' expr ')'
              | 'timeout' DURATION | 'iteration' INTEGER
literal      := WORD | QUOTED
varref       := '$' IDENT path? | '${' IDENT path '}'
cmdsub       := '[' (expr | command) ']'
path         := ('.' IDENT | '[' DIGITS ']')+
adapter      := 'file' ':' varref

BINOP        := 'eq' | 'ne' | 'lt' | 'le' | 'gt' | 'ge'
              | '==' | '!=' | '<' | '<=' | '>' | '>='
UNARY-PRED   := 'true' | 'false' | 'not-empty'
SEP          := newline | ';'
```

Comments begin with `#` (outside quoted strings) and run to the end
of the line.

## Statements

### let

```
let NAME = EXPR
```

Binds the result of evaluating `EXPR` to `NAME` in the session. The
RHS is always an expression; literal strings, variable references,
and command substitutions all fit:

```
let iface = "eth0"                       # quoted literal
let id    = 42                           # bare literal
let prog  = [bpfman program get 123]     # command substitution
let count = $prog.record.program_id      # path on a structured var
```

If the expression produces no assignable value (e.g. a domain
command with no return, or `[help]`), `let` reports an error and
does not create the binding.

### if / elif / else

```
if EXPR { STMTS } [ elif EXPR { STMTS } ]* [ else { STMTS } ]
```

Conditions must evaluate to a `boolean` value (`OriginBool`). There
is no generic truthiness: `if $count { ... }` is a type error if
`$count` is a scalar. Write `if $count > 0 { ... }` or `if
not-empty $count { ... }` instead.

Blocks are delimited by `{` and `}`. Statements inside a block are
separated by `;` (one-liner) or newlines (multi-line). When the REPL
is used interactively, an unclosed `{` puts the line reader into a
continuation state that accumulates lines until the braces balance.

```
if $prog.record.status.kernel_seen eq true {
    bpfman show program $prog.record.program_id
} elif not-empty $prog.record.tag {
    bpfman show program $prog.record.program_id paths
} else {
    require fail
}
```

### foreach

```
foreach NAME in EXPR { STMTS }
```

Iterates a block over the elements of a list.  `EXPR` is
evaluated once, and the resulting value must be a structured
list; anything else (a scalar, a map, a nil value) is a runtime
error.  For each element, `NAME` is bound to that element in the
session and the body runs; an error from any iteration halts the
loop and propagates.

```
foreach p in [bpfman program list -o json] {
    assert ok bpfman program get $p.record.program_id
}

foreach item in [jq "." '{"items":[{"v":1},{"v":2},{"v":3}]}'] |> jq ".items" {
    dump item.v
}
```

The loop variable persists after the loop ends, holding the last
element's value, matching shell-style for-each semantics.  An
empty list runs the body zero times and leaves the loop variable
untouched.  Nested `foreach` is legal and iterates naturally.

`break` terminates the nearest enclosing `foreach`; `continue`
skips the remainder of the current iteration and advances to the
next element.  Both take no arguments and error at parse time on
trailing tokens (`break 2` is not supported).  Used outside a
`foreach`, either keyword is a runtime error citing the source
location.

```
foreach p in [bpfman program list -o json] {
    if $p.record.meta.stale eq true { continue }
    if $p.record.program_id eq $target        { break }
    assert ok bpfman program get $p.record.program_id
}
```

### retry / until

```
retry { STMTS } until EXPR
```

Runs the body repeatedly with a small backoff between iterations
until `EXPR` evaluates to true.  Body errors do **not** halt the
retry — they are expected during polling.  The body's most recent
error is carried across iterations and returned as the statement's
error if and when `EXPR` finally becomes true; a timeout-style exit
therefore surfaces the reason the body was failing at the moment
the budget ran out.

Two retry-scoped primary expressions keep `EXPR` purely
expression-based:

- `timeout DURATION` — true once the retry has been running for at
  least `DURATION`.  `DURATION` is any Go duration literal
  (`30s`, `5m`, `200ms`, `1h30m`).
- `iteration INTEGER` — true once the retry has executed at least
  `INTEGER` iterations.

Both compose with the full expression grammar, so combined
conditions read naturally:

```
retry {
    let c = [exec bpftool map dump id $mid -j] |> jq "..."
    assert $c > 100
} until timeout 30s                                      # pure timeout

retry { let phase = [bpfman doctor checkup] |> jq ".phase" }
until $phase eq ready or timeout 60s                     # condition or timeout

retry { require ok exec ping -W 1 198.51.100.2 }
until iteration 5                                        # retry up to five times

retry { ... }
until $done and not timeout 10s                          # "done within 10s"
```

Outside a retry body (or an `until` clause attached to one),
`timeout` and `iteration` are runtime errors with a source
location, since there is no retry clock to measure against.

Nested `retry` is supported: the inner retry gets its own clock
and iteration counter, independent of any outer retry.

Ctrl-C interrupts the process; there is no context plumbing
through the evaluator yet, so a retry whose `until` never
becomes true loops until the process is killed.  Always include
a cap (`or timeout DURATION` or `or iteration N`) in long-running
retries.

### Plain commands

Any statement that is not `let` or `if` is a command. Domain
commands are introduced by the `bpfman` prefix:

```
bpfman program list
bpfman show program 42 maps
bpfman doctor checkup
```

Shell-language commands are bare: `assert`, `require`, `exec`,
`json`, `file`, `dump`, `vars`, `unset`, `source`, `alias`,
`aliases`, `unalias`, `help`, `version`.

## Expressions

### Literals

Bare words and quoted strings are literals. Quoted strings preserve
internal whitespace; `$` is literal inside quotes.

```
let a = foo
let b = "hello world"
```

### Variable references

`$NAME` is a bare reference. `$NAME.PATH` walks into a structured
value using dotted fields and `[N]` indexing. `${NAME.PATH}` is the
braced form and accepts the same path grammar.

```
let id  = $prog.record.program_id
let elt = ${items[0].name}
```

A bare reference to a structured variable inside an argument list
passes the full value to a typed command parser. For example, `bpfman
show program $prog` is shorthand for `bpfman show program
$prog.record.program_id` when `$prog` is an `OriginProgram` value.

### Bracketed evaluation `[...]`

Square brackets are the universal "evaluate this, give me the
value" form. The content is parsed first as an expression; if that
parse matches, the expression's value is the bracket's result. If
it does not match (for example, because the content starts with a
command name followed by its own arguments), the content is parsed
as a command invocation and dispatched — the command's return
value is the bracket's result. Domain commands and shell builtins
are both legal command forms.

```
let p = [bpfman program get 123]        # command invocation
let j = [jq "." '{"name":"test"}']      # command invocation
let r = [exec echo hello]               # command invocation
let a = [1 eq 1]                        # expression: true
let b = [$count > 0]                    # expression: boolean
let c = [$prog.id + 1]                  # expression: arithmetic
```

Bracketed evaluation is mandatory when binding a command's result
(`let p = bpfman program get 123` is a parse error — commands as
bare RHS are not allowed). It is also the canonical way to embed
an expression where the surrounding grammar expects a value:
`dump [1 eq 1]`, `foreach x in [jq ".[]" $data] { ... }`.

### Comparisons

Word operators (`eq`, `ne`, `lt`, `le`, `gt`, `ge`) compare
**textually** (lexicographic). Symbol operators (`==`, `!=`, `<`,
`<=`, `>`, `>=`) compare **numerically** (floating-point parsed from
both operands).

```
assert $prog.record.name eq tracepoint_kill_recorder   # textual
assert $count > 0                                      # numeric
assert $a lt $b                                        # textual
```

Both operand positions may be arbitrary expressions, including
command substitutions: `assert $count > [length $items]`.

### Unary predicates

- `true OPERAND` — tests whether the operand equals the string
  `"true"`.
- `false OPERAND` — tests whether the operand equals the string
  `"false"`.
- `not-empty OPERAND` — tests whether the operand is a non-empty
  string.

### Logical operators

`and`, `or`, `not` combine boolean expressions.  Both operands
of `and`/`or` must evaluate to a boolean; `not` takes a single
boolean operand.  Evaluation short-circuits: `true or X` never
evaluates `X`, and `false and X` never evaluates `X`.

Precedence, loosest to tightest: `or` → `and` → `not` →
comparison → unary predicate → `|>` → primary.  Parentheses
`(` `)` override precedence:

```
if $count > 0 and $count < 100 { ... }
if not $ready eq true { ... }                 # not ($ready eq true)
if $count == 0 or $count > 100 { ... }
if ($flag1 or $flag2) and $enabled { ... }    # parens invert the default
```

`not $a` binds looser than `$a eq b`, so `not $a eq b` reads as
`not ($a eq b)` — SQL / Python convention rather than C
convention.

## Values and origin types

Every value in the session carries an origin kind that identifies
what it represents. Origin is a closed set:

| Kind              | Producer                                  |
|-------------------|-------------------------------------------|
| `scalar`          | string/number literals, `StringValue`     |
| `boolean`         | comparison and unary predicate results    |
| `program`         | `bpfman program get`, `load file`, ...    |
| `link`            | `bpfman link get`, `link attach`          |
| `dispatcher`      | (reserved for future use)                 |
| `map`             | (reserved for future use)                 |
| `exec.result`     | `exec` and `exec status`                  |
| `unknown`         | `ValueFromJSON`, `ValueFromMap`, `jq`, untagged |

Command parsers that accept typed references (e.g. `bpfman program
get $var`) check the origin. `$link` passed where a program is
expected produces:

```
[repl] error: variable "$link" is a link; expected program
```

`unknown` acts as a wildcard; it matches any typed position and
falls through to structural extraction (path lookup).

## Namespaces

### Domain prefix

Domain commands require the `bpfman` prefix:

```
bpfman program list           # canonical
program list                  # error: not a shell command
```

The prefix is a grammar-level boundary: it keeps the shell-language
namespace (`let`, `if`, `assert`, `exec`, ...) and the domain
namespace (`program`, `link`, `dispatcher`, ...) from colliding as
either grows.

### Aliases

`alias` creates first-token shorthand for interactive convenience:

```
alias b = bpfman
b program list
b show program 42
```

Aliases rewrite only the first token, non-recursively, and may not
shadow shell keywords (`let`, `if`, `elif`, `else`, `assert`,
`exec`, ...) or shell commands.

## Completion

Interactive completion fires on Tab and offers:

- Top-level keywords and command names (`let`, `if`, `bpfman`,
  `assert`, ...).
- Sub-commands after `bpfman` (`program`, `link`, `show`, ...).
- Session variables after `$`.
- Subview names after `bpfman show program <id>` (`maps`, `links`,
  `paths`, ...).
- Source files after `source`.

Alias-defined shortcuts complete against the aliased expansion.

## Assertions

### Value-based assertions (expression form)

Binary comparisons and unary predicates are assertions over values:

```
assert $count > 0
assert $prog.record.name eq tracepoint_kill_recorder
assert not-empty $ipv6_addr
assert $a == $b
```

The comparison evaluator is the same one used by `if` conditions.

### Command-based assertions

Two forms check command exit status rather than inspect a value:

```
assert ok   exec ip link show dummy0
assert fail exec ip link show does-not-exist
```

### Negation

`not` negates a unary-predicate or command-based assertion. It is
**not** allowed before binary comparisons — use the complementary
operator instead (e.g. `ne` for `not eq`):

```
assert not-empty $name     # unary predicate
assert not not-empty $dry  # negated unary predicate
assert not ok exec bogus   # negated command assertion
```

### require vs assert

`assert` records failures and continues; `require` halts execution
on failure. Both share the same surface grammar.

## Variables and the session

- `let NAME = EXPR` binds.
- `vars` lists bound variables.
- `dump NAME` pretty-prints a variable.
- `unset NAME` removes a binding.
- Variables persist for the life of the session.

## Running scripts

- `source FILE` executes a file's contents as REPL input. Errors in
  script mode halt execution (with `require` or with any top-level
  error); errors in interactive mode are reported and the session
  continues.
- The CLI entry point `bpfman --file PATH` (or `-f`) runs a script
  and exits with a non-zero status on failure.

## Non-goals

The following are deliberately out of scope:

- Loops (`for`, `while`), early return, non-local control flow.
- User-defined functions, closures, or modules.
- Operator overloading, user-defined operators.
- String interpolation. `$` is literal inside quoted strings.
- Truthiness. Conditions must be booleans.
- A foreign runtime.
