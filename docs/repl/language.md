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
              | expr-stmt | command-stmt | 'break' | 'continue'
expr-stmt    := expr    (* when the first token leads expression *)
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
comparison   := additive (BINOP additive)?
additive     := multiplicative (('+' | '-') multiplicative)*
multiplicative := predicate (('*' | '/' | '%') predicate)*
predicate    := UNARY-PRED term | negate
negate       := '-' negate | thread
thread       := term ('|>' command)*
term         := literal | varref | cmdsub | adapter | '(' expr ')'
              | 'timeout' DURATION | 'iteration' INTEGER
literal      := WORD | QUOTED
varref       := '$' IDENT path? | '${' IDENT path '}'
cmdsub       := '[' (expr | command) ']'
path         := ('.' IDENT | '[' DIGITS ']')+
adapter      := 'file' ':' varref

BINOP        := '==' | '!=' | '<' | '<=' | '>' | '>='
UNARY-PRED   := 'true' | 'false' | 'not-empty'
SEP          := newline | ';'
```

Comments begin with `#` (outside quoted strings) and run to the end
of the line.

A backslash at the very end of a line (optionally followed by `\r`)
is a line continuation: the backslash and the following newline are
consumed as a single whitespace run. The rule applies everywhere
the tokeniser runs — at statement level, inside blocks, and inside
`[...]` substitutions — so multi-line commands read the same way
whether they are bare or bracketed:

```
bpfman load file                        \
    --path ./prog.o                     \
    --programs xdp:xdp_stats            \
    -m owner=test-team

let prog = [bpfman load file            \
    --path ./prog.o                     \
    --programs xdp:xdp_stats]
```

Inside a quoted string, `\` is an escape character (double-quoted
strings) or a literal byte (single-quoted strings); the
continuation rule only applies outside quotes.

A line is parsed as an **expression statement** when its first
token can only start an expression: `$var`, `[cmd]` or `[expr]`,
`"string"`, `'string'`, `(`, `not`, `not-empty`, `true`, or
`false`.  The whole line is then parsed against the expression
grammar and, at the REPL prompt, its resulting value is
auto-printed.  Scripts run by `source` or `-f` auto-print too --
bare expression lines are only ever written intentionally.  Every
other leading token (bare words, keywords) routes to the command
grammar.

```
$prog                    # prints the binding
$prog.record.program_id  # prints the scalar
$prog.kind == "xdp"      # prints a boolean
[1 == 1]                 # prints true
(not-empty $name)        # prints a boolean
```

To print a bare literal (or an expression whose first token does
not lead an expression), wrap it: `[1 == 1]`, `["hello"]`. To run
a command, start the line with its name as usual.

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
if $prog.record.status.kernel_seen {
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

Iterates a block over the elements of an array. `EXPR` must
evaluate to a concrete array; anything else (a scalar, a map, a
nil value, a stream) is a runtime error. For each element, `NAME`
is bound to that element and the body runs; an error from any
iteration halts the loop and propagates.

`EXPR` is the full expression grammar, including parenthesised
threading and command substitution. The two canonical shapes:

```
# Two-step form: shape the array in a let, iterate the binding.
let listed = [bpfman program list -o json]
let programs = $listed |> jq ".programs"
foreach prog in $programs {
    assert ok bpfman program get $prog.record.program_id
}

# Inline form: parenthesise the threading pipeline at the call
# site when the intermediate binding has no other use.
foreach prog in ($listed |> jq ".programs") {
    assert $prog.record.kind == kprobe
}
```

The "concrete array" requirement is deliberate. `jq` may do rich
selection and produce streams of values internally, but value
generation is not foreach's job. Avoid `jq ".programs[]"` as the
upstream of a foreach binding: `[]` is jq's stream form, and
conflating jq's stream model with foreach's iteration model
fuzzes the boundary. Wrap the stream into an array explicitly
when needed (`jq "[.programs[]]"`), or use `jq ".programs"` on
data that already exposes the array.

The loop variable is body-scoped: it is defined only inside the
block and disappears when the loop ends. If the same name was
already bound in the enclosing scope, that prior binding is
restored on exit. An empty list runs the body zero times; the
prior binding (or absence) is preserved unchanged. Nested
`foreach` is legal and iterates naturally.

`break` terminates the nearest enclosing `foreach`; `continue`
skips the remainder of the current iteration and advances to the
next element.  Both take no arguments and error at parse time on
trailing tokens (`break 2` is not supported).  Used outside a
`foreach`, either keyword is a runtime error citing the source
location.

```
foreach p in [bpfman program list -o json] {
    if $p.record.meta.stale { continue }
    if $p.record.program_id == $target        { break }
    assert ok bpfman program get $p.record.program_id
}
```

### retry / until

```
retry { STMTS } until EXPR
```

Runs the body repeatedly with a small backoff between iterations
until `EXPR` evaluates to true. Body errors do **not** halt the
retry: they are expected during polling. When the retry exits
because `EXPR` legitimately became true, the statement returns
nil and any prior body error is discarded. When the retry exits
because a `timeout` or `iteration` cap inside `EXPR` forced it
true, the body's most recent error is returned as the
statement's error so the diagnostic surfaces the reason the body
was failing at the moment the budget ran out.

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
until $phase == ready or timeout 60s                     # condition or timeout

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
`json`, `file`, `print`, `vars`, `unset`, `source`, `alias`,
`aliases`, `unalias`, `help`, `version`.

## Expressions

### Literals

Bare words and quoted strings are literals. Single-quoted strings
are fully literal — nothing inside them is interpreted, including
`$`. Double-quoted strings preserve internal whitespace and
support `${...}` interpolation; a bare `$` in a double-quoted
string that is not the start of a `${...}` form is a lex-time
error. Use single quotes when you want a literal `$`.

```
let a = foo
let b = "hello world"
let c = 'literal $ and # and ${...} inside single quotes'
```

### String interpolation

`"${...}"` splices a value into a double-quoted string. The body
inside the braces is one of three shapes:

1. A bare variable reference: `${name}`, `${name.path}`,
   `${name[0]}`. No `$` prefix — the braces are the signal that
   the content is a variable name. This matches bash, zsh, Perl,
   Tcl, Ruby, and Python f-strings.
2. An expression substitution: `${[[expr]]}`. Used when the
   interpolated value is arithmetic or a comparison, since the
   bare-identifier form does not accept operators.
3. A command substitution: `${[cmd args]}`. Used when the
   interpolated value comes from a command's output.

```
let n    = 60
let wait = "${n}s"                         # "60s"  (form 1)
let path = "/sys/fs/bpf/prog-${id}/map"    # path with a variable slot
let mix  = "${[[$n * 2]]}s"                # arithmetic via [[...]] (form 2)
let cap  = "count=${[jq '.total' $data]}"  # command result (form 3)
```

Rules:

- Only the `${...}` form is recognised. Bare `$var` inside double
  quotes errors with a pointer to the braced form.
- Writing `${$name}` is a parse error — use `${name}`. Every
  shell-flavoured language spells it that way, and having two
  spellings of the same thing is a maintenance tax for readers.
- Single-quoted strings never interpolate. `'${x}'` is five
  characters, not a lookup of `x`.
- Structured values render as compact one-line JSON:
  `"${r}"` where `$r` is `{"a":1,"b":2}` splices that exact text.
  Arrays render as `[1,2,3]`. Use `${r.path}` to splice a single
  field instead; use `${[jq "." $r]}` if you want pretty-printed
  output.
- Nil renders as `null` so the output string is always
  well-formed.
- Double-quoted strings support C-style escape sequences:
  `\n`, `\t`, `\r`, `\\`, `\"`, `\$`. Single-quoted strings
  are fully literal — nothing is interpreted.
- No escape sequence for `$`, `"`, or any other character. Use
  single quotes if a literal `$` or `${` needs to appear.

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
let a = [1 == 1]                        # expression: true
let b = [$count > 0]                    # expression: boolean
let c = [$prog.id + 1]                  # expression: arithmetic
```

Threading with `|>` (see below) stays in single brackets
because the RHS is a command call whose flags and paths need
shell tokenisation: `[$x |> jq -c "."]` works; the strict
tokeniser inside `[[...]]` would split `-c` into `-` and `c`
and break the flag.

Bracketed evaluation is mandatory when binding a command's result
(`let p = bpfman program get 123` is a parse error — commands as
bare RHS are not allowed). It is also the canonical way to embed
an expression where the surrounding grammar expects a value:
`print [[1 == 1]]`, `foreach x in [jq ".[]" $data] { ... }`.

### Expression islands `[[...]]`

`[[expr]]` is an *expression island*: a parser-mode delimiter
that switches the inner content from command-and-argument
grammar to expression grammar, regardless of the surrounding
context. It is the language's equivalent of bash's `(( ... ))`
for arithmetic; the role is the same, switch-into-expression-
mode, and the framing is worth keeping in mind:

> `[[...]]` is not a subshell, not a command, and not jq. It is
> an expression island.

The DSL has two competing syntactic worlds. Command position
treats bare words as command names, flags, paths, and IDs:
`bpfman program get 42`, `exec ip link show dummy0`, `--path
./prog.o`. Expression position treats them as operators,
literals, and operands: `$after - $before`, `$count > 0`. Most
constructs commit to one world or the other from context (`let
NAME = EXPR` is expression-mode; the args after a command name
are command-mode). `[[...]]` opts into expression-mode inside a
command-mode region:

```
print [[$after - $before]]
bpfman show program [[$base + 1]]
let delta = [[$after - $before]]
let r = "${[[$n * 2]]}s"
```

The single-bracket `[expr]` form is auto-detecting: it tries
expression first and falls back to command. Use `[[expr]]` when
the content might otherwise be misread as command syntax, or
when you want the strict expression tokeniser, which keeps `-`
and `/` as operator tokens rather than letting them attach to
adjacent words as flags or path separators. Use `[cmd args]`
for explicit command substitution including threading
(`[$x |> jq -c "."]`), because the strict tokeniser inside
`[[...]]` would split `-c` into `-` and `c` and break the flag.

### Bracket meanings, in one place

The DSL's three bracket forms each have one job, and the
boundary is worth pinning so the meanings stay crisp:

| Form          | Meaning                                            |
|---------------|----------------------------------------------------|
| `[cmd args]`  | Command capture: run a command, take its value     |
| `[[expr]]`    | Expression island: parser-mode switch, no commands |
| `jq "[...]"`  | Array/object construction (inside the jq filter)   |

`[cmd args]` and `[[expr]]` are DSL surface; `[...]` inside a
jq filter is jq's own array literal and lives inside the quoted
filter string. The DSL has no array-literal syntax of its own:
data construction is jq's job.

```
let xs = $listed |> jq "[.items[] | select(.v > 0)]"
let delta = [[$after - $before]]
let prog = [bpfman program get $pid]
```

That split reinforces the orchestration / domain / projection
rails: the DSL composes commands and expressions, jq composes
data.

### Comparisons

The DSL provides one comparison family: `==`, `!=`, `<`, `<=`, `>`,
`>=`. Semantics is selected by the operand types at evaluation time,
not by the operator's spelling -- jq's strict-equality model:

- Both numeric (`json.Number`, `float64`) -- compare as floats.
- Both stringy (plain strings) -- compare textually (lexicographic).
- Both boolean -- only `==` and `!=` are defined; ordering errors.
- Mixed kinds -- error rather than silent false. Coerce explicitly
  via `[$x |> jq tonumber]` to compare stringy numeric input
  (e.g. `exec` stdout) against a number.

Unquoted literals are typed by shape: `5` is a number, `true` and
`false` are booleans, `"5"` (quoted) is a string. So
`assert $prog.record.name == tracepoint_kill_recorder` compares text;
`assert $count > 0` compares numbers; `assert $kind == fentry`
compares text. Both operand positions may be arbitrary expressions,
including command substitutions: `assert $count > [length $items]`.

### Arithmetic

Five binary operators and unary negation:

- `a + b` — addition.
- `a - b` — subtraction (with whitespace around `-`).
- `a * b` — multiplication.
- `a / b` — division.  `a / 0` is a runtime error.
- `a % b` — modulo (Go's `math.Mod`; the result takes the sign of
  the dividend, so `-7 % 3` is `-1`, not `2`).  `a % 0` is a
  runtime error.
- `-a` — unary negation.  Stacks right-associatively: `- -$x`.

Arithmetic is **numeric only**.  Both operands are parsed via
`strconv.ParseFloat`, so `"abc" + 1` is a runtime error rather
than a string concatenation.  Internally every result is a
`float64`; integer-valued results render without a trailing
`.0` (`5 * 2` prints as `10`).  `5 / 2` is `2.5`; users who want
integer division can pipe through `jq`
(`[jq "(.a / .b) | floor" ...]`).

Whitespace rules around binary operators are split by operator:

- `+`, `*`, `%` are tokeniser-level word terminators.  `1+1`,
  `$x*2`, and `7%3` all tokenise correctly as three tokens, so
  whitespace around them is optional.
- `-` and `/` stay word-interior characters.  `-` is part of
  negative literals (`-3`), short flags (`-x`), and long flags
  (`--path`); `/` is part of file paths (`/sys/fs/bpf`).
  Arithmetic expressions using `-` or `/` therefore **require
  whitespace** around the operator.  `$x - 1` parses as
  subtraction; `$x -1` tokenises as two adjacent primaries and
  is a parse error.  The error message points at whitespace.

Unary negation follows the same rule: `-3` with no space is a
single-WORD negative literal; `- 3` with space is
`NegateExpr(3)`.

Precedence (tightest arithmetic rung first): unary negation →
multiplicative (`*`, `/`, `%`) → additive (`+`, `-`).  All three
rungs bind **tighter than comparison**, so `$x + 1 == 5` reads
as `(($x) + 1) == 5`. Parenthesise to force a different shape:
`($a + $b) / 2`.

```
let count    = 10
let doubled  = $count * 2            # 20
let average  = ($a + $b) / 2         # halfway between a and b
let mod      = 7 % 3                 # 1
let neg      = -$count               # -10

$count + 1                           # auto-prints 11
$count > $doubled / 3                # auto-prints a boolean
```

**Not included** (deliberately, to keep the language small):

- String concatenation via `+` (`"a" + "b"` is a runtime error).
  Concat stays via `jq` or future interpolation.
- Integer division (`//`) or power (`**`).
- Bitwise operators (`&`, `|`, `^`, `~`, `<<`, `>>`).
- In-place forms (`+=`, `-=`, etc.).  The language has no
  mutation story beyond `let`.
- Arithmetic builtins (`min`, `max`, `abs`, `floor`, `ceil`).
  Covered by `jq` for now.
- Comparison chains (`1 < $x < 10`).  Comparison stays binary.

### Unary predicates

- `not-empty OPERAND` -- tests whether the operand is a non-empty
  string.

For boolean truthiness, use the operand directly as a single-arg
expression assertion: `assert $flag` reads the bool via `AsBool`,
`assert not $flag` negates. Bare `true` and `false` are boolean
literals: `assert true` trivially passes, `assert false` fails.
The explicit comparison `assert $flag == true` is still valid but
redundant when `$flag` is already a `BoolValue`.

### Logical operators

`and`, `or`, `not` combine boolean expressions.  Both operands
of `and`/`or` must evaluate to a boolean; `not` takes a single
boolean operand.  Evaluation short-circuits: `true or X` never
evaluates `X`, and `false and X` never evaluates `X`.

Precedence, loosest to tightest: `or` → `and` → `not` →
comparison → additive (`+`, `-`) → multiplicative (`*`, `/`,
`%`) → unary predicate → unary negate (`-`) → `|>` → primary.
Parentheses `(` `)` override precedence:

```
if $count > 0 and $count < 100 { ... }
if not $ready { ... }                         # truthiness via AsBool
if $count == 0 or $count > 100 { ... }
if ($flag1 or $flag2) and $enabled { ... }    # parens invert the default
```

`not $a` binds looser than `$a == b`, so `not $a == b` reads as
`not ($a == b)` -- SQL / Python convention rather than C
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

`unknown` acts as a wildcard so values produced by `jq`,
`ValueFromJSON`, or `ValueFromMap` (which leave the kind untagged)
can flow into typed command positions without an explicit cast.
The fallback follows a single rule: each typed-origin parser
declares the dotted path it would have used to extract the scalar
out of its native value (for example `record.program_id` for the
program-ID position, `record.id` for the link-ID position), and an
`unknown` value is walked using that path. If the path resolves to
a scalar of the expected shape, the call proceeds. If the path is
missing or the leaf is not a scalar, the parser errors with a
message naming both the variable and the path it tried, so a user
who passed the wrong shape sees exactly what was expected:

```
[repl] error: variable "$x" is unknown; expected a path lookup at
    "record.program_id" to a numeric ID, but the field is missing
```

There is no list of fallback paths or ambiguity resolution: one
typed position, one path, and a clear error if it does not match.
This keeps the wildcard a convenience for jq output, not a
type-erasure escape hatch.

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
assert $prog.record.name == tracepoint_kill_recorder
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
**not** allowed before binary comparisons -- use the complementary
operator instead (e.g. `!=` for `not ==`):

```
assert not-empty $name     # unary predicate
assert not not-empty $dry  # negated unary predicate
assert not ok exec bogus   # negated command assertion
```

### require vs assert

The distinction is **fatal vs non-fatal**, not precondition vs
assertion in the abstract.

- `require EXPR` -- this must be true for the script to continue.
  On failure, render the failure context and halt immediately.
  Use for setup conditions whose violation makes the rest of the
  script meaningless: a load that produced no program, a path
  that does not exist, a variable that came back nil.

- `assert EXPR` -- record the failure on a per-session counter,
  render the failure context, but continue. At end of script-mode
  execution the counter is checked, and a non-zero count produces
  a non-zero exit status. Use for test expectations that should
  collect failures so a single run reports all of them, not just
  the first.

The distinction matters most inside loops:

```
foreach prog in $programs {
    assert $prog.status.kernel_seen
    assert $prog.record.meta.name != ""
}
```

A failed `assert` records the diagnostic and the loop keeps
running; the script reports every program that violated the
expectation. With `require` in the same place, the first failure
would halt and the rest of the programs go unchecked.

Setup, by contrast, wants the opposite: stop early, before the
test runs against an inconsistent state.

```
let prog = [bpfman program load file --path ./prog.o ...]
require not nil $prog
```

Both verbs share the same surface grammar. Parse errors, type
errors, and unhandled runtime errors halt the script in script
mode the same way `require` failures do.

## Variables and the session

- `let NAME = EXPR` binds.
- `vars` lists bound variables.
- `print $NAME` pretty-prints a variable's value.  `print` takes an
  expression, not a name: `print foo` prints the literal string
  `foo`; to see a binding, dereference with `$`.
- `unset NAME` removes a binding.  `unset` operates on *names*, so
  the bare form is the correct one here; `$` is not used.
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
- Truthiness. Conditions must be booleans.
- A foreign runtime.
