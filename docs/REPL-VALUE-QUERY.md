# REPL value query and JSON ingestion

## Summary

Add a small structured-data layer to the bpfman REPL so users can:

- parse JSON text into a `shell.Value`
- inspect and extract fields from structured values
- apply the same query model to:
  - JSON parsed from exec output
  - values returned directly by native bpfman commands

The REPL already has a structured value model. This design does not
add a new query model. It adds one new constructor for that model:
parsing JSON text into a `shell.Value`.

That is the key design point. This is not fundamentally a JSON
feature. It is a structured value feature. JSON is one producer of
structured values; native bpfman commands are another. Once a value
exists, it does not matter where it came from.

JSON matters because tools such as `bpftool`, `ip -j`, and `tc -j`
produce it. But once parsed, JSON should become the same kind of REPL
value as any other structured result.

That keeps the model uniform:

- native bpfman commands return typed results
- exec can return text
- JSON text can be converted into a structured value
- structured values can then be queried consistently, regardless of
  origin

This document proposes a deliberately narrow design:

- add one explicit JSON parsing constructor (`json parse`)
- treat parsed JSON as ordinary `shell.Value`
- add only the minimum extra query helpers the current REPL path model
  cannot express cleanly
- avoid turning the REPL into jq, SQL, or a general data language

## Motivation

The REPL already has a useful structured value model.

A user can write:

```
let prog = program get 123
print prog.record.program_id
```

and navigate the result using the existing path expression machinery.

That is good.

However, once exec exists, many useful host-side tools will return
JSON rather than plain text. Examples include:

- `bpftool prog show --json`
- `bpftool map show --json`
- `ip -j link show`
- `tc -j qdisc show`

Without a JSON ingestion mechanism, the user experience becomes
awkward:

- exec captures JSON as plain text in stdout
- the REPL cannot traverse that text structurally
- users must leave the REPL or pipe into external tools such as jq
- scripts cannot naturally mix:
  - host-side inspection via exec
  - structured assertions over results
  - native bpfman commands

A small structured-value ingestion layer solves that problem.

It also addresses a broader issue: the REPL already has structured
values, but its user-facing query model is currently split between:

- path access in some places
- print for display
- scalar extraction in variable expansion

If this is going to grow at all, it should grow around one explicit
concept:

**the REPL operates on structured values, and JSON is one way to make
them.**

## Goals

The feature should satisfy the following goals.

### 1. Parse JSON text into a structured REPL value

Users should be able to take JSON text and convert it into a
`shell.Value`.

Examples:

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
```

or equivalent syntax.

### 2. Query structured values consistently

Once a value is structured, the same mental model should apply whether
it came from:

- `program get`
- `link get`
- `exec ... --json`
- any future command that yields structured data

### 3. Reuse the existing path model where possible

The current dotted/indexed path model is already understandable:

- `record.program_id`
- `maps[0].name`

The design should build on that rather than inventing a completely new
query syntax.

### 4. Support practical scripting and assertions

Users should be able to write things such as:

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
print data[0].id
assert not-empty $data[0].name
```

or, for native values:

```
let prog = program get 123
print prog.record.name
```

### 5. Keep the language small

This should remain a small REPL data model, not a query language
ecosystem.

## Non-goals

This feature is not intended to turn the REPL into any of the
following:

- jq
- JSONPath
- JMESPath
- SQL over values
- a template language
- a generic expression evaluator
- a functional collection-processing language

The following are explicitly out of scope for the initial design:

- arbitrary predicates over arrays
- mapping and filtering expressions
- sorting and grouping primitives
- recursive descent query syntax
- implicit auto-parsing of any string that happens to look like JSON
- schema inference beyond what `shell.Value` already does
- type-directed decoding back into domain-specific Go structs
- object construction syntax inside the REPL
- mutation of structured values in place

The first version should focus on:

- explicit parsing
- explicit path-based extraction
- a very small number of convenience helpers if needed

## Core design question

The first question is architectural:

**is this a JSON feature, or a structured-value feature?**

It should be designed as a structured-value feature. `json parse` is
the first producer-side helper, not the centre of the feature. The
centre is `shell.Value` and the existing path model that operates on
it.

JSON is simply another producer of structured values, alongside native
commands like `program get` and `link get`.

If the design is framed as "JSON support", users may incorrectly infer
that:

- JSON-derived values are special
- native values should be treated differently
- query helpers only apply to JSON

That would be the wrong abstraction.

The correct abstraction is:

- `shell.Value` is the common structured value type
- JSON parsing is one way to construct a `shell.Value`
- path lookup and display operate on `shell.Value`, regardless of
  origin

## Current state

Today the REPL already supports:

- storing structured values in variables
- looking up nested fields via `LookupValue` and `Lookup`
- indexing arrays with `[n]`
- printing structured values as indented JSON
- expanding scalar paths such as `$prog.record.program_id`

That is a strong base.

What is missing is mainly one thing:

- a user-facing way to convert JSON text into a `shell.Value`

There may also be some secondary usability gaps:

- extracting a non-scalar sub-value into a new variable
- listing keys or iterating arrays in a more direct way
- making structured queries more discoverable than raw dotted paths

But those should be assessed after the smallest viable design is
defined.

## Current expression boundary

Today, `let` binds command results, not value expressions.

The current model is:

- command -> value
- path expression -> scalar expansion or print target

but not:

- value expression -> value

That means `json parse` fits cleanly as a command:

```
let data = json parse $raw.stdout
```

But something like this does not work:

```
let first = data[0]
```

because `data[0]` is a path expression, not a command invocation.

The REPL currently allows structured navigation in two places:

- variable expansion into command arguments (e.g., `$data[0].id`)
- `print` of a path expression

But it does not yet treat variable-path expressions themselves as
first-class bindable expressions.

This document does not change that boundary. If experience shows that
binding sub-values is a real usability gap, a later extension such as
`value get` could address it. That is a separate concern from JSON
ingestion.

## Proposed model

The model should have two layers.

### 1. Ingestion

Convert text into a structured value.

For the initial design, that means:

- parse JSON string input into `shell.Value`

### 2. Query

Operate on structured values using the existing path model, with
minimal helper commands only where that model is insufficient or too
awkward.

This separation matters.

It prevents accidental coupling such as:

- "JSON commands" that only work on parsed JSON values
- separate navigation rules for native values and JSON values

Instead, the flow becomes:

1. obtain a value
2. if it is text containing JSON, parse it
3. query the resulting structured value using the ordinary value model

## Proposed syntax

The initial design should add one explicit command:

```
json parse <string>
```

Examples:

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout

let links = exec ip -j link show
let ifaces = json parse $links.stdout
```

The result of `json parse` should be a structured `shell.Value`
suitable for `let`.

In plain command form, the command may print the parsed JSON as
indented JSON for visibility:

```
json parse $raw.stdout
```

But its primary value is in bound form.

### Why explicit parsing is better than implicit parsing

It may be tempting to auto-parse JSON in cases such as:

- exec output that looks like JSON
- variable expansion from strings that happen to contain JSON

That would be a mistake.

Explicit parsing is better because:

- it keeps type transitions visible
- it avoids surprising behaviour
- it lets users choose when text remains text
- it keeps error reporting precise

The user should always know when a string became a structured value.

## Interaction with existing path syntax

Once a value is structured, the existing path model should remain the
primary query mechanism.

Examples:

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
print data[0].id
print data[0].name
```

Likewise for native values:

```
let prog = program get 123
print prog.record.program_id
print prog.status.kernel_seen
```

This is the strongest reason not to make JSON a separate subsystem.

The user should not need to think:

- "this is JSON, so I must use JSON commands"
- "this is native, so I must use value paths"

There should be one ordinary access model.

## Do we need additional query commands?

Possibly, but not necessarily in the first version.

The current path model already supports:

- field access
- array indexing
- scalar extraction
- structured display through print

That may already be enough for many cases.

For example:

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
print data[0]
print data[0].id
assert not-empty $data[0].name
```

If that works well, then the first implementation may require only:

- `json parse`

and no further query commands.

That would be ideal.

### Candidate helper commands for later

If the current path model proves too limited or too hidden, later
helpers could include small, explicit commands such as:

```
value keys <variable>[.path]
value type <variable>[.path]
value len <variable>[.path]
```

Examples:

```
value keys data[0]
value len data
value type prog.record
```

These should be considered only after experience shows real need.

They should not be part of the first version unless the current
mechanics are clearly insufficient.

## Result model

`json parse` should return a structured `shell.Value`.

No new intermediate wrapper type is needed.

The natural implementation is already present in `shell.ValueFromJSON`,
which preserves:

- objects as `map[string]any`
- arrays as `[]any`
- numbers as `json.Number`
- strings, booleans, and null

That is a good fit for the current REPL value model.

Once parsed, the result should behave like any other structured value:

- assignable with `let`
- displayable with `print`
- navigable with `.field` and `[index]`
- scalar-expandable when the final path resolves to a scalar

## Error model

JSON parsing errors should be ordinary command errors.

Examples:

- invalid JSON syntax
- trailing garbage after a JSON value
- attempts to parse non-JSON text

This means:

- `json parse ...` fails with an ordinary REPL error
- `assert ok json parse ...` fails when parsing fails
- `assert fail json parse ...` passes when parsing fails

The error message should be direct and source-oriented, for example:

```
json parse: decode JSON: invalid character ...
```

There should be no fallback mode that silently leaves the value as
text. Parsing either succeeds and yields a structured value, or fails.

## Plain form versus bound form

As with exec, there are two reasonable modes.

### Bound form

This is the primary form:

```
let data = json parse $raw.stdout
```

It should return a structured value for later use.

### Plain form

If the user runs:

```
json parse $raw.stdout
```

the command can print the parsed value as indented JSON.

That is useful interactively and matches the REPL's existing display
model.

However, this is secondary. The core purpose of the command is to
produce a bindable structured value.

## Relation to exec

This design depends on exec only as a producer of text.

Typical usage will be:

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
```

But the JSON design should not be written as if exec were the only
producer. JSON text might also come from:

- `set`
- future file-reading helpers
- future network helpers
- native commands that intentionally emit JSON text

That is another reason to keep this document separate from
REPL-EXEC.md.

REPL-EXEC.md defines how host commands run.

This document defines how structured values are created from JSON and
then queried.

## Relation to native bpfman commands

A central question is whether JSON-specific helpers should be required
for native command results.

They should not be.

For example, this should remain ordinary:

```
let prog = program get 123
print prog.record.program_id
```

The JSON pathway should converge onto the existing value model, not
create a rival one.

If later helper commands such as `value keys` are added, they should
apply equally to:

- `prog`
- `data`
- any other structured variable

### Why not auto-convert typed values to JSON first?

One possible idea is:

- convert any structured value to JSON text
- then operate on it via JSON commands

That would be the wrong direction.

The REPL already has structured values. Re-serialising them to JSON and
then reparsing them would:

- add unnecessary work
- blur the distinction between text and structure
- create needless user ceremony
- risk losing any future non-JSON metadata attached to values

The structured value should stay primary.

JSON parsing is only for when the user starts with JSON text.

## SQLite analogy

SQLite is useful mainly as a reminder that raw JSON text and parsed
structured values are different things. SQLite's JSON support works on
the principle that text becomes a structured thing for query purposes.

The REPL should follow that principle. It should not imitate SQLite's
JSON function surface.

## Examples

### Parsing bpftool JSON

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
print data[0].id
print data[0].name
```

### Parsing ip -j

```
let raw = exec ip -j link show
let ifaces = json parse $raw.stdout
print ifaces[0].ifname
```

### Native value and parsed JSON side by side

```
let prog = program get 123
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout

print prog.record.program_id
print data[0].id
```

### Parsed JSON feeding a native command

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
program get $data[0].id
```

This demonstrates convergence of the two worlds: external JSON is
parsed to a structured value, a scalar is extracted through the normal
path model, and fed into a native bpfman command. No special
interoperability layer is needed because the value model is uniform.

### Assertions

```
let raw = exec bpftool prog show --json
let data = json parse $raw.stdout
assert not-empty $data[0].name
```

### Parsing a scalar JSON value

```
let v = json parse "123"
print v
```

This should work, because a JSON value need not be an object or array.

## Alternatives considered

### 1. Do nothing

Users can always leave the REPL and use jq.

This keeps the REPL smaller, but it weakens the scripting story and
undermines the point of adding exec.

### 2. Make JSON a special variable kind

For example, add a distinct "JSON value" wrapper and separate JSON-only
query commands.

This would complicate the model for little benefit. Parsed JSON should
just become `shell.Value`.

### 3. Auto-parse exec stdout when it looks like JSON

This is superficially convenient, but it is too magical. It makes type
changes implicit and risks surprising behaviour.

### 4. Add a large JSON query language

This would make the REPL more powerful, but it would also create a new
language surface that is much larger than the current REPL design
wants.

That is the wrong trade-off for now.

## Resolved questions

### 1. Command name

`json parse` was chosen. It groups naturally if later JSON helpers
exist (e.g., `json format`) and reads clearly in both plain and bound
form.

### 2. Display model

`dump` is the standard display mechanism for structured values. No
separate JSON display command is needed. Plain-form `json parse`
prints using `json.MarshalIndent` through the same output path.

### 3. Trailing garbage rejection

`ValueFromJSON` now rejects trailing data after a valid JSON value.
Input such as `123 junk` or `{"a":1} {"b":2}` produces a parse error.
Trailing whitespace is accepted.

## Open questions

### 1. Sub-value binding

At present, `let` binds command results, not arbitrary sub-values.
This does not work:

```
let first = data[0]
```

because `data[0]` is a path expression, not a command invocation.

A helper such as `value get data[0]` could address this. It is likely
the next real usability gap, but it is a separate concern from JSON
ingestion and should be assessed after more experience with the
current model.

### 2. Collection helpers

Small structured-value helpers such as `value keys`, `value len`, and
`value type` may be useful. The internal `Value.Keys()` method
already exists. Whether to expose these as user-facing commands
should be decided after real usage shows the current path model is
insufficient.

### 3. Non-identifier JSON keys

JSON objects may contain keys that do not fit the current path
grammar (e.g., `"map-id"`, `"foo.bar"`). These values can be parsed
and stored, but cannot be navigated with the current `$var.path`
syntax. This limitation is acceptable for now but may need a
bracket-string accessor (e.g., `$data["map-id"]`) if it becomes a
real obstacle.

## Implementation status

### Done

- `json parse <string>` shell command in `cmd/bpfman/repl.go`
- returns a structured `shell.Value` via `shell.ValueFromJSON`
- plain form prints indented JSON; bound form returns the value
  silently (suppressed by `WithDiscardOutput` in `LetStmt`)
- `json` added to `shellCommands`, `replCommandNames`, and
  `replSubcommands` for dispatch and tab completion
- help text updated
- trailing garbage rejection in `ValueFromJSON` via `dec.More()`
- parsed JSON values share the same `shell.Value` model as native
  command results: `Lookup`, `LookupValue`, `Scalar`, `dump`, and
  path expansion all work identically regardless of origin
- 14 unit tests covering object/array/scalar parsing, plain and
  bound form, invalid JSON, no-args error, assert ok/fail, nested
  access, exec+json integration, non-value shell command rejection,
  and tab completion
- 3 unit tests for trailing garbage rejection in `ValueFromJSON`

### Not yet done

- sub-value binding (e.g., `let first = value get data[0]`)
- user-facing `value keys`, `value type`, `value len` helpers
- bracket-string accessor for non-identifier keys
- `json parse` currently accepts only a single scalar text argument;
  passing a bare structured variable resolves to its display text
  rather than its JSON serialisation

## Recommendation

Phase 1 is complete. The minimal implementation satisfies the core
design: explicit JSON parsing into the existing structured value
model, with no separate query subsystem.

Phase 2 should be driven by real usage. The most likely next steps
are sub-value binding and collection helpers, but neither should be
added speculatively.

The important thing is not to build "JSON support" as a separate
mini-system.

The important thing is to strengthen the REPL around one coherent
abstraction:

**the REPL operates on structured values, and JSON is one explicit way
to create them.**
