# Architectural Review

An architectural review of go-bpfman through the lenses of two books:
*Functional Programming in Scala* (the Red Book, 2nd edition) and
Ousterhout's *A Philosophy of Software Design*.

## What the codebase does well

### Effects as values (Red Book, chapters 13-15)

The `action/` package reifies every side effect as a data structure. A
single interpreter (`executor.go`) owns the type-switch. This is the
IO monad idea -- separate *describing* what to do from *doing* it --
implemented idiomatically in Go. The SANS-IO discipline is real and
consistently held.

### Algebraic data types (Red Book, chapter 3)

Sealed interfaces with unexported marker methods approximate sum
types. `LinkDetails`, `Action`, `ProgramType`, `LinkKind` -- all use
this pattern. Newtypes prevent string/int confusion. The "illegal
states unrepresentable" philosophy is visible throughout: `LoadSpec`
uses private fields with validation in constructors, `ProgramRecord`
separates input from stored form from observed status.

### Deep modules (Ousterhout, chapter 4)

The `operation/` package is a good example: small interface (`Run`,
`Build`, `Produce`, `Do`), significant internal machinery (interpret,
rollback, binding registry). Similarly, `fs.BPFFS` hides a pile of
path computation behind a clean capability token. The lock package
(`lock/`) is another: cross-process flock + in-process mutex + FD
inheritance + exponential backoff, all behind `WriterScope`.

### Information hiding (Ousterhout, chapter 5)

The platform interfaces compose small single-method interfaces into
aggregate ones (`ProgramReader`, `ProgramWriter` -> `ProgramStore` ->
`Store`). Callers take the narrowest interface they need. The
dependency flow is strictly downward: `manager/ -> platform/ ->
kernel/`. Pure packages never import impure ones.

## Red Book alignment: the three-layer model

bpfman is a natural fit for the Red Book's core architectural idea:
separate describing effects from performing them. The domain is dense
with side effects -- bpffs pins, BPF syscalls, netlink, SQLite, mount
namespace switching, logging -- and every operation mixes domain
decisions with multiple effect types. The Red Book prescription is a
three-layer split: a pure domain layer, a program layer that builds
plans from intent and observed state, and an interpreter layer that
owns all I/O. The codebase implements this pattern in Go-native form
with varying completeness across the different operation paths.

### Layer A: domain types (fully in place)

Pure types with enforced invariants. `LoadSpec` uses private fields
with validation in constructors. `ProgramRecord` separates input form
from stored form from observed status. `ProgramType`, `LinkKind`,
`LinkDetails` are sealed sum types approximated via unexported marker
methods. Newtypes (`kernel.ProgramID`, `kernel.LinkID`, `PinPath`)
prevent primitive confusion. No I/O in any of these packages.

### Layer B: plan builders (mostly in place)

Manager methods build plans as values: `loadPlan`, `unloadPlan`,
`detachPlan`, `simpleAttachPlan`, `dispatcherAttachPlan`,
`attachTCXPlan`. Each returns an `operation.Plan` -- a sequence of
typed nodes (`Produce`, `Do`, `Try`) with compensation declarations
(`UndoFrom`). The plan builder does not perform I/O; `operation.Run`
does.

**Observation: plan builders close over Manager, but narrowly.** The
plan builders are methods on `*Manager` and close over Manager, but
the actual references touched are limited to `m.rt.BPFFS()` and
`m.rt.Bytecode()` -- both capability tokens used for pure path
computation. No plan builder touches `m.store`, `m.kernel`,
`m.executor`, or `m.logger`. A strict Red Book reading would have the
plan builder take these two capabilities as arguments, but the gap is
cosmetic: the discipline of keeping I/O out of plan builders is
already held in practice. Widening six function signatures to pass
capabilities that are already available on the receiver would add
noise without changing behaviour.

**Gap: no unified snapshot.** The ideal Red Book pattern has
operations shaped as `snap := observe(); plan := build(intent, snap);
outcome := execute(plan)`. The coherency system comes closest:
`ObservedState` in `manager/coherency/` gathers kernel, filesystem,
and database state into a single value, then rules evaluate against
it purely. But the load and attach paths do not work from a snapshot.
`Load` interleaves resolution steps (`resolveBatchSource`,
`resolveBatchPrograms`) with plan execution. The attach paths fetch
program records inside the plan nodes themselves rather than
pre-fetching into a snapshot.

### Layer C: interpreter / executor (fully in place)

The `action/` package defines actions as dumb structs with no
behaviour: `LoadProgram`, `UnloadProgram`, `SaveProgram`,
`DetachLink`, `RemoveProgramDir`, `PublishBytecode`, etc. A single
executor owns the type-switch and performs all I/O. The
`operation.Run` interpreter walks plan nodes sequentially, threading
the executor through each closure. On failure it executes accumulated
compensation actions in reverse order.

Capabilities follow Go's version of typeclasses: `Store`,
`KernelOperations`, `BPFFS` are narrow interfaces composed into
aggregates. Plan builders do not receive these capabilities; only the
executor does. This enforces effects at the edge.

### Layer B/C boundary: where the model breaks down

The dispatcher operations are the main holdout from the clean
three-layer split. `ensureDispatcher` in the executor does a store
lookup, conditionally creates a dispatcher, then the extension attach
uses retry with stale-dispatcher recovery. This is interleaved
fetch/compute/execute rather than snap/plan/execute. The executor is
performing conditional logic (does the dispatcher exist? is it stale?
do we need to retry with recovery?) that conceptually belongs in a
plan builder operating on a snapshot.

This is the tension identified in critique item 6: decomposing the
dispatcher operations into plan-level composition would require the
plan infrastructure to support conditional or branching primitives.
The current plan VM is a linear sequence with accumulated compensation
and a single scope. It handles the straightforward operations (load,
unload, detach, simple attach) cleanly. The dispatcher operations
have control flow that does not fit the linear model without
extending it.

### The plan system is a saga runtime

The plan infrastructure is, structurally, an orchestrated saga
runtime. The term comes from a 1987 database paper (Garcia-Molina &
Salem) describing multi-step operations where each step commits
independently and failures are handled by compensating actions rather
than a single global transaction. The pattern is now widely used in
microservices orchestration (Temporal, Cadence, Netflix Conductor),
but it applies equally to single-process systems that coordinate
across multiple commit domains -- which is exactly what bpfman does
(kernel state, bpffs, SQLite).

The mapping is direct:

| Saga concept           | bpfman implementation              |
|------------------------|------------------------------------|
| Step                   | `node` (`Produce` / `Do` / `Try`) |
| Compensating action    | `UndoFrom`                        |
| Orchestrator           | `interpret()` in `run.go`         |
| Saga execution         | `operation.Run`                    |
| Step result threading  | `Bindings` + `Key[T]`             |

A typical bpfman operation is literally a saga. For example, loading
a program:

1. Load program into kernel
2. Publish bytecode to filesystem
3. Save to database (commit boundary)

If step 2 fails: compensate step 1 (unload program from kernel).
If step 3 fails: compensate step 2 (remove bytecode directory),
compensate step 1 (unload program). The database save is the truth
boundary -- compensation covers the pre-commit steps whose durable
effects (kernel state, filesystem artefacts) must be reversed. Each
compensation is a domain-specific action, not a database rollback.
This is the saga pattern applied to local multi-system coordination.

The Red Book connection reinforces this. The book encourages programs
as data, interpreters, and composable effects. A saga implementation
has the same shape:

```go
type Step struct {
    Do   Action
    Undo Action
}
```

The executor walks steps forward; on failure, it walks compensations
in reverse. This is the same structure used in FP libraries
implementing IO or free monads. The plan system is the Red Book's
"IO as a description, interpreter executes it" idea applied to
multi-system coordination with compensation.

**What makes this powerful for bpfman.** The saga framing gives
uniform compensation (no ad-hoc cleanup scattered across operations),
structured observability (each step is a reportable event), and
simpler reasoning (operations become deterministic plans). If
durability were ever needed, saga progress could be persisted for
crash recovery. The three commit domains (kernel, filesystem,
database) are exactly the kind of multi-system coordination sagas
were invented for.

**The plan system is a complete single-scope saga orchestrator.**
Nodes are steps, `UndoFrom` closures are compensations, `interpret()`
is the orchestrator, and bindings thread step results. Each `Produce`
or `Do` node can declare compensating actions via `UndoFrom`
(late-bound, computed from bindings after execution). The interpreter
guarantees reverse-order execution on failure. This is a textbook
saga, fully realised.

**Vocabulary.** To reason precisely about what the plan system
supports and what it does not, three terms need to be distinguished:

- *Rollback:* undo for partial work within a failing step. This is
  step-local cleanup: a defer, a closure that tears down what the
  step partially built. The plan infrastructure does not manage
  this; steps handle it themselves (e.g., `createDispatcher`'s
  `result.rollback()` on persist failure).

- *Compensation:* undo for work that was considered committed in a
  completed step, because a later step failed. The undo stack in
  `interpret()` is accumulated on successful nodes only
  (`appendUndos` is called on the success branch). When a later
  node fails, `executeRollback` walks this stack in reverse,
  compensating previously committed steps. Despite the function
  name, the mechanism is compensation, not rollback.

- *Scope:* the boundary within which "committed" is defined. The
  current plan system has a single scope per `operation.Run` call.

The plan system supports compensation within a single scope. It does
not support compensation across scopes -- that is, expressing a node
that is itself a saga, where failure inside that node triggers its
internal compensation, but failure after it completes triggers a
distinct outer compensation strategy. This is not missing saga
machinery; it is the separate problem of composing sagas into larger
sagas.

**The batch load makes this concrete.** Batch load is a parent saga
whose steps are child sagas (one per program):

- *Parent saga:* load program A, load program B, load program C
- *Child saga (each load):* kernel load -> bpffs publish -> store
  save

If program C's child saga fails mid-execution, its child-scope
compensation runs: unpin the half-loaded program, remove the bytecode
directory. This is compensation within the child scope -- the undo
stack contains entries for child nodes that succeeded before the
failing node.

If program C's child saga fails after programs A and B have fully
completed, programs A and B need parent-scope compensation -- a full
`unload` that removes kernel state, filesystem artefacts, and the
database record. This is a different operation from A's or B's
child-scope compensation. A's child-scope compensation would unpin a
half-loaded program; A's parent-scope compensation must unload a
fully persisted one.

The current plan system has no way to express this distinction. Each
plan is a closed scope: once `Run` returns successfully, the undo
information is gone. The manual `cleanupLoaded` loop in `Load`
handles the parent-level compensation explicitly.

**Comparison with composed saga systems.** Temporal and Cadence solve
this with parent/child workflows: if a parent workflow fails after a
child completes, the parent's compensation runs the child's
compensating workflow, which is a different operation from the
child's internal compensation. They can do this because they have durable
execution (workflow state is persisted for replay and resume). bpfman
does not need durability -- everything is in-process and synchronous
-- but the structural insight is the same.

**The open question** is whether the plan system needs multiple
scopes, or whether a single scope per CLI/API operation is
sufficient. The batch load is currently the sole multi-saga
coordination site. The manual cleanup loop is seven lines, well
tested, and easy to reason about. The saga composition machinery
(phase/compensate nodes, cross-scope binding projection) would add
real infrastructure for one consumer. The framing is worth preserving
so that if additional multi-operation workflows emerge, the path
forward is clear: extend the plan system from a single-scope saga
into a composable one by adding "saga-as-step" nodes with separate
compensation semantics.

### The intellectual lineage: Red Book to workflow engines to sagas

The Red Book, workflow engines like Temporal and Cadence, and saga
orchestrators are all variants of the same architectural move:
separate describing work from executing work. Recognising this
progression clarifies why bpfman's architecture looks the way it does
and where it sits on the spectrum.

**Stage 1: the Red Book core idea.** Instead of executing effects
directly (`loadProgram(); pinProgram(); updateDB()`), you describe a
program as a value (`LoadProgram -> PinProgram -> UpdateDB`) and an
interpreter runs it. Programs are values. This is the fundamental FP
move.

**Stage 2: workflow engines.** Temporal and Cadence apply the same
pattern to distributed systems. You write what looks like normal
code (`reserveInventory(); chargeCard(); shipOrder()`), but the
runtime does not execute it directly. Instead it records a workflow
history and interprets the steps deterministically. The architecture
is: workflow (pure logic) -> workflow interpreter -> activities (real
side effects). That is the same separation the Red Book encourages.

**Stage 3: the hidden interpreter.** Under the hood, the workflow
engine does something conceptually identical to the Red Book
interpreter: for each step, record the event, execute the step, and
on failure trigger compensation. The workflow engine adds retries,
timeouts, distributed execution, crash recovery, and durable
history, but the core loop is the same.

**Stage 4: sagas.** A saga is a workflow where every step has a
compensation. The execution model is: run steps forward; on failure,
walk compensations in reverse. This matches the FP model exactly --
you are describing a transactional program as data and interpreting
it.

**Why workflow engines feel functional.** Even though they are
written in Go or Java, Temporal-style workflows rely on several FP
ideas:

- *Deterministic workflows.* Workflows must be deterministic so they
  can replay history. This is essentially pure function discipline:
  no randomness, no wall clock, no network calls inside workflow
  code. Those become activities (effects). The same separation FP
  enforces between pure code and IO.

- *Effects are explicit.* Workflow code is pure logic; activity calls
  are side effects. This is the IO monad pattern without the name.

- *Programs become data.* Temporal persists workflow history as an
  event log (`ActivityScheduled`, `ActivityCompleted`,
  `TimerFired`). The engine replays this log to reconstruct state.
  That is interpreting a program from its serialised representation.

**Where bpfman sits.** bpfman coordinates three commit domains
(kernel state, bpffs, SQLite) within a single process. Its
operations are local workflows:

```
LoadProgram -> CreateDirectory -> PinProgram -> InsertDBRow -> AttachDispatcher
```

Each step has a compensation. The plan system describes this workflow
as data and the executor interprets it. This is a local saga
orchestrator -- the same architecture Temporal uses for distributed
systems, applied to single-process multi-system coordination.

**What this architecture gives you.** Because operations are
represented as plans:

- *Deterministic reasoning.* You can inspect the plan before
  execution.

- *Uniform compensation.* Compensation steps are defined alongside
  forward steps, not scattered across the codebase.

- *Observability.* Each step is a structured event that can be
  logged, recorded, or reported (the coherency doctor already does
  this).

- *Testing.* Plans can run against fake kernel, fake filesystem, and
  fake database implementations without touching the real system.
  The recording executor in the test suite demonstrates this.

**The mental model.** When applying the Red Book to a systems project
like bpfman, the translation is:

```
Domain logic
    |
    v
Plan builder (pure)
    |
    v
Plan (data)
    |
    v
Executor (effects)
```

That single idea gives 80% of the benefit of FP without importing FP
libraries. The Red Book, workflow engines, and saga orchestrators are
all teaching the same lesson from different angles.

### Reducing compensation wiring

The codebase has exactly five `UndoFrom` declarations. Every one
compensates the same pattern: an irreversible kernel operation
(program load or link attach) that crossed the commit boundary before
the database did.

| Site | Compensates | Trigger |
|---|---|---|
| `load.go:192` | Unload program + maps | Later node failed |
| `load.go:226` | Remove bytecode dir | Store save failed |
| `attach_simple.go:113` | Detach link | Store save failed |
| `attach_dispatcher.go:132` | Detach link | Store save failed |
| `attach_tc.go:191` | Detach link | Store save failed |

These are the irreducible saga compensations. Kernel program loading
and kernel link attaching are immediate, irreversible effects -- the
BPF subsystem has no concept of tentative load or staged attach. If
the store save (the truth boundary) fails after a kernel operation
succeeds, compensation is mandatory. No amount of staging,
idempotency, or reconciliation eliminates this; it is a fundamental
property of the BPF kernel interface.

Three alternative strategies were evaluated for reducing compensation
code without eliminating the compensations themselves.

**Strategy 1: reconcile-to-target (idempotent actions).** Express a
target state and make the executor run convergent reconcilers.
Compensation becomes "set target back to previous snapshot, reconcile
again." The coherency system already does this: `ObservedState` +
rules + remediation is exactly reconcile-to-target for the backward
(cleanup) path. But the forward path creates new resources.
`bpf_prog_load()` is inherently non-idempotent -- each call creates
a new program with a new kernel ID. `EnsurePinned` does not make
sense when the program does not exist yet. This strategy applies to
cleanup but not to creation.

**Strategy 2: atomic publication.** Stage intermediate artefacts in a
temporary location, then atomically publish when everything is ready.
`PublishBytecode` already does this: it stages into `.staging/`,
writes bytecode and provenance JSON, then atomically renames to the
final path (`runtime.go:179`). It does its own step-local cleanup on
failure via a defer-based flag (`runtime.go:160-165`). Extending
this to bpffs pin layout is theoretically possible (bpffs supports
`rename(2)` within the same mount), but the kernel program is loaded
the moment `bpf_prog_load()` runs -- the pin is just a filesystem
reference keeping it alive. Staging the pin does not make the load
tentative.

**Strategy 3: step-local cleanup.** Each action cleans up its own
partial state on failure, so the saga only handles compensation for
committed steps. `PublishBytecode` already does this. Dispatcher
creation does it (`createDispatcher` calls `result.rollback()` on
persist failure at `executor_dispatcher.go:73`). But the five
`UndoFrom` sites are not compensating failed steps -- they are
compensating *succeeded* steps when a *later* node fails. Step-local
cleanup cannot address this; it is inherently saga-level.

**What could genuinely improve: Compensatable as a property of
produced values.** Currently, compensation knowledge lives at the
plan construction site: each `Produce` or `Do` node declares its
`UndoFrom` closure, and plan authors must remember to place it
correctly. The alternative is to make compensation a property of the
values that steps produce. If a `Produce` node returns a value that
implements a `Compensatable` interface, the interpreter automatically
registers it on the compensation stack. No explicit `UndoFrom`
needed.

```go
// Compensatable may be implemented by values produced by a
// Produce node. If a produced value implements this interface,
// its compensation actions are automatically registered on the
// undo stack when the node succeeds.
type Compensatable interface {
    Compensation() []action.Action
}
```

`LoadOutput` would implement it (returning `UnloadProgram` actions
for its pin path and maps directory). `AttachOutput` and
`extensionResult` would implement it (returning `DetachLink` for
their pin paths).

**Implementation sketch.** The interpreter change is surgical. In
`interpret()`, after a `Produce` node succeeds and the value is
stored in bindings, call `appendCompensation` before the existing
`appendUndos`:

```go
case flavourProduce:
    val, err := n.produceFn(ctx, exec, bindings)
    if err != nil {
        opErr = err
    } else {
        bindings.m[n.bindKey] = val
        undos = appendCompensation(undos, val)
        undos = appendUndos(undos, &n, bindings)
    }
```

Where `appendCompensation` is:

```go
func appendCompensation(
    undos [][]action.Action,
    val any,
) [][]action.Action {
    c, ok := val.(Compensatable)
    if !ok {
        return undos
    }
    if actions := c.Compensation(); len(actions) > 0 {
        return append(undos, actions)
    }
    return undos
}
```

Compensation actions are registered before any explicit `UndoFrom`
on the same node, so they appear earlier in the compensation stack
and execute later during the reverse traversal. The explicit
`UndoFrom` actions, added after, execute first (LIFO). Both
mechanisms coexist on a single node, enabling incremental migration.
The effects-as-values discipline is preserved because
`Compensation()` returns `[]action.Action`, not a direct method call.

The existing `appendUndos` path is unchanged, so late-bound undos
continue to work for non-kernel cases. The five
`UndoFrom` declarations can be removed one at a time as each
produced type gains a `Compensation()` method.

**How the five sites migrate.** Each produced value implements
`Compensation()` returning the actions currently wired via
`UndoFrom`. The existing code uses `UnloadProgram` for both the
program pin and the maps directory pin. This is a semantic overload
-- `UnloadProgram` is documented as "removes a BPF program from the
kernel," but removing a maps directory is a different operation that
happens to use the same bpffs unpin mechanism. The migration is an
opportunity to introduce a distinct action for maps cleanup (the
example is illustrative; concrete action names may differ):

```go
func (o LoadOutput) Compensation() []action.Action {
    return []action.Action{
        action.UnloadProgram{PinPath: o.PinPath},
        action.RemoveMapsPins{MapsDir: o.MapsDir},
    }
}
```

The `UndoFrom` at `load.go:192` is then deleted. The same pattern
applies to `AttachOutput` (returning `DetachLink{PinPath:
o.PinPath}`) and `extensionResult` (returning `DetachLink{PinPath:
r.pinPath}`). The bytecode publication compensation at `load.go:226`
is slightly different -- it compensates a filesystem publish, not a
kernel operation -- so it may remain as an explicit `UndoFrom` or
be absorbed into a composite produced value.

This does not change what gets compensated -- the irreducible kernel
operations remain. It changes where the knowledge lives:

- *Today:* plan authors must remember to add `UndoFrom` at the plan
  site, and the compensation closure must correctly extract values
  from bindings.
- *Proposed:* the value that was produced carries its own
  compensator. The interpreter registers it generically.

The benefit scales with new features. As new attach types, batch
operations, link variants, and multi-object loads are added, the
default becomes "if you produced something irreversible, the runtime
will compensate it" rather than "remember to add `UndoFrom` at the
plan site."

**Explicit commit boundary.** The store save step is implicitly the
commit boundary -- compensation runs if anything fails before it,
coherency/GC handles cleanup after it. Making this a first-class
concept in the plan system (e.g., a `Commit` marker node, or a flag
on the store save node) would make the convention explicit:
compensations only run if the operation aborts before commit; after
commit, remediation rules handle any inconsistency. This aligns with
the single-scope saga framing: the scope is "until commit." It
eliminates subtle "should this undo run here?" questions and keeps
the runtime's behaviour crisp.

**Counter-argument.** The commit boundary is always the store save --
it is the last `Do` node in every forward plan. The convention is
immediately apparent from reading any single plan builder. Adding a
`Commit` concept introduces a new abstraction that readers must learn,
without changing behaviour or catching errors that the current
convention misses. If a plan ever emerges whose commit boundary is
not the store save, an explicit marker would earn its keep. Today
none exists, and the convention is clear. The investment would be
better spent on `Compensatable`, which addresses the same fragility
concern (compensation wiring correctness) with a mechanism that also
reduces boilerplate.

**Net assessment.** The compensation surface is already small (five
sites) and correctly placed. The `Compensatable` interface and
explicit commit boundary would not reduce the number of compensations
but would make the system more robust to growth: new operations would
get automatic compensation without plan-site wiring, and the commit
boundary would be a declared property rather than an implicit
convention. The interpreter change is minimal (one type assertion,
one helper function) and the migration is incremental -- each
`UndoFrom` can be removed independently as its produced type gains
a `Compensation()` method.

### Summary

The architecture follows the Red Book pattern systematically for the
straightforward operations. The domain layer is clean. Actions are
pure data. The executor owns effects. Plan builders produce plans as
values with modelled compensation. The plan system is a complete
single-scope saga orchestrator operating across three commit domains
(kernel, bpffs, SQLite). The architecture sits on the same spectrum
as Temporal and Cadence -- local rather than distributed, synchronous
rather than durable, but structurally identical.

The compensation surface is small (five `UndoFrom` sites) and
irreducible: BPF kernel operations are immediate and irreversible,
and they must be compensated if the database commit fails. Atomic
publication and step-local cleanup are already applied where they
help. Three evolution paths are identified:

- `Compensatable` values to move compensation knowledge from plan
  sites to produced results.
- An explicit commit boundary to formalise the implicit convention
  that compensations apply only before the store save.
- `RunRetaining` with retained compensation tokens to enable
  multi-saga composition without extending the plan VM's execution
  model.

The gaps are at the edges: plan builders that close over Manager
rather than taking snapshots, and dispatcher operations whose
conditional logic lives in the interpreter rather than in plan-level
composition (correctly, as a deep-module boundary). The absence of
saga composition is addressed by retained compensation tokens, which
provide the minimal outer-scope primitive needed for multi-operation
workflows without introducing nested scopes, branching primitives,
or a Free Monad-style bind operator.

## Critique

### 1. The dispatcher duplication: a deep module trying to escape

**RESOLVED.**

`attachXDPExtensionWithRetry` and `attachTCExtensionWithRetry` have
been unified into a single `attachExtensionWithRetry` parameterised by
an `extensionOps` struct of closures. The struct captures the attach
call and the dispatcher recreation call; everything else (slot
counting, pin path construction, stale-dispatcher recovery) is written
once. The executor constructs the closures at the call site, capturing
type-specific kernel methods.

The `EnsureXDPDispatcher` / `EnsureTCDispatcher` duplication was also
resolved by extracting an `ensureDispatcher` helper on executor that
factors out the shared nsid lookup, store check, and conditional
creation into one method with a create closure.

The `createXDPDispatcherHelper` / `createTCDispatcherHelper`
duplication was resolved by extracting a `createDispatcher` method
parameterised by a `dispatcherCreateOps` struct of closures. The
struct carries a `kernelCreate` closure that absorbs spec
construction, validation, the kernel attach call, state computation,
and the rollback closure. The shared skeleton handles revision
initialisation, program pin path computation, logging, store
persistence, and rollback dispatch on persist failure. Two constructor
methods (`xdpDispatcherCreateOps`, `tcDispatcherCreateOps`) build the
type-specific ops.

### 2. The simpleAttachPlan / dispatcherAttachPlan / attachTCXPlan triplication (resolved)

**RESOLVED.** See action item 9.

Extracted `saveLinkNode` helper that builds the common "save link
record" Produce node. Each call site supplies a short `extract`
closure returning the four variable parts (link ID, details, pin
path, attach output). The three 20-line closures are now three
5-line call sites.

### 3. The unloadPlan closure boilerplate

**RESOLVED.**

`DoAction` and `TryAction` combinators were added to the `operation/`
package. They wrap a fixed action value in a Do or Try node,
replacing the five-line closure-per-node pattern. Both `unloadPlan`
and `detachPlan` have been rewritten to use them; each node is now a
single line.

### 4. tcParentHandle defined in three places

**RESOLVED.**

Moved to `dispatcher.TCParentHandle`. The duplicate definitions in
`manager/detach.go` and `manager/coherency/gather.go` have been
deleted. All call sites updated.

### 5. The operation infrastructure: slightly ahead of its usage

**PARTIALLY RESOLVED.**

`Validate` has been removed (no production call sites existed). The
plan system now has three node types: `Produce`, `Do`, `Try`.
`TryAction` was added as a convenience combinator alongside `DoAction`.

**Remaining:** `WithUndo` (static undo) still has no production call
sites; all undo uses `UndoFrom`. `Run0` is used only twice.

**Recommendation: delete `WithUndo`.** Every public symbol in the
operation package is something a reader must learn and decide whether
to use. `WithUndo` occupies the same conceptual slot as `UndoFrom`
but with different binding semantics, so a reader encountering both
must understand when each applies -- only to discover that one is
never used. The `Compensatable` proposal (see "Reducing compensation
wiring") would register compensation via `appendCompensation` in the
interpreter, not via `WithUndo` on the node, so `WithUndo` is not a
prerequisite for that evolution path. Removing it makes the node
options unambiguous: `UndoFrom` is the only undo mechanism. If a
future use case genuinely needs static undo known at plan construction
time, re-adding `WithUndo` is trivial (the implementation is four
lines). YAGNI applies.

### 6. The executor is a deep module with a hidden complexity gradient

**PARTIALLY RESOLVED.**

The `EnsureXDPDispatcher` / `EnsureTCDispatcher` duplication was
eliminated by extracting an `ensureDispatcher` helper. The two switch
cases are now two-line calls that pass a create closure. The
`AttachXDPExtension` / `AttachTCExtension` duplication was resolved
via `extensionOps` (see point 1).

**Remaining:** The executor still has non-uniform complexity. The
`Ensure*` and `Cleanup*` cases remain composite operations with
branching inside the interpreter rather than being decomposed into
plan-level composition. Decomposing them would require extending the
plan infrastructure with conditional or branching primitives. The
current conditional logic is well-contained (20-30 lines per helper)
and the deep actions are bounded to two; the cost of adding plan-level
control flow outweighs the benefit.

**Effect of Compensatable on dispatcher operations.** The
`Compensatable` interface (see "Reducing compensation wiring"
in the Red Book alignment section) would help in one specific place:
the `extensionResult` produced by node 3 of `dispatcherAttachPlan`
currently declares `UndoFrom` returning `DetachLink` for its pin
path. If `extensionResult` implemented `Compensatable`, this
declaration vanishes -- one of the five `UndoFrom` sites eliminated.

It would not help with the internal complexity of the deep actions.
`createDispatcher` performs step-local cleanup (`result.rollback`
at `executor_dispatcher.go:73`) when the store persist fails after a
kernel create succeeds. This is cleanup within a failing action, not
saga compensation for a succeeded one. `Compensatable` only applies
to successfully produced values.

`createDispatcher`'s internal `rollback` closure is, however,
structurally identical to what `Compensatable` describes: the kernel
result carries a closure that knows how to undo itself.
`dispatcherKernelResult` already bundles state with a rollback
function. This is the same pattern expressed manually rather than
through an interface. Unifying the two (plan-level `Compensatable`
and action-level rollback) would require the executor to handle
compensation at both levels through the same mechanism, which may
not simplify things given that one is triggered on success and the
other on failure.

**Implicit reliance on coherency for retry cleanup.** During
stale-dispatcher recovery in `attachExtensionWithRetry`, the old
dispatcher is deleted from the store and a new one is created (both
kernel state and store record). If the *retry attach* then fails,
the function returns an error. The recreated dispatcher exists in
both kernel and store but has no extension links. This state is
neither compensated by the plan (the node failed, so its `UndoFrom`
does not run) nor cleaned up by the action. It relies on the
coherency system's empty-dispatcher cleanup rules to detect and
remove it. This is correct -- the coherency rules handle orphaned
dispatchers -- but the reliance is implicit and worth documenting.
A `Compensatable` value on the recreated `dispatcher.State` would
not help because the value is created inside the action handler, not
by a `Produce` node.

**The blocker for further decomposition remains control flow, not
compensation.** The `Compensatable` pattern removes explicit wiring
from plan sites but does not address why `ensureDispatcher` and
`attachExtensionWithRetry` live in the executor rather than as plan
nodes. That blocker is conditional logic (check-then-create) and
retry with recovery (attach, detect stale, recreate, retry). These
require branching and looping that the linear plan model does not
support. This is the correct deep-module boundary (Ousterhout,
chapter 4): the plan says "ensure dispatcher" and "attach extension";
the executor absorbs the conditional and retry complexity behind a
simple action interface. Decomposing this into plan-level nodes
would push control flow into the plan infrastructure for no gain --
the complexity would move, not shrink. The executor is the right
place for it.

**Readability: signpost the complexity gradient.** The non-uniformity
is the correct design, but it is not signposted for readers. The
simple action cases in `executor.go` (lines 47-140) are thin
wrappers -- one method call each. The dispatcher cases in
`executor_dispatcher.go` are substantial multi-step transactions with
internal rollback. A reader entering the executor with the mental
model "each case is a thin translation" will be surprised by
`createDispatcher` (87 lines) and `attachExtensionWithRetry` (53
lines). A brief comment at the top of `executor_dispatcher.go`
noting that these are composite operations with internal rollback,
unlike the single-call cases in `executor.go`, would help readers
form the right mental model before encountering the complexity.

### 7. The batch load cleanup inconsistency

**PARTIALLY RESOLVED.**

**File:** `load.go:124-131`

Batch load uses an explicit `cleanupLoaded` closure that unloads
previously succeeded programs in reverse order when a later program
in the batch fails. This is a manual undo mechanism that operates
alongside the plan-based undo system used within each individual
load.

So a single Load call has *two* undo strategies:

- Per-program: plan-based compensation via `operation.Run` (automatic)
- Cross-program: explicit closure calling `m.unload` (manual)

The cleanup closure originally called the public `m.Unload()` method,
which performs preflight checks (store lookup, dependency counting,
link enumeration, dispatcher key collection) that are unnecessary for
freshly loaded programs with no links and no dependents. This has
been changed to call `m.unload()` directly, passing `nil` for links
and `true` for persisted, since we already know the program state.
The public method exists to protect callers who lack that knowledge;
inside `Load`, we have it.

**Red Book lens.** The two-strategy split is a failure of
compositionality. If plans composed (i.e., you could sequence plans
and get a bigger plan with merged undo), the batch cleanup would fall
out naturally. The current design forces a manual undo layer at the
orchestration level because plans are isolated units.

**Ousterhout lens.** Two mechanisms for the same concern (undo) is
conceptual complexity. A reader must understand both systems and know
when each applies.

**Saga framing.** As described in the Red Book alignment section, the
plan system is a complete single-scope saga orchestrator. This item
is the concrete case where single-scope is insufficient: each
per-program load is its own saga with internal compensation, but the
batch as a whole needs saga composition -- the ability to run N sagas
in sequence where failure in saga K triggers compensation of sagas
1..K-1 using a different compensation strategy (full `unload`) than
the one used for internal partial failures (unpin, remove directory).

Three properties of the plan VM make this composition hard:

1. *Plans are closed scopes.* Once `Run` returns successfully, the
   compensation information is discarded. There is no way to say "I
   succeeded, but hold onto my compensation in case a sibling fails
   later."

2. *Bindings are plan-local.* The output of saga 1 (e.g., the
   program ID needed for ShareMaps) cannot flow into saga 2's
   construction within the plan system. The loop in `Load` does this
   manually.

3. *Compensation is one-level.* A node has one compensation strategy.
   There is no distinction between inner compensation (partial
   failure within a step) and outer compensation (a fully committed
   step being undone because a later sibling failed). These are
   semantically different operations -- the former unpins a
   half-loaded program, the latter runs a full unload of a persisted
   program.

Three composition strategies were considered:

- **Phase/compensate (nested sagas).** A node type that runs an inner
  saga in its own scope and declares a separate compensating action
  for outer compensation. The inner saga handles its own partial failure;
  if a later phase fails, the outer compensation runs instead. This
  is how Temporal/Cadence handle child workflows. But the outer
  compensation needs data produced by the inner saga (e.g., the
  program ID), which means bindings must be shared across scopes,
  breaking isolation. This is the free monad `flatMap` problem -- the
  output of one computation feeds the next -- and Go cannot express
  typed continuations without significant ceremony.

- **Flat concatenation with key namespacing.** Concatenate node lists
  across sagas with prefixed keys to avoid collisions. But this loses
  the inner/outer compensation distinction entirely. A failure in
  iteration 3 would replay all accumulated compensations from
  iterations 1-3, which is wrong: iterations 1 and 2 succeeded and
  their per-node compensations (unpin program) are not the correct
  compensation when the program has already been persisted to the store.

- **Explicit compensation escalation.** Nodes declare two
  compensation levels: inner (used if this saga fails) and outer
  (used if a parent scope fails after this saga succeeds). This is
  what production saga orchestrators do, but it doubles the
  compensation surface area.

**Current assessment.** The batch load is the only place in the
codebase that needs retained compensation after success. Every other
use of `operation.Run` is a single call whose compensation is
discarded on success. The manual cleanup loop is seven lines, well
tested (`TestLoad_Rollback_SecondProgramFails`,
`TestLoad_Rollback_ThirdProgramFails`), and easy to reason about.
The saga composition machinery (phase/compensate nodes, cross-scope
binding projection) would add real infrastructure for one consumer.
If additional multi-operation workflows emerge, the path forward is
clear: extend the plan system from a single-scope saga into a
composable one.

### Retained compensation tokens (minimal saga composition)

There is a small extension that closes the batch-load composition
gap without introducing bind/flatMap, branching plan primitives, or
nested scopes inside the plan VM: allow a successful `operation.Run`
to retain a parent-scope compensation handle as a value.

**The core idea.** Today, `Run` is a closed scope: on success it
discards the internal compensation stack and returns bindings. On
failure it executes the compensation stack internally. The extension
adds `RunRetaining`, which on success returns an opaque `Token`
alongside the bindings. The token represents parent-scope
compensation: how to reverse this plan's committed effects as a unit
if a later sibling fails.

**Two compensation levels: child-scope vs parent-scope.** The
internal undo stack accumulated by `interpret()` captures
child-scope compensation: undo of completed steps when a later node
in the same plan fails. This is the right mechanism for internal
plan failure. Batch load needs parent-scope compensation: undo of a
fully successful child saga because a later sibling saga failed.
For a load plan, parent-scope compensation is a full unload (remove
DB record, unpin filesystem artefacts, unload kernel program). This
is not the same operation as the child-scope compensation used for
partial failure (unpin and remove staging artefacts).

`RunRetaining` therefore should not retain the internal undo stack
directly. The internal undo stack was designed for mid-plan failure
recovery; reusing it as parent-scope compensation conflates the two
levels. Instead, `RunRetaining` should retain an explicit
parent-compensation handle produced by the plan's result value.
This is a natural fit for `Compensatable`: the value that represents
the committed result (`LoadOutput`, `AttachOutput`,
`extensionResult`) already knows how to reverse its durable effects.

```go
// Token represents parent-scope compensation for a successful
// plan execution. It can be executed later to reverse the plan's
// committed effects if outer-scope failure requires it.
type Token struct {
    actions []action.Action
}

func (t *Token) Compensate(
    ctx context.Context,
    logger *slog.Logger,
    exec action.ExecutorWithResult,
)

// RunRetaining executes a plan. On success it returns bindings
// and a Token built from the plan's Compensatable result value.
// On failure it performs internal compensation and returns a nil
// Token.
func RunRetaining(
    ctx context.Context,
    logger *slog.Logger,
    exec action.ExecutorWithResult,
    plan Plan,
) (*Bindings, *Token, error)
```

The token's actions come from the produced result's
`Compensation()` method, not from the internal undo stack. For a
load plan, `LoadOutput.Compensation()` returns actions that
constitute a full unload (unpin program, remove maps, remove
bytecode directory, delete DB record). For an attach plan,
`AttachOutput.Compensation()` returns actions that detach the link
and remove the link record. This keeps child-scope and parent-scope
compensation cleanly separated: the internal undo stack handles
partial failure within a plan, and the token handles reversal of a
fully committed plan.

The implementation is minimal -- roughly 25 lines. On success,
`RunRetaining` collects `Compensation()` actions from produced
values that implement `Compensatable` and wraps them in a `Token`.
On failure, it behaves like `Run` and performs internal compensation
before returning error.

**How batch load changes.** The manual `cleanupLoaded` closure
disappears. The loop retains tokens and compensates them generically:

```go
var tokens []*operation.Token

for i, spec := range specs {
    if opts.ShareMaps && i > 0 && spec.MapOwnerID() == 0 {
        spec = spec.WithMapOwnerID(loaded[0].Record.ProgramID)
    }

    now := time.Now()
    b, token, err := operation.RunRetaining(
        ctx, m.logger, m.executor, m.loadPlan(spec, perProgOpts, now))
    if err != nil {
        for j := len(tokens) - 1; j >= 0; j-- {
            tokens[j].Compensate(ctx, m.logger, m.executor)
        }
        return nil, err
    }
    tokens = append(tokens, token)

    lo := operation.Get(b, loadedKey)
    // ... build program record, collect maps, append to loaded ...
}
```

The manual loop remains because `ShareMaps` requires the output of
iteration K to construct iteration K+1's `LoadSpec`. That is genuine
flatMap -- the result shapes the continuation -- and Go handles it
naturally as a for-loop. But the compensation is now plan-derived
rather than a bespoke `m.unload()` call. The two undo strategies
collapse into one mechanism.

**What this gives you.** Plans become composable sagas at the
orchestration layer without changing the plan VM's execution model:

- *Child saga failure:* handled internally by `Run`/`RunRetaining`
  compensation (inner scope, unchanged).
- *Parent failure after child success:* handled by executing
  retained tokens in reverse order (outer compensation, new).
- *FlatMap between sagas:* handled by the Go loop (unchanged).

The interpreter stays a simple forward-walk with reverse
compensation. No branching, no nested scopes, no continuations. The
composition primitive lives at the call site, not inside the VM.

**Interaction with Compensatable.** `Compensatable` serves double
duty. Within a single plan, it removes explicit `UndoFrom` wiring
by letting produced values declare their own child-scope
compensation (registered on the internal undo stack by
`appendCompensation`). Across multiple plans, the same
`Compensation()` method provides the parent-scope actions that
`RunRetaining` captures in the token. The interface is the bridge
between the two levels.

**Comparison with the three composition strategies.** This approach
is narrower than any of the three strategies considered above. It
does not introduce phase/compensate nodes (strategy 1), does not
concatenate plans (strategy 2), and does not double the compensation
surface with two-level undo (strategy 3). It retains a
parent-compensation handle derived from the plan's result value.
The caller decides what to do with it. The only prerequisite is that
produced result types implement `Compensatable`, which is
independently desirable for reducing compensation wiring.

**Generalisation.** If other multi-step workflows emerge (e.g.,
"load a set of programs then attach them all"), they use the same
pattern: `RunRetaining` per step, stash tokens, compensate on
failure. The mechanism is generic and does not need to know about
load, attach, or any specific operation.

**Where bpfman sits on the spectrum (updated).** With `RunRetaining`,
the architecture covers the full practical range:

```
imperative code
      |
actions as values
      |
plan + interpreter  (single-scope saga)
      |
retained tokens     (composable sagas)  <-- proposed
      |
composable programs (Free Monad / Temporal)
```

Most production Go systems stop at the plan+interpreter level.
`RunRetaining` takes one small step further -- retained compensation
-- without crossing into the Free Monad territory of typed
continuations and dynamic program construction. It is the smallest
composition primitive that addresses the only real multi-saga
orchestration site (batch load) while preserving the linear VM,
preserving Go ergonomics for flatMap, and keeping the child-scope vs
parent-scope distinction explicit. It generalises to future
multi-step workflows without committing the codebase to nested
scopes or typed continuations.

### 8. Coherency rules: verbose but not duplicated (understated)

**Files:** `coherency/rules.go`

The CoherencyRules (13 rules) are genuinely distinct: different
predicates, different data sources, different remediations. The
verbosity is inherent in the domain.

The GC rules are a different story. Four orphan-handling rules
(`orphan-program-artefacts`, `orphan-dispatcher-artefacts`,
`orphan-program-dirs`, `orphan-staging-dirs`) plus `PruneRule` share
identical control flow:

```go
for _, o := range s.OrphanFsEntries() {
    if o.Kind != <filter> { continue }
    if <liveness-check> { continue }
    out = append(out, Violation{...orphanPinAction(o)...})
```

The `orphanPinAction` helper at `rules.go:733` already acknowledges
the shared structure by centralising the kind-to-action mapping. But
the five enclosing rules repeat the iteration, filtering, and
violation construction identically. This is roughly 120 lines of
duplicated control flow that must be kept in sync when a new orphan
kind is added.

A table-driven approach collapses all five into a single function:

```go
type orphanRuleSpec struct {
    name, description string
    kinds             []OrphanKind
    skipAlive         bool // false = remove dead only; true = remove alive too (prune)
}
```

Each spec becomes one struct literal. The loop, filter, liveness
check, and violation construction are written once. The existing
`orphanPinAction` helper handles the kind-to-action dispatch. This
is not a minor improvement -- it removes structural duplication that
currently scales linearly with orphan kinds.

### 9. Store scanning duplication (resolved)

**Files:** `platform/store/sqlite/programs.go`,
`platform/store/sqlite/links.go`

**Status:** Resolved. See action items 7 and 8.

This was the single largest concentration of duplicated code in the
codebase. The original assessment ("the win is modest") undersells
both the scale and the maintenance hazard.

**Program scanning.** `scanProgram` (lines 38-167, single `*sql.Row`)
and `scanProgramFromRows` (lines 335-469, `*sql.Rows`) are not
"nearly identical" -- they are identical. Lines 73-166 and 374-468
are the same code: nullable scalar field handling, JSON unmarshalling,
timestamp parsing, `ProgramRecord` construction. The only difference
is the Scan call (one scans from `*sql.Row`, the other from
`*sql.Rows` with an additional `programID` column) and the zero-value
return shape. A schema change to the programs table requires updating
both functions with identical edits. A `buildProgramRecord` helper
that takes the already-scanned local variables and returns
`(bpfman.ProgramRecord, error)` eliminates ~90 lines of exact
duplication. This is mechanical extraction with no design trade-off.

**Link detail functions.** 24 functions share identical scaffolding:

- 8 `batchPopulate*Details` (lines 188-396): `rows.Next()` / `Scan()`
  / index-lookup / assign
- 8 `get*Details` (lines 695-890): `QueryRow` / `Scan` /
  three-branch error handling
- 8 `save*Details` (lines 441-555): `ExecContext` / timing / logging

The inner logic varies (different Scan columns, JSON unmarshalling for
XDP/TC, bool-to-int conversion for kprobe/uprobe), but the
scaffolding is the same. Each new link type requires cloning three
functions. Go's `database/sql` package does not support generics on
`Scan`, but the scaffolding can be extracted without generics. For
the batch populate case:

```go
func (s *sqliteStore) batchPopulate(
    ctx context.Context,
    stmt *sql.Stmt,
    label string,
    links []bpfman.LinkRecord,
    linkIndex map[kernel.LinkID]int,
    scanRow func(*sql.Rows) (kernel.LinkID, bpfman.LinkDetails, error),
) error
```

Each link type supplies a `scanRow` closure. The 8 functions become 8
two-line closures. The same pattern applies to the get and save
families. This would eliminate roughly 350 lines of repetitive code
and make adding a new link type a two-line closure instead of a
three-function clone.

**Ousterhout lens.** The 24 functions are shallow modules -- each does
little internal work, mostly boilerplate. The abstraction layer that
wraps them (`populateLinkDetails`, `SaveLink`) is the deep module.
Pushing the scaffolding into shared helpers deepens the individual
functions.

### 10. Try nodes silently swallow errors

**File:** `operation/run.go`

`interpret()` handles Try nodes by discarding the error entirely
(line 75: `_ = n.execFn(...)`). The error is not logged. A Try node
that fails produces no observable signal unless the caller
independently checks the outcome. The unload plan uses Try for
`fs-remove-program` (best-effort filesystem cleanup), which is
correct semantics -- but the absence of any logging means a reader
cannot tell whether the Try failed or succeeded without adding ad-hoc
instrumentation.

The interpreter already receives a logger. Logging Try failures at
debug level is one line of code and gives observability without
changing the non-fatal semantics.

### 11. CLI batch-mutation boilerplate

**Files:** `cmd/bpfman/unload.go`, `cmd/bpfman/detach.go`

Both commands implement the same batch-mutation skeleton: define a
local result struct with ID + error, create a results slice, lock,
iterate, collect results, count failures, print errors, return
aggregated error. The two files are character-for-character identical
modulo `ProgramID`/`LinkID`, `Unload`/`Detach`, and
`"program"`/`"link"`. A generic helper collapses both:

```go
func runBatchMutation[ID ~uint32](
    ctx context.Context, cli *CLI,
    ids []ID, label string,
    mutate func(context.Context, ID) error,
) error
```

Each command becomes a 5-line call. The `delete.go` command has extra
logic (cascading, recursive flag) so it does not collapse as cleanly,
but unload and detach are perfect candidates.

### 12. Action vocabulary: semantic overloads

**File:** `action/action.go`

**~~`RemoveProgramDir` vs `RemoveProgramDirByPath`.~~** RESOLVED.
The two action types have been unified into a single
`RemoveProgramDir{Path string}`. `Bytecode.ProgramDir` was exported
so forward-path callers (load, unload) resolve the path before
constructing the action. The executor no longer does path resolution
inside the interpreter. The GC path (coherency rules) continues to
pass discovered paths directly. One action type, one executor case.

**~~`UnloadProgram` used for two distinct operations.~~** RESOLVED.
`RemoveMapsPins` action introduced at `action.go:86-91`.
`load.go:196` now uses `RemoveMapsPins{PinPath: l.MapsDir}` for
maps directory compensation while `UnloadProgram` is reserved for
program pin removal. The vocabulary is honest.

Both semantic overloads from the original assessment are resolved.

### 13. `Describe()` exhaustiveness is not compile-time enforced

**File:** `action/describe.go`

The `Describe()` function has a type-switch covering all 35 action
types. But Go does not enforce exhaustive switching across files. If
action type 36 is added and `Describe()` is not updated, it falls
through silently. The sealed interface with `isAction()` prevents
external implementations but does not enforce exhaustive coverage
across secondary switches.

The mitigation is a test that reflects over all types implementing
`Action` and asserts `Describe()` returns a non-default string. A
`TestDescribe_Exhaustive` catches this at CI time.

### 14. Forward nodes are opaque closures

**File:** `operation/doc.go` (lines 27-56)

The operation package documents this trade-off explicitly, but the
consequence is worth stating plainly: it is impossible to write a
test that asserts "the load plan for this spec will produce these
three actions in this order with these undo actions" without running
the plan against a recording executor.

The undo side *is* inspectable -- `appendUndos` returns
`[][]action.Action`, which is pure data. The forward side is a black
box until execution. This is the correct trade-off for Go (typed
continuations would require generics abuse), but it means the plan
system's value is in structuring compensation, not in enabling
structural plan assertions. If plan-level assertions are ever
desired (e.g., "this plan will touch the store exactly once"), the
representation would need to move toward more value-oriented
forward nodes.

### 15. Coherency gather is a hidden complexity hotspot

**File:** `coherency/gather.go`

`GatherState()` (~260 lines) performs five phases of state
collection, building indexes and joining facts across three domains
(kernel, filesystem, database). This is the "fetch" in
fetch/compute/execute. The code is well-structured (lazy views,
cached joins), but the sheer volume of I/O is invisible from the
rules, which look pure and simple.

This is information hiding working correctly (Ousterhout, chapter 5)
-- the rules do not need to know about gathering. But the gather
function itself could benefit from documenting its phase boundaries
more explicitly. The current comments partially mark phases, but the
transitions blur. Numbering the phases and documenting the data each
one produces would help readers navigate the function.

### 16. `ExecuteGC` discards the computed plan's violations

**Files:** `manager/gc.go`

`ComputeGC` (line 181) gathers coherency state inside a rolled-back
transaction and populates `GCPlan.Violations`. But `ExecuteGC`
(line 316) never uses those violations. After executing the store
actions, it re-gathers coherency state against the real post-deletion
store (line 344) and evaluates rules afresh. The plan's violations
are only useful for dry-run reporting; in the execute path they are
silently ignored.

This is not a bug. Store action execution is best-effort (line
326-329 logs and continues), so the actual post-deletion state may
differ from the dry-run projection. Re-evaluation is correct. But
the `GCPlan` type promises violations that `ExecuteGC` ignores, and
a reader must trace through both code paths to discover this.

**Ousterhout lens.** The `GCPlan` type's shape implies a
snap/plan/execute split where the plan is executed as computed. In
practice the execute phase re-snaps. The API shape over-promises the
separation.

**Red Book lens.** The domain constraint is genuine: coherency rules
depend on filesystem state that is only orphaned after store
mutations run, so a fully pre-computed plan cannot capture the
complete picture. The re-evaluation is the right behaviour. The
issue is that the type does not communicate this.

**Possible improvements.** Document the re-evaluation explicitly on
`ExecuteGC`, or rename `GCPlan.Violations` to something like
`DryRunViolations` to signal that they are projections, not the
final set. Alternatively, remove `Violations` from `GCPlan`
entirely and have `ComputeGC` return store actions only, with a
separate `DryRunGC` method for reporting that includes the projected
violations. The current code is correct; the concern is purely about
API honesty.

### 17. TCX preflight performs a side effect outside the plan

**File:** `manager/attach_tc.go` (line 128)

The `attachTCX` method performs substantial preflight I/O before
building the plan: store lookup, namespace syscall, link listing,
and pure priority computation. This is the "observe" phase in
observe/plan/execute and is structurally correct. However, line 128
also executes a `RemovePin` action directly via the executor:

```go
if err := m.executor.Execute(ctx, action.RemovePin{Path: linkPinPath}); err != nil {
    return bpfman.Link{}, fmt.Errorf("remove stale TCX link pin %s: %w", linkPinPath, err)
}
```

This is the only place in the attach paths where a side effect runs
outside a plan. The stale pin removal is safe -- if it fails no
rollback is needed, and if it succeeds no compensation is needed.
But it breaks the expectation that preflight is pure observation.

**Ousterhout lens.** A reader expects preflight to gather data for
plan construction. Mixing observation with side effects requires the
reader to reason about which preflight steps are pure and which are
not.

**Red Book lens.** The effect is outside the plan boundary, so it
has no compensation and no structured observability. It is a fire-
and-forget side effect in what is otherwise a disciplined
effects-as-values architecture.

**Possible improvement.** Move the stale pin removal into the plan
as a `Try` node at position 0, before the `AttachTCX` Produce node.
This gives it structured observability (Try node debug logging) and
aligns with the pattern used everywhere else. The semantic change is
zero: a Try node that fails does not abort the plan, matching the
current behaviour where a missing pin is not an error.

### 18. `computeStoreGC` is non-deterministic

**File:** `manager/gc.go` (lines 22-102)

`computeStoreGC` iterates `map[kernel.ProgramID]bpfman.ProgramRecord`
to find stale programs. Map iteration in Go is non-deterministic.
The function appends to `dependents` and `owners` slices in whatever
order the map yields. The resulting action sequence is correct
(dependents before owners for FK ordering) but non-deterministic
within each group.

For GC actions executed in a transaction, ordering within a group
does not matter semantically. But it makes test assertions fragile
(tests must sort or use set comparison) and makes logs
non-reproducible across runs.

**Red Book lens.** Pure functions should be deterministic. Sorting
the program IDs before appending to each slice costs nothing and
makes the function fully deterministic: same input, same output,
every time. The same applies to the dispatchers slice in phase 2 and
the links slice in phase 3, though those are already slices (not
maps) so their iteration order is stable if the caller provides a
stable order.

### 19. `verbose` is a required constructor parameter

**File:** `manager/manager.go` (line 74)

The `New` constructor takes `verbose io.Writer` as a required
parameter. Line 89 wraps the executor with
`verboseExecutor{real: exec, w: verbose}`. Callers that do not want
narration must pass `io.Discard`.

**Ousterhout lens.** This is a configuration concern leaking into the
constructor signature. Every caller must decide what to pass even if
narration is unwanted. A functional option `WithVerbose(w io.Writer)`
with `io.Discard` as the default would reduce the required parameter
surface by one and make the constructor's essential dependencies
(runtime, store, kernel, discoverer, logger) stand out more clearly.

This is minor. The current code is correct and the parameter count is
not excessive. But if the constructor ever grows another optional
capability, the option pattern would become worth the investment.

## Where to invest effort

Ranked by impact-to-effort ratio. Items marked DONE have been
resolved; remaining items are renumbered.

1. ~~**Unify dispatcher extension retry logic.**~~ DONE. Unified into
   `attachExtensionWithRetry` with `extensionOps`. `ensureDispatcher`
   helper extracted.

2. ~~**Add `DoAction` combinator.**~~ DONE. `DoAction` and `TryAction`
   added; `unloadPlan` and `detachPlan` rewritten.

3. ~~**Move `tcParentHandle` to `dispatcher` package.**~~ DONE.
   Exported as `dispatcher.TCParentHandle`.

4. ~~**Remove `Validate` node type.**~~ DONE. Removed; three node
   types remain (`Produce`, `Do`, `Try`).

5. ~~**Unify `createXDPDispatcherHelper` /
   `createTCDispatcherHelper`.**~~ DONE. Unified into
   `createDispatcher` with `dispatcherCreateOps`. Constructor methods
   `xdpDispatcherCreateOps` and `tcDispatcherCreateOps` supply the
   type-specific closures.

6. ~~**Use internal `m.unload` for batch cleanup.**~~ DONE. The
   `cleanupLoaded` closure now calls `m.unload()` directly, bypassing
   the public method's unnecessary preflight checks for programs
   whose state is already known.

7. ~~**Extract `buildProgramRecord` from the two scan functions**
   (`programs.go`).~~ DONE. Extracted `scannedProgram` struct and
   `buildProgramRecord` helper. Both `scanProgram` and
   `scanProgramFromRows` now scan into the struct and delegate to
   the shared builder.

8. ~~**Extract scaffolding helpers for link detail batch/get/save**
   (`links.go`).~~ DONE. Extracted `batchPopulateDetails`,
   `getDetailsFromRow`, and `saveDetails` helpers. The 24 per-type
   functions are replaced by closures at each call site.

9. ~~**Factor the shared "save link record" node**~~ out of
   `simpleAttachPlan` / `dispatcherAttachPlan` / `attachTCXPlan`.
   DONE. Extracted `saveLinkNode` helper; each call site supplies a
   short `extract` closure for the four variable parts.

10. ~~**Log Try node failures at debug level** in the interpreter.
    Thread the logger into `interpret()` and log at debug level when
    a Try node's closure returns an error. One line of code, immediate
    observability win.~~ DONE. Threaded `*slog.Logger` into
    `interpret()` and added `DebugContext` logging for Try node
    failures. Also corrected `doc.go` to say three node types.

11. ~~**Delete `WithUndo`.** No production call sites exist. Not a
    prerequisite for `Compensatable`. Removing it makes the node
    options unambiguous: `UndoFrom` is the only undo mechanism.~~
    DONE. Removed `WithUndo` and `staticUndo` field from the node
    struct. Test call sites use a local `staticUndo` helper.

12. ~~**Unify CLI batch-mutation boilerplate** in unload/detach
    commands via a generic `runBatchMutation[ID]` helper.~~
    DONE. Generic `runBatchMutation[ID ~uint32]` in `cli.go`;
    `unload.go` and `detach.go` now delegate to it.

13. ~~**Table-driven orphan GC rules.** Collapse the five
    orphan-handling rules (four GC rules plus PruneRule) into a
    single table-driven function. Removes ~120 lines of duplicated
    control flow.~~ DONE. Introduced `orphanGCSpec` struct and
    `orphanRule` builder; four GC specs plus `PruneRule` use it.

14. ~~**Signpost executor complexity gradient.** Add a brief comment
    at the top of `executor_dispatcher.go` noting that its cases are
    composite operations with internal rollback, unlike the
    single-call thin wrappers in `executor.go`.~~ DONE. File-level
    comment added.

15. ~~**Unify `RemoveProgramDir` / `RemoveProgramDirByPath`.** One
    action type with a resolved path. Callers resolve paths before
    constructing the action.~~ DONE. `RemoveProgramDirByPath` deleted;
    `RemoveProgramDir` now takes a `Path string`. `Bytecode.ProgramDir`
    exported so forward-path callers resolve before constructing the
    action. One action type, one executor case.

16. ~~**Introduce `RemoveMapsPins` action.** Replace the overloaded
    `UnloadProgram{PinPath: mapsDir}` with a semantically distinct
    action for maps directory removal.~~ DONE. `RemoveMapsPins`
    action added at `action.go:86-91`; `load.go:196` uses it for maps
    directory compensation. `UnloadProgram` now only removes program
    pins.

17. ~~**Add `TestDescribe_Exhaustive`.** A test that enumerates all
    concrete Action types and asserts `Describe()` returns a
    non-default string, catching missing cases at CI time.~~ DONE.
    Test enumerates all 35 action types and checks for the `%T`
    fallback.

18. **Implement `Compensatable`** as described in the Red Book
    alignment section. Makes correct compensation automatic for new
    attach/load types. (moderate effort, incremental migration)

19. ~~**Document `GatherState` phases.** Number the phases explicitly
    and document the data each one produces.~~ DONE. All five phases
    are numbered and documented with comment separators in
    `gather.go`.

20. **Document `ExecuteGC` re-evaluation.** The `GCPlan.Violations`
    field is only used for dry-run reporting; `ExecuteGC` always
    re-gathers coherency state after store mutations. Either document
    this on `ExecuteGC`, rename the field to `DryRunViolations`, or
    separate the dry-run and execute APIs so the type does not
    over-promise. (small effort, API clarity)

21. **Move TCX stale pin removal into the plan.** Replace the direct
    `m.executor.Execute(ctx, action.RemovePin{...})` call at
    `attach_tc.go:128` with a `Try` node at position 0 of the TCX
    plan. Gives structured observability and aligns with the
    effects-as-values discipline. (small effort)

22. **Make `computeStoreGC` deterministic.** Sort program IDs before
    appending to `dependents` and `owners` slices. Same input, same
    output, every time. Makes test assertions stable and logs
    reproducible. (trivial effort)

23. **Make `verbose` an optional constructor parameter.** Replace the
    required `verbose io.Writer` parameter in `manager.New` with a
    `WithVerbose(w io.Writer)` functional option, defaulting to
    `io.Discard`. Reduces the required parameter surface. (small
    effort, low priority)

The overall architecture is strong. The SANS-IO discipline, sealed
type hierarchies, and effects-as-values approach are well-executed.
The main complexity comes from dispatcher management, which is
inherently complex (two subsystems with different kernel APIs sharing
the same lifecycle pattern).

The action vocabulary is honest and nearly complete. The plan system
is a well-realised single-scope saga orchestrator. The dependency
flow is strictly downward with no violations. The store and CLI
layers are clean after recent refactoring. The remaining debt is
small: one action vocabulary overload, one plan discipline
inconsistency, and a few API clarity items. None of these are
urgent; all are straightforward to address.
