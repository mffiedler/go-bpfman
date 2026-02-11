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

The Red Book's central idea is "programs as values": a plan should be
a data structure you can inspect, transform, print, or diff before
running it. The current plan model gets this right for the undo path
(UndoFrom returns `[]action.Action` -- pure data) but not for the
forward path.

Forward nodes are opaque closures that capture the manager's executor,
filesystem context, and logger by reference:

    // manager/load.go:176-198
    operation.Produce(loadedKey, programName,
        func(ctx context.Context, b *operation.Bindings) (bpfman.LoadOutput, error) {
            loaded, err := action.Produce[bpfman.LoadOutput](ctx, m.executor, action.LoadProgram{...})
            ...
        },
        operation.UndoFrom(func(b *operation.Bindings) []action.Action { ... }),
    )

You cannot ask "what actions will this plan produce?" without running
it. A fully Red-Book plan would be a free monad over the Action type
(`Free[Action, A]`), where the interpreter threads results through via
flatMap. In Go, that requires massive ceremony because the type system
cannot express typed continuations, so closures are the pragmatic
escape hatch.

**What is lost:** plan-level testing without an executor ("given this
input, what actions *would* be produced?"), and inspectability
(logging the plan before execution, comparing two plans structurally).

**Possible resolution:** if the need arises, factor each node's logic
into a function that returns `[]action.Action` given bindings, and
have the interpreter execute those actions. This lifts the forward
path from closures to data. The cost is more boilerplate.

---

## 2. Bindings are stringly-typed with runtime panics

**Partially resolved.** `NewKey` now registers each key name (with its
`reflect.Type`) in a global registry and panics at process startup if
a duplicate name is registered. `Build` additionally panics if two
`Produce` nodes in the same plan bind the same key. Together these
catch name collisions and silent-overwrite bugs before any real
operation executes.

The remaining gap is ordering: nothing verifies at build time that a
`Get` for a key appears only in a node sequenced after the `Produce`
that binds it. This cannot be checked statically because `Get` calls
live inside opaque closures; it would require nodes to declare key
dependencies as data, which pulls into wart 1 territory. The
sequential plan interpreter and the convention of declaring keys
alongside their producer continue to prevent this in practice.

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

Within a single plan node, the forward path is a closure and the undo
path is `[]action.Action`. This means undo is inspectable and
testable independently of the executor, but the forward path is not.
The asymmetry is not accidental -- forward nodes often have data
dependencies (the output of node 1 feeds node 2) that require
closures in Go -- but it does mean the two halves of a node's
semantics have different testability characteristics.

**Risk level:** low. The asymmetry is a direct consequence of Go's
type system limitations and the pragmatic choice to use closures for
forward execution. Noting it here so it remains a conscious trade-off
rather than an accidental one.

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

`inspect/` builds a correlated view of BPF objects across store,
kernel, and filesystem -- the `World` type with `ManagedPrograms()`,
`ManagedLinks()`, `ManagedDispatchers()`. `manager/coherency/gather.go`
builds `ObservedState` doing the same thing: correlating programs,
links, and dispatchers across the same three sources.

Both solve "given three sources of truth, produce a unified snapshot."
They differ in audience: `inspect` serves the CLI (list, get, doctor
output) and gRPC handlers; `coherency` serves GC and doctor rules.
But the correlation logic is reimplemented rather than shared.

`inspect/` cannot move under `manager/` because `server/` also imports
it, and `server/` should not reach into `manager/` subpackages. But
`coherency/gather.go` could build its `ObservedState` from an
`inspect.World` rather than reimplementing the correlation. That would
make `inspect` the single correlation layer, with `coherency` as a
pure rule engine over it.

**Possible resolution:** refactor `coherency/gather.go` to accept an
`inspect.World` (or a subset of it) and derive `ObservedState` from
that. This removes the duplicated correlation logic and ensures that
the two views are always consistent.

---

## 11. `netns/` and `nsenter/` are unrelated top-level packages for related work

**Resolved.** Moved to `ns/netns/` and `ns/nsenter/`.
