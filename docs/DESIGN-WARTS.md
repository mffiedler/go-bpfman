# Design Warts

An honest catalogue of where the architecture falls short, assessed
against two reference texts:

- Chiusano & Bjarnason, *Functional Programming in Scala* (2nd ed.)
  -- "the Red Book"
- Ousterhout, *A Philosophy of Software Design*

Each section states the tension, points at concrete code, and sketches
what a resolution might look like. Nothing here is urgent; the list is
a backlog of design debts to revisit when adjacent code is changing.

---

## 1. Plans are closures, not values

**Documented.** The trade-off is captured in `manager/operation/doc.go`
alongside the related binding and forward/undo asymmetry compromises.

---

## 2. Bindings are stringly-typed with runtime panics

**Documented.** The remaining ordering gap is captured in
`manager/operation/doc.go` alongside the closure and asymmetry
trade-offs.

---

## 3. Coherency operations are opaque closures, not actions

**Resolved.** `Operation` now carries `[]action.Action` instead of a
closure. Coherency rules produce reified action values that flow
through the same executor as plan-based operations. This enabled
`action.Describe` (human-readable descriptions for every GC-relevant
action), `bpfman gc --dry-run` (full preview of what GC would do
without executing), and the transaction-rollback technique that lets
dry-run show coherency violations that depend on store deletions
completing first.

---

## 4. Batch and Sequence have identical implementations

**Resolved.** Removed both types; they were dead code with no
construction sites in the codebase.

---

## 5. Two scopes of rollback atomicity

**Assessed, not a wart.** The plan interpreter (`operation/run.go`)
and the executor's dispatcher helpers (`manager/executor_dispatcher.go`)
both perform rollback, but at different scopes that compose correctly.

The plan interpreter handles rollback *across* actions: if node 3
fails, accumulated undo groups from nodes 1 and 2 execute in reverse
order. The executor's dispatcher helpers handle rollback *within* a
single action: if `createXDPDispatcherHelper` succeeds at kernel I/O
but fails at store persistence, it rolls back the kernel artefacts
before returning an error. The plan interpreter never sees the partial
internal state.

These nest cleanly. If `EnsureXDPDispatcher` fails internally, the
inline rollback cleans up the kernel artefact, then the action returns
an error, then the plan interpreter undoes any earlier nodes that
succeeded. A reader needs to know that deep actions manage their own
partial-failure cleanup, but this is Ousterhout's "pull complexity
downward" -- the plan interpreter stays simple because the executor
absorbs the sub-transaction complexity.

---

## 6. Forward/undo asymmetry

**Documented.** Captured in `manager/operation/doc.go` as a direct
consequence of the closure compromise (wart 1).

---

# Package Structure

The layering is clean, dependencies flow downward, and there are no
circular imports. The following items are places where the package
hierarchy could be tightened.

---

## 7. `bpffs/` and `bpfmanfs/` are confusingly named siblings

**Resolved.** Dissolved the `bpffs/` package entirely. `LinkPath` and
`NewLinkPath` moved to the root `bpfman` package alongside
`LinkRecord` (where they belong as domain types). Mount functions
(`IsMounted`, `Mount`, `Unmount`, `EnsureMounted`, `EnsureMountedWith`)
moved to `fs/runtime/`, which already owned the mounting
responsibility. The dead `MountPoint` type was dropped. The
`intent.go` op-type abstraction (six wrapper types around trivial
`os.*` calls) was replaced with plain helper functions. Subsequently
renamed `bpfmanfs/` to `fs/` and eliminated type stutter (`FSLayout`
to `Layout`, `BytecodeFS` to `Bytecode`, `FilesystemContext` to
`Runtime`).

---

## 8. `bpfmanfs/` claims to be I/O-free but performs I/O

**Resolved.** Updated `fs/doc.go` to accurately describe the
two layers: pure path computation (I/O-free) and filesystem operations
(real I/O with path-safety invariants).

---

## 9. `platform/store/` exists only for one sentinel error

**Resolved.** Moved `store.ErrNotFound` to `platform.ErrRecordNotFound`
(renamed to reflect its semantics: a store record lookup that found no
row). Deleted `platform/store/errors.go`.

---

## 10. `inspect/` and `manager/coherency/gather.go` duplicate state correlation

**Resolved.** Coherency now builds from `inspect.World` rather than
reimplementing the correlation (commit 7319348).

---

## 11. `netns/` and `nsenter/` are unrelated top-level packages for related work

**Resolved.** Moved to `ns/netns/` and `ns/nsenter/`.
