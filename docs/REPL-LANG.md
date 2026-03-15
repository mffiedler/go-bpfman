# REPL language: design and architecture

## Summary

The REPL has a small language layer that supports interactive
exploration and batch scripting of BPF lifecycle operations:

- load a program and capture the result
- pass structured values from that result to later commands
- attach, inspect, detach, and delete using explicit data flow
- narrow output with format flags and sub-views

The language has three statement forms (`let`, `set`, plain command),
structured variables with dotted field access, and typed argument
expansion. It is intentionally small: line-oriented, command-oriented,
no expressions, no block structure.

Example session:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
let link = link attach tracepoint --tracepoint sched/sched_switch $prog
show program $prog maps
link detach $link
program delete $prog.record.program_id
```

## Design decisions

### 1. Explicit assignment is the only binding mechanism

There are no automatic variables such as `$_`, `$1`, `$PROG_ID`,
or "last result" slots. All binding is through `let` (command result)
or `set` (scalar literal).

**Why:**

- Data flow is visible in the script.
- It scales beyond one-liners without ambiguity.
- "undefined variable prog" is clearer than implicit result-slot
  behaviour.
- It supports structured results naturally.

### 2. Variables hold structured values

Variables store `Value`, a shell runtime type backed by a JSON-like
tree (`map[string]any`). Domain result structs are JSON-round-tripped
into `Value` at the command boundary. `Value` supports field lookup by
name, array indexing, scalar leaf conversion to string, and optional
origin metadata for type-safety checks.

### 3. Two binding forms: `let` and `set`

`let` binds the structured result of a command:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
```

`set` binds a scalar literal or the expansion of a variable reference:

```
set iface = eth0
set prog_id = $prog.record.program_id
```

The distinction is syntactic: `let` executes a command, `set` assigns
a value directly.

### 4. Bare structured references are passed through

A bare reference to a structured variable (`$prog` with no field path)
is not an error. Expansion preserves it as a `StructuredValueArg`
carrying the resolved `Value` directly. The receiving command parser
decides how to use it -- typically by extracting a domain-specific
field such as `.record.program_id` for programs or `.record.id` for
links.

This enables concise command syntax:

```
show program $prog
link detach $link
```

rather than requiring explicit path access for every use:

```
show program $prog.record.program_id
link detach $link.record.id
```

The command parser performs origin-type checking on the `Value` to
ensure, for example, that a link variable is not passed where a
program is expected.

### 5. Output format is a command-level flag

Commands that produce rendered output accept `-o <format>` to control
the output format. Supported formats are `table` (default), `wide`,
`json`, and `jsonpath=<expr>`. Sub-views (e.g. `maps`, `links`,
`paths` for `show program`) are positional arguments to the command,
not language-level syntax.

### 6. JSONPath is an escape hatch

Ordinary field access uses dotted syntax (`$prog.record.name`).
JSONPath is available via `-o jsonpath='{.maps[*].name}'` for complex
selection. It is not the primary language of the shell.

## Syntax

### Statements

```
line     := let-stmt | set-stmt | command-stmt
let-stmt := "let" IDENT "=" TOKEN+
set-stmt := "set" IDENT "=" TOKEN
command  := TOKEN+
```

### Variable references

```
varref   := '$' ident path? | '${' ident path? '}'
path     := segment+
segment  := '.' ident | '[' digits ']'
ident    := [a-zA-Z_][a-zA-Z0-9_]*
digits   := [0-9]+
```

Both bare and braced forms accept the same path grammar. Bare
`$prog.record.program_id` is the normal form. Braces are supported
for disambiguation but not promoted; they become essential only if the
language later grows string interpolation.

Variable references are lexed as single `TokenVarRef` tokens. They
are never re-lexed after expansion.

### Comments

A `#` and everything after it is discarded, unless the `#` appears
inside a quoted string. Comments are stripped during tokenisation.

### Quoting

Single-quoted and double-quoted strings are supported. The delimiters
are stripped by the tokeniser. `$` is literal inside quotes.

## Token types

The tokeniser (`shell.Tokenise`) produces four token kinds:

- `TokenWord`: unquoted word (command name, flag, path, numeric ID)
- `TokenAssign`: standalone `=` at a token boundary
- `TokenVarRef`: variable reference (`$prog.id`, `${prog.maps[0]}`)
- `TokenQuoted`: quoted string with delimiters stripped

`TokenAssign` is only valid inside `let` and `set` expressions. Its
presence in a plain command is a syntax error.

## Expansion and the Arg types

`Session.Expand` resolves variable references in a token slice and
returns `[]Arg`, a typed representation of the expanded arguments.
`Arg` is a sealed sum type with four variants:

- **`WordArg`**: literal command text supplied by the user. Never
  came from a variable reference.
- **`QuotedArg`**: a quoted string literal, preserving the syntactic
  distinction from unquoted words.
- **`ScalarValueArg`**: a value produced by variable expansion. The
  original variable reference has been resolved to a string. This
  covers both scalar variables (`$count`) and dotted paths into
  structured values (`$prog.record.program_id`).
- **`StructuredValueArg`**: a bare reference to a structured variable
  (`$prog` with no field path). The resolved `Value` is carried
  directly so that command parsers can extract the relevant field
  without re-parsing dollar prefixes.

This is the contract between `shell` and its clients. Clients
receive typed, structured arguments and never need to re-discover
variable references from strings.

### Expansion rules

Expansion operates token by token. For each `TokenVarRef`:

1. Look up the variable name in session state.
2. If a field path is present, walk the structured value and resolve
   to a scalar. The result becomes a `ScalarValueArg`.
3. If the variable is structured and has no field path, the `Value`
   is preserved as a `StructuredValueArg`.
4. If the variable is scalar and has no field path, the string value
   becomes a `ScalarValueArg`.
5. Undefined variables, missing fields, and invalid indices are
   errors.

Non-variable tokens pass through as `WordArg` or `QuotedArg`. The
expanded result is never re-lexed.

Expansion occurs on the right-hand side of `let`, on the value of
`set`, and on all tokens for plain commands. Expansion never occurs
on the left-hand side of a binding.

## Command result model

Commands that support variable binding return a `shell.Value` from
execution. Commands that do not produce a bindable result return an
empty value. Assignment to such a command is an error:

```
[repl] error: command produced no result to assign
```

| Command          | Assignable | Returned Value                          |
|------------------|------------|-----------------------------------------|
| `load file`      | yes        | loaded program(s) as structured value   |
| `load image`     | yes        | loaded program(s) as structured value   |
| `link attach`    | yes        | attached link as structured value        |
| `program get`    | yes        | program record as structured value      |
| `link get`       | yes        | link record as structured value          |
| `show program`   | no         | rendered output only                    |
| `list programs`  | no         | rendered output only                    |
| `list links`     | no         | rendered output only                    |
| `program unload` | no         | side effect only                        |
| `program delete` | no         | side effect only                        |
| `link detach`    | no         | side effect only                        |
| `link delete`    | no         | side effect only                        |
| `gc`             | no         | rendered output only                    |
| `doctor`         | no         | rendered output only                    |

## Execution model

The REPL processes one line at a time through a fixed sequence of
phases. The language is line-oriented: no multi-line blocks, operator
precedence, pipelines, or nested expressions.

### Pipeline

```
read line
  -> shell.Tokenise           (string -> []Token)
  -> shell.ParseStmt          ([]Token -> Stmt)
  -> session.Expand             ([]Token -> []Arg)
  -> shell or domain dispatch
       shell: replShellCmd      (assert, require, dump, help, ...)
       domain: parseCommand     ([]Arg -> Command)
             + execCommand      (Command -> Value, render)
  -> bind variable (if let)
```

### Phase detail

1. **Read line.** The line editor provides prompt, history, and tab
   completion.

2. **Tokenise.** `shell.Tokenise` produces `[]Token` from the
   input string. Comments are stripped. Variable references are lexed
   as single `TokenVarRef` tokens.

3. **Parse statement.** `shell.ParseStmt` classifies the token
   sequence into one of three `Stmt` variants: `LetStmt`, `SetStmt`,
   or `CommandStmt`.

4. **Expand variables.** `session.Expand` resolves variable
   references and returns `[]Arg`. Scalar references become
   `ScalarValueArg`. Bare structured references become
   `StructuredValueArg`. Literal tokens become `WordArg` or
   `QuotedArg`. The result is never re-lexed.

5. **Dispatch.** Shell-language commands (`assert`, `require`,
   `dump`, `help`, `source`, `unset`, `vars`, `version`) are handled
   directly by `replShellCmd`. Domain commands flow to the typed
   command pipeline.

6. **Parse command.** `parseCommand` routes expanded `[]Arg` to the
   per-command parser based on keyword patterns, returning a typed
   `Command` node with fully resolved, validated fields.

7. **Execute command.** `execCommand` dispatches on the `Command`
   type-switch, calling the per-command executor against
   `manager.Manager`. Execution is the only phase that produces side
   effects.

8. **Bind variable.** For `let` statements, the returned `Value` is
   stored in session state. For `set` statements, the expanded value
   is stored directly.

9. **Render.** Output is formatted according to the command's output
   flags (`-o table`, `-o json`, etc.) and written to the CLI output.

### Phase ordering

Variable expansion (phase 4) completes before command parsing
(phase 6). Command parsers receive `[]Arg` with all variable
references already resolved. They never encounter variable syntax
directly.

Parsing (phase 6) completes before execution (phase 7). The
`parseCommand` / `execCommand` split means command routing is
decoupled from execution. A command can be parsed without executing
it.

## Package boundary

### `shell` owns language mechanics

The `shell` package is pure (no I/O, standard library only). It
provides:

- **Tokens**: `Token`, `TokenKind`, `Tokenise`, variable-reference
  lexing, comment stripping, identifier validation.
- **Statements**: `Stmt` (`LetStmt`, `SetStmt`, `CommandStmt`),
  `ParseStmt`.
- **Expansion**: `Session.Expand` returns `[]Arg`.
- **Arg types**: `WordArg`, `QuotedArg`, `ScalarValueArg`,
  `StructuredValueArg`.
- **Session and Value**: variable store, structured value type,
  field lookup, origin metadata.

`shell` knows nothing about bpfman commands. It never requires
downstream clients to re-parse `$` prefixes out of strings. It hands
clients typed, structured arguments.

### `cmd/bpfman/` owns domain semantics

The REPL client layer provides:

- **Shell-language dispatch**: `replShellCmd` handles `assert`,
  `require`, `dump`, `help`, `source`, `unset`, `vars`, `version`.
- **Command routing**: `parseCommand([]Arg) (Command, error)` matches
  keyword patterns and delegates to per-command parsers.
- **Typed command nodes**: `Command` interface with concrete types
  (`ShowProgramCommand`, `LoadFileCommand`, `LinkAttachCommand`,
  etc.). Each carries fully resolved, validated fields.
- **Command execution**: `execCommand` dispatches on the `Command`
  type-switch.
- **Rendering and formatting**: output format handling, tab writers,
  column layout.

The client layer does not tokenise or parse shell-language syntax. It
consumes typed statements and arguments from `shell` and parses only
bpfman command semantics.

## Error behaviour

Errors are explicit.

- undefined variable: `"undefined variable: foo"`
- missing field: `"field not found: bar"`
- invalid index: `"index 3 out of range"`
- no result to assign: `"command produced no result to assign"`
- wrong origin type: `"$mylink is not a program"`
- unknown command: `"unknown command \"bogus\". Type \"help\" for
  available commands."`
- parse errors: `"show program: requires a program ID"`
- command failure during assignment: the command error is reported and
  no variable is created or updated

In interactive mode, non-fatal errors are printed and the session
continues. In script mode (sourced files), errors halt execution. The
`require` command provides fatal assertions; `assert` provides
non-fatal assertions.

## Rationale

The language is small by design:

- explicit data flow between lifecycle steps
- structured values with typed expansion
- two clean binding forms (`let` for commands, `set` for scalars)
- bare structured references passed directly to commands
- no automatic variables or implicit "current result"
- no general-purpose expression language
- no block structure or control flow

The result is a shell that remains domain-specific and readable,
while being powerful enough to load, attach, inspect, detach, and
delete objects with explicit, typed data flow. The package boundary
between `shell` (language) and `cmd/bpfman` (domain) keeps each
layer focused and independently testable.
