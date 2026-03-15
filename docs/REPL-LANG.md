# REPL language: variables, field access, and projection

## Summary

The REPL needs a small amount of language support so that users can
explore state interactively today and, as lifecycle commands are
added, drive a full lifecycle in scripts:

- load a program
- capture the result
- pass fields from that result to later commands
- inspect only the subset of information relevant to the current step
- delete, and eventually detach, using explicit data flow

This document proposes three related features:

1. **Explicit assignment** for capturing command results into
   variables.
2. **Structured field access** for using selected fields from those
   variables in later commands.
3. **Projection** for narrowing large command outputs during
   exploration.

The design is intentionally small. Variables are explicit. There are
no implicit or automatic variables. Ordinary access uses dotted field
syntax. JSONPath remains available only as an advanced escape hatch
for complex selection.

## Motivation

The REPL already supports commands such as load, delete, show, and
list, but without variable binding it cannot express a real lifecycle.

A useful exploratory or test-oriented session should be able to:

- load a program
- capture the resulting program identity
- attach that program using fields from the load result
- inspect only the fields or subviews of interest
- detach the resulting link
- delete the program

For example:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
let link = link attach tracepoint --tracepoint sched/sched_switch --program-id $prog.id
show program $prog.id maps
link detach $link.id
program delete $prog.id
```

Without explicit data flow, the shell remains useful for one-off
inspection but cannot support a full lifecycle cleanly.

A second problem is output size. Commands such as `show program`
often return more information than is needed in the moment. In an
exploratory shell, users frequently want only a subset of fields.

For example:

```
show program $prog.id select id,name,type
show program $prog.id maps select name,pin
show dispatcher xdp nsid=1 ifindex=2 slots select slot,program_id,link_id
```

The shell therefore needs not just variables, but also a simple way
to project or narrow structured results for display.

## Design decisions

### 1. Explicit assignment is the only binding mechanism

Explicit assignment is the primary and only way to bind variables.

There are no automatic variables such as `$_`, `$1`, `$PROG_ID`,
or "last result" slots.

Example:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
show program $prog.id maps
program delete $prog.id
```

**Why explicit assignment:**

- **Data flow is obvious.** The source of each value is visible in
  the script.
- **It scales beyond one-liners.** There is no ambiguity about
  whether a reference points to the previous result, a positional
  slot, or a named value.
- **It matches the API-first design.** The shell remains a thin
  control layer over typed operations rather than becoming magical.
- **Errors are clearer.** "undefined variable prog" is much easier
  to understand than implicit result-slot behaviour.
- **It supports structured results naturally.** Once variables hold
  records rather than strings, explicit binding becomes the least
  surprising model.

**Why not automatic variables:**

Automatic variables encourage scripts that are short-lived,
positional, and difficult to read once they grow beyond a few
lines. They are convenient for demos but poor as the primary
language model. Given the choice, explicit assignment alone is
sufficient.

### 2. Variables hold structured values

Variables store structured command results, not plain strings.

A command such as `load file` returns a shell-facing result object.
That object is what gets bound to the variable.

For example, `load file` might produce a result conceptually like:

```go
type LoadResult struct {
    ID   uint32 `json:"id"`
    Name string `json:"name"`
    Type string `json:"type"`
}
```

Then:

```
prog = load file --path foo.o ...
```

binds a structured value to `prog`, and later commands can access
its fields:

```
show program $prog.id
link attach tracepoint --tracepoint sched/sched_switch --program-id $prog.id
```

The design requirement is that variables support structured field
lookup. The internal representation may be:

- a `map[string]any`
- a dedicated dynamic value tree
- JSON round-tripping from typed result structs

JSON round-tripping is a reasonable implementation strategy, but it
is not itself the design goal. The design goal is uniform structured
field access over shell-facing result values.

### 3. Projection is a first-class part of the shell model

This shell is exploratory. Users often want only part of a large
result.

Therefore the language should support projection for display.

Examples:

```
show program $prog.id select id,name,type
show program $prog.id maps select name,pin
show dispatcher xdp nsid=1 ifindex=2 slots select slot,program_id
```

This is distinct from variable binding:

- variables carry structured results forward to later commands
- projection narrows what is displayed to the user

Both are needed.

### 4. JSONPath is an advanced escape hatch, not the main syntax

Ordinary field access uses dotted syntax:

```
$prog.id
$prog.name
$prog.maps[0].name
```

JSONPath remains useful for machine-oriented or complex selection:

```
show program 12 --format=jsonpath '{.maps[*].name}'
```

But JSONPath is not the primary language of the shell, because it
is:

- more verbose
- less readable for common cases
- more foreign to a small domain-specific shell

Dotted field access handles the common case. JSONPath handles
complex selection when needed.

## Syntax

### Assignment

Assignment syntax is:

```
let name = command args...
```

Rules:

- the first token must be the keyword `let`
- the second token must be a valid identifier:
  `[a-zA-Z_][a-zA-Z0-9_]*`
- the third token must be the literal `=`
- the remainder of the line is parsed as a command
- the left-hand side is never expanded

Examples:

```
let prog = load file --path foo.o ...
let link = link attach tracepoint --tracepoint sched/sched_switch --program-id $prog.id
```

The command still prints its normal output unless the user has
chosen a quiet mode. Assignment captures the structured result for
later use; it does not suppress normal interactive behaviour by
default.

### Variable references

Variable references use either unbraced or braced syntax:

```
$prog.id
$prog.name
${prog.id}
${prog.maps[0].name}
```

Braces are available for disambiguation when adjacent characters
would otherwise make parsing unclear.

### Field access

Field access supports:

- dotted member lookup: `.field`
- array indexing: `[n]`

Examples:

```
$prog.id
$prog.links[0].id
$prog.maps[0].name
```

### Bare variable references

Bare access to a structured variable is an error.

This is rejected:

```
$prog
```

with an error such as:

```
variable prog is structured; use field access (e.g. $prog.id)
```

This avoids introducing a magical "default" value for structured
results.

### Expansion rules

Expansion operates token by token, not by text substitution. Each
variable reference token resolves to a scalar value and becomes a
plain token. The expanded result is never re-lexed.

Resolution proceeds as follows:

1. Look up the variable name in session state.
2. If a field path is present, walk the structured value using
   dotted field lookup and array indexing.
3. The resolved value must be a scalar non-null leaf. Scalars such
   as strings, integers, and booleans are eligible for
   substitution. Attempting to substitute a null, object, or array
   value is an error unless later syntax is introduced for that
   purpose.
4. Convert the leaf to its string representation.
5. If the variable is undefined, the field is missing, or indexing
   is invalid, return an error.

Expansion occurs:

- on the right-hand side of assignments
- on all command tokens for non-assignment commands

Expansion never occurs on the left-hand side of an assignment.

## Command result model

Command execution produces a result with two independent parts:

- **AssignValue**: the shell-facing value for variable binding
  (nil if the command is not assignable)
- **RenderValue**: the structured value for rendering (nil if the
  command produces no displayable output)

These are independent. A command may produce a RenderValue without
an AssignValue. Commands such as `show` produce structured results
for rendering and projection, even when they do not yield an
assignable shell value. Keeping `show` and `list`
non-assignable in Phase 1 preserves a useful distinction between
operational commands that yield shell-facing result values and
rendering commands whose primary job is display.

Initial examples:

| Command          | AssignValue fields         | RenderValue |
|------------------|----------------------------|-------------|
| `load file`      | `id`, `name`, `type`       | yes         |
| `link attach`    | `id`, `program_id`, `kind` | yes         |
| `program delete` | none                       | optional    |
| `link detach`    | none                       | optional    |
| `show program`   | none                       | yes         |
| `list programs`  | none                       | yes         |

Mutating commands may produce renderable status records, simple
confirmations, or no output at all, even when they do not yield
assignable shell values. Whether they do is a per-command decision,
not a structural requirement.

Assignment to a command that has no AssignValue is an error:

```
command produced no result to assign
```

Phase 1 restricts assignment to commands with explicitly defined
shell-facing result values. This is a deliberate scoping decision,
not a fundamental property of the shell. The model leaves room for
making rendering commands assignable later if that proves useful.

## Projection

Projection narrows what a command prints.

The proposed user-facing syntax is:

```
show program $prog.id select id,name,type
show program $prog.id maps select name,pin
show dispatcher xdp nsid=1 ifindex=2 slots select slot,program_id,link_id
```

### Why `select`

`select` is simple, readable, and domain-friendly.

It avoids turning ordinary exploratory use into a JSON extraction
exercise, while still giving users a quick way to focus on the
fields they care about.

### Scope of projection

Projection is initially a display feature, not a
data-transformation language. It should support:

- selecting named fields from a result or subview
- selecting columns for tabular display
- narrowing interactive output

It should not initially try to support:

- arbitrary expressions
- computed fields
- filtering predicates
- general-purpose transformations

Those can be added later if real need emerges.

`select` projects the current view. For `show program <id>`, that
is the program result itself; for `show program <id> maps`, it is
the maps subview.

In Phase 1, `select` is a command-level trailing modifier supported
by `show` and any later commands that render structured tabular or
record output. Projection is a rendering concern: it narrows what
is displayed, not what is computed or stored. It does not belong in
the expansion or substitution layer.

## Internal representation

The REPL needs a session state holding bound variables.

Conceptually:

```go
type replState struct {
    vars map[string]Value
}
```

Where `Value` is a shell runtime value, not a domain model object.
It exists at the shell boundary and carries only the fields that
the shell exposes, not the full internal domain representation.

A Value must support:

- field lookup by name
- array indexing
- conversion of scalar leaves to string
- reporting type and path errors clearly

A JSON-round-tripped `map[string]any` representation is acceptable
as an implementation technique, especially early on. A custom value
model may later be preferable if the shell grows more structured
semantics.

The important design point is that commands return shell-facing
result objects, and those objects can be bound, traversed, and
projected consistently.

## Phased rollout

### Phase 1: assignment, field access, and projection

Deliver:

- explicit assignment
- dotted field access
- array indexing
- assignment errors for nil-result commands
- `select` projection for narrowing display output

This is enough to support exploratory lifecycle scripting.

Example:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
show program $prog.id select id,name,type
show program $prog.id maps select name,pin
program delete $prog.id
```

### Phase 2: lifecycle completion

Add the remaining commands needed for a full lifecycle, especially
attach and detach.

Example:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
let link = link attach tracepoint --tracepoint sched/sched_switch --program-id $prog.id
show program $prog.id maps
link detach $link.id
program delete $prog.id
```

### Phase 3: JSONPath escape hatch

Retain or add JSON output / JSONPath support for complex
extraction.

Example:

```
show program 12 --format=jsonpath '{.maps[*].name}'
```

This is a secondary access mode, not the primary language.

## Execution model

The REPL processes one line at a time through a fixed sequence of
phases. This line-oriented model is deliberate: the language does
not need multi-line blocks, operator precedence, pipelines, or
nested expressions. One line, one command, one result.

### Phases

1. **Read line.** The line editor provides prompt, history, and
   completion. It returns a string.

2. **Tokenize.** A small lexer produces tokens from the input
   string. This is not ad hoc string splitting. The lexer
   recognises at least identifiers, quoted strings, `=`, variable
   references (`$name.path` and `${name.path}`), and flags; the
   exact token type set will be determined during implementation.
   Variable references such as `$prog.id` and
   `${prog.maps[0].name}` are lexed as single tokens and resolved
   during expansion. Proper tokenization avoids the fragility that
   ad hoc splitting encounters with quoting, braces, and array
   indexing syntax.

   Comments are stripped before tokenization. A `#` and everything
   after it is discarded, unless the `#` appears inside a quoted
   string.

3. **Parse line.** The top-level grammar is tiny:

   ```
   line       := let-assignment | command
   let-assignment := "let" IDENT "=" command
   command    := TOKEN+
   ```

   The parser only answers: is this `let name = ...` or a plain
   command?

4. **Expand variables.** Variable references in the command tokens
   are resolved against session state. For assignment lines,
   expansion applies only to the right-hand side. For plain
   commands, expansion applies to all tokens.

   Expansion operates token by token, not by text substitution.
   A variable reference token such as `$prog.id` resolves to a
   scalar value and becomes a plain token. The expanded result is
   never re-lexed. This avoids the fragility and injection risks
   of textual substitution.

   Expansion is token-preserving: it replaces variable-reference
   tokens with scalar argument tokens and never injects new syntax
   that must be re-lexed. Parsing and expansion are deterministic
   and side-effect free.

5. **Parse command.** The expanded tokens are dispatched to a
   per-command family parser. There is no single universal grammar.
   Each command family (load, show, list, link, gc, etc.) has a
   typed parser that consumes tokens and produces a typed command
   value. Command-family parsers are responsible for trailing
   modifiers such as `select` and `--format`, after variable
   expansion has already occurred.

6. **Execute.** The typed command is executed against the manager
   API. Execution is the only phase that may touch the bpfman API
   layer or produce side effects. It produces a result with two
   independent parts:

   - **AssignValue**: the shell-facing value for variable binding
     (nil if the command is not assignable)
   - **RenderValue**: the structured value for rendering (nil if
     the command produces no displayable output)

   A command may have a RenderValue without an AssignValue. For
   example, `show program 12` produces structured data for
   rendering but is not assignable in Phase 1.

7. **Bind variable.** If the line was an assignment:
   - if AssignValue is nil, report an error
   - otherwise store the value in session state

8. **Render.** The RenderValue is formatted according to explicit
   render metadata: output format, selected columns, and subview.
   The renderer receives these options, not the entire parsed
   command. Projection is a rendering concern: it narrows what is
   displayed, not what is computed or stored.

### Phase ordering

Variable expansion (phase 4) completes before command-family
parsing (phase 5). By the time a command parser such as `show`
sees its tokens, all variable references have already been resolved
to scalar values. Command parsers never encounter variable syntax;
they only see plain arguments, flags, and trailing modifiers like
`select`.

### Why line-oriented

This model maps cleanly to both interactive use and batch scripts:

```
let prog = load file --path foo.o --programs tracepoint:my_prog:sched/sched_switch
let link = link attach tracepoint --tracepoint sched/sched_switch --program-id $prog.id
show program $prog.id select id,name,type
link detach $link.id
program delete $prog.id
```

It also enforces useful separation:

- language mechanics (lexing, parsing, expansion) are independent
  of bpfman domain logic
- commands operate on typed requests and typed results, never on
  rendered text
- rendering decisions (format, column selection) are applied at the
  boundary, not entangled with command execution
- the shell never needs to parse its own output

## Error behaviour

Errors should be explicit and non-magical.

Examples:

- undefined variable: `"undefined variable: foo"`
- missing field: `"field bar not found in variable foo"`
- invalid index: `"index 3 out of range for variable foo.maps"`
- bare structured variable: `"variable foo is structured; use
  field access (e.g. $foo.id)"`
- null leaf: `"variable foo.bar is null"`
- non-scalar leaf: `"variable foo.bar is an object; use field
  access to reach a scalar value"`
- no result to assign: `"command produced no result to assign"`
- command failure during assignment: if command execution fails
  during assignment, no variable is created or updated; the
  command error is reported

## Rationale

This design keeps the language small while solving the real
problems:

- explicit data flow between lifecycle steps
- readable access to structured command results
- narrower output for exploratory work

It avoids over-design in several ways:

- no automatic variables
- no implicit "current result"
- no default scalar value for structured variables
- no JSONPath-heavy everyday syntax
- no general-purpose expression language

The result is a shell that remains domain-specific and readable,
while still being powerful enough to load, attach, inspect, detach,
and delete objects with explicit data flow.
