# OpContext: declarative operation recording for the manager

## Problem statement

Every manager operation (load, unload, attach, detach, GC) repeats the
same mechanical structure:

* create an `OperationOutcome` and `Recorder`
* define a `fail` closure that sets `PrimaryError`, finalises, and wraps a
  `*ManagerError`
* manually drive the recorder (`FailStep` / `CompleteStep` / `Skip`)
* maintain a separate rollback mechanism (undo stack, ad-hoc rollback
  functions, or bespoke loops)
* on failure, run rollback and record it

This accounts for a large fraction of the line count in each operation
method and is duplicated across multiple codepaths.

A series of commits have reduced local duplication and fixed correctness
issues:

* **3df7fc8** Extract `simpleAttach` helper to eliminate attach method
  duplication.
* **4590171** Extract `dispatcherAttach` to deduplicate AttachXDP and
  AttachTC.
* **06b4c85** Unify Load rollback handling across all failure phases.
* **7fb1b69** Replace parallel unload arrays with paired `unloadEntry`
  structure (action + step), removing index coupling.
* **70b5e0e** Report recorder invariant violations via `onErr` callback.
* **66fa047** Fix incorrect `FailStep` call in Unload best-effort cleanup
  (a "dead stack-local variable" issue).

These changes help, but the root cause remains: the recorder is a passive
data structure, so every operation must orchestrate its state machine
correctly. The recorder's invariant violations (`ErrAlreadyFailed`,
`ErrRollbackNotActive`) exist because callers can record steps in invalid
phases or sequences.

A better design makes "record steps correctly and consistently" a deep
module owned by one abstraction, not repeated across every operation.

## Current shape of the boilerplate

Most operations begin with:

```go
var o outcome.OperationOutcome
rec := outcome.NewRecorder(&o, func(err error) {
    m.logger.Error("outcome recorder: invariant violation", "error", err)
})

fail := func(primaryErr error) (T, error) {
    o.PrimaryError = primaryErr.Error()
    rec.Finalise()
    return zero, &ManagerError{Outcome: o, Cause: primaryErr}
}
```

Each step repeats:

```go
err := doSomething()
if err != nil {
    primaryErr := fmt.Errorf("context: %w", err)
    return fail(rec.FailStep(outcome.StepKindFoo, target, primaryErr))
}
rec.CompleteStep(outcome.StepKindFoo, target, outcome.Details{...})
```

Rollback is separate and ad-hoc:

```go
var undo undoStack
undo.push(func() error { return cleanup() })
recordRollback(&rec, undo, outcome.Step{...}, m.logger)
```

The operation's intent is buried under recording and rollback ceremony.

## Design principles (Ousterhout)

Three principles from *A Philosophy of Software Design* apply:

1. **Deep modules**: expose a small interface that hides substantial
   complexity. The current recorder is shallow; callers must coordinate
   many small methods correctly.

2. **Define errors out of existence**: invariant violations exist because
   callers can drive the recorder incorrectly. If a transaction object
   owns the state machine, these invalid sequences become structurally
   impossible.

3. **General-purpose modules**: one transaction mechanism serving all
   operations (load, unload, attach, detach, GC) is deeper than bespoke
   patterns in each method.

## Proposed design: OpContext

### Summary

`OpContext` is a transaction object for a multi-step manager operation.
It owns:

* the operation outcome + recorder
* the first failure (`oc.err`)
* the rollback plan, expressed as `[]rollbackEntry` -- Action values
  paired with rollback step metadata and a policy
* phase transitions (primary -> rollback -> done)
* standard finalisation + `*ManagerError` wrapping

Callers declare steps. Steps may declare undo as Action values.
On failure, remaining steps auto-skip and rollback runs via the existing
executor, keeping one interpreter for all side effects.

### Transaction structure

```go
type OpContext struct {
    outcome outcome.OperationOutcome
    rec     outcome.ManagerOperationRecorder
    logger  *slog.Logger

    err   error
    phase opPhase

    rollback []rollbackEntry

    residualProbe ResidualProbe
}

// RollbackPolicy controls whether a rollback entry stops the rollback
// sequence on failure or allows it to continue.
type RollbackPolicy int

const (
    RollbackStrict      RollbackPolicy = iota // stop rollback on failure
    RollbackBestEffort                        // log/record failure, keep going
)

type rollbackEntry struct {
    action Action
    step   outcome.Step
    policy RollbackPolicy
}

type ResidualProbe func(ctx context.Context) ([]outcome.Artefact, error)

func (m *Manager) beginOp(ctx context.Context) *OpContext { ... }
```

`beginOp(ctx)` also sets standard fields such as OpID (from context), so
all operations carry correlation data consistently.

### SANS-IO alignment

The codebase already models effects as values:

* `DetachLink{PinPath: ...}`
* `UnloadProgram{PinPath: ...}`
* `RemovePin{Path: ...}`
* `DeleteProgram{KernelID: ...}`

These are interpreted by a central executor (`ExecuteAllWithResult`).

Rollback must follow the same pattern. Closure-based undo keeps the
boilerplate problem alive, just relocated. OpContext records undo as
Action values and delegates execution to the executor.

### Action set for bytecode removal

"No special-cases" implies that bytecode directory removal must also be
expressed as an Action interpreted by the executor. Introduce one new
Action:

* `RemoveProgramDir{KernelID uint32}`

This wraps the existing `rt.RemoveProgram(kernelID)` call. It allows both:

* Unload's best-effort bytecode cleanup to be expressed as an Action
* load rollback to include bytecode removal in its rollback plan once the
  publish step succeeds

This keeps the language uniform: everything that can be undone is an
Action.

### Declarative steps with co-located undo

OpContext removes boilerplate by making step recording and rollback-plan
construction the default behaviour of the transaction. Callers declare a
sequence of steps; each step may declare how it should be undone if a
later step fails.

The key rule is:

**Undo is always expressed as an Action value, interpreted by the same
executor as Unload. No closure-based undo. No special cases.**

#### Undo is an Action, not a closure

As established in the SANS-IO alignment section above, the codebase
models all side effects as Action values interpreted by the executor.
Rollback follows the same pattern: each step declares its undo as an
Action paired with an `outcome.Step` describing the rollback.

This keeps load rollback and explicit Unload speaking the same language:
a plan of `(Action, outcome.Step)` pairs.

#### Two undo patterns: late-bind and inline

There are only two cases:

1. **Late-bind undo**: the undo Action depends on values produced by the
   step (typically paths returned by the kernel).
2. **Inline undo**: the undo Action can be constructed from values that
   are already in scope at the time the step is declared (typically an
   ID).

OpContext supports both without introducing a second abstraction.

**Pattern A: late-bind (Fetch + OnUndo)**

Fetch returns a value, and the undo Action depends on that value (e.g.
`loaded.PinPath`, `loaded.MapsDir`). In this case the undo cannot be a
Step option: it can only be declared once the value exists.

So the pattern is:

* `loaded := Fetch(...)`
* `if oc.Failed() { return ... }`
* `oc.OnUndo(action, rollbackStep)`

This keeps all state transitions inside OpContext, while still allowing
the caller to construct the undo Action from the fetched value.

**Pattern B: inline (Step + WithUndo)**

Some undo Actions depend only on values already in scope (e.g. kernel ID,
program name). These undos are co-located directly on the Step:

* `oc.Step(..., WithUndo(action, rollbackStep))`

This is the most readable form and is the common case after the initial
Fetch.

#### Step options

```go
type StepOpt interface{ apply(*stepCfg) }

func WithDetails(d any) StepOpt { ... }

// WithUndo declares a strict rollback entry (stop on failure).
func WithUndo(action Action, step outcome.Step) StepOpt { ... }

// WithBestEffortUndo declares a best-effort rollback entry (continue on failure).
func WithBestEffortUndo(action Action, step outcome.Step) StepOpt { ... }
```

`WithUndo` defaults to strict (stop rollback on failure). Use
`WithBestEffortUndo` for cleanup that is safe to skip or retry later.

To avoid repeating `outcome.Step{Kind: ..., Target: ..., Details: ...}`
literals at every call site, add a constructor:

```go
func NewStep(kind StepKind, target string, details any) Step {
    return Step{Kind: kind, Target: target, Details: details}
}
```

This is cosmetic but materially improves readability.

#### Step (required)

```go
func (oc *OpContext) Step(kind outcome.StepKind, target string, fn func() error, opts ...StepOpt) {
    if oc.phase != phasePrimary || oc.err != nil {
        return
    }
    cfg := applyOpts(opts)

    if err := fn(); err != nil {
        oc.err = err
        oc.rec.FailStep(kind, target, err, cfg.details...)
        return
    }

    oc.rec.CompleteStep(kind, target, cfg.details...)

    if cfg.rb != nil {
        oc.rollback = append(oc.rollback, *cfg.rb)
    }
}
```

#### TryStep (best-effort)

TryStep is for non-fatal steps (currently needed for bytecode removal in
Unload). It records success but does not fail the operation on error.

```go
func (oc *OpContext) TryStep(kind outcome.StepKind, target string, fn func() error, opts ...StepOpt) {
    if oc.phase != phasePrimary || oc.err != nil {
        return
    }
    cfg := applyOpts(opts)

    if err := fn(); err != nil {
        oc.logger.Warn("best-effort step failed",
            "kind", kind, "target", target, "error", err)
        return
    }

    oc.rec.CompleteStep(kind, target, cfg.details...)
}
```

TryStep intentionally does not create a failed timeline entry, because a
failed primary step means "the operation failed". If TryStep failures
need to be surfaced structurally later, that is a separate Phase 2
refinement.

#### Fetch (value-producing step)

```go
func Fetch[T any](oc *OpContext, kind outcome.StepKind, target string,
    fn func() (T, error), opts ...StepOpt) T {

    var zero T
    if oc.phase != phasePrimary || oc.err != nil {
        return zero
    }
    cfg := applyOpts(opts)

    val, err := fn()
    if err != nil {
        oc.err = err
        oc.rec.FailStep(kind, target, err, cfg.details...)
        return zero
    }

    oc.rec.CompleteStep(kind, target, cfg.details...)

    if cfg.rb != nil {
        oc.rollback = append(oc.rollback, *cfg.rb)
    }

    return val
}
```

#### OnUndo (late-bind undo registration)

Fetch frequently returns values needed to construct the undo action (pin
paths, IDs). Use `OnUndo` after Fetch returns.

```go
func (oc *OpContext) OnUndo(action Action, step outcome.Step) {
    if oc.phase != phasePrimary || oc.err != nil {
        return
    }
    oc.rollback = append(oc.rollback, rollbackEntry{
        action: action, step: step, policy: RollbackStrict,
    })
}

func (oc *OpContext) OnBestEffortUndo(action Action, step outcome.Step) {
    if oc.phase != phasePrimary || oc.err != nil {
        return
    }
    oc.rollback = append(oc.rollback, rollbackEntry{
        action: action, step: step, policy: RollbackBestEffort,
    })
}
```

### Details: primary steps vs rollback steps

There are two distinct "details" channels:

* **Primary-step details**: details recorded on the forward timeline
  (`CompleteStep` / `FailStep`).
* **Rollback-step details**: details recorded when rollback executes
  (`RollbackComplete` / `RollbackFail`).

The rollback step must carry its own details because rollback recording
happens later and must stand alone. This is why `WithUndo` pairs an
Action with an explicit rollback `outcome.Step`.

Because of that, **`WithDetails` is usually redundant**: if you already
declared an undo with a rollback step that includes `ProgramDetails`, you
do not need to also attach the same `ProgramDetails` to the primary step.

There is one important exception:

**Primary-step details matter even when rollback never runs.**

For example, `KernelLoad` details (kernel ID, pin path) should be
recorded on the primary timeline for success and for early failures that
happen before rollback is reached. These details are about what happened
in the forward path, not about rollback.

So the rule of thumb is:

* use `SetDetails` (or `WithDetails`) for **primary-step details you want
  visible on the forward timeline**
* include details in the rollback `outcome.Step` for **rollback recording
  correctness**
* avoid duplicating the same details in both places unless the forward
  timeline genuinely benefits

In practice:

* `KernelLoad`: attach primary-step details (because rollback may never
  run, and it is valuable to see what was loaded)
* `FSPublish` / `StoreSaveProgram`: often do not need redundant primary
  details if the rollback steps already carry the essentials

```go
func (oc *OpContext) SetDetails(kind outcome.StepKind, target string, details any) { ... }
```

`SetDetails` attaches details to a primary-step timeline entry that has
already been recorded. This preserves intent without requiring callers to
mutate the timeline by index.

## Rollback: executor-driven, policy-classified

Two concerns are separated:

1. **Execution semantics**: whether rollback stops at first failure or
   continues.
2. **Recording semantics**: how rollback results are surfaced to callers.

### Rollback policy

Each rollback entry carries a `RollbackPolicy`:

* **`RollbackStrict`**: stop the rollback sequence on failure. Used for
  kernel-facing operations where order and dependencies matter (a later
  undo may assume an earlier undo succeeded) and where failure indicates
  the system is not in the expected state.

* **`RollbackBestEffort`**: record the failure and continue. Used for
  cleanup that is safe to skip or retry later (GC will catch it).

This matches the forward path: `Step` stops on failure, `TryStep`
continues. The same distinction applies to rollback entries.

### Default classification

**Strict (stop on failure)**:

* `UnloadProgram{PinPath: ...}` -- program pin, maps dir
* `DetachLink{...}` -- rolling back an attach
* `DeleteProgram{KernelID: ...}` -- store row removal
* anything where failure indicates unexpected system state

**Best-effort (continue)**:

* `RemoveProgramDir{KernelID: ...}` -- bytecode directory removal
* removing empty directories
* cleanup that GC can retry later

### Execution: one action per executor call (Phase 1)

For Phase 1, rollback calls the executor once per entry rather than
batching. This gives per-entry policy control without extending the
executor:

```go
func (oc *OpContext) RollbackOnFailure(ctx context.Context, exec ActionExecutorWithResult) {
    if oc.phase != phasePrimary {
        return
    }
    oc.phase = phaseRollback

    if oc.err == nil || len(oc.rollback) == 0 {
        return
    }

    oc.rec.BeginRollback()

    // Execute rollback entries in reverse order.
    anyFailed := false
    for i := len(oc.rollback) - 1; i >= 0; i-- {
        entry := oc.rollback[i]
        result := exec.ExecuteAllWithResult(ctx, []Action{entry.action})

        if result.Error == nil {
            oc.rec.RollbackComplete(entry.step)
            continue
        }

        failed := entry.step
        failed.Error = result.Error.Error()
        oc.rec.RollbackFail(failed)
        anyFailed = true

        if entry.policy == RollbackStrict {
            // Strict entry failed: stop rollback entirely.
            break
        }
        // Best-effort entry failed: log and continue.
        oc.logger.WarnContext(ctx, "best-effort rollback step failed",
            "kind", entry.step.Kind, "target", entry.step.Target,
            "error", result.Error)
    }

    if anyFailed && oc.residualProbe != nil {
        arts, probeErr := oc.residualProbe(ctx)
        oc.rec.SetResidual(arts, probeErr)
    }
}
```

Rollback entries that were never attempted are not recorded. Residual
probing surfaces what remains after partial rollback.

A future Phase 2 could extend the executor with a "continue on error"
mode (`ExecuteAllContinue`) returning per-action results, but the
one-at-a-time approach is sufficient and explicit for now.

### Recording semantics

Only attempted rollback entries appear in the timeline:

* **`RollbackComplete`**: the undo action succeeded.
* **`RollbackFail`**: the undo action was attempted and failed.
* **Not recorded**: entries after a strict failure that were never
  attempted. Residual probing (not timeline entries) surfaces what
  remains.

This avoids ambiguity: every recorded rollback entry was actually
attempted. "Skip" is reserved for the primary timeline (where it means
"auto-skipped due to prior failure").

## Error returns

Phase 1 preserves `*ManagerError` and existing `(T, error)` signatures.

```go
func (oc *OpContext) Error() error {
    if oc.phase == phaseDone {
        return nil
    }
    oc.phase = phaseDone

    if oc.err == nil {
        oc.rec.Finalise()
        return nil
    }

    oc.outcome.PrimaryError = oc.err.Error()
    oc.rec.Finalise()
    return &ManagerError{Outcome: oc.outcome, Cause: oc.err}
}
```

Phase 2 (optional): introduce `OpResult` that implements `error` and
carries the outcome, enabling first-class outcome access on success.

## Before and after

This section is consistent with the actual code and Action set, with one
explicit addition: `RemoveProgramDir{KernelID}`.

### load (current)

Key characteristics of the current `load` implementation:

* manual recorder driving:

  * `CompleteStep(StepKindKernelLoad, ...)`
  * `FailStep(StepKindPreflight, ...)`, etc.
* closure-based undo stack:

  * `undo.push(func() error { m.kernel.UnloadProgram(ctx, ...) })`
  * `undo.push(func() error { rt.RemoveProgram(id) })`
* a bespoke `rollbackLoad` closure that:

  * calls `recordRollback(...)`
  * sets residual artefacts if rollback fails
  * finalises + wraps `*ManagerError`

The core business logic is correct, but the "record + rollback + wrap"
machinery is interleaved with every phase.

### load (with OpContext)

Reads as a straight pipeline: preflight -> kernel load -> DB check ->
publish -> store save. Undo is co-located with each step that produces
reversible side effects. Both undo patterns appear:

* **Pattern A** (late-bind): kernel unload depends on returned pin paths.
* **Pattern B** (inline): filesystem removal and store deletion depend on
  kernel ID already in scope.

```go
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec, opts loadOpts) (bpfman.Program, error) {
    oc := m.beginOp(ctx)
    defer oc.RollbackOnFailure(ctx, m.executor)

    name := spec.ProgramName()
    rt := m.fsctx.BytecodeFS()
    now := time.Now()

    // Preflight validation.
    oc.Step(outcome.StepKindPreflight, "validation", func() error {
        if spec.HasImageSource() && spec.ObjectPath() == "" {
            return fmt.Errorf("load requires objectPath to be set; image pulling is handled by Load")
        }
        return nil
    })

    // Phase 1: load into kernel and pin to bpffs.
    loaded := Fetch(oc, outcome.StepKindKernelLoad, name, func() (*platform.KernelLoaded, error) {
        return m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
    })
    if oc.Failed() {
        return bpfman.Program{}, oc.Error()
    }

    // Forward-path details: visible even on success (rollback may never run).
    oc.SetDetails(outcome.StepKindKernelLoad, name, outcome.ProgramDetails{
        KernelID: loaded.Program.ID,
        PinPath:  loaded.PinPath,
    })

    // Pattern A: late-bind undo (depends on returned pin paths).
    oc.OnUndo(
        UnloadProgram{PinPath: loaded.PinPath},
        outcome.NewStep(outcome.StepKindKernelUnload, name, outcome.ProgramDetails{
            KernelID: loaded.Program.ID,
            PinPath:  loaded.PinPath,
        }),
    )
    oc.OnUndo(
        UnloadProgram{PinPath: loaded.MapsDir},
        outcome.NewStep(outcome.StepKindKernelUnload, name, outcome.ProgramDetails{
            KernelID:    loaded.Program.ID,
            MapsDirPath: loaded.MapsDir,
        }),
    )

    // Phase 1.5: DB existence check.
    oc.Step(outcome.StepKindPreflight, name, func() error {
        if _, err := m.store.Get(ctx, loaded.Program.ID); err == nil {
            return fmt.Errorf("program %d already exists in database", loaded.Program.ID)
        } else if !errors.Is(err, store.ErrNotFound) {
            return fmt.Errorf("check existing program %d: %w", loaded.Program.ID, err)
        }
        return nil
    })

    // Phase 1.6: publish bytecode.
    prov := buildProvenance(spec, loaded, now)

    oc.Step(outcome.StepKindFSPublish, name, func() error {
        return rt.PublishBytecode(loaded.Program.ID, spec.ObjectPath(), prov)
    },
        // Pattern B: inline undo (depends only on ID in scope).
        // Best-effort: bytecode dir removal is safe to retry via GC.
        WithBestEffortUndo(
            RemoveProgramDir{KernelID: loaded.Program.ID},
            outcome.NewStep(outcome.StepKindFSRemoveProgram, name, outcome.ProgramDetails{
                KernelID: loaded.Program.ID,
            }),
        ),
    )

    // Phase 2: persist metadata (transaction).
    record := buildProgramRecord(spec, loaded, opts, rt, now)

    oc.Step(outcome.StepKindStoreSaveProgram, name, func() error {
        return m.store.RunInTransaction(ctx, func(tx platform.Store) error {
            return tx.Save(ctx, loaded.Program.ID, record)
        })
    },
        WithUndo(
            DeleteProgram{KernelID: loaded.Program.ID},
            outcome.NewStep(outcome.StepKindStoreDeleteProgram, name, outcome.ProgramDetails{
                KernelID: loaded.Program.ID,
            }),
        ),
    )

    if oc.Failed() {
        return bpfman.Program{}, oc.Error()
    }

    return constructProgram(loaded, record), nil
}
```

Key properties:

* The code reads as a straight pipeline. Undo declarations are directly
  adjacent to the step they undo.
* `KernelLoad` demonstrates the only "ceremony" needed: one `Failed()`
  guard after Fetch and two late-bind undos.
* All rollback effects are Actions (`UnloadProgram`, `RemoveProgramDir`,
  `DeleteProgram`). No closure-based undo, no rollback special-cases.
* Rollback entries are classified: kernel-facing undos (`UnloadProgram`,
  `DeleteProgram`) are strict (stop on failure); cleanup
  (`RemoveProgramDir`) is best-effort (continue on failure, GC retries).
* The rollback plan is entirely Action-based. `RemoveProgramDir` exists
  specifically to avoid a filesystem special-case and to keep Unload and
  load rollback speaking the same language.
* Store save declares undo even though it is currently the final step.
  This preserves the invariant: every side-effecting step declares its
  undo, so future steps can be appended safely.
* Provenance and record construction are pulled into pure helpers,
  keeping the pipeline free of incidental noise.
* `SetDetails` is used only for the KernelLoad Fetch (primary-step
  details on the forward timeline). The void Steps do not need
  `WithDetails` because their undo steps already carry the relevant
  details for rollback recording.

### Unload (with OpContext)

Unload already follows FETCH -> COMPUTE -> EXECUTE. OpContext can absorb the
boilerplate around recorder setup, fail wrapping, and (where applicable)
best-effort steps.

Compute still returns `[]unloadEntry`:

```go
plan := computeUnloadPlan(kernelID, programName, progPinPath, mapsDir, linksDir, links)
```

Execute becomes:

* convert `unloadEntry` values to `rollbackEntry` values (adding policy)
* run them through the executor
* OpContext records completion/failure in one place

Bytecode removal (`RemoveProgramDir{KernelID}`) is classified as
best-effort in both Unload's forward path and load's rollback plan.
Strict entries (link detach, program unload, DB delete) stop execution
on failure.

This removes the remaining ad-hoc "call rt.RemoveProgram directly" path
and keeps all side effects within the Action language.

## Load and Unload: same language, different sources

Both load rollback and explicit unload speak the same language:

* **Unload** builds `[]unloadEntry` from stored state (`computeUnloadPlan`),
  converts to `[]rollbackEntry` with policy classification, and executes
  actions through the executor.
* **load rollback** accumulates `[]rollbackEntry` progressively as forward
  steps succeed (via `WithUndo` / `WithBestEffortUndo` and `OnUndo` /
  `OnBestEffortUndo`) and executes them through the same executor on
  failure.

The differences are where the plan comes from (persisted state vs forward
progress) and who classifies the policy (Unload at plan conversion time,
load at declaration time).

## Cascade semantics

* **OpContext**: transaction
* **Step/Fetch**: operations in the transaction
* **WithUndo/OnUndo**: declarative cascades expressed as Action values
* **RollbackPolicy**: strict (stop on failure) vs best-effort (continue)
* **Auto-skip**: abort remaining work after first failure
* **RollbackOnFailure**: execute cascades in reverse via the executor,
  respecting per-entry policy
* **TryStep**: best-effort work on the forward path (non-fatal)
* **Residual probe**: observe remaining artefacts after rollback failure

Rollback can partially fail; OpContext records only attempted entries and
surfaces residual artefacts to callers via the probe hook.

## Package structure

### Goal

`Manager.load` should read like a specification:

* preflight
* kernel load
* DB check
* publish
* store save

and nothing else. Recording, rollback, phase transitions, and
finalisation should be invisible at the call site.

This requires pushing the "machines" out of `manager` and into
subpackages that are tested independently.

### Three machines in the manager today

1. **Outcome recording state machine** -- primary steps, rollback steps,
   finalisation, residual artefacts.

2. **Rollback planning + execution** -- collect undo entries, reverse,
   strict vs best-effort policy, execute via executor, record results.

3. **Operation-specific planning** -- `computeUnloadPlan`, dispatcher
   retry logic, map sharing rules.

OpContext absorbs (1) and (2). (3) remains domain logic in `manager`.

### The import cycle constraint

Go's unexported interface methods can only be satisfied by types in the
same package. The `Action` interface uses an unexported `isAction()`
marker, so concrete Action types must live in the same package as the
interface.

If OpContext lives in a subpackage and needs to reference `Action` (it
does -- `rollbackEntry` stores an `action Action`), the Action interface
must live in a package that both `manager` and the subpackage can import.
This rules out keeping the Action interface in `manager` itself.

### Target layout

```
manager/action/
    action.go       Action interface (sealed, unexported marker)
    types.go        All concrete types: UnloadProgram, DetachLink,
                    RemovePin, DeleteProgram, SaveProgram, SaveLink,
                    LoadProgram, Batch, Sequence, SaveDispatcher,
                    DeleteDispatcher, DetachTCFilter, RemoveProgramDir
    executor.go     Executor, ExecutorWithResult interfaces,
                    ExecutionResult struct

    Imports: bpfman, bpfmanfs, dispatcher (leaf packages only)


manager/operation/
    opcontext.go    OpContext, Step, TryStep, Fetch, OnUndo,
                    RollbackOnFailure, Error, SetDetails, Failed
    types.go        rollbackEntry, RollbackPolicy, StepOpt family,
                    ResidualProbe

    Imports: manager/action (Action + executor interfaces)
             outcome (recorder, Step, StepKind, OperationOutcome)
             log/slog

    Does NOT import manager.


manager/
    manager.go      Manager struct, beginOp, GC
    executor.go     Concrete executor (type-switch over action.* types)
    load.go         load, Load (orchestration pipelines)
    unload.go       Unload, computeUnloadPlan, unloadEntry
    attach_*.go     Attach methods (orchestration)
    detach.go       Detach (orchestration)

    Imports: manager/action, manager/operation,
             outcome, platform, bpfman, etc.
```

### Dependency graph

```
bpfman, bpfmanfs, dispatcher, platform, outcome   (leaf packages)
        |
        v
manager/action       (effect vocabulary)
        |
        v
manager/operation    (transaction engine)
        |
        v
manager              (orchestration)
```

No cycles. Each layer depends only on layers below.

### What moves where

**To `manager/action`:**

* The `Action` interface and all 13 concrete types (currently in
  `manager/action.go`). These are pure data with no manager
  dependencies -- they import only `bpfman`, `bpfmanfs`, `dispatcher`.
* The `Executor`, `ExecutorWithResult` interfaces and `ExecutionResult`
  struct (currently in `manager/executor.go`). These reference only
  `Action` and `context`.
* The new `RemoveProgramDir{KernelID uint32}` type.

**To `manager/operation`:**

* `OpContext` and all supporting types (`rollbackEntry`, `RollbackPolicy`,
  `StepOpt`, `ResidualProbe`).
* The `Step`, `TryStep`, `Fetch`, `OnUndo` / `OnBestEffortUndo`,
  `WithUndo` / `WithBestEffortUndo`, `SetDetails`, `Failed`, `Error`,
  `RollbackOnFailure` methods.

**Stays in `manager`:**

* `Manager` struct and `beginOp` (creates `operation.OpContext`).
* Concrete executor (type-switch over `action.UnloadProgram`, etc.;
  calls `store` and `kernel` methods).
* `unloadEntry` (Unload's plan unit: `action.Action` + `outcome.Step`).
* `computeUnloadPlan` (pure function returning `[]unloadEntry`).
* `undoStack` and `recordRollback` (kept until all operations are
  migrated, then deleted).
* All operation methods (the declarative pipelines).

### `unloadEntry` vs `rollbackEntry`

Both pair an Action with an `outcome.Step`. They serve different roles:

* **`unloadEntry`** is the Unload plan unit. It lives in `manager` and
  has no policy field -- Unload's forward execution runs all entries
  strictly.

* **`rollbackEntry`** is OpContext's rollback plan unit. It lives in
  `manager/operation` and adds a `RollbackPolicy` field.

When Unload migrates to OpContext, it converts `[]unloadEntry` to
`[]rollbackEntry` with appropriate policy classification at the
orchestration layer. The plan computation (`computeUnloadPlan`) stays
pure and policy-free.

### What `Manager.load` looks like after the split

The only visible change is the `action.` prefix on Action types and
`operation.Fetch` instead of a local `Fetch`:

```go
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec, opts loadOpts) (bpfman.Program, error) {
    oc := m.beginOp(ctx) // returns *operation.OpContext
    defer oc.RollbackOnFailure(ctx, m.executor)

    name := spec.ProgramName()
    rt := m.fsctx.BytecodeFS()
    now := time.Now()

    oc.Step(outcome.StepKindPreflight, "validation", func() error {
        ...
    })

    loaded := operation.Fetch(oc, outcome.StepKindKernelLoad, name, func() (*platform.KernelLoaded, error) {
        return m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
    })
    if oc.Failed() {
        return bpfman.Program{}, oc.Error()
    }

    oc.SetDetails(outcome.StepKindKernelLoad, name, outcome.ProgramDetails{...})

    oc.OnUndo(
        action.UnloadProgram{PinPath: loaded.PinPath},
        outcome.NewStep(outcome.StepKindKernelUnload, name, outcome.ProgramDetails{...}),
    )

    oc.Step(outcome.StepKindFSPublish, name, func() error {
        return rt.PublishBytecode(loaded.Program.ID, spec.ObjectPath(), prov)
    },
        operation.WithBestEffortUndo(
            action.RemoveProgramDir{KernelID: loaded.Program.ID},
            outcome.NewStep(outcome.StepKindFSRemoveProgram, name, outcome.ProgramDetails{...}),
        ),
    )

    oc.Step(outcome.StepKindStoreSaveProgram, name, func() error {
        return m.store.RunInTransaction(ctx, ...)
    },
        operation.WithUndo(
            action.DeleteProgram{KernelID: loaded.Program.ID},
            outcome.NewStep(outcome.StepKindStoreDeleteProgram, name, outcome.ProgramDetails{...}),
        ),
    )

    if oc.Failed() {
        return bpfman.Program{}, oc.Error()
    }
    return constructProgram(loaded, record), nil
}
```

The method reads as a specification. The transaction engine, recording
state machine, and rollback policy are all behind the `operation` package
boundary.

### Why not a two-stage rollout?

An alternative is to implement OpContext in `package manager` first, then
extract to subpackages once the API stabilises.

This avoids the package surgery upfront but means doing the split twice:
once as "pretend boundaries" (file-level separation) and once as real
package moves. The boundaries are already clear from the dependency
analysis above:

* `manager/action` imports only leaf packages (`bpfman`, `bpfmanfs`,
  `dispatcher`).
* `manager/operation` imports only `manager/action` + `outcome`.
* Neither needs to import `manager`.

The Action types and executor interfaces have been stable across multiple
commits. OpContext's external interface (what `manager` calls) is small
and well-defined. The risk of API churn affecting the package boundary is
low.

Do the split once, from the start.

### Test strategy

`manager/operation` can be tested exhaustively before touching any
operation method:

* **Fake recorder**: stores calls in slices for assertion.
* **Fake executor**: configurable to succeed or fail at index N.

Test matrix:

* Step success/failure/auto-skip
* Fetch success/failure/zero return
* OnUndo/WithUndo registration only on success
* Rollback reversal order
* Rollback strict-stop vs best-effort-continue
* RollbackOnFailure is no-op on success
* SetDetails finds correct timeline entry
* Error() finalises exactly once
* Phase transitions prevent out-of-order recording

This locks down semantics before any migration.

## Limitations

* **Go lacks monadic composition**: Fetch still needs a guard after it
  returns if subsequent code would dereference a zero value. This is one
  guard per Fetch, not per Step.

* **Fetch undo requires late bind**: undo actions frequently depend on
  returned values. Start with the explicit `OnUndo` call; introduce an
  undo-builder only if it reduces real noise.

* **Dispatcher attach retry**: retry-based control flow remains
  imperative; OpContext still removes recorder/rollback boilerplate but
  does not force a linear pipeline.

* **Batch Load rollback**: the outer Load currently rolls back by calling
  `Unload` for previously loaded programs. That remains a higher-level
  concern than single-program operation rollback.

## Rollout

### Phase 1: package structure + OpContext

* Create `manager/action`: move Action interface, all concrete types,
  executor interfaces, ExecutionResult. Add `RemoveProgramDir{KernelID}`
  and interpret it in the concrete executor via
  `rt.RemoveProgram(kernelID)`.
* Create `manager/operation`: implement OpContext with full unit test
  coverage (fake recorder, fake executor).
* Migrate operations incrementally: start with `Unload` (already
  FETCH -> COMPUTE -> EXECUTE), then `load`, then attach/detach.
* Preserve `*ManagerError` and current outcome semantics.
* Delete `undoStack` and `recordRollback` once all operations are
  migrated.

### Phase 2: optional API refinement

* Introduce `OpResult` to surface outcomes on success without
  `errors.As`.
* If needed, add a structured "best-effort" reporting phase for TryStep
  failures.
* Consider extending the executor with `ExecuteAllContinue` for
  best-effort batch execution (replacing the one-at-a-time rollback
  approach if it proves too chatty).
