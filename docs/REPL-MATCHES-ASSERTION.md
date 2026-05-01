# REPL `assert ... matches { ... }` — Design Note

Status: proposal, not implemented. Captured for handoff to a fresh
context where the REPL enhancement work will happen.

## Motivation

The REPL test scripts under `e2e/scripts/*.bpfman` validate
load/attach/detach/unload contracts by calling `bpfman` and
asserting on individual fields of the returned records. The
existing assertion form is one path per line:

```
assert $prog.record.load.program_type eq tracepoint
assert $prog.status.kernel.program_type eq tracepoint
assert $prog.record.meta.name eq tracepoint_kill_recorder
assert $prog.status.kernel.id == $pid
assert not-empty $prog.status.kernel.name
assert not-empty $prog.status.kernel.tag
assert not-empty $prog.status.kernel.loaded_at
assert not-empty $prog.record.handles.pin_path
```

Eight lines for one record's contract. Several scripts repeat this
block almost verbatim per program type. The shell hybrid example
(`examples/tracepoint.sh`) needs comparable boilerplate per record;
the painful "construct full wanted JSON" approach was abandoned
because non-deterministic fields (`loaded_at`, `tag`,
`handles.pin_path`, kernel-assigned IDs, auto-generated `.rodata`
maps) are impossible to mask without `jq` and a JSON-aware
normaliser.

Both shells of the same problem: the contract is "these specific
paths must hold these specific values; everything else can vary".
Neither current syntax expresses this directly.

## Proposal

Add one new form to the REPL `assert` builtin:

```
assert <expr> matches {
    <path>: <pattern>
    <path>: <pattern>
    ...
}
```

Entries are separated by newlines.  The block is a *table* of
path-pattern relations, not a sequence of statements, so neither
`,` nor `;` is accepted between entries — both surface a dedicated
diagnostic pointing at the newline rule.  This is a deliberate
departure from the rest of the language, where `\n` and `;` are
interchangeable inside braces.

The form is a **subset match**. The block enumerates the paths the
caller cares about; everything else in the record is ignored. The
assertion passes iff every listed `path: pattern` pair matches.

### Semantics

* `<expr>` evaluates to a structured value (typically a record
  returned from `bpfman program get`, `link attach`, etc.).
* Each `<path>` is a dot-separated key path into the value.
  Existing `$prog.record.meta.name` syntax already parses this
  shape; reuse the same parser.
* Each `<pattern>` is one of:
  * a literal value (`tracepoint_kill_recorder`, `42`, `true`,
    `false`),
  * a variable reference (`$pid`),
  * `not-empty` (the existing guard, lifted into the pattern
    position),
  * (optionally, follow-up) `not-nil`.
* **Disambiguating a literal that collides with a pattern
  keyword.**  Bare `not-empty` is the unary predicate; if you
  actually want to assert that a field equals the string
  `"not-empty"`, quote it: `path: "not-empty"` (or `'not-empty'`)
  produces a literal compare instead of triggering the predicate.
  The same escape rule covers any future bare-word pattern
  keyword.
* The actual record may have any number of additional fields not
  mentioned in the block; those are not checked.

### Forward compatibility

The subset-match shape is the design's load-bearing property: a
record that grows a new field tomorrow does not silently break
every existing test. To start checking the new field, the script
adds one `path: pattern` line to the block. To stop checking
something, it removes the line. No reformatting, no struct
templating, no field-by-field assertion bloat.

### Failure messages

Failures must be path-localised so a multi-mismatch failure
identifies every diverging path in one message:

```
assert $prog matches { ... }: 2 mismatches at testdata/foo.bpfman:42
  status.kernel.tag: expected non-empty, got ""
  record.handles.pin_path: expected non-empty, got ""
```

Match precision matches the existing per-line `assert path eq
value` form; only the call shape changes.

### What it replaces

The block above becomes:

```
assert $prog matches {
    record.load.program_type:       tracepoint
    status.kernel.program_type:     tracepoint
    record.meta.name:               tracepoint_kill_recorder
    status.kernel.id:               $pid
    status.kernel.name:             not-empty
    status.kernel.tag:              not-empty
    status.kernel.loaded_at:        not-empty
    record.handles.pin_path:        not-empty
}
```

Same coverage, one assert call, easier to scan.

## Non-goals

The design deliberately does **not** add:

* **List-manipulation primitives** (`filter`, `map`, `find`,
  `sum`). These reinvent what the embedded `gojq` already does.
  The existing `|> jq <expr>` pipe stays as the structured-data
  hammer.
* **Regex / comparison operators inside patterns** (`> 0`,
  `matches /foo/`). Those expand the pattern grammar and start
  competing with `jq`. Keep patterns to: literal, `$var`,
  `not-empty`. If a user needs more, they fall back to a
  separate `assert path eq value` line or a `|> jq` filter.
* **List patterns** (asserting array shape). Use repeated
  `matches` against indexed paths (`programs.0.record.meta.name`)
  or fall back to `jq`.
* **Schema-style "exact match"**. The semantics are intentionally
  one-sided: actual must include the patterns listed, no more is
  required. There is no `matches strict { ... }` variant.
* **Domain bundles** (e.g. `assert tracepoint $prog`). Users can
  build these themselves once `def` exists (separate proposal).
  Builtin bundles are a maintenance burden and a vocabulary
  expansion every time bpfman grows a new program type.

## Scope of the change

* Lexer: extend the `assert` parsing path to recognise `matches {
  ... }` after the expression. The `{ ... }` block uses the same
  key/value separator the rest of the language already uses (TBD
  during implementation).
* AST: a new `AssertMatches` node holding the target expression
  and the `[(path, pattern)]` list.
* Evaluator: walk each path against the evaluated record, compare
  per the pattern, accumulate mismatches, fail with the
  consolidated message at the end.
* Tests: unit tests for the evaluator covering literal /
  variable / not-empty patterns, missing paths, mismatched
  values, and the forward-compatibility property (extra fields
  do not fail the match).

Estimated size: 200-400 LOC in `cmd/bpfman/repl_assert.go` and
the parser, plus tests.

## Migration

Existing `assert path eq value` lines stay valid. The two forms
co-exist. Per-script migration is opportunistic: when a maintainer
opens a `.bpfman` script for any reason, replace the per-line
block with a `matches { ... }` block in the same commit. No
forced sweep.

## Follow-up: `def name(args) { ... }`

The other half of the script-DRY story — first-class user-defined
commands — is independent and can land separately. With both
features, a script's lifecycle test becomes:

```
def expect_tracepoint(prog, name, run_label) {
    assert $prog matches {
        record.meta.name:               $name,
        record.load.program_type:       tracepoint,
        status.kernel.id:               $prog.record.program_id,
        record.meta.metadata.test:      $run_label,
        status.kernel.tag:              not-empty,
        record.handles.pin_path:        not-empty,
    }
}

expect_tracepoint $prog_a tp_a $run_label
expect_tracepoint $prog_b tp_b $run_label
expect_tracepoint $prog_c tp_c $run_label
```

`def` is a separate, larger change (variable scoping, parameter
parsing, recursion considerations). It is explicitly **not part
of this proposal** but is included here so the reader sees where
`matches` fits in the larger story.

## Test scripts that would shrink immediately

After landing `matches` (without `def`), the following scripts
have visible per-record assertion blocks that would each collapse
to a single `matches { ... }` block:

* `e2e/scripts/TestTracepoint_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestKprobe_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestKretprobe_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestUprobe_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestUretprobe_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestFentry_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestFexit_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestTC_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestTCX_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestXDP_LoadAttachDetachUnload.bpfman`
* `e2e/scripts/TestLoadWithMetadataAndGlobalData.bpfman`

Each script has both a "load response" assertion block and a "Get
round-trip" assertion block. Both become single `matches` calls.

## Why this and not something more general

The REPL was designed not to invent primitives where existing
tools (the embedded `gojq`, the `exec` escape hatch) already
solve the problem. `matches` does not violate that:

* **Value access and manipulation stay in `jq`.** No new filter,
  map, find, sum, or aggregate primitives are introduced.
* **Failure semantics stay in `assert`.** `jq` produces values
  but does not fail-the-script-on-mismatch; `assert matches` is
  a script-test concern that has no `jq` analogue.

The new surface area is narrow (one assert form, three pattern
shapes) and orthogonal to `jq`. No existing escape hatch is
hidden or replaced.
