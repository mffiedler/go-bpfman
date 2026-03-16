# REPL value adaptation for host commands

## Summary

The REPL can pass scalar values as argv strings to `exec`, but many
host tools expect input as file paths or standard input rather than
command-line arguments. This document defines an adapter model that
bridges REPL values to the forms host commands actually consume.

Phase 1 proposes a file adapter: an explicit `file temp` command that
writes a REPL value to a private temporary file and returns the path,
plus inline `file:$value` sugar for single-use adaptation inside
`exec` argument lists.

## Motivation

The `exec` command gives the REPL a practical escape hatch for running
host-side helpers. Combined with `json parse`, it supports workflows
like:

    let raw = exec bpftool map dump id $map_id
    let data = json parse $raw.stdout

But the return path is missing. When a host tool needs a REPL value as
input, the only option today is to pass it as an argv string. That
works for short scalars but breaks down for anything larger.

Consider comparing two BPF map snapshots:

    let raw1 = exec bpftool map dump id $map_id
    let raw2 = exec bpftool map dump id $map_id
    # No way to run: exec diff <raw1.stdout> <raw2.stdout>

The user cannot express this because `diff` expects file paths, not
inline text. The same limitation applies to any tool that reads from
files: `jq`, `wc`, `grep`, `sha256sum`, and others.

Without adaptation, users must leave the REPL to write temporary files
by hand, which defeats the purpose of scripting the workflow in a
single session.

The missing capability is not "temp file support". It is the ability
to adapt REPL values into forms that host commands can consume.

## Current model

Today the REPL has exactly one value adapter:

- **Scalar expansion**: `$value` resolves to an argv string via
  `argTexts`. Structured values are resolved through path syntax
  (`$value.field`, `$value[index]`) to reach a scalar.

That is sufficient when the target command accepts short text
arguments. It is insufficient when the target command expects a file
path, a stream on stdin, or a large text block.

## Goals

### 1. Define the adapter abstraction

Establish adapters as a named concept in the REPL. An adapter
converts a REPL value into a form suitable for consumption by a host
command. The set of adapters is curated, not user-extensible.

### 2. Implement the file adapter

Provide a way to write a REPL value to a private temporary file and
obtain the path. This is the minimal adapter that unblocks the most
common workflows.

### 3. Support both explicit and inline forms

The explicit form is a standalone command that returns a reusable
path. The inline form is scoped to a single `exec` invocation and
cleaned up automatically. Both forms share the same rendering
semantics.

### 4. Define clear lifetime and cleanup rules

Temporary files must have well-defined ownership, lifetime, and
cleanup behaviour. The rules must be simple enough that users do not
have to think about resource leaks.

### 5. Preserve the REPL model

Adapters are not expressions. They do not introduce a general
expression language, nested evaluation, or implicit coercion. They
are a narrow mechanism for bridging REPL values to host command
expectations.

## Non-goals

This feature does not provide:

- a general expression evaluator or nested evaluation syntax
- Bash process substitution (`<(...)` or `>(...)`)
- arbitrary value coercion between REPL types
- a persistence mechanism or user-visible filesystem API
- stdin adaptation (deferred to future work)
- user-defined or plugin-based adapters
- named pipes, FIFOs, or shared memory transports

## Proposed model

### Adapters

An adapter is a function from a REPL value to a transport form. The
REPL defines a small, fixed set of adapters.

Phase 1 defines one adapter:

| Adapter | Input             | Output         | Lifetime           |
|---------|-------------------|----------------|--------------------|
| file    | any REPL value    | temp file path | explicit or scoped |

Future phases may add:

| Adapter | Input             | Output         | Lifetime        |
|---------|-------------------|----------------|-----------------|
| stdin   | any REPL value    | piped to stdin | command-scoped  |

The adapter set is intentionally small. Each addition must justify
itself against the same criteria: does it bridge a real gap between
REPL values and host command expectations?

### Explicit file adapter

The explicit form is a shell command:

    file temp <value>

The argument is a variable reference with optional field and index
path, using the same `$var.path[index]` syntax accepted by commands
such as `dump`. All of the following are valid:

    file temp $raw.stdout
    file temp $snap[2]
    file temp $prog.record

When the argument resolves to a scalar, the file contains that
scalar's text. When it resolves to a structured value, the file
contains deterministic pretty-printed JSON. This is slightly broader
than ordinary scalar argv expansion, which requires a scalar leaf.
The file adapter deliberately accepts structured values because the
primary use case — writing data for host tools to consume — benefits
from it.

The command creates a private temporary file, writes the rendered
value into it, and returns the path as a scalar string.

Example:

    let f = file temp $raw1.stdout
    exec wc -l $f

The returned path is a plain string. The user owns its lifetime and
may pass it to multiple commands or inspect it.

Cleanup is the user's responsibility in the explicit form. The REPL
does not automatically delete files returned by `file temp`. The file
persists until the user removes it. It may also be removed by
OS-level temp-directory cleanup. The REPL does not currently guarantee
automatic cleanup for explicit temp files.

Users may remove explicit temp files with ordinary host tools, for
example `exec rm $f`.

### Inline argument adapter

The inline form uses a prefix sigil to mark adapter invocations
inside `exec` argument lists:

    exec diff file:$raw1.stdout file:$raw2.stdout

This is equivalent to:

    let _tmp1 = file temp $raw1.stdout
    let _tmp2 = file temp $raw2.stdout
    exec diff $_tmp1 $_tmp2
    # temp files cleaned up automatically

The `file:` prefix is argument adapter syntax. It is not a general
expression form. The REPL recognises adapter prefixes only in `exec`
argument position.

The inline form:

- is valid only in `exec` argument position
- accepts both scalar and structured values
- creates a private temporary file per `file:` occurrence
- renders the value using the same rules as the explicit form
- passes the temp path as that argv element
- removes all temp files after the command completes
- removes temp files on both success and failure

The power of the adapter is that both scalar and structured values
pass through the same surface form. For example:

    exec diff file:$raw1.stdout file:$raw2.stdout
    exec jq . file:$snap

In the first line, `$raw1.stdout` is a scalar string; the file
contains that string verbatim. In the second line, `$snap` is a
structured value; the file contains deterministic JSON. The user does
not need different syntax for the two cases.

### Why prefix syntax rather than parentheses

Parentheses are a natural candidate for inline adapters, but they
carry a risk: if the REPL later introduces a general expression
language, parenthesised expressions will be the obvious syntax for
grouping and nesting. Burning parentheses now for a narrow adapter
form would either constrain future expression design or create two
different meanings for parentheses.

The `file:$value` prefix avoids that problem:

- it is visually local and obviously not a plain scalar expansion
- it does not steal parentheses for future use
- it does not suggest a general expression or type-cast system
- it generalises cleanly to other adapters (`stdin:$value`)

The prefix reads as "adapt this value as a file for this command",
which is exactly what is happening.

### Tokenisation

`file:<value-expr>` is recognised as a single adapter argument in
`exec` argument position. The adapter prefix must be identified before
ordinary variable expansion so the adapter can intercept the whole
form and manage the temporary file lifecycle.

The adapter prefix is not variable expansion. It is a distinct
syntactic form: `<adapter>:$var.path[index]`. The REPL recognises
the fixed set of adapter names followed by `:` and treats the
remainder as a variable reference to be expanded and adapted.

## Lifetime and cleanup

The two forms have different lifetime models.

### Explicit form: user-managed

    let f = file temp $value

`file temp` returns a reusable path whose lifetime is user-managed.
The user asked for a handle, so the user controls when it is removed.
See the explicit file adapter section for cleanup details.

### Inline form: command-scoped

    exec diff file:$x file:$y

The REPL creates the backing temporary files immediately before
launching the `exec` command and removes them unconditionally once
that `exec` invocation completes, whether it succeeds or fails. The
user never sees or manages the paths.

Cleanup happens in a deferred block, so it runs even if the command
returns an error, the context is cancelled, or the process exits
non-zero.

### Session cleanup

As a safety net, the REPL may register a session-level cleanup hook
that removes any remaining temp files when the session ends. This
catches the case where the user creates explicit temp files and
forgets to remove them.

Whether to implement this is an open question. The simplest approach
is to use the OS temp directory and rely on system-level cleanup.

## Rendering semantics

When a value is written to a temporary file, the rendering depends on
the value type:

| Value type        | Rendering                          |
|-------------------|------------------------------------|
| scalar string     | string contents, no trailing newline |
| number            | decimal text representation        |
| boolean           | `true` or `false`                  |
| null              | `null`                             |
| structured value  | deterministic pretty-printed JSON  |

Numbers, booleans, and null are rendered as their JSON-like text
forms. Structured values are rendered as deterministic pretty-printed
JSON. Scalar strings are the special case: they are written exactly
as their string contents, with no added newline or transformation.

For structured values, "deterministic" means:

- object keys are sorted alphabetically
- array element order is preserved as-is
- indentation is two spaces
- no trailing whitespace
- a trailing newline after the final closing bracket

This ensures that `diff` and similar tools produce meaningful output
when comparing two rendered structured values.

If a scalar string contains a trailing newline, it is preserved. If
it does not, none is added. This is the right default for passing
captured command output to file-based tools.

## File properties

Temporary files are:

- created in the OS default temp directory (`os.CreateTemp`)
- created with a recognisable prefix (e.g., `bpfman-repl-`)
- created with mode 0600 (owner read/write only)
- regular files, not named pipes or device nodes

## Syntax

### Explicit form

    file temp <value>

Where `<value>` is a variable reference with optional field and index
path, using the same `$var.path[index]` syntax accepted by commands
such as `dump`. Both scalar leaves (`$raw.stdout`) and structured
subtrees (`$snap[2]`) are valid.

The command returns a scalar string containing the absolute path to
the temporary file.

### Inline form

    exec <command> [args | file:<value>]...

Where `file:<value>` appears in argument position and is replaced by
the temporary file path before the command is executed.

The value expression after `file:` is a `$var.path[index]` reference,
the same form accepted by the explicit `file temp` command. In Phase
1 only variable references are supported, not literal values or
general expressions.

## Integration with let and assert

### Explicit form

The explicit form is assignable:

    let f = file temp $data
    assert not-empty $f

### Inline form

The inline form is not independently assignable. It exists only as
part of an `exec` invocation:

    # Valid:
    exec diff file:$x file:$y
    let out = exec diff file:$x file:$y

    # Not valid:
    let f = file:$x

The inline form produces no REPL value of its own. It adapts a value
into an argv element. Inline adapters do not change the normal `exec`
result contract: `out` in the example above is the same structured
result (argv, stdout, stderr, exit_code) that `exec` always returns.

## Examples

### Comparing two map snapshots

    let raw1 = exec bpftool map dump id $map_id
    let raw2 = exec bpftool map dump id $map_id
    exec diff file:$raw1.stdout file:$raw2.stdout

### Explicit temp file for reuse

    let f = file temp $data
    exec wc -l $f
    exec sha256sum $f
    exec rm $f

### Pretty-printing structured data

    let snap = json parse $raw.stdout
    exec jq '.[] | select(.key == 2)' file:$snap

### Asserting file content

    let f = file temp $value
    assert ok exec grep -q expected $f

### Mixed adapter and scalar arguments

    set map_id = 42
    let raw = exec bpftool map dump id $map_id
    exec wc -l file:$raw.stdout

## Alternatives considered

### 1. Magic inside exec

Add special handling so that `exec diff $x $y` automatically detects
when a value is "too large" for argv and writes it to a temp file.

This is fragile and surprising. The user cannot predict when the
magic triggers. It violates the principle that `$value` always
resolves to an argv string.

### 2. Bash-style process substitution

Use `<(...)` syntax:

    exec diff <(file $x) <(file $y)

This is elegant for shell users but imports Bash baggage into the
REPL. The REPL is not a shell, and its syntax should not look like
one. It also suggests capabilities (named pipes, async subprocesses)
that the REPL does not provide.

### 3. Parenthesised adapter syntax

Use parentheses for inline adaptation:

    exec diff (file $x) (file $y)

This reads well but reserves parentheses for a narrow purpose. If the
REPL later needs parentheses for general expressions, grouping, or
subexpressions, the adapter meaning would conflict. The prefix syntax
`file:$x` avoids this by occupying a distinct syntactic space that
does not compete with future expression forms.

### 4. Postfix adapter syntax

Use a postfix marker:

    exec diff $x:file $y:file

This is compact but scans poorly and is too easily confused with
path-like or label-like syntax. It also reads backwards: the adapter
name comes after the value, but conceptually the adaptation happens
before the value is consumed.

### 5. General expression syntax

Allow arbitrary nested expressions:

    exec diff (uppercase (file $x)) (file $y)

This turns argument adaptation into a pipeline, which turns the REPL
into an expression language. That is a much larger commitment than
the feature requires. The adapter model is intentionally flat: one
adapter per argument position, no composition.

### 6. Stdin-only approach

Instead of temp files, pipe the value to the command's stdin:

    exec diff --stdin $x $y   # hypothetical

This works for some tools but not for tools that require two file
paths (like `diff`), tools that require seekable input, or tools that
use filenames for output labelling. Stdin adaptation is useful but
does not replace file adaptation.

### 7. Do nothing

Users can leave the REPL, write temp files by hand, and run commands
externally.

This defeats the purpose of `exec` and `json parse`. Those features
exist precisely to keep workflows inside the REPL. A missing return
path leaves the story incomplete.

## Open questions

### 1. Session-level cleanup for explicit temp files

Should the REPL register a cleanup hook that removes all
`file temp`-created files when the session ends? This would prevent
temp file leaks from forgotten explicit handles, but it changes the
lifetime semantics: the user cannot rely on the file surviving beyond
the session.

The conservative choice is to not auto-clean explicit files and rely
on OS temp directory cleanup. The aggressive choice is to track and
clean them. Both are defensible.

### 2. Inline adapter set expansion

When (if ever) should a second inline adapter be added? The `stdin`
adapter is the obvious candidate:

    exec jq '.' stdin:$data

But its semantics are different from `file:` because it affects the
command's stdin rather than an argv position. That may require
different syntax or a different integration point (e.g., a command
option like `exec --stdin $data jq '.'` rather than a prefix).

### 3. Binary values

The current value model is text-oriented. If the REPL later supports
binary values (raw bytes from BPF map reads, for example), should
`file temp` write them verbatim? The rendering rules above assume
text. Binary support would require a separate rendering path or a
flag.

### 4. Temp file naming

Should inline temp files have predictable names (e.g., based on the
variable name) to make `diff` output more readable?

    --- /tmp/bpfman-repl-raw1.stdout
    +++ /tmp/bpfman-repl-raw2.stdout

This is a nice-to-have but adds complexity to the temp file creation
path.

### 5. Adapter prefix ambiguity

The `file:` prefix resembles a URI scheme. In practice this is
unlikely to cause confusion because the REPL tokeniser recognises
only the fixed set of adapter names, and `file:` followed by `$` is
not a valid URI. But if future adapters use names that overlap with
common URI schemes, the resemblance could become confusing.

## Recommendation

Implement the explicit `file temp` command first. This is the deep
primitive that defines rendering semantics, lifetime rules, and the
adapter concept. It can be tested, documented, and used immediately.

Add the inline `file:$value` sugar second, once the explicit form is
stable. The sugar is a tokeniser-level convenience over the same
primitive, not a new capability. Implementing it second ensures the
semantics are nailed down before the ergonomic layer is added.

Defer stdin adaptation to a future proposal. It has different
integration requirements and should not be bundled with file
adaptation.
