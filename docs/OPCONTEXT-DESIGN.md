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
* the rollback plan, expressed as `[]PlanEntry` -- Action values paired
  with rollback step metadata
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

    rollback []PlanEntry

    residualProbe ResidualProbe
}

// PlanEntry pairs an Action with the outcome.Step metadata for recording.
// Used by both rollback (accumulated undos) and forward plan execution
// (Unload's pre-computed plan).
type PlanEntry struct {
    Action Action
    Step   outcome.Step
}

type ResidualProbe func(ctx context.Context) ([]outcome.Artefact, error)

func (m *Manager) beginOp(ctx context.Context) *OpContext { ... }
```

`beginOp(ctx)` also sets standard fields such as OpID (from context), so
all operations carry correlation data consistently.

The `ResidualProbe` hook is set by the operation method after the values
it needs to probe are available (kernel IDs, pin paths, bytecode
directories). It is only called when rollback has at least one failure.

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

**Executor dependency.** Today the executor holds `store` +
`kernel`. Bytecode removal (`rt.RemoveProgram`) hangs off
`fsctx.BytecodeFS()`, not the kernel interface. Adding
`RemoveProgramDir` means the executor needs a third dependency --
either the `BytecodeFS` handle directly or a narrow interface
exposing `RemoveProgram(ctx, uint32) error`. This is a small
expansion of the executor's responsibility surface; it is the
natural consequence of expressing all side effects as Actions.

If bytecode FS management later moves to a separate component, the
executor either splits or this action moves with it. That is an
explicit trade-off of the "no special-cases" constraint: consistency
now, at the cost of coupling the executor to one more dependency.

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

// WithUndo declares a rollback entry (Action + outcome.Step).
func WithUndo(action Action, step outcome.Step) StepOpt { ... }
```

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

    if cfg.undo != nil {
        oc.rollback = append(oc.rollback, *cfg.undo)
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
failed primary step means "the operation failed".

**Information gap.** TryStep failures are only visible in logs. If the
whole point of the outcome is a rich structured record, this is a gap:
operators may not understand why residuals exist. Phase 2 should add a
structured way to surface "non-fatal failure" on the timeline (distinct
from primary failure) so the outcome is self-contained.

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

    if cfg.undo != nil {
        oc.rollback = append(oc.rollback, *cfg.undo)
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
    oc.rollback = append(oc.rollback, PlanEntry{Action: action, Step: step})
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
already been recorded. It finds the **most recent** matching
`(kind, target)` entry in the primary phase. If multiple steps share the
same kind and target (e.g. repeated `Preflight` validations), the last
one wins. This preserves intent without requiring callers to mutate the
timeline by index.

**Rule of thumb:** Fetch steps always set forward details via
`SetDetails` (because the details depend on the returned value). Void
Steps only add `WithDetails` when the forward timeline benefits beyond
what the undo step already carries for rollback recording.

## Execution: rollback and forward plans

### Rollback: always try all, record all

Rollback always tries every accumulated undo entry in reverse order.
It does not stop on failure. Rollback entries are typically independent
(kernel unload, DB delete, filesystem removal operate on different
subsystems), so stopping early leaves obvious cleanup unattempted.

This matches what the existing `undoStack.rollback()` already does:
iterate all entries, collect errors.

Phase 1 calls the executor once per entry (one action per call). This
is slightly chatty but gives per-entry recording without extending the
executor.

```go
func (oc *OpContext) RollbackOnFailure(ctx context.Context, exec ExecutorWithResult) {
    if oc.phase != phasePrimary {
        return
    }

    if oc.err == nil || len(oc.rollback) == 0 {
        oc.phase = phaseRollback
        return
    }

    oc.phase = phaseRollback
    oc.rec.BeginRollback()

    anyFailed := false
    for i := len(oc.rollback) - 1; i >= 0; i-- {
        entry := oc.rollback[i]
        result := exec.ExecuteAllWithResult(ctx, []Action{entry.Action})

        if result.Error == nil {
            oc.rec.RollbackComplete(entry.Step)
            continue
        }

        failed := entry.Step
        failed.Error = result.Error.Error()
        oc.rec.RollbackFail(failed)
        anyFailed = true

        oc.logger.WarnContext(ctx, "rollback step failed",
            "kind", entry.Step.Kind, "target", entry.Step.Target,
            "error", result.Error)
    }

    if anyFailed && oc.residualProbe != nil {
        arts, probeErr := oc.residualProbe(ctx)
        oc.rec.SetResidual(arts, probeErr)
    }
}
```

Note: the phase transition only happens after checking whether rollback
is needed. `RollbackOnFailure` is called via `defer`, so it must be
safe on the success path (where it sets the phase and returns
immediately).

### Recording semantics

Every rollback entry is attempted and recorded:

* **`RollbackComplete`**: the undo action succeeded.
* **`RollbackFail`**: the undo action was attempted and failed.

There are no unattempted entries. Residual probing surfaces what
physically remains after any rollback failures.

"Skip" is reserved for the primary timeline (where it means
"auto-skipped due to prior failure").

### Why not stop on first failure?

An earlier version of this design classified rollback entries as
"strict" (stop on failure) or "best-effort" (continue). That was
removed because:

* Rollback entries are typically independent: failing to unload a kernel
  pin does not prevent deleting the DB row.
* Stopping early leaves cleanup unattempted that would have succeeded.
* Severity classification for *reporting* (which failures matter) is a
  separate concern from *execution* (whether to continue). If needed,
  add severity classification to `PlanEntry` in Phase 2 for reporting
  purposes, without affecting the "try all" execution semantics.

### Forward plan execution (ExecutePlan)

Unload's forward path is not rollback -- it executes a pre-computed plan
as primary steps, in order, on the forward timeline. OpContext needs a
separate method for this:

```go
func (oc *OpContext) ExecutePlan(ctx context.Context, exec Executor, plan []PlanEntry) {
    for _, entry := range plan {
        oc.Step(entry.Step.Kind, entry.Step.Target, func() error {
            return exec.Execute(ctx, entry.Action)
        }, WithDetails(entry.Step.Details))
    }
}
```

This uses the existing `Step` method, so auto-skip on failure applies:
if any plan entry fails, the remaining entries are silently skipped
(not recorded). This is the correct forward-path semantics -- unlike
rollback, forward execution *should* stop on failure because later steps
may depend on earlier ones.

`PlanEntry` is the shared vocabulary between `computeUnloadPlan` (which
builds the plan from stored state) and `ExecutePlan` (which runs it).
In `manager`, the existing `unloadEntry` type becomes an alias or is
replaced by `operation.PlanEntry`.

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
        WithUndo(
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
* Rollback always tries all entries. Failing to unload a pin does not
  prevent deleting the DB row -- they are independent.
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

Unload already follows FETCH -> COMPUTE -> EXECUTE. OpContext absorbs the
boilerplate around recorder setup, fail wrapping, and best-effort steps.

Compute returns `[]PlanEntry` (replacing `[]unloadEntry`):

```go
plan := computeUnloadPlan(kernelID, programName, progPinPath, mapsDir, linksDir, links)
```

Execute uses `ExecutePlan` for the forward path (primary steps, in
order, stop on failure):

```go
oc.ExecutePlan(ctx, m.executor, plan)
```

Bytecode removal uses `TryStep` (best-effort, non-fatal):

```go
oc.TryStep(outcome.StepKindFSRemoveProgram, name, func() error {
    return m.executor.Execute(ctx, action.RemoveProgramDir{KernelID: kernelID})
})
```

This removes the remaining ad-hoc "call rt.RemoveProgram directly" path
and keeps all side effects within the Action language.

## Load and Unload: same language, different sources

Both load rollback and explicit unload speak the same language:

* **Unload** builds `[]PlanEntry` from stored state (`computeUnloadPlan`)
  and executes them as primary steps via `ExecutePlan`.
* **load rollback** accumulates `[]PlanEntry` progressively as forward
  steps succeed (via `WithUndo` and `OnUndo`) and executes them in
  reverse through the same executor on failure.

The shared unit is `PlanEntry{Action, Step}`. The difference is
direction: Unload runs in order (forward), load rollback runs in reverse.

## Cascade semantics

* **OpContext**: transaction
* **Step/Fetch**: operations in the transaction
* **WithUndo/OnUndo**: declarative cascades expressed as Action values
* **Auto-skip**: abort remaining work after first failure (forward path)
* **RollbackOnFailure**: execute cascades in reverse, always try all
* **ExecutePlan**: execute a pre-computed plan as primary steps (forward)
* **TryStep**: best-effort work on the forward path (non-fatal)
* **Residual probe**: observe remaining artefacts after rollback failure

Forward execution stops on failure (later steps may depend on earlier
ones). Rollback always tries all entries (they are typically independent).
Both record every attempted entry.

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
does -- `PlanEntry` stores an `Action`), the Action interface
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
                    RollbackOnFailure, ExecutePlan, Error,
                    SetDetails, Failed
    types.go        PlanEntry, StepOpt family, ResidualProbe

    Imports: manager/action (Action + executor interfaces)
             outcome (recorder, Step, StepKind, OperationOutcome)
             log/slog

    Does NOT import manager.


manager/
    manager.go      Manager struct, beginOp, GC
    executor.go     Concrete executor (type-switch over action.* types)
    load.go         load, Load (orchestration pipelines)
    unload.go       Unload, computeUnloadPlan
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

* `OpContext` and all supporting types (`PlanEntry`, `StepOpt`,
  `ResidualProbe`).
* The `Step`, `TryStep`, `Fetch`, `OnUndo`, `WithUndo`, `SetDetails`,
  `Failed`, `Error`, `RollbackOnFailure`, `ExecutePlan` methods.

**Stays in `manager`:**

* `Manager` struct and `beginOp` (creates `operation.OpContext`).
* Concrete executor (type-switch over `action.UnloadProgram`, etc.;
  calls `store`, `kernel`, and bytecode FS methods).
* `computeUnloadPlan` (pure function returning `[]operation.PlanEntry`).
* `undoStack` and `recordRollback` (kept until all operations are
  migrated, then deleted).
* All operation methods (the declarative pipelines).

### `PlanEntry` as shared vocabulary

`PlanEntry{Action, Step}` lives in `manager/operation` and serves both
directions:

* **Forward execution** (Unload): `computeUnloadPlan` builds
  `[]PlanEntry` from stored state; `ExecutePlan` runs them as primary
  steps in order.
* **Rollback** (load): `WithUndo` / `OnUndo` accumulate `[]PlanEntry`;
  `RollbackOnFailure` runs them in reverse.

The existing `unloadEntry` in `manager` becomes
`operation.PlanEntry` directly.

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
        operation.WithUndo(
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
* Rollback always tries all entries (failure does not stop)
* Rollback records Complete/Fail per entry
* RollbackOnFailure is no-op on success
* ExecutePlan runs entries in order, stops on failure
* SetDetails finds most recent matching (kind, target) entry
* Error() finalises exactly once
* Phase transitions prevent out-of-order recording
* Residual probe called only when rollback has failures

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
  executor interfaces, ExecutionResult. Add `RemoveProgramDir{KernelID}`.
* Thread bytecode FS handle into the concrete executor so it can
  interpret `RemoveProgramDir` via `rt.RemoveProgram(kernelID)`.
* Create `manager/operation`: implement OpContext with full unit test
  coverage (fake recorder, fake executor).
* Migrate operations incrementally: start with `Unload` (already
  FETCH -> COMPUTE -> EXECUTE and uses `ExecutePlan`), then `load`,
  then attach/detach.
* Preserve `*ManagerError` and current outcome semantics.
* Delete `undoStack` and `recordRollback` once all operations are
  migrated.

### Phase 2: optional API refinement

* Introduce `OpResult` to surface outcomes on success without
  `errors.As`.
* Add structured "non-fatal failure" recording for TryStep, so the
  outcome is self-contained (operators should not need logs to
  understand residuals).
* If one-at-a-time rollback execution proves too chatty, extend the
  executor with a batch-with-continue mode.
* If needed, add severity classification to `PlanEntry` for reporting
  purposes (without affecting the "try all" execution semantics).
