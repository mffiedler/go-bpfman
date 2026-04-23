# JSON encoding policy

The bpfman REPL, CLI `-o json`, and any future API surface all share
one JSON schema per public type. The schema is a contract with every
downstream consumer: scripts, tests, jq filters, typed clients. This
document sets the rules for struct-tag choices on types that cross
that boundary.

## Principle

**Every domain-meaningful field is emitted explicitly.** A consumer
must be able to assert on any field value — including zero, false,
empty — without special-casing absence. Absence is reserved for
fields that are genuinely "not applicable" in the current context.

The corollary: `,omitempty` and `,omitzero` are tools for encoding
optionality, not for tidying output. Terminal readability is handled
by `-o table` / `-o wide`, not by dropping fields from JSON.

## Default

No tag modifier. The JSON key is present, always:

```go
Offset   uint64 `json:"offset"`
Retprobe bool   `json:"retprobe"`
```

A new field arriving at review without a modifier is the expected
shape and needs no justification.

## Allowed with justification

A tag modifier on a public field must carry a one-line comment
explaining why. Review rejects modifiers that lack one.

### `,omitempty` — acceptable uses

1. **Metadata maps** where nil and `{}` are interchangeable and the
   map is genuinely optional:

   ```go
   // Labels are optional user annotations; nil and empty are equivalent.
   Labels map[string]string `json:"labels,omitempty"`
   ```

2. **Pointer-typed optionals** where nil is the "unset" signal that
   the rest of the code already relies on:

   ```go
   // Deadline is absent when no timeout was configured.
   Deadline *time.Time `json:"deadline,omitempty"`
   ```

   Only reach for the pointer when "unset" is distinct from "set to
   zero". If you are adding a pointer *to make omitempty work*, that
   is the wrong direction.

3. **String discriminators** where empty already means "this
   alternative was not used" in the surrounding design, and
   consumers understand the convention. Prefer an explicit
   discriminator field over this pattern.

### `,omitzero` — acceptable uses

Go 1.24's `,omitzero` calls the field's `IsZero()` method. Use it
exclusively for types where `,omitempty` does not work as expected,
principally `time.Time{}`:

```go
// CreatedAt is absent until the record is persisted.
CreatedAt time.Time `json:"created_at,omitzero"`
```

Do not use `,omitzero` on ordinary scalars. It has the same
downsides as `,omitempty` plus the indirection of a method call.

## Forbidden

### No modifier on bool

False is always a real state. `Retprobe false` is distinct from "we
do not know whether this is a retprobe", and the JSON must reflect
that.

### No modifier on numeric scalars that can legitimately be zero

Offsets, PIDs, kernel addresses, counts — all have meaningful zero
values. `PID 0` is a valid domain value in tracing contexts even
when it also happens to be kswapd. Emit explicitly.

### No modifier on IDs, names, kinds, or other load-bearing strings

If a consumer is going to assert on the value, the field must be
present. A missing `kind` is not the same as `kind: ""`.

### No modifier introduced for schema-evolution reasons

"Old clients might not know this field" is not a justification —
well-behaved clients ignore unknown fields regardless. Omitting a
new field to "not confuse" old clients trades present-day clarity
for an imaginary future reader.

## Review checklist

When a public struct adds or changes a field:

- [ ] No `,omitempty` or `,omitzero` at all, **or**
- [ ] The modifier has a one-line comment explaining why, **and**
- [ ] The field is a pointer, map, slice, or `time.Time`, **and**
- [ ] Zero/empty genuinely means "not applicable" in this type's
      domain, not merely "the default that most callers use".

If the justification is terminal-readability, the answer is no;
route that through the table formatter instead.

## Audit

Scan public-type files for `omitempty` / `omitzero` occurrences and
check each against the rules above:

```sh
grep -rn --include='*.go' 'json:"[^"]*,\(omitempty\|omitzero\)"' \
  | grep -v _test.go
```

A future lint should enforce the rule mechanically: flag any
`,omitempty` on a non-pointer, non-slice, non-map field, and any
`,omitzero` on a type whose `IsZero` is not the intended semantic.

### Known gaps at time of writing

Cleared against the policy:

- `link.go`, `program.go`, `load_spec.go`, `attach_target.go`,
  `version/version.go` — scalar fields emit explicitly; pointer,
  map, and discriminator-style string fields keep `,omitempty`
  with inline justification comments.
- `kernel/link.go`, `kernel/map.go`, `kernel/program.go` — scalar
  fields emit explicitly. `kernel.Link` is a flat union where
  type-specific fields only apply for certain `link_type` values;
  `link_type` is the discriminator consumers key off. A future
  refactor into per-kind detail structs is the long-term fix.
- `inspect/inspect.go` — scalar fields emit explicitly; the
  `*Managed` / `*Kernel` pointer fields keep `,omitempty` with
  inline comments documenting what nil means.
- `dispatcher/specs.go`, `dispatcher/state.go` — scalar fields
  emit explicitly; the `LinkPinPath` discriminator string keeps
  `,omitempty` with inline justification.

Not yet audited:

- `config/config.go` — config-file schema (dual `toml` + `json`
  tags). Config files conventionally omit defaults; this is the
  one category the policy does not automatically apply to. Review
  separately.
- `platform/store/sqlite/programs.go` — internal persistence shape,
  not a public consumer type. Audit if these rows become
  consumer-visible.
- `platform/image/oci/puller.go`, `logging/spec.go` — internal
  bookkeeping. Same caveat.
- `server/pb/bpfman.pb.go` — generated protobuf code. The protobuf
  toolchain owns the tags; out of scope.

Update this section as further violations are cleared.
