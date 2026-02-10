# Spec and Status Guidelines

This document defines the **spec/status split** used across bpfman's Go implementation and
lays out concrete rules for using the type system to prevent programming mistakes.

The goal is to make illegal states either:

* **unrepresentable** (preferred), or
* **un-callable** (the compiler prevents reaching the wrong code path).

Where Go cannot enforce a property statically, we centralise construction in a small number
of places and keep "stringly" data at the boundary.

## Vocabulary

### Spec

**Spec** is user intent: what bpfman intends to manage.

Spec values come from:

* explicit user configuration / CLI flags / API objects, and
* bpfman-controlled inference (e.g. deriving a type from an ELF section name when the user
  leaves it unspecified).

Spec is the "desired state" for bpfman-managed objects.

Spec should be:

* stable across restarts,
* expressed in bpfman's user-facing vocabulary, and
* validated by construction ("parse, don't validate").

### Status

**Status** is observed reality: what exists in the kernel and filesystem right now.

Status values come from:

* kernel enumeration APIs,
* bpffs state (pins), and
* other environment-derived observations.

Status should be:

* honest (do not translate kernel vocabulary into bpfman vocabulary),
* capable of representing unknowns / future kernel additions, and
* separated from spec so we do not conflate "what we asked for" with "what we see".

### Controlled data vs observed data

A quick rule:

* **Spec** is controlled by bpfman.
* **Status** is not controlled by bpfman.

When tests assert on bpfman behaviour, they should prefer spec / managed records. When tests
assert on kernel behaviour, they should assert on kernel vocabulary explicitly.

### Quick reference

1. **Spec is intent; Status is observation.** Never compare them directly.
2. **Spec uses bpfman vocabulary; Status uses kernel/bpffs vocabulary.**
3. **Parse at boundaries; pass typed values internally.**
4. **Never put manager/runtime wiring in Spec.** Manager injects it.
5. **Avoid sentinel values.** Don't encode variants as `""` / `0` / `nil`.
6. **If you write `HasX()` or repeat `field != ""`, split the type.**
7. **Use newtypes + constructors to reduce stringly APIs.**
8. **Make invalid states unrepresentable or un-callable** (constructors, unexported fields,
   resolved specs).

## Design goal: parse, don't validate

We apply "parse, don't validate" by ensuring invalid inputs cannot enter core domain types.

Concretely:

* boundary layers parse strings into typed values,
* internal code passes typed values, not raw strings, and
* optional/variant behaviour is modelled with types, not sentinel values.

"Parse" here means "convert from unstructured to structured", not "check a boolean at
runtime everywhere".

## Boundaries

A "boundary" is any layer where unstructured or external data enters the domain model.

In this codebase, typical boundaries include:

* **CLI parsing layer** (kong args, flags, env vars)
* **Config/spec parsing** (YAML/JSON/TOML, CRDs if/when applicable)
* **Image / OCI loader** (image references, digests, tags)
* **Kernel observation boundary** (`interpreter/ebpf` reading cilium/ebpf and converting to
  `kernel.*` status types)
* **Filesystem/bpffs boundary** (reading pins, paths, directory layouts)
* **RPC/API handlers** (gRPC/HTTP if introduced later)
* **Tests and fixtures** (any place that constructs domain objects directly)

**Rule:** values crossing a boundary must be converted into typed domain forms immediately.
Internal packages should not accept raw strings that represent structured concepts.

## The spec/status split in code

A canonical domain object contains both:

```go
type Program struct {
    Spec   ProgramSpec
    Status ProgramStatus
}
```

* `ProgramSpec` uses bpfman's vocabulary and is validated at construction.
* `ProgramStatus` uses kernel + filesystem vocabulary and is derived from observation.

This makes it explicit that:

* spec and status may diverge (and bpfman must reconcile),
* status may be partial (nil pointers / missing observations), and
* spec is still the source of truth for "what we intended".

## Guidelines

### 1) Do not store manager/runtime wiring in Spec

Spec must contain **only** user intent (and bpfman-controlled inference).

Manager-owned runtime configuration must not leak into spec. Examples:

* bpffs root directory
* pin layout conventions
* runtime dir layout
* lock paths
* sockets

**Rule:** if a value is "known by the manager" and "required for correctness", it must be
an explicit parameter or injected by the manager, not remembered by callers.

**Good**

```go
kernel.Load(ctx, spec, bpffsRoot) // explicit requirement
```

**Bad**

```go
spec = spec.WithPinPath(bpffsRoot) // caller must remember
```

### 2) Prefer closed sets for Spec, open sets for Status

Spec should generally use closed sets (enums) because the user can only request a supported
set.

Status should generally remain open because the kernel can report values bpfman doesn't
know yet.

Example:

* `bpfman.ProgramType` (spec) can distinguish `tc` vs `tcx`, `kprobe` vs `kretprobe`, etc.
* `kernel.ProgramType` (status) should remain the kernel's coarse type classification:
  `schedcls`, `kprobe`, `tracing`, etc.

Do not force kernel status to speak bpfman's vocabulary.

#### Unknowns in Status

Status must handle kernel values bpfman does not recognise (new program types, new map
types, library changes).

We handle this by **preserving the raw kernel vocabulary** rather than forcing a closed
enum:

* `kernel.ProgramType` and `kernel.MapType` are **string-based newtypes** that store the
  (normalised) kernel-reported name, even if bpfman doesn't understand it.
* Code that needs bpfman semantics must perform an explicit mapping step:

  * `ToSupported() (bpfman.ProgramType, bool)` or similar.
  * The `bool` indicates whether a mapping exists.

**Rule:** do not require an `Unknown` variant in `kernel.ProgramType`. The string newtype is
already an "open set". Unknowns become just another string value.

What should code do when it encounters an unknown kernel type?

* **Status paths:** keep it as-is and display it (honest observation).
* **Spec/behaviour paths:** treat it as "unsupported" unless explicitly handled. This must
  be a deliberate mapping decision, not a default translation.

This avoids lying while still allowing bpfman to remain strict about what it supports.

### 3) If you need `HasX()` helpers, you probably need a sum type

A common smell is a helper that simply checks for an empty value:

```go
func (s LinkSpec) HasPin() bool { return s.PinPath != "" }
```

This is almost always a sign of an encoded variant:

* pinned vs unpinned
* resolved vs unresolved
* managed vs unmanaged
* synthetic vs real

**Rule:** if many call sites check `field != ""` or `field != nil`, split the type.

#### Modelling variants in Go

Go doesn't have algebraic data types, but we can still make variants unrepresentable with
common patterns:

**Pattern 1: Separate types**

Use when callers should not mix variants.

```go
type PinnedLinkSpec struct { PinPath bpfman.LinkPath /* ... */ }
type EphemeralLinkSpec struct { /* ... */ }
```

**Pattern 2: Wrapper + optional sub-struct**

Good when you want a single JSON shape but still eliminate sentinel fields.

```go
type LinkSpec struct {
    Common LinkSpecCommon
    Pinned *PinnedLink // nil means ephemeral by construction
}
type PinnedLink struct { PinPath bpfman.LinkPath }
```

**Pattern 3: Sealed interface**

Use when you want compile-time exhaustiveness at call sites (via type switch).

```go
type ProgramMaps interface{ sealed() }
type NoProgramMaps struct{}
func (NoProgramMaps) sealed() {}
type OwnedProgramMaps struct{ OwnerID uint32; PinPath bpffs.MapDir }
func (OwnedProgramMaps) sealed() {}
```

**Guidance:**

* Prefer **Pattern 1** when variants imply different valid operations.
* Prefer **Pattern 2** when serialisation format matters.
* Prefer **Pattern 3** when you want a "sum type feel" without exporting constructors.

**Smell:** if you need both `HasX()` and "special-case nil/empty" checks, you've encoded a
variant in a field — refactor to one of the patterns above.

### 4) Avoid sentinel values for meaning ("empty means auto")

The pattern "empty string means auto-generate" is convenient but fragile.

It creates two meanings for one field and depends on humans remembering which meaning
applies.

**Rule:** do not use sentinel values to encode semantic variants.

Use one of:

* separate types (variants),
* explicit options objects with typed fields,
* or make the value required and always provided by the manager.

### 5) Make runtime configuration unforgeable

Types representing runtime layout must not be constructible as raw structs.

**Rule:** runtime configuration structs must have unexported fields and be created by a
constructor that returns an error for invalid inputs.

Example:

* `RuntimeDirs` should not be `RuntimeDirs{Base: ""}`-constructible.
* `NewRuntimeDirs(base)` must reject empty or non-absolute bases. Canonicalisation (e.g.
  `filepath.Clean`) is optional but recommended for consistent logging and comparisons.

### 6) Use newtypes to reduce stringly APIs

When a value has semantics, don't represent it as a `string`.

Prefer newtypes with constructors that enforce syntactic invariants.

Common candidates:

* absolute paths
* bpffs subpaths
* interface indices (non-zero)
* directions (`ingress`/`egress`)
* OCI image references

Newtypes should enforce what they can enforce locally:

* non-empty
* well-formed
* normalised/canonicalised (e.g. lowercase)

Context-dependent checks (e.g. "under this bpffs root") belong where the context exists,
typically in the manager.

### 7) Construction rules: centralise and narrow the "escape hatches"

Whenever possible:

* make fields unexported and require constructors,
* keep the number of constructors small,
* keep "unsafe" constructors private (package-internal),
* and provide the minimum API surface needed.

If you absolutely must allow direct casting (e.g. in tests), treat it as an "I already know
this is normalised" escape hatch, and keep it local.

### 8) Tests should assert the right ontology

If a test is verifying bpfman behaviour, it should assert against bpfman-controlled data:

* spec values,
* managed records,
* inferred program types (when spec is unspecified).

If a test is verifying kernel observation, it should assert kernel vocabulary explicitly
or only assert presence.

**Bad (ontology mismatch)**

```go
require.Equal(t, bpfman.ProgramTypeTC.String(), prog.Kernel.ProgramType.String())
```

**Good**

```go
require.Equal(t, bpfman.ProgramTypeTC, prog.Managed.Type)          // bpfman behaviour
require.Equal(t, kernel.ProgramType("schedcls"), prog.Kernel.Type) // kernel observation
```

## Practical patterns

### Pattern A: Resolution Boundary (Spec -> ResolvedSpec -> Kernel call)

This pattern is the preferred design for operations where kernel calls require runtime
wiring (e.g. bpffs root paths). Some parts of the codebase already follow this direction;
others still pass runtime concerns in spec-like structs. We are incrementally refactoring
toward this pattern; treat it as guidance for new APIs and refactors.

When adopted, it ensures illegal states are **un-callable**:

* `Spec` contains user intent only.
* `ResolvedSpec` adds manager-owned wiring and is only constructible by the manager.
* kernel adapter requires the resolved type (or requires explicit runtime params).

### Pattern B: Variant types instead of optional fields

Use when a struct contains optional fields that are semantically tied to meaning.

If you ever see:

* multiple fields with cross-field invariants
* a lot of `if x != ""` and `if x != nil`

split into variant structs or nest an optional sub-struct.

### Pattern C: Newtype + constructor for syntactic invariants

Use when you want to prevent accidental misuse without requiring global context.

Examples:

* `TCDirection`
* `AbsPath`
* `Ifindex`

## Code smells checklist

Treat these as refactor triggers:

* `HasX()` that is only `return field != ""` or `field != nil`
* many call sites check `field != ""`
* empty string means "auto", "none", or "default"
* structs with exported fields where invariants matter
* options structs where most fields are only meaningful in certain combinations
* converting raw strings repeatedly instead of parsing once
* kernel status mapped into bpfman vocabulary (lying)
* kernel vocabulary appearing directly in **user-facing output** when a spec/managed
  vocabulary is available (e.g. showing `schedcls` for managed TC programs). Prefer spec
  type for managed objects; show kernel type for unmanaged objects.

## Summary

* Spec is bpfman's controlled intent; status is observed reality.
* Parse at the boundary into typed values; pass typed values internally.
* Don't let runtime wiring leak into spec.
* If you see repeated `field != ""`, you probably need a variant split.
* Prefer newtypes and constructors to reduce "stringly" APIs.
* Make invalid states unrepresentable or un-callable by construction.
