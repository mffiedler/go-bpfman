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
* the first failure (`op.err`)
* the rollback plan, expressed as `[]rollbackEntry` -- Action values
  paired with rollback step metadata and a per-entry severity
  classification (error or warning)
* phase transitions (primary -> rollback -> done)
* standard finalisation + `*ManagerError` wrapping

Callers declare steps. `Step` and `Fetch` return an opaque `StepHandle`
(issued by the recorder) that identifies the recorded timeline entry.
Details are attached by handle (`op.Details(h, ...)`), not by
`(kind, target)` search -- eliminating ambiguity when multiple steps
share the same kind and target. OpContext never indexes into the
timeline directly.

Steps may declare undo as Action values. On failure, remaining steps
auto-record Skip entries on the timeline (preserving the full pipeline
shape) and rollback runs all accumulated entries via the existing
executor, keeping one interpreter for all side effects. Severity
controls reporting, not execution: all rollback entries are always
attempted.

Two entrypoints wrap the lifecycle: `WithOp[T]` (returns a value) and
`WithOp0` (void). These closures manage begin, rollback-on-failure,
and finalisation so the operation code focuses on the pipeline.

### Transaction structure

```go
type OpContext struct {
    outcome outcome.OperationOutcome
    rec     outcome.ManagerOperationRecorder
    logger  *slog.Logger

    err   error
    phase opPhase

    rollback []rollbackEntry

    probe ResidualProbe
}

// StepHandle is an opaque reference to a recorded timeline entry.
// Issued by the recorder on CompleteStep, FailStep, and SkipStep.
// Externally it is only used with op.Details(h, ...) to attach
// details to the exact entry that was recorded. OpContext never
// indexes into outcome.Timeline directly.
type StepHandle struct {
    ix int
}

// RollbackSeverity classifies how a rollback entry failure is reported.
// All entries are always attempted regardless of prior failures.
type RollbackSeverity uint8

const (
    // SeverityError marks the entry as severe: failure is logged at
    // error level and ensures the residual probe runs. Use for kernel
    // operations where failure indicates real inconsistency.
    SeverityError RollbackSeverity = iota

    // SeverityWarning marks the entry as advisory: failure is logged
    // at warning level. Use for secondary cleanup (filesystem, caches)
    // where failure does not affect coherency.
    SeverityWarning
)

// rollbackEntry pairs an Action with rollback metadata and severity.
type rollbackEntry struct {
    action   action.Action
    step     outcome.Step
    severity RollbackSeverity
}

// PlanEntry pairs an Action with the outcome.Step metadata for recording.
// Used by forward plan execution (Unload's pre-computed plan).
type PlanEntry struct {
    Action action.Action
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

* `hLoad, loaded := Fetch(...)`
* `if op.Failed() { return ... }`
* `op.Details(hLoad, ...)`
* `op.OnUndo(action, rollbackStep)`

This keeps all state transitions inside OpContext, while still allowing
the caller to construct the undo Action from the fetched value.

**Pattern B: inline (Step + WithUndo)**

Some undo Actions depend only on values already in scope (e.g. kernel ID,
program name). These undos are co-located directly on the Step:

* `op.Step(..., WithUndo(action, rollbackStep))`

This is the most readable form and is the common case after the initial
Fetch.

#### Step options

```go
type stepCfg struct {
    details []any
    rb      *rollbackEntry
}

type StepOpt interface{ apply(*stepCfg) }

func WithDetails(d any) StepOpt { ... }

// WithUndo declares a rollback entry with error severity (Action + outcome.Step).
func WithUndo(a action.Action, rbStep outcome.Step) StepOpt {
    return stepOptFunc(func(cfg *stepCfg) {
        cfg.rb = &rollbackEntry{action: a, step: rbStep, severity: SeverityError}
    })
}

// WithBestEffortUndo declares a rollback entry with warning severity.
// Use for secondary cleanup where failure does not affect coherency.
func WithBestEffortUndo(a action.Action, rbStep outcome.Step) StepOpt {
    return stepOptFunc(func(cfg *stepCfg) {
        cfg.rb = &rollbackEntry{action: a, step: rbStep, severity: SeverityWarning}
    })
}
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

Returns a `StepHandle` identifying the recorded timeline entry. On
auto-skip (prior failure), records a `Skip` entry on the timeline so the
full pipeline shape is always visible.  Outside `phasePrimary` (i.e.
after rollback/done), returns an invalid handle (`ix: -1`) -- this
should never happen in well-structured code.

```go
func (op *OpContext) Step(kind outcome.StepKind, target string, fn func() error, opts ...StepOpt) StepHandle {
    if op.phase != phasePrimary {
        return StepHandle{ix: -1}
    }
    cfg := applyOpts(opts)

    if op.err != nil {
        return op.rec.SkipStep(kind, target, "prior failure")
    }

    if err := fn(); err != nil {
        op.err = err
        return op.rec.FailStep(kind, target, err, cfg.details...)
    }

    h := op.rec.CompleteStep(kind, target, cfg.details...)

    if cfg.rb != nil {
        op.rollback = append(op.rollback, *cfg.rb)
    }

    return h
}
```

Because every declared step records something (complete, fail, or skip),
callers can declare the entire pipeline unconditionally.  The only guard
still needed is after `Fetch`, when the returned value would be
dereferenced -- that is inherent to Go, not to OpContext.

#### TryStep (best-effort)

TryStep is for non-fatal steps (currently needed for bytecode removal in
Unload).  It always records a timeline entry: `CompleteStep` on success,
`WarnStep` on failure.  It never sets `op.err`, so the operation
continues regardless.  When `op.err` is already set, TryStep records a
skip (same as `Step`).

```go
func (op *OpContext) TryStep(kind outcome.StepKind, target string, fn func() error, opts ...StepOpt) StepHandle {
    if op.phase != phasePrimary {
        return StepHandle{ix: -1}
    }
    cfg := applyOpts(opts)

    if op.err != nil {
        return op.rec.SkipStep(kind, target, "prior failure")
    }

    if err := fn(); err != nil {
        return op.rec.WarnStep(kind, target, err, cfg.details...)
    }

    return op.rec.CompleteStep(kind, target, cfg.details...)
}
```

`WarnStep` records a timeline entry with a non-fatal error.  This is
distinct from `FailStep` (which means "the operation failed").  The
outcome is self-contained: operators can see what was attempted, what
succeeded, and what failed non-fatally, without consulting logs.

This requires one addition to the recorder: `WarnStep` alongside
`CompleteStep`, `FailStep`, and `SkipStep`.

#### Fetch (value-producing step)

Returns `(StepHandle, T)`. The handle enables `op.Details(h, ...)`
to attach details that depend on the returned value. The canonical
pattern is:

```go
hLoad, loaded := operation.Fetch(op, StepKindKernelLoad, name, func() (*KernelLoaded, error) { ... })
if op.Failed() { return ..., nil }
op.Details(hLoad, outcome.ProgramDetails{KernelID: loaded.Program.ID, PinPath: loaded.PinPath})
```

Implementation:

```go
func Fetch[T any](
    op *OpContext,
    kind outcome.StepKind,
    target string,
    fn func() (T, error),
    opts ...StepOpt,
) (StepHandle, T) {

    var zero T
    if op.phase != phasePrimary {
        return StepHandle{ix: -1}, zero
    }
    cfg := applyOpts(opts)

    if op.err != nil {
        return op.rec.SkipStep(kind, target, "prior failure"), zero
    }

    val, err := fn()
    if err != nil {
        op.err = err
        return op.rec.FailStep(kind, target, err, cfg.details...), zero
    }

    h := op.rec.CompleteStep(kind, target, cfg.details...)

    if cfg.rb != nil {
        op.rollback = append(op.rollback, *cfg.rb)
    }

    return h, val
}
```

#### OnUndo (late-bind undo registration)

Fetch frequently returns values needed to construct the undo action (pin
paths, IDs). Use `OnUndo` after Fetch returns. Default severity is error.

```go
func (op *OpContext) OnUndo(a action.Action, rbStep outcome.Step) {
    if op.phase != phasePrimary || op.err != nil {
        return
    }
    op.rollback = append(op.rollback, rollbackEntry{
        action:   a,
        step:     rbStep,
        severity: SeverityError,
    })
}

func (op *OpContext) OnBestEffortUndo(a action.Action, rbStep outcome.Step) {
    if op.phase != phasePrimary || op.err != nil {
        return
    }
    op.rollback = append(op.rollback, rollbackEntry{
        action:   a,
        step:     rbStep,
        severity: SeverityWarning,
    })
}
```

### Details: StepHandle eliminates ambiguity

There are two distinct "details" channels:

* **Primary-step details**: details recorded on the forward timeline
  (`CompleteStep` / `FailStep`).
* **Rollback-step details**: details recorded when rollback executes
  (`RollbackComplete` / `RollbackFail`).

The rollback step must carry its own details because rollback recording
happens later and must stand alone. This is why `WithUndo` pairs an
Action with an explicit rollback `outcome.Step`.

An earlier design used `SetDetails(kind, target, details)` to attach
primary-step details by searching for the most recent matching
`(kind, target)` entry. This was ambiguous when multiple steps share the
same kind and target (e.g. repeated `Preflight` validations).

StepHandle fixes this at the root: `Step` and `Fetch` return an opaque
handle (issued by the recorder) to the exact timeline entry they
recorded. `Details(handle, ...)` delegates to the recorder's
`SetDetails` -- no search, no ambiguity, no direct timeline access.

#### Recorder interface: handle-returning

The recorder API returns `StepHandle` from every recording method.
OpContext never indexes into `outcome.Timeline` directly -- the handle
is entirely owned by the recorder.

```go
type ManagerOperationRecorder interface {
    CompleteStep(kind StepKind, target string, details ...any) StepHandle
    FailStep(kind StepKind, target string, err error, details ...any) StepHandle
    SkipStep(kind StepKind, target, reason string) StepHandle
    WarnStep(kind StepKind, target string, err error, details ...any) StepHandle
    SetDetails(h StepHandle, details any)
    BeginRollback()
    RollbackComplete(step Step)
    RollbackFail(step Step)
    SetResidual(arts []Artefact, probeErr error)
    Finalise()
}
```

This is a deliberate inversion: the earlier design had OpContext reach
into the timeline by index (`len(op.outcome.Timeline) - 1`), coupling
it to recorder behaviour rather than recorder API.  Now the recorder
owns the mapping from handle to timeline entry, and OpContext delegates
via `rec.SetDetails(h, ...)`.

If an existing recorder implementation cannot return handles immediately,
a thin adapter that captures the index internally and returns it as a
`StepHandle` is sufficient.  The key constraint is: OpContext must not
know how indices work.

#### Details

```go
func (op *OpContext) Details(h StepHandle, details any) {
    if op.phase == phaseDone {
        op.logger.Error("opcontext: Details called after finalisation")
        return
    }
    if h.ix < 0 {
        return // skip/invalid handle -- silently ignored
    }
    op.rec.SetDetails(h, details)
}
```

`Details` is not gated on `op.err != nil` -- attaching details after a
failure is still useful (e.g. recording what was loaded before a later
step failed).  It is gated on phase: calls after `phaseDone` are
rejected because the outcome has been finalised and should not be
mutated.

#### When to use Details vs WithDetails

* **Fetch steps** always use `op.Details(h, ...)` after the Fetch
  returns, because the details depend on the returned value.
* **Void Steps** can use `WithDetails(...)` inline when the details are
  known at declaration time. They typically do not need `WithDetails` if
  the undo step already carries the relevant details for rollback
  recording.
* **Rollback steps** always carry their own details in the
  `outcome.Step` passed to `WithUndo`/`OnUndo`.

Primary-step details matter even when rollback never runs. For example,
`KernelLoad` details (kernel ID, pin path) should be visible on the
forward timeline on both success and failure. Use `op.Details(h, ...)`
for these.

## Execution: rollback and forward plans

### Rollback: always attempt all entries

Rollback always attempts every accumulated entry in reverse order.  It
never stops early.  Each entry carries a `RollbackSeverity` that
controls **reporting**, not control flow:

* **`SeverityError`**: failure is logged at error level.
* **`SeverityWarning`**: failure is logged at warning level.

Both severities record the failure on the rollback timeline.  If any
entry fails (at either severity level), the residual probe runs to
surface what physically remains.

The rationale for always continuing: stopping rollback early can
*increase* residuals.  A kernel undo failure does not mean DB/filesystem
cleanup should be skipped -- the coherency check (doctor/GC) can
discover and reconcile kernel-side orphans, but only if the rest of the
undo was at least attempted.  Deterministic "always try everything"
rollback is easier to reason about than conditional early-exit.

```go
func (op *OpContext) rollbackOnFailure(ctx context.Context, exec action.ExecutorWithResult) {
    if op.phase != phasePrimary {
        return
    }
    if op.err == nil || len(op.rollback) == 0 {
        return
    }

    op.phase = phaseRollback
    op.rec.BeginRollback()

    anyFailed := false

    for i := len(op.rollback) - 1; i >= 0; i-- {
        e := op.rollback[i]
        r := exec.ExecuteAllWithResult(ctx, []action.Action{e.action})

        if r.Error == nil {
            op.rec.RollbackComplete(e.step)
            continue
        }

        failed := e.step
        failed.Error = r.Error.Error()
        op.rec.RollbackFail(failed)
        anyFailed = true

        level := slog.LevelError
        if e.severity == SeverityWarning {
            level = slog.LevelWarn
        }
        op.logger.Log(ctx, level, "rollback step failed",
            "kind", e.step.Kind, "target", e.step.Target,
            "error", r.Error)
    }

    if anyFailed && op.probe != nil {
        arts, probeErr := op.probe(ctx)
        op.rec.SetResidual(arts, probeErr)
    }
}
```

Note: the phase transition only happens when rollback is actually
needed. On the success path, `rollbackOnFailure` returns immediately
without touching the phase.

### Recording semantics

Every rollback entry is always attempted and recorded:

* **`RollbackComplete`**: the undo action succeeded.
* **`RollbackFail`**: the undo action was attempted and failed.

Residual probing surfaces what physically remains after any rollback
failures.

"Skip" is reserved for the primary timeline (where it means
"auto-skipped due to prior failure").  "Warn" is reserved for the
primary timeline (where it means "attempted but failed non-fatally",
i.e. `TryStep`).

### Rollback severity classification

The default severity is error. Warning severity is the explicit opt-in
for entries where failure does not affect system coherency.

In `load` rollback:

* `UnloadProgram` (kernel pin removal) -- **error**. Failure to remove
  the kernel program is a real inconsistency that the residual probe
  should surface.
* `RemoveProgramDir` (filesystem cleanup) -- **warning**. The bytecode
  directory is a cache; failure is logged but does not affect kernel/DB
  coherency.
* `DeleteProgram` (DB removal) -- **error**. A failed DB delete is a
  real inconsistency.

All three are always attempted.  The severity classification controls
log level and guides operator triage, not execution order.  The
coherency engine (doctor/GC) provides the recovery path for any
residuals regardless of severity.

### Forward plan execution (ExecutePlan)

Unload's forward path is not rollback -- it executes a pre-computed plan
as primary steps, in order, on the forward timeline. OpContext needs a
separate method for this:

```go
func (op *OpContext) ExecutePlan(ctx context.Context, exec Executor, plan []PlanEntry) {
    for _, entry := range plan {
        op.Step(entry.Step.Kind, entry.Step.Target, func() error {
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

## Error returns and lifecycle wrappers

### Failed, Err, and finish

```go
func (op *OpContext) Failed() bool {
    return op.err != nil
}

func (op *OpContext) Err() error {
    return op.err
}

func (op *OpContext) finish() error {
    if op.phase == phaseDone {
        return nil
    }
    op.phase = phaseDone

    if op.err != nil {
        op.outcome.PrimaryError = op.err.Error()
    }
    op.rec.Finalise()

    if op.err == nil {
        return nil
    }
    return &ManagerError{Outcome: op.outcome, Cause: op.err}
}
```

### WithOp and WithOp0

Two entrypoints wrap the full lifecycle: create the transaction, run the
closure, roll back on failure, and finalise. `WithOp` is generic (Go
methods cannot have independent type parameters); `WithOp0` is for
operations that return only an error.

```go
func WithOp0(
    ctx context.Context,
    begin func(context.Context) *OpContext,
    exec action.ExecutorWithResult,
    fn func(*OpContext) error,
) error {
    op := begin(ctx)

    if err := fn(op); err != nil && op.err == nil {
        op.err = err
    }

    op.rollbackOnFailure(ctx, exec)
    return op.finish()
}

func WithOp[T any](
    ctx context.Context,
    begin func(context.Context) *OpContext,
    exec action.ExecutorWithResult,
    fn func(*OpContext) (T, error),
) (T, error) {
    op := begin(ctx)

    val, err := fn(op)
    if err != nil && op.err == nil {
        op.err = err
    }

    op.rollbackOnFailure(ctx, exec)
    finErr := op.finish()

    var zero T
    if finErr != nil {
        return zero, finErr
    }
    return val, nil
}
```

The `begin` parameter is typically `m.beginOp`, so `Manager` owns how
the recorder, logger, and residual probe are wired.

Inside the closure, callers return `op.Err()` after `op.Failed()` checks
-- the error propagation is explicit.  `WithOp` also checks `op.err`
independently (in case the closure returns a different error), but the
primary pattern is: `op.Err()` makes early returns unsurprising.

### Convenience wrapper on Manager

Passing `begin` and `exec` at every call site is repetitive.  Manager
methods should use a convenience wrapper that supplies both implicitly:

```go
func withOp[T any](m *Manager, ctx context.Context, fn func(*operation.OpContext) (T, error)) (T, error) {
    return operation.WithOp(ctx, m.beginOp, m.executor, fn)
}

func withOp0(m *Manager, ctx context.Context, fn func(*operation.OpContext) error) error {
    return operation.WithOp0(ctx, m.beginOp, m.executor, fn)
}
```

These are package-level functions (not methods) because Go methods
cannot have independent type parameters.

### Non-closure usage

For operations that do not fit the closure pattern (e.g. dispatcher
attach with stale-dispatcher retry), the lifecycle methods are available
directly:

```go
op := m.beginOp(ctx)
// ... steps with retry logic ...
op.rollbackOnFailure(ctx, m.executor)
return result, op.finish()
```

### Phase 2

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
publish -> store save. `WithOp` manages the lifecycle; `StepHandle`
makes details precise and local. Both undo patterns appear:

* **Pattern A** (late-bind): kernel unload depends on returned pin paths.
* **Pattern B** (inline): filesystem removal and store deletion depend on
  kernel ID already in scope.

```go
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec, opts loadOpts) (bpfman.Program, error) {
    return operation.WithOp(ctx, m.beginOp, m.executor, func(op *operation.OpContext) (bpfman.Program, error) {
        name := spec.ProgramName()
        rt := m.fsctx.BytecodeFS()
        now := time.Now()

        // Preflight validation.
        op.Step(outcome.StepKindPreflight, "validation", func() error {
            if spec.HasImageSource() && spec.ObjectPath() == "" {
                return fmt.Errorf("load requires objectPath to be set; image pulling is handled by Load")
            }
            return nil
        })

        // Phase 1: load into kernel and pin to bpffs.
        hLoad, loaded := operation.Fetch(op, outcome.StepKindKernelLoad, name, func() (*platform.KernelLoaded, error) {
            return m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
        })
        if op.Failed() {
            return bpfman.Program{}, op.Err()
        }

        // Forward-path details: attached by handle, no search.
        op.Details(hLoad, outcome.ProgramDetails{
            KernelID: loaded.Program.ID,
            PinPath:  loaded.PinPath,
        })

        // Pattern A: late-bind undo (depends on returned pin paths).
        op.OnUndo(
            action.UnloadProgram{PinPath: loaded.PinPath},
            outcome.NewStep(outcome.StepKindKernelUnload, name, outcome.ProgramDetails{
                KernelID: loaded.Program.ID,
                PinPath:  loaded.PinPath,
            }),
        )
        op.OnUndo(
            action.UnloadProgram{PinPath: loaded.MapsDir},
            outcome.NewStep(outcome.StepKindKernelUnload, name, outcome.ProgramDetails{
                KernelID:    loaded.Program.ID,
                MapsDirPath: loaded.MapsDir,
            }),
        )

        // DB existence check.
        op.Step(outcome.StepKindPreflight, name, func() error {
            if _, err := m.store.Get(ctx, loaded.Program.ID); err == nil {
                return fmt.Errorf("program %d already exists in database", loaded.Program.ID)
            } else if !errors.Is(err, store.ErrNotFound) {
                return fmt.Errorf("check existing program %d: %w", loaded.Program.ID, err)
            }
            return nil
        })

        // Publish bytecode.
        prov := buildProvenance(spec, loaded, now)
        record := buildProgramRecord(spec, loaded, opts, rt, now)

        op.Step(outcome.StepKindFSPublish, name, func() error {
            return rt.PublishBytecode(loaded.Program.ID, spec.ObjectPath(), prov)
        },
            // Pattern B: inline undo, best-effort (filesystem is a cache).
            operation.WithBestEffortUndo(
                action.RemoveProgramDir{KernelID: loaded.Program.ID},
                outcome.NewStep(outcome.StepKindFSRemoveProgram, name, outcome.ProgramDetails{
                    KernelID: loaded.Program.ID,
                }),
            ),
        )

        // Persist metadata (transaction).
        op.Step(outcome.StepKindStoreSaveProgram, name, func() error {
            return m.store.RunInTransaction(ctx, func(tx platform.Store) error {
                return tx.Save(ctx, loaded.Program.ID, record)
            })
        },
            operation.WithUndo(
                action.DeleteProgram{KernelID: loaded.Program.ID},
                outcome.NewStep(outcome.StepKindStoreDeleteProgram, name, outcome.ProgramDetails{
                    KernelID: loaded.Program.ID,
                }),
            ),
        )

        if op.Failed() {
            return bpfman.Program{}, op.Err()
        }
        return constructProgram(loaded, record), nil
    })
}
```

Key properties:

* `WithOp` manages the full lifecycle: begin, rollback-on-failure,
  finalise. Early returns use `op.Err()` so the error propagation is
  explicit and unsurprising -- no "nil means fail" trick.
* `StepHandle` makes details precise: `op.Details(hLoad, ...)` mutates
  the exact timeline entry for `KernelLoad`. No `(kind, target)` search,
  no ambiguity with repeated `Preflight` steps.
* `KernelLoad` demonstrates the only "ceremony" needed: one `Failed()`
  guard after Fetch, one `Details` call, two late-bind undos.
* All rollback effects are Actions (`UnloadProgram`, `RemoveProgramDir`,
  `DeleteProgram`). No closure-based undo, no rollback special-cases.
* Rollback severity matches the semantics: kernel undos are error-level
  (default), `RemoveProgramDir` is warning-level (filesystem cache).
  All entries are always attempted regardless of prior failures.
* The rollback plan is entirely Action-based. `RemoveProgramDir` exists
  specifically to avoid a filesystem special-case and to keep Unload and
  load rollback speaking the same language.
* Store save declares undo even though it is currently the final step.
  This preserves the invariant: every side-effecting step declares its
  undo, so future steps can be appended safely.
* Provenance and record construction are pulled into pure helpers,
  keeping the pipeline free of incidental noise.

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
op.ExecutePlan(ctx, m.executor, plan)
```

Bytecode removal uses `TryStep` (best-effort, non-fatal):

```go
op.TryStep(outcome.StepKindFSRemoveProgram, name, func() error {
    return m.executor.Execute(ctx, action.RemoveProgramDir{KernelID: kernelID})
})
```

This removes the remaining ad-hoc "call rt.RemoveProgram directly" path
and keeps all side effects within the Action language.

## Load and Unload: same language, different sources

Both load rollback and explicit unload speak the same language:

* **Unload** builds `[]PlanEntry` from stored state (`computeUnloadPlan`)
  and executes them as primary steps via `ExecutePlan`.
* **load rollback** accumulates `[]rollbackEntry` progressively as
  forward steps succeed (via `WithUndo`/`WithBestEffortUndo` and
  `OnUndo`/`OnBestEffortUndo`) and executes them in reverse through
  the same executor on failure.

The shared Action vocabulary (`UnloadProgram`, `RemoveProgramDir`,
`DeleteProgram`, etc.) is the same in both directions. The difference
is:

* direction: Unload runs in order (forward), load rollback runs in
  reverse
* severity: rollback entries carry `RollbackSeverity` (error/warning)
  for reporting; forward plan entries stop on any failure

## Cascade semantics

* **OpContext**: transaction
* **Step/Fetch**: operations returning StepHandle for details; auto-record
  Skip on prior failure so the full pipeline shape is always visible
* **TryStep**: best-effort work on the forward path; records WarnStep on
  non-fatal failure (never sets `op.err`)
* **Details(h, ...)**: attach details to exact timeline entry by handle
  (delegated to recorder via `SetDetails`)
* **WithUndo/OnUndo**: error-severity rollback entries as Action values
* **WithBestEffortUndo/OnBestEffortUndo**: warning-severity rollback entries
* **Auto-skip**: remaining steps record Skip after first failure (forward path)
* **rollbackOnFailure**: always attempt all entries in reverse; severity
  controls reporting, not execution
* **WithOp/WithOp0**: lifecycle wrappers (begin, run, rollback, finalise)
* **ExecutePlan**: execute a pre-computed plan as primary steps (forward)
* **Err()**: expose the first failure for explicit early returns
* **Residual probe**: observe remaining artefacts after any rollback failure

Forward execution stops on failure (later steps may depend on earlier
ones) but records a Skip entry for each skipped step.  Rollback always
attempts every entry; severity classification controls log level and
guides operator triage.

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
   severity classification, execute via executor, record results.

3. **Operation-specific planning** -- `computeUnloadPlan`, dispatcher
   retry logic, map sharing rules.

OpContext absorbs (1) and (2). (3) remains domain logic in `manager`.

### The import cycle constraint

Go's unexported interface methods can only be satisfied by types in the
same package. The `Action` interface uses an unexported `isAction()`
marker, so concrete Action types must live in the same package as the
interface.

If OpContext lives in a subpackage and needs to reference `Action` (it
does -- both `PlanEntry` and `rollbackEntry` store an `Action`), the
Action interface must live in a package that both `manager` and the
subpackage can import. This rules out keeping the Action interface in
`manager` itself.

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
                    OnBestEffortUndo, Details, Failed,
                    rollbackOnFailure, finish,
                    ExecutePlan, WithOp, WithOp0
    types.go        StepHandle, PlanEntry, rollbackEntry,
                    RollbackSeverity, StepOpt family,
                    ResidualProbe

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

* `OpContext` and all supporting types (`StepHandle`, `PlanEntry`,
  `rollbackEntry`, `RollbackSeverity`, `StepOpt`, `ResidualProbe`).
* The `Step`, `TryStep`, `Fetch`, `OnUndo`, `OnBestEffortUndo`,
  `WithUndo`, `WithBestEffortUndo`, `Details`, `Failed`, `finish`,
  `rollbackOnFailure`, `ExecutePlan`, `WithOp`, `WithOp0`.

**Stays in `manager`:**

* `Manager` struct and `beginOp` (creates `operation.OpContext`).
* Concrete executor (type-switch over `action.UnloadProgram`, etc.;
  calls `store`, `kernel`, and bytecode FS methods).
* `computeUnloadPlan` (pure function returning `[]operation.PlanEntry`).
* `undoStack` and `recordRollback` (kept until all operations are
  migrated, then deleted).
* All operation methods (the declarative pipelines).

### `PlanEntry` and `rollbackEntry`

Two entry types serve the two directions:

* **`PlanEntry{Action, Step}`**: for forward execution (Unload).
  `computeUnloadPlan` builds `[]PlanEntry` from stored state;
  `ExecutePlan` runs them as primary steps in order.
* **`rollbackEntry{action, step, severity}`**: for rollback (load).
  `WithUndo`/`OnUndo`/`WithBestEffortUndo`/`OnBestEffortUndo` accumulate
  `[]rollbackEntry`; `rollbackOnFailure` runs them all in reverse,
  using severity for reporting.

The existing `unloadEntry` in `manager` becomes
`operation.PlanEntry` directly. `rollbackEntry` is unexported within
`manager/operation`.

### What `Manager.load` looks like after the split

The visible changes are: `operation.WithOp` wraps the lifecycle,
`operation.Fetch` returns `(StepHandle, T)`, `op.Details(h, ...)` uses
the handle, and Action types gain the `action.` prefix.

```go
func (m *Manager) load(ctx context.Context, spec bpfman.LoadSpec, opts loadOpts) (bpfman.Program, error) {
    return operation.WithOp(ctx, m.beginOp, m.executor, func(op *operation.OpContext) (bpfman.Program, error) {
        name := spec.ProgramName()
        rt := m.fsctx.BytecodeFS()
        now := time.Now()

        op.Step(outcome.StepKindPreflight, "validation", func() error { ... })

        hLoad, loaded := operation.Fetch(op, outcome.StepKindKernelLoad, name, func() (*platform.KernelLoaded, error) {
            return m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
        })
        if op.Failed() {
            return bpfman.Program{}, op.Err()
        }

        op.Details(hLoad, outcome.ProgramDetails{...})

        op.OnUndo(
            action.UnloadProgram{PinPath: loaded.PinPath},
            outcome.NewStep(outcome.StepKindKernelUnload, name, outcome.ProgramDetails{...}),
        )

        op.Step(outcome.StepKindFSPublish, name, func() error {
            return rt.PublishBytecode(loaded.Program.ID, spec.ObjectPath(), prov)
        },
            operation.WithBestEffortUndo(
                action.RemoveProgramDir{KernelID: loaded.Program.ID},
                outcome.NewStep(outcome.StepKindFSRemoveProgram, name, outcome.ProgramDetails{...}),
            ),
        )

        op.Step(outcome.StepKindStoreSaveProgram, name, func() error {
            return m.store.RunInTransaction(ctx, ...)
        },
            operation.WithUndo(
                action.DeleteProgram{KernelID: loaded.Program.ID},
                outcome.NewStep(outcome.StepKindStoreDeleteProgram, name, outcome.ProgramDetails{...}),
            ),
        )

        if op.Failed() {
            return bpfman.Program{}, op.Err()
        }
        return constructProgram(loaded, record), nil
    })
}
```

The method reads as a specification. The transaction engine, recording
state machine, and rollback severity are all behind the `operation`
package boundary.

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

* Step success/failure records Complete/Fail via recorder
* Step after prior failure records Skip (auto-skip)
* Fetch success/failure/zero return, returns (StepHandle, T)
* Fetch after prior failure records Skip and returns zero
* TryStep success records CompleteStep
* TryStep failure records WarnStep (non-fatal, op continues)
* TryStep after prior failure records Skip
* Details(h, ...) delegates to recorder via SetDetails
* Details with invalid handle (ix < 0) is a silent no-op
* Details after phaseDone is rejected with error log
* OnUndo/WithUndo registration only on success
* OnBestEffortUndo/WithBestEffortUndo set correct severity
* Rollback reversal order
* All rollback entries always attempted (no early stop)
* Error-severity failure logged at error level
* Warning-severity failure logged at warning level
* Rollback records Complete/Fail per attempted entry
* rollbackOnFailure is no-op on success
* Residual probe called when any rollback entry fails
* WithOp/WithOp0 manage full lifecycle (begin, rollback, finalise)
* WithOp returns zero value on failure, value on success
* Closure returning op.Err() propagates correctly through WithOp
* ExecutePlan runs entries in order, stops on failure, skips rest
* finish() finalises exactly once
* Phase transitions prevent out-of-order recording

This locks down semantics before any migration.

## Limitations

* **Go lacks monadic composition**: Fetch still needs a guard after it
  returns if subsequent code would dereference the returned value. This
  is one guard per Fetch, not per Step.  Non-value steps (`Step`,
  `TryStep`) need no guard at all: they auto-record Skip on prior
  failure and the pipeline continues.

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
* Create `manager/operation`: implement OpContext with StepHandle,
  rollback severity, WithOp/WithOp0, and full unit test coverage
  (fake recorder, fake executor).
* Extend recorder API to return `StepHandle` from recording methods
  and accept `StepHandle` in `SetDetails`. Add `SkipStep` and
  `WarnStep` methods.
* Migrate operations incrementally: start with `Unload` (already
  FETCH -> COMPUTE -> EXECUTE and uses `ExecutePlan`), then `load`,
  then attach/detach.
* Preserve `*ManagerError` and current outcome semantics.
* Delete `undoStack` and `recordRollback` once all operations are
  migrated.

### Phase 2: optional API refinement

* Introduce `OpResult` to surface outcomes on success without
  `errors.As`.
* If one-at-a-time rollback execution proves too chatty, extend the
  executor with a batch-with-continue mode.
* If error/warning severity proves too coarse, add a third level
  without changing the handle/details design.

## Addendum: OpContext as transaction manager vs plan interpreter

### What OpContext is

OpContext is a transaction manager with declarative undo.  It removes
recording, rollback, finalisation, and phase-transition boilerplate.
But the forward path is still imperative Go with closures:

```go
op.Step(outcome.StepKindPreflight, "validation", func() error {
    // arbitrary Go code
})

hLoad, loaded := operation.Fetch(op, outcome.StepKindKernelLoad, name, func() (*KernelLoaded, error) {
    // arbitrary Go code
})
if op.Failed() { return ..., op.Err() }

op.Details(hLoad, ...)
op.OnUndo(...)

op.Step(outcome.StepKindFSPublish, name, func() error {
    // arbitrary Go code that captures `loaded`
})
```

The closures are opaque to OpContext.  It cannot inspect them, reorder
them, serialise them, or reason about their dependencies.  The
"program" only exists at runtime as a chain of function calls.

### Unload already has the right shape

Unload is the exception: `computeUnloadPlan` builds a `[]PlanEntry`
(data) and `ExecutePlan` interprets it.  That is a mini-VM: the plan
is a value, the executor is an interpreter, and business logic is the
pure function that produces the plan.

### Load does not

Load cannot be expressed as a plan today because of value dependencies
between steps:

1. `KernelLoad` produces `loaded` (pin paths, kernel ID).
2. Undo entries, details, publish, and store save all depend on
   `loaded`.
3. The DB check depends on `loaded.Program.ID`.

OpContext hides the transaction mechanics but does not change this
shape.  The forward path remains an imperative pipeline with mid-stream
bindings.

### What a plan interpreter would need

To make Load (and attach/detach) look like Unload, you would need a
binding mechanism: steps produce named values, later steps and undo
declarations reference those names, and the interpreter resolves
bindings at execution time.

Sketch of what Load would look like:

```go
func (m *Manager) loadPlan(spec bpfman.LoadSpec, opts loadOpts) operation.Plan {
    name := spec.ProgramName()

    return operation.Build(
        operation.Validate("preflight", func() error {
            if spec.HasImageSource() && spec.ObjectPath() == "" {
                return fmt.Errorf("...")
            }
            return nil
        }),

        operation.Produce("kernel-load", outcome.StepKindKernelLoad, name,
            func(ctx context.Context) (*KernelLoaded, error) {
                return m.kernel.Load(ctx, spec, m.fsctx.BPFFS())
            },
            operation.UndoFrom(func(loaded *KernelLoaded) []operation.UndoEntry {
                return []operation.UndoEntry{
                    {Action: action.UnloadProgram{PinPath: loaded.PinPath}, ...},
                    {Action: action.UnloadProgram{PinPath: loaded.MapsDir}, ...},
                }
            }),
        ),

        operation.Do("fs-publish", outcome.StepKindFSPublish, name,
            operation.DependsOn("kernel-load"),
            func(ctx context.Context, loaded *KernelLoaded) error {
                prov := buildProvenance(spec, loaded, time.Now())
                return m.fsctx.BytecodeFS().PublishBytecode(loaded.Program.ID, ...)
            },
            operation.BestEffortUndo(func(loaded *KernelLoaded) operation.UndoEntry {
                return operation.UndoEntry{
                    Action: action.RemoveProgramDir{KernelID: loaded.Program.ID}, ...}
            }),
        ),

        // ...
    )
}
```

Execution becomes:

```go
result, err := m.executor.RunPlan(ctx, m.loadPlan(spec, opts))
```

The plan is a value.  The interpreter owns step execution, binding
resolution, auto-skip, recording, rollback, and finalisation.  Business
logic is the pure function that produces the plan.

### Cost

This requires:

* A `Plan` type (ordered list of plan nodes).
* A binding/dependency mechanism (typed keys or generics).
* A plan interpreter that resolves bindings and drives execution.
* Type safety for produced values (Go generics make this awkward but
  possible).

### What a plan-as-value gives you

Two things that imperative-with-OpContext does not:

1. **Inspectability.**  You can log, diff, or dry-run a plan before
   executing it.  Useful for debugging and for `doctor`-style tooling.
2. **Uniform shape.**  Load, Unload, attach, and detach all become
   "compute plan, execute plan".  One interpreter, one test strategy,
   one mental model.

### Recommendation

OpContext as designed is the right first step.  It extracts the
transaction mechanics and gets operations to clean imperative pipelines.
If, after migrating all five operations, the remaining closure
boilerplate still feels like the wrong shape, the path from OpContext to
a plan interpreter is incremental -- `ExecutePlan` is already the seed.
Building the full binding/dependency mechanism now, before validating
OpContext itself, is premature.
