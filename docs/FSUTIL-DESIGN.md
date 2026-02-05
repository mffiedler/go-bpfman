# Filesystem Layout Ownership

This document describes a design for centralising filesystem layout and
operations into a single `fs` package that exposes operations rather
than paths, using compiler-enforced capability tokens to make misuse a
compile error. Filesystem effects are reified as action data, interpreted
by `fs`, and executed through a runner that unifies outcome recording,
compensation, and rollback.

## Problem

Filesystem operations are scattered across the codebase with
inconsistent error handling, ad-hoc rollback, and duplicated cleanup
logic. A survey of the codebase found:

- **20+ inline ENOENT guards**: every `os.Remove` and `os.RemoveAll`
  call site reimplements `if !os.IsNotExist(err)`. Some forget
  entirely (`coherency.go:1153-1157` has bare
  `os.Remove`/`os.RemoveAll` with no error handling).
- **3 different rollback strategies**: a LIFO slice in `load.go`, a
  success-flag defer in `attach_xdp.go`, and cascading inline cleanup
  in dispatcher attachment. Each is ad-hoc and subtly incomplete.
- **No path validation**: `os.RemoveAll` is called on paths
  constructed from user-influenced data with no guard that the target
  is under the expected parent directory.
- **No atomic-write primitive**: the OCI puller has a one-off
  temp-dir-then-rename pattern; nothing else does. Bytecode
  persistence needs the same pattern.
- **Raw `filepath.Join` in non-layout code**: pin paths, maps
  directories, and link directories are assembled from string
  components with no typed guarantee that the result is under the
  correct root.
- **Inconsistent effect patterns**: `Unload` uses reified actions
  (`computeUnloadActions` + executor) but `Load` does all I/O inline
  with closure-based rollback and manual outcome recording. These
  should follow the same pattern.

The codebase is already partway there. `bpffs` provides typed wrappers
at I/O boundaries and mounting; `config.RuntimeDirs` centralises path
derivation; `action/` defines reified effects for kernel and store
operations. The gap is that filesystem effects are not reified, the
`fs` layer leaks raw strings everywhere, and `Load` does not follow the
fetch/compute/execute pattern that the rest of the codebase uses.

## Design Principles

### Follow the `lock/` Package Pattern

The `lock/` package makes it impossible to construct a `WriterScope`
without going through `lock.Run()`. It achieves this with an exported
interface containing an unexported marker method:

```go
type WriterScope interface {
    DupFD() (*os.File, error)
    FD() int
    writerScopeMarker() // unexported: external packages cannot implement
}
```

We apply the same technique to filesystem ownership. Root types act as
capability tokens: you cannot construct one without providing a
validated base path, and all operations are methods on the root that
enforce layout invariants internally. There is no public constructor
for the underlying concrete types.

### Fetch/Compute/Execute Uniformity

The codebase already follows fetch/compute/execute (from the "Red
Book" -- Functional Programming in Scala):

- **Pure packages** (`kernel/`, `action/`, `compute/`) perform no I/O.
- **Interpreter layer** (`interpreter/`) provides I/O abstractions.
- **Manager** orchestrates: fetch state (impure), compute actions
  (pure), execute effects (impure).

This design extends the pattern to filesystem operations: `fs` becomes
an interpreter for filesystem action types, just as the kernel adapter
interprets kernel actions and the store interprets store actions.

### `bpffs/` Is Mounting Only

The existing `bpffs` package remains the small, easy-to-test home for:

- mount detection (`IsMounted`) via `/proc/self/mountinfo`
- mount and unmount (`Mount`, `Unmount`)
- `EnsureMounted`

It does not own bpfman's directory conventions under the mount. All
bpfman-specific bpffs layout and operations move to the new `fs`
package.

This keeps the name honest: `bpffs` is about the bpffs filesystem,
not bpfman's conventions inside it.

### One Canonical Layout Package: `fs/`

Introduce a new top-level package `fs` as the single canonical place
for all path construction and intent-based filesystem operations
rooted at a single configured base directory.

Within `fs`, we model two conceptual domains:

- **Runtime**: regular filesystem state under `<base>/...`
- **BPFFS**: bpffs pins under `<base>/fs`

Both domains share the same base root and are constructed from the
same validated `fs.Root`. The manager orchestrates behaviour but never
constructs paths.

`<base>/fs` is the only supported bpffs mountpoint. The base root
determines all runtime and bpffs paths.

## DB Is Commit, FS Is Artefacts

### Core Invariant

SQLite is the sole source of truth for managed state.

The filesystem holds only artefacts and crash-residue markers. Any
artefact not referenced by a DB row is uncommitted residue and is safe
to delete.

The filesystem must not become a second database. No metadata store
beyond what is required to write artefact bytes. Anything queryable
(program name, source, timestamps, labels) stays in SQLite.

The one exception is `provenance.json`, written alongside `bytecode.o`
in the published bundle. It exists purely as a diagnostic trace for
orphan identification and debugging. It is never read by bpfman's
operational code path. Only `gc`, `doctor`, and human inspection read
it. The DB remains the sole authoritative record for all program
metadata.

### Naming

The public, stable final path for bytecode is:

```
<base>/programs/{kernel_id}/bytecode.o
```

This matches the layout in
[BYTECODE-PERSISTENCE.md](BYTECODE-PERSISTENCE.md). The two-phase
publish mechanism (staging directory + atomic rename) is an
implementation detail inside `fs`. Staging directories live under
`<base>/.staging/` and are opaque to callers.

### Ordering: Two-Stage Fetch/Compute/Execute

The current `Manager.Load` performs all I/O inline: kernel load,
bytecode copy, DB persist, rollback via closure-based `undoStack`,
outcome recording via manual `rec.Complete()`/`rec.Fail()` calls.
This is the only mutating operation that does not use the
`action`/executor pattern (contrast with `Unload`, which uses
`computeUnloadActions` + executor).

The revised ordering follows two-stage fetch/compute/execute:

**Stage 1 -- Kernel Load (imperative)**

The kernel load is a direct call because it yields data (kernel ID,
map IDs, pin paths) needed by the compute phase:

```go
loaded, err := m.kernel.Load(ctx, spec, bpffsRoot)
```

If this fails, there is nothing to clean up.

**Stage 2 -- Post-Load (fetch/compute/execute)**

1. **Fetch facts**: query the store for `kernel_id` existence.
2. **Compute plan**: a pure function `computePostLoadPlan` returns
   `[]action.Action` -- no I/O. Returns an error if an invariant is
   violated (e.g. kernel ID already in DB).
3. **Execute plan**: the runner processes the action sequence through
   the executor, records outcomes, collects compensations. On failure,
   the runner automatically reverses the compensations (including the
   pre-seeded kernel rollback from stage 1).

The concrete action sequence from `computePostLoadPlan`:

1. `action.PublishBytecode{KernelID, SrcPath, Prov}` -- crash-residue
   wipe + staging + atomic rename + provenance.
2. `action.SaveProgram{KernelID, Metadata}` -- DB transaction commit.

If `SaveProgram` fails, the runner executes compensations in reverse:
`RemoveProgramDir(id)`, then `UnloadProgram(pinPath)`.

**Existence checks are facts, not guards**

The DB existence check ("does a committed program own this kernel
ID?") is a fact gathered in the fetch phase. The pure compute function
uses this fact to decide whether to produce a plan or an error. No
inline `if err != nil { rollbackKernel(); return }` -- the compute
function returns an error and the runner never executes anything.

The `PublishBytecode` interpreter handles crash-residue wipe
internally (removing an uncommitted directory for the same kernel ID),
but the decision to proceed is made in the compute phase based on
fetched facts.

### Crash Recovery Rule

At startup (or before any operation that might reuse a `kernel_id`):

- If `<base>/programs/{kernel_id}` exists but DB has no row: delete
  it (uncommitted residue).

`bpfman gc` / `bpfman doctor` can scan `<base>/programs/` and remove
any directory not backed by a DB record.

### Consequences

- Filesystem state does not imply "managed". Only the DB does.
- A crash between bytecode publish and DB commit leaves published
  artefacts behind. Safe to clean because DB has no record.
- A crash after DB commit is fine: DB is committed, FS exists.
- The alternative (DB commit then filesystem publish) would require
  compensating DB deletes on filesystem failure or startup repair to
  finish incomplete publications -- both add complexity and fragility.

## Filesystem Effects as Actions

The `action/` package already defines reified effects for kernel
operations (`LoadProgram`, `UnloadProgram`, `DetachLink`), store
operations (`SaveProgram`, `DeleteProgram`, `SaveLink`), and bpffs
operations (`RemovePin`). Filesystem operations join the same
taxonomy:

```go
// action/fs.go

// PublishBytecode copies bytecode to the runtime directory via
// staging and atomic rename. If a directory for the given ID
// already exists (crash residue), it is removed first.
type PublishBytecode struct {
    KernelID uint32
    SrcPath  string
    Prov     Provenance
}

func (PublishBytecode) isAction() {}

// RemoveProgramDir removes <base>/programs/{id}/ and its contents.
// Tolerates non-existence. Used as compensation for PublishBytecode
// and in the unload path.
type RemoveProgramDir struct {
    KernelID uint32
}

func (RemoveProgramDir) isAction() {}
```

The smell test remains: if the manager does `filepath.Join`, something
is wrong. But the mechanism changes from method calls to action data.
The manager produces `action.PublishBytecode{...}` in the compute
phase; the executor interprets it by calling `fs.Runtime()` methods.

### Provenance

`Provenance` lives in `action/` because it is pure data referenced by
`PublishBytecode`. It cannot live in `fs/` because `action/` must not
import `fs/` (the dependency flows the other way: `interpreter/`
imports both `action/` and `fs/`).

```go
// action/provenance.go

type Provenance struct {
    Version     int       `json:"version"`
    KernelID    uint32    `json:"kernel_id"`
    ProgramName string    `json:"program_name"`
    Source      string    `json:"source"`
    SourceKind  string    `json:"source_kind"`
    LoadedAt    time.Time `json:"loaded_at"`
}
```

| Field | Type | Description |
|-------|------|-------------|
| `version` | int | Schema version, start at 1 |
| `kernel_id` | uint32 | Kernel program ID at load time |
| `program_name` | string | BPF program name |
| `source` | string | Original path or image URL |
| `source_kind` | string | `"file"`, `"image"`, or `"unknown"` |
| `loaded_at` | string | RFC 3339 UTC timestamp |

Schema follows [BYTECODE-PERSISTENCE.md](BYTECODE-PERSISTENCE.md).
The file is written once at load time and never updated. It is never
read by bpfman's operational code path. The version field allows
adding fields later without breaking existing readers.

### Compensation Table

Each action type has a known compensation (or none):

| Action | Compensation | Notes |
|--------|-------------|-------|
| `PublishBytecode` | `RemoveProgramDir` | Same kernel ID |
| `SaveProgram` | `DeleteProgram` | Same kernel ID |
| `RemoveProgramDir` | none | Crash-residue wipe or teardown |
| `UnloadProgram` | none | Terminal teardown |
| `DeleteProgram` | none | Terminal teardown |

The compensation mapping is a pure function. Adding a new action type
without defining its compensation is a compile-time error (exhaustive
switch, or the `default` case panics in tests).

## Runner: Unified Execution with Compensation

### What the Runner Replaces

The codebase has three overlapping mechanisms for managing multi-step
operations:

1. **`undoStack`** (`manager/undo.go`): a LIFO slice of `func() error`
   closures executed in reverse on failure. Closures are opaque -- they
   cannot be inspected, logged before execution, or tested
   independently.

2. **`outcome.ManagerOperationRecorder`**: a state machine that records
   timeline entries. The manager must call `rec.Complete()`,
   `rec.Fail()`, `rec.BeginRollback()`, etc. manually at every step
   boundary. This is error-prone and produces boilerplate.

3. **`ActionExecutorWithResult`**: the executor runs a sequence of
   actions and reports what completed and where it failed. Used by
   `Unload` but not by `Load`.

These three concerns -- compensation, observation, and execution --
are currently wired together by hand in each operation. The runner
subsumes them into a single mechanism.

### How It Works

The runner takes:

- An executor (the existing `ActionExecutor`).
- An outcome recorder.
- An optional pre-seeded compensation stack (for effects from before
  the runner starts, e.g. the kernel load from stage 1).
- A plan: `[]action.Action`.

For each action in the plan:

1. Execute through the executor.
2. On success: record `rec.Complete(stepFor(action))`, push
   `compensationFor(action)` onto the compensation stack.
3. On failure: record `rec.Fail(stepFor(action))`, then
   `rec.BeginRollback()`, execute all compensations in reverse
   through the executor, record each compensation result.

The runner is the only place that calls the recorder's step-recording
methods. Manager code produces plans and reads outcomes; it no longer
sprinkles recording calls through imperative code.

### StepKind Derivation

The runner derives `outcome.StepKind` from the action type via a pure
function:

| Action | StepKind |
|--------|----------|
| `PublishBytecode` | `fs.publish_bytecode` (new) |
| `RemoveProgramDir` | `fs.remove_program_dir` (new) |
| `SaveProgram` | `store.save_program` (existing) |
| `DeleteProgram` | `store.delete_program` (existing) |
| `UnloadProgram` | `kernel.unload_program` (existing) |

### What the Runner Does Not Do

- **Decide what actions to run**: that is the compute function's job.
- **Know about domain semantics**: it does not know what a "program"
  or "bytecode" is. It processes `[]action.Action`.
- **Replace the outcome recorder**: it drives the recorder, not
  replaces it. The recorder's invariants (phase transitions, step
  sequencing) are preserved.
- **Handle stage 1**: the kernel load remains a direct call outside
  the runner because it yields data needed by the compute phase.

### Where the Runner Lives

The runner sits in `manager/` (not `interpreter/`). It is a
manager-level orchestration concern: it combines the executor, the
recorder, and compensation logic. The executor remains in
`interpreter/` as the low-level "interpret this single action" layer.

### Incremental Adoption

The runner is primarily motivated by `Load` (the only mutating
operation not using the action/executor pattern). Other operations
can adopt it incrementally:

- **Unload**: already uses `computeUnloadActions` + executor. Could
  adopt the runner for automatic outcome recording, but existing
  approach works and is not broken.
- **Attach/Detach**: currently use mixed patterns. Can migrate to the
  runner when touched for other reasons.

## The `fs` Package

### Root and Sub-roots

```go
package fs

// Root is an immutable, validated filesystem root. All fields are
// unexported; external packages cannot construct a non-zero Root
// without calling Open or FromRuntimeDirs.
type Root struct {
    base string
}

func Open(base string) (Root, error)

// Runtime returns the regular-filesystem domain rooted at base.
func (r Root) Runtime() Runtime

// BPFFS returns the bpffs layout domain rooted at base.
func (r Root) BPFFS() BPFFS
```

`Root` is a value type. Once constructed, it is immutable: all methods
use value receivers. Both `Runtime` and `BPFFS` are views derived from
the same `Root`. They are not constructible outside the package.

### Bootstrap: `FromRuntimeDirs`

During the migration, `fs.Root` must coexist with
`config.RuntimeDirs`. A bridge constructor allows incremental
adoption:

```go
func FromRuntimeDirs(d config.RuntimeDirs) Root
```

Tests assert that `FromRuntimeDirs(DefaultRuntimeDirs())` produces
the same paths as the current `RuntimeDirs` methods. Once all call
sites are migrated, `FromRuntimeDirs` and `config.RuntimeDirs` are
deleted.

### Infrastructure Paths

Some paths must escape as strings because their consumers are external
libraries or syscalls that require a file path:

```go
func (r Root) LockPath() string
func (r Root) DBPath() string
func (r Root) SocketPath() string
func (r Root) CSISocketPath() string

// BPFFSMountPoint is always <base>/fs. The base root determines
// all runtime and bpffs paths; there is no separate mountpoint
// configuration.
func (r Root) BPFFSMountPoint() string
```

### Mounting BPFFS

Mounting is performed by the existing `bpffs` package, but the mount
point is owned by `fs`:

```go
func (r Root) EnsureBPFFSMounted(mountInfoPath string) error {
    return bpffs.EnsureMounted(mountInfoPath, r.BPFFSMountPoint())
}
```

The layout under `<base>/fs` is owned by `fs.BPFFS()`.

### Runtime Operations (Interpreter Target)

`Runtime` methods are the interpreter target for filesystem actions.
The executor delegates `action.PublishBytecode` to
`rt.PublishBytecode()` and `action.RemoveProgramDir` to
`rt.RemoveProgram()`. Manager code should not call these methods
directly; it should produce action data instead.

```go
// PublishBytecode copies the ELF from srcPath to
// <base>/programs/{id}/bytecode.o via a staging directory under
// <base>/.staging/. If a directory for the given id already exists
// (crash residue from a previous attempt), it is removed first.
// The caller must have confirmed no DB row owns the id before
// calling. A provenance.json is written alongside the bytecode.
// The publish is atomic (rename on the same filesystem).
func (rt Runtime) PublishBytecode(id uint32, srcPath string, prov action.Provenance) error

// RemoveProgram removes <base>/programs/{id}/ and its contents.
// Returns nil if the directory does not exist. Uses safeRemoveAll
// internally to verify the target is under the programs directory
// before calling os.RemoveAll.
func (rt Runtime) RemoveProgram(id uint32) error

// ProgramExists reports whether <base>/programs/{id}/ exists.
func (rt Runtime) ProgramExists(id uint32) bool

// ProgramBytecodePath returns the path to the published bytecode.
// The caller needs this to store in the DB as ObjectPath. This is
// pure path derivation (no I/O) and is safe to call from compute
// functions.
func (rt Runtime) ProgramBytecodePath(id uint32) string

// CleanStaging removes any leftover staging directories under
// <base>/.staging/. Called at startup.
func (rt Runtime) CleanStaging() error
```

### Executor Integration

The existing executor (`interpreter/executor.go`) gains two new cases
in its type switch:

```go
case action.PublishBytecode:
    return e.fs.PublishBytecode(a.KernelID, a.SrcPath, a.Prov)

case action.RemoveProgramDir:
    return e.fs.RemoveProgram(a.KernelID)
```

The executor receives `fs.Runtime` as a constructor argument alongside
the existing `Store` and `KernelOperations`:

```go
func NewExecutor(store Store, kernel KernelOperations, fs fs.Runtime) ActionExecutor
```

### Path Safety: `safeRemoveAll`

All removal operations in `fs` use `safeRemoveAll` rather than bare
`os.RemoveAll`. This prevents a path-construction bug from escalating
into deletion of unintended directories:

```go
func safeRemoveAll(parent, target string) error {
    cleanParent := filepath.Clean(parent) + string(os.PathSeparator)
    cleanTarget := filepath.Clean(target)
    if !strings.HasPrefix(cleanTarget, cleanParent) {
        return fmt.Errorf("refusing to remove %q: not under %q", target, parent)
    }
    return os.RemoveAll(cleanTarget)
}
```

`safeRemoveAll` is unexported. Callers use `RemoveProgram(id)` and
`CleanStaging()`, which construct safe paths internally.

### BPFFS Operations

`fs.BPFFS()` wraps bpffs pin/unpin operations and scanner logic. The
existing `bpffs.Scanner` moves here (or `fs` wraps it), so that all
layout parsing lives in one package.

## Absorbing `config.RuntimeDirs`

`config.RuntimeDirs` currently owns all path construction for both the
regular filesystem and bpffs. Once `fs` is in place, `RuntimeDirs` is
replaced entirely.

### Paths Moving to `fs.Root` / `fs.Runtime`

| Current `RuntimeDirs` method | New owner |
|------------------------------|-----------|
| `Base()` | `fs.Root` (implicit) |
| `DBPath()` | `fs.Root.DBPath()` |
| `SocketPath()` | `fs.Root.SocketPath()` |
| `Lock()` | `fs.Root.LockPath()` |
| `CSISocketPath()` | `fs.Root.CSISocketPath()` |
| `EnsureDirectories()` | `fs.Root.EnsureDirectories()` |
| `EnsureCSIDirectories()` | `fs.Root.EnsureCSIDirectories()` |
| bytecode paths | `fs.Runtime()` methods |

### Paths Moving to `fs.BPFFS`

| Current `RuntimeDirs` method | New owner |
|------------------------------|-----------|
| `FS_*()` dirs | `fs.BPFFS()` methods |
| `ProgPinPath(id)` | `fs.BPFFS().ProgPinPath(id)` |
| `MapPinDir(id)` | `fs.BPFFS().MapPinDir(id)` |
| `LinkPinDir(id)` | `fs.BPFFS().LinkPinDir(id)` |
| `ScannerDirs()` | removed; scanner uses `fs.BPFFS()` directly |

### Construction Flow

Configuration shrinks to "read the base path and construct `fs.Root`":

```go
base := flagOrDefault("/run/bpfman")
root, err := fs.Open(base)

if err := root.EnsureBPFFSMounted(bpffs.DefaultMountInfoPath); err != nil {
    return err
}

store, err := sqlite.Open(root.DBPath())
lis, err := net.Listen("unix", root.SocketPath())
lock.Run(root.LockPath(), func(scope lock.WriterScope) { ... })
```

`config.RuntimeDirs` and `config.NewRuntimeDirs` are deleted. The
`config` package may still exist for reading configuration, but it no
longer owns filesystem layout.

## `filepath.Join` Restriction

Once `fs` exists, enforce that only `fs` (and tightly-scoped
exceptions) may import `path/filepath`. The intent is: bpfman-specific
layout lives in one place.

Recommended allow-list:

- `fs/**`
- `config/**` (only for config file discovery)
- `dispatcher/**` (temporary; to be absorbed into `fs.BPFFS()` over
  time)
- `*_test.go`

Enforce with depguard:

```yaml
linters:
  enable:
    - depguard

linters-settings:
  depguard:
    rules:
      no-filepath-outside-fs:
        deny:
          - pkg: "path/filepath"
            desc: >-
              path/filepath is restricted to fs, config, and
              dispatcher. Use fs operations instead of
              constructing paths directly.
        files:
          - "!**/fs/**"
          - "!**/config/**"
          - "!**/dispatcher/**"
          - "!**/*_test.go"
```

Note that `bpffs` should not need `filepath` at all.

## Package Structure

```
action/
    action.go           -- existing action types
    fs.go               -- PublishBytecode, RemoveProgramDir (NEW)
    provenance.go       -- Provenance struct (NEW)

bpffs/
    bpffs.go            -- mount detection/mounting only

fs/
    root.go             -- Root, Open, FromRuntimeDirs, validation,
                           infrastructure paths, EnsureDirectories,
                           EnsureBPFFSMounted
    runtime.go          -- Runtime type, PublishBytecode,
                           RemoveProgram, ProgramBytecodePath,
                           ProgramExists, CleanStaging, safeRemoveAll,
                           copyFile, writeJSON
    bpffs.go            -- BPFFS type, bpfman bpffs layout under
                           <base>/fs, scanner

interpreter/
    executor.go         -- existing executor, add fs.Runtime field
                           and cases for PublishBytecode,
                           RemoveProgramDir

manager/
    runner.go           -- runner: execute plan, record outcomes,
                           collect compensations, rollback on failure
                           (NEW)
    load.go             -- two-stage Load using runner
    undo.go             -- retained during migration; runner
                           supersedes for new code
```

Everything under `fs` is one package, one namespace, one import path.
If file count grows enough to justify subdirectories, the split is
mechanical (move files, update nothing -- same package). All
bpfman-specific layout sits under `fs`.

## Implementation Plan

Adoption is incremental. Each phase keeps the tree building and
passing tests. Bytecode persistence is the first real consumer.

### Phase 0 -- Introduce `fs` as an Adapter (No Behaviour Change)

1. Add `fs/root.go` with `Root`, `Open(base)`, and
   `FromRuntimeDirs(d config.RuntimeDirs)`.
2. Mirror existing `RuntimeDirs` paths 1:1 (including `fs/`, `db/`,
   `<base>-sock`, `csi/`, lock file).
3. Add tests asserting `FromRuntimeDirs(DefaultRuntimeDirs())`
   produces the same strings as today.

Done: no call sites changed; this is scaffolding only.

### Phase 1 -- Move Directory/Mount Initialisation Behind `fs.Root`

1. Implement `Root.EnsureDirectories()` doing exactly what
   `RuntimeDirs.EnsureDirectories()` does today: mkdir base, db,
   sock; ensure bpffs mounted at `<base>/fs`.
2. Change `manager.SetupRuntimeEnv` to call
   `root.EnsureDirectories()`.
3. Keep `RuntimeDirs.EnsureDirectories()` as a thin wrapper around
   `fs.FromRuntimeDirs(dirs).EnsureDirectories()` to avoid
   divergence.

Done: behaviour identical; only one implementation of "ensure dirs".

### Phase 2 -- Implement Runtime and Action Types

**`fs/runtime.go`**:

- `PublishBytecode(id uint32, srcObjPath string, prov action.Provenance) error`:
  if `<base>/programs/{id}` exists, remove it (crash residue). Create
  staging dir under `<base>/.staging/{id}.{rand}/`, copy bytecode,
  write provenance, `os.Rename` to `<base>/programs/{id}/`.
- `RemoveProgram(id uint32) error`: `safeRemoveAll` under the
  programs directory, ignore ENOENT.
- `ProgramBytecodePath(id uint32) string`.
- `ProgramExists(id uint32) bool`.
- `CleanStaging() error`: remove all entries under `<base>/.staging/`.

Internal helpers (unexported):

- `safeRemoveAll(parent, target string) error`: verify target is
  under parent before calling `os.RemoveAll`.
- `copyFile(src, dst string) error`: open source, create dest,
  `io.Copy`, close both. Explicit `0644` mode.
- `writeJSON(path string, v any) error`: `json.MarshalIndent`,
  write with `0644` mode.

**`action/fs.go`**:

- `PublishBytecode` and `RemoveProgramDir` action types.

**`action/provenance.go`**:

- `Provenance` struct.

**`interpreter/executor.go`**:

- Add `fs.Runtime` field to `executor`.
- Add cases for `action.PublishBytecode` and `action.RemoveProgramDir`.

Tests:

- Publish creates final dir with `bytecode.o` and `provenance.json`.
- Publish is atomic: simulated crash before rename leaves no final
  dir.
- Publish wipes existing directory (crash residue) before publishing.
- `CleanStaging` removes leftover staging dirs.
- `RemoveProgram` is idempotent.
- `safeRemoveAll` rejects targets outside parent.

Done: `fs` can publish bytecode, action types and executor wiring
exist; no manager call sites changed yet.

### Phase 3 -- Wire Bytecode Persistence into `Manager.Load`

Add `fs.Root` as an immutable value field on `Manager`, constructed
via `fs.FromRuntimeDirs(dirs)` in `New()`.

**Runner**

Introduce `manager/runner.go`:

```go
type runner struct {
    executor    interpreter.ActionExecutor
    rec         *outcome.ManagerOperationRecorder
    compensations []action.Action
}
```

The runner:

1. Takes a plan (`[]action.Action`).
2. Executes each action through the executor.
3. On success: records `rec.Complete(stepFor(action))`, pushes
   `compensationFor(action)` onto the compensation stack.
4. On failure: records `rec.Fail(stepFor(action))`, calls
   `rec.BeginRollback()`, executes compensations in reverse,
   records each result.

Pre-seeded compensations (for stage 1 effects) are pushed before
execution starts.

**Two-Stage Load**

Restructure `Manager.Load` as:

```go
func (m *Manager) Load(ctx context.Context, spec bpfman.LoadSpec, opts LoadOpts) (bpfman.Program, error) {
    var o outcome.OperationOutcome
    rec := outcome.NewRecorder(&o)

    // Stage 1: Kernel load (imperative, yields data)
    loaded, err := m.kernel.Load(ctx, spec, bpffs.Root(m.dirs.FS()))
    if err != nil {
        _ = rec.Fail(outcome.Step{Kind: outcome.StepKindKernelLoad, ...})
        return fail(...)
    }
    _ = rec.Complete(outcome.Step{Kind: outcome.StepKindKernelLoad, ...})

    // Stage 2: Fetch facts
    facts := fetchPostLoadFacts(ctx, m.store, loaded.Kernel.ID)

    // Stage 2: Compute plan (pure)
    plan, metadata, err := computePostLoadPlan(facts, loaded, spec, m.fsRoot.Runtime(), time.Now().UTC())
    if err != nil {
        rollbackKernel(ctx, m.kernel, loaded, &rec)
        return fail(err)
    }

    // Stage 2: Execute plan through runner
    r := newRunner(m.executor, &rec)
    r.seedCompensation(action.UnloadProgram{PinPath: loaded.Managed.PinPath})

    if err := r.Execute(ctx, plan); err != nil {
        // Runner already handled rollback and outcome recording
        return fail(err)
    }

    return buildProgram(loaded, metadata), nil
}
```

**Pure Compute Function**

```go
type postLoadFacts struct {
    kernelID      uint32
    programExists bool
}

func computePostLoadPlan(
    facts postLoadFacts,
    loaded interpreter.LoadedProgram,
    spec bpfman.LoadSpec,
    rt fs.Runtime,
    now time.Time,
) ([]action.Action, bpfman.ProgramSpec, error) {

    if facts.programExists {
        return nil, bpfman.ProgramSpec{}, fmt.Errorf(
            "invariant: program with kernel_id=%d already in DB", facts.kernelID)
    }

    prov := action.Provenance{
        Version:     1,
        KernelID:    loaded.Kernel.ID,
        ProgramName: spec.ProgramName(),
        Source:      spec.ObjectPath(),
        SourceKind:  sourceKindFromSpec(spec),
        LoadedAt:    now,
    }

    metadata := buildMetadata(loaded, spec, rt.ProgramBytecodePath(loaded.Kernel.ID), now)

    plan := []action.Action{
        action.PublishBytecode{KernelID: loaded.Kernel.ID, SrcPath: spec.ObjectPath(), Prov: prov},
        action.SaveProgram{KernelID: loaded.Kernel.ID, Metadata: metadata},
    }

    return plan, metadata, nil
}
```

Testable without mocks: give it facts, get back actions. Path
derivation via `rt.ProgramBytecodePath()` is pure string computation
(no I/O), safe to call from the compute function.

**Unload**

Add `action.RemoveProgramDir{KernelID: kernelID}` to the unload
action sequence produced by `computeUnloadActions`, before the
`DeleteProgram` action.

**`rollbackKernel` helper**

For the gap between stage 1 (kernel load succeeds) and stage 2
(runner takes over), a `rollbackKernel` helper handles the kernel
unload with full outcome recording. This replaces the inline rollback
code currently in `Load`.

Done: deleting the original object file no longer breaks
`get program`; DB `ObjectPath` points at
`/run/bpfman/programs/<id>/bytecode.o`.

### Phase 4 -- Migrate Scanner Ownership into `fs`

1. Move `bpffs.Scanner` into `fs` (or wrap it, forbidding new
   imports of `bpffs/scanner.go`).
2. Update `inspect.Snapshot()` to depend on `fs.BPFFS()` rather
   than `bpffs.ScannerDirs`.

Done: all layout parsing lives under `fs`.

### Phase 5 -- Replace Remaining `RuntimeDirs` Usage

Change runtime wiring to carry `fs.Root` rather than
`config.RuntimeDirs`:

- `manager.RuntimeEnv.Dirs` becomes `fs.Root`.
- Kernel loads use `fs.BPFFS()` for pin paths.
- CSI driver uses `fs.Root.CSISocketPath()` and
  `fs.Root.EnsureCSIDirectories()` instead of hardcoded defaults.
- Lock file uses `fs.Root.LockPath()`.

Done: only `fs` owns layout strings.

### Phase 6 -- Delete Old Layout Code

- Remove `config/runtime_dirs.go` (or reduce to config file
  parsing with no path derivation).
- Remove `bpffs/scanner.go` once all call sites are moved.
- Remove `manager/undo.go` once all operations use the runner.
- Add depguard rule to prevent new `filepath.Join` usage outside
  `fs`.

Done: single layout authority.

## Files to Modify

| File | Change |
|------|--------|
| `action/fs.go` | New: `PublishBytecode`, `RemoveProgramDir` action types |
| `action/provenance.go` | New: `Provenance` struct |
| `fs/` | New package (implements this design) |
| `bpffs/` | Keep mounting-only scope; scanner moves to `fs` |
| `config/runtime_dirs.go` | Phase 0: add `FromRuntimeDirs` bridge. Phase 6: delete |
| `interpreter/executor.go` | Phase 2: add `fs.Runtime` field, cases for filesystem actions |
| `manager/runner.go` | Phase 3: new runner (execute plan, record outcomes, compensate) |
| `manager/manager.go` | Phase 3: add `fs.Root` field, construct via `FromRuntimeDirs` |
| `manager/load.go` | Phase 3: two-stage Load with runner, `computePostLoadPlan`, add `RemoveProgramDir` to unload |
| `cmd/bpfman/cli/` | Phase 5: construct `fs.Root` from base path |
| `interpreter/ebpf/*` | Phase 5: replace `filepath.Join` with `fs.BPFFS()` ops |
| `manager/coherency.go` | Phase 4: use `fs` scanning APIs |
| `lock/` | Phase 5: accept `fs.Root.LockPath()` string |
| `manager/undo.go` | Phase 6: delete once runner covers all operations |

## Verification

1. `go build ./...`
2. `go test ./fs/...`
3. `go test ./...`
4. `go vet ./...`
5. Verify `fs.Root{}` does not compile outside the package
6. Verify `FromRuntimeDirs` produces identical paths to current code
7. Load from file: verify `ObjectPath` points to
   `<base>/programs/<id>/bytecode.o`
8. Unload: verify `<base>/programs/<id>/` is removed
9. Kill process between publish and DB commit: verify startup GC
   cleans the orphaned directory
10. Inject DB persist failure: verify runner compensates by removing
    the published bytecode directory and unloading the kernel program
11. Verify `computePostLoadPlan` is testable with zero mocks: supply
    facts, assert returned actions match expected sequence
