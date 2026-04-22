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
- A full programming language. No loops, user-defined functions,
  closures, or modules.
- A foreign runtime (Lua, TCL, Python, Starlark).

## Grammar

```
program    := { stmt (SEP stmt)* }
stmt       := let-stmt | if-stmt | command-stmt
let-stmt   := 'let' IDENT '=' expr
if-stmt    := 'if' expr block { 'elif' expr block } [ 'else' block ]
block      := '{' { stmt (SEP stmt)* } '}'
command    := IDENT arg*
arg        := WORD | QUOTED | varref | cmdsub | adapter

expr       := primary | unary | binary
primary    := literal | varref | cmdsub
unary      := UNARY-PRED primary
binary     := primary BINOP primary
literal    := WORD | QUOTED
varref     := '$' IDENT path? | '${' IDENT path '}'
cmdsub     := '[' command ']'
path       := ('.' IDENT | '[' DIGITS ']')+
adapter    := 'file' ':' varref

BINOP      := 'eq' | 'ne' | 'lt' | 'le' | 'gt' | 'ge'
            | '==' | '!=' | '<' | '<=' | '>' | '>='
UNARY-PRED := 'true' | 'false' | 'not-empty'
SEP        := newline | ';'
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

### Command substitution

`[cmd args...]` dispatches a command inside an expression and
resolves to the command's return value. Domain commands and shell
builtins are both legal inside the brackets.

```
let p = [bpfman program get 123]
let j = [jq "." '{"name":"test"}']
let r = [exec echo hello]
```

Command substitution is mandatory when binding a command's result:
`let p = bpfman program get 123` is a parse error (operand count
too large); wrap in brackets.

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
