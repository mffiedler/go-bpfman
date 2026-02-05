# Filesystem Layout and Intent Design

Centralise all bpfman filesystem layout and filesystem mutations into
a single `fs` package. Callers should express operations (intent),
not compute paths or call `os.*` directly.

This refactor targets two concrete outcomes:

1. **Filesystem use is safe and uniform**:
   - path safety for deletions
   - consistent ENOENT handling
   - atomic publish primitive (staging + rename)
   - no ad-hoc path joins in non-layout code
   - readers never block on writer operations or GC

2. **Multi-step manager operations remain reliable**:
   - integrate filesystem operations into existing `undoStack` rollback
   - translate filesystem failures into outcome steps and residual
     artefacts
   - ship bytecode persistence without introducing a new execution
     abstraction

## Non-goals

This work does not:

- Replace `undoStack` or `outcome`
- Introduce a generic runner / effect system framework
- Move filesystem operations into `action/` or executor wiring
- Hide kernel/store operations behind the filesystem abstraction
- Restructure `Load` into a new execution model as a prerequisite

(Those may be worthwhile later, but they are independent.)

## Scope

This design covers two filesystem domains rooted at a single base
(default `/run/bpfman`):

- **Runtime FS**: regular filesystem state under `<base>/...`
- **BPFFS**: bpfman's bpffs conventions under `<base>/fs`

Mounting remains in `bpffs` (mount detection / mounting only). Layout
under `<base>/fs` is owned by `fs`.

## Runtime Environment Assumptions

Production uses a base directory under `/run` (tmpfs). All filesystem
state under `<base>` is ephemeral: it is expected to disappear on
reboot. bpfman's persistence guarantees are therefore crash-only
(daemon/process restarts), not power-loss durability. On host reboot,
the system starts from a clean slate by design.

Consequences:

- No fsync rules are needed; atomic rename is sufficient for crash
  consistency.
- GC and doctor can treat any `<base>` residue as disposable.
- `provenance.json` is purely diagnostic for crash residue, not
  long-term audit.

The DB (`<base>/db/store.db`) is also under `/run` and is therefore
ephemeral. `ObjectPath` can be stored as a literal string path without
concern for base-path migration across boots.

`Open(base)` accepts any absolute path (tests use `t.TempDir()`), but
rejects `/` and relative paths. The recommended production default is
`/run/bpfman`.

All `fs` mutations happen under the global writer lock (the `lock/`
package BGL). No concurrent writers exist within a single `<base>`.

### Concurrency Model

bpfman uses a single writer / multiple readers model:

- **Writers** (load, unload, attach, detach, dispatcher changes) hold
  the global writer lock (BGL). Only one writer runs at a time.
- **Readers** (get, list) do not acquire the BGL. Multiple readers may
  run concurrently with each other and with a single writer.

Readers only consult published paths (`<base>/programs/{id}/`). They
never see staging directories because atomic rename is the visibility
boundary: a published directory either exists completely or not at all.

Writers are responsible for GC, rollback, and staging. Readers are
never exposed to intermediate state.

## Problem

Filesystem operations are scattered across the codebase with:

- repeated inline ENOENT guards and inconsistent error handling
- ad-hoc rollback patterns (defer flags, bespoke LIFO slices, cascades)
- unsafe deletions (no guard that `RemoveAll` targets are under
  expected parent)
- duplicated tempdir/rename logic and no common atomic publish utility
- raw `filepath.Join` usage in non-layout code

The codebase is already partway there. `bpffs` provides mounting and
typed wrappers; `config.RuntimeDirs` centralises path derivation. The
gap is that both leak raw strings and encourage ad-hoc `os.*` calls.
We need one canonical place that owns bpfman layout and exposes
operations.

## Terminology

### Undo step

An undo step is a best-effort action registered after a successful
operation that removes or reverses the artefact if a later step fails.

Undo steps are stored on `undoStack` and executed in reverse order
during rollback.

### Rollback

Rollback is the process of executing undo steps after an error in a
later phase.

## Design Principles

### Follow the `lock/` Package Pattern

Callers cannot construct the filesystem root without going through a
validated constructor. Root types act as capability tokens, and all
operations are methods on those roots that enforce layout invariants
internally. All three types (`Root`, `Runtime`, `BPFFS`) are concrete
structs with unexported fields, not interfaces. Callers may refer to
these types but cannot construct a usable value: their fields are
unexported and the only constructors are in `fs`.

### One Canonical Layout Package: `fs/`

Introduce a top-level `fs` package as the single canonical place for:

- all path construction rooted at the configured base directory
- all filesystem operations that mutate bpfman state

Callers should almost never need a path string. They call operations.

### `bpffs/` Is Mounting Only

The existing `bpffs` package remains the small, testable home for:

- mount detection via `/proc/self/mountinfo`
- mount/unmount
- `EnsureMounted`

It does not own bpfman's directory conventions under the mount. Those
move to `fs.BPFFS()`.

### The Interpreter Is the Only Place That Calls `os.*`

Inside `fs`, any direct OS I/O is confined to a small internal
interpreter layer. Public methods in `fs` must not call `os.*`
directly; they build a sequence of internal primitives and execute
them via the interpreter.

External packages should not call `os.RemoveAll` on bpfman paths.

### `undoStack` and `outcome` Are Not Replaced

`fs` returns normal errors. The manager remains the orchestrator:

- it records outcomes
- it registers undo steps
- it performs rollback on failure
- it reports residual artefacts

No new `operr` package. No new cleanup stack.

## DB Is Commit, FS Is Artefacts

### Core Invariant

SQLite is the sole source of truth for managed state.

The filesystem holds only artefacts and crash-residue markers. Any
artefact not referenced by a DB row is uncommitted residue and is safe
to delete.

The filesystem must not become a second database. Queryable metadata
(program name, source, timestamps, labels) stays in SQLite.

### Published vs Staging Paths

Only published paths are externally visible:

- **Published**: `<base>/programs/{id}/` (runtime FS) and
  `<base>/fs/programs/{id}/` (bpffs, target layout) -- appear
  atomically via rename. Readers and DB fields reference these paths.
- **Staging**: `<base>/.staging/` (runtime FS) and
  `<base>/fs/.staging/` (bpffs) -- transient scratch space visible
  only to the writer that created it. Never referenced by DB rows,
  never consulted by readers.

This separation ensures readers never encounter partial state: a
directory is either fully published or absent. Each domain has its own
staging root to guarantee same-filesystem renames.

### Provenance Exception

`provenance.json`, written alongside `bytecode.o` in the published
bundle, exists purely as a diagnostic trace. It is never read by
bpfman's operational code path. Only `gc`, `doctor`, and humans read
it.

### Bytecode Persistence Layout

"Persisted bytecode" means durable across bpfman restarts on the same
boot. It is not expected to survive a host reboot because `<base>` is
under `/run`.

The stable published path is:

```
<base>/programs/{kernel_id}/bytecode.o
<base>/programs/{kernel_id}/provenance.json
```

Two-phase publish (staging + atomic rename) is an implementation
detail inside `fs`. Staging lives under `<base>/.staging/` to
guarantee `os.Rename` is atomic (same filesystem). Staging is opaque
to callers. Readers never consult staging paths; they only access
published directories under `<base>/programs/{id}/`, which appear
atomically via rename.

### Manager Clean-Slate Contract

The manager is written under a clean-slate assumption:

- Before any mutating manager operation (load, unload, attach, detach,
  dispatcher changes), bpfman runs the coherency engine's GC pass
  under the global writer lock.
- If coherency evaluation reports any ERROR findings, the manager
  refuses to mutate state.
- After the GC pass completes successfully, manager operations may
  assume the filesystem is already normalised (no unowned residue
  under `<base>` that would interfere with the operation).

This keeps manager flows simple and deterministic: they do not contain
ad-hoc "accommodate residue" logic. The coherency model is the single
place that decides what is safe to delete.

The clean-slate contract governs writers only. Concurrent readers
continue to serve requests without acquiring the writer lock and
without waiting for GC to complete.

### Ordering for Load

#### Framing

Because runtime FS (`<base>`, tmpfs) and bpffs (`<base>/fs`) are
separate mounts, no single rename can span both domains. Each domain
must be internally atomic (staging + rename within its own filesystem),
and the DB write is the global commit boundary that readers trust.

#### Load steps

After kernel load succeeds and yields `kernel_id`:

1. **DB existence check** (hard gate).
   - If a DB row already exists for `kernel_id`: return a hard error
     and rollback kernel state. No runtime FS or bpffs writes.
   - The store must expose a stable `ErrNotFound` sentinel (or
     equivalent) so callers can distinguish absence from failure.
2. **Runtime FS publish** (tmpfs domain).
   - `rt.PublishBytecode(kernel_id, srcPath, prov)`.
   - Coherency/GC ran before this operation was accepted, so orphan
     `programs/{id}` directories should not exist.
   - If `<base>/programs/{kernel_id}` already exists, this is an
     invariant violation: return `ErrFinalExists` and do not write
     any data.
   - Atomic rename is the reader visibility boundary: before rename,
     no reader can observe the directory; after rename, the directory
     is fully formed.
   - Register undo step: `rt.RemoveProgram(kernel_id)`.
3. **BPFFS publish** (bpffs domain).
   - *Today (pre-restructure)*: pin objects as currently done
     (scattered `prog_<id>`, `maps/<id>/`, `links/<id>/`).
   - *Target (per-program directory)*:
     `bpffs.PublishProgramPins(kernel_id, buildFn)` which pins into
     bpffs staging and renames into `<base>/fs/programs/<id>/`.
   - Register undo step: remove/unpin the published bpffs directory
     (or the individual pins, depending on phase).
4. **DB transaction commit** (global commit).
   - Store is always last.
   - The DB row becomes the definition of existence for readers.
5. **On DB commit failure**:
   - Rollback kernel state via existing `undoStack`.
   - Remove `<base>/programs/{kernel_id}` via `fs` (undo step).
   - Remove bpffs pins (undo step).
   - Record residual artefacts in `outcome.OperationOutcome`.

Step 1 must happen before steps 2 and 3. The DB existence check
guards against clobbering a committed program's artefacts in either
domain. Steps 2 and 3 both happen before step 4 so that both domains
are internally consistent before the DB commit makes the program
visible to readers.

### Crash Recovery via Coherency Rules

Crash residue is handled by the coherency rule engine in
`manager/coherency.go`, not by ad-hoc cleanup in operational code
paths.

#### Runtime program directories

`GatherState` gains a new scan phase that lists `<base>/programs/`
and emits filesystem orphans for any entry not backed by a DB record:

- For each entry under `<base>/programs/`:
  - If the directory name parses as `uint32` and `kernel_id` is not
    present in `dbProgIDs`, emit
    `FsOrphan{Kind: "program-dir", KernelID: id, Path: ...}`.
  - If the name does not parse as `uint32`, emit
    `FsOrphan{Kind: "program-dir-unknown", Path: ...}`. This is still
    safe to delete because it is within bpfman's runtime-owned tree.

A new GC rule (`orphan-program-dirs`) removes these orphans. This
ensures that by the time a mutating manager operation runs, unowned
`programs/{id}` directories have already been deleted.

`PublishBytecode` does not wipe crash residue. If the final directory
exists when `PublishBytecode` is called, it returns `ErrFinalExists`
(invariant violation).

#### Staging directories

Staging directories under `<base>/.staging/` are never "owned" by the
DB. They exist only as transient publish scratch space and may remain
after a crash.

`GatherState` scans `<base>/.staging/` and emits
`FsOrphan{Kind: "staging-dir", Path: ...}` for each entry found. A
GC rule (`orphan-staging-dirs`) removes all such entries.

With this model, staging cleanup is part of the coherency GC pass and
is not a responsibility of normal manager operations.

#### BPFFS program directories (target layout)

Once the bpffs per-program directory layout is adopted (Phase 4),
`GatherState` gains a scan of `<base>/fs/programs/` mirroring the
runtime program directory scan. Entries with no DB program row are
emitted as `FsOrphan{Kind: "bpffs-program-dir"}`. A GC rule removes
these orphans, subject to the existing live-orphan policy.

#### BPFFS staging directories

`GatherState` scans `<base>/fs/.staging/` and emits
`FsOrphan{Kind: "bpffs-staging-dir"}` for each entry. These are
always safe to delete (staging is never owned by DB).

#### Provenance

`doctor` may read `provenance.json` from orphaned runtime program
directories to aid diagnosis. Provenance is not read on operational
code paths.

### Expose Operations, Not Paths

Callers should almost never need a string path. Operations are
intent-based:

- `PublishBytecode(id, srcPath, prov)`
- `RemoveProgram(id)`
- `ProgramBytecodePath(id)` (needed to persist DB `ObjectPath`)
- BPFFS ops for pins and scanning (later phases)

The `<base>/programs/{id}/` directory is owned by `fs` and may gain
additional files in future. Callers must treat it as opaque; only
`ProgramBytecodePath` provides a path that escapes.

Smell test: if a caller does `filepath.Join`, something is wrong.

## The `fs` Package

### Root and Sub-roots

```go
package fs

// Root is an immutable, validated filesystem root. Fields are
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

`Root` is a value type and immutable. `Runtime` and `BPFFS` are views
derived from the same root and are not constructible outside the
package (capability token). The zero value of `Root`, `Runtime`, and
`BPFFS` is invalid; methods return `ErrInvalidRoot`.

### Bootstrap: `FromRuntimeDirs`

During migration, `fs.Root` coexists with `config.RuntimeDirs`:

```go
func FromRuntimeDirs(d config.RuntimeDirs) Root
```

Tests assert that `FromRuntimeDirs(DefaultRuntimeDirs())` produces the
same paths as the current `RuntimeDirs` methods. Once call sites are
migrated, `FromRuntimeDirs` and `config.RuntimeDirs` are deleted.

### Infrastructure Paths

Some paths must escape as strings (required by external libraries or
syscalls):

```go
func (r Root) LockPath() string
func (r Root) DBPath() string
func (r Root) SocketPath() string
func (r Root) CSISocketPath() string

// BPFFSMountPoint is always <base>/fs.
func (r Root) BPFFSMountPoint() string
```

### Mounting BPFFS

Mounting is performed by `bpffs`, but the mount point is owned by
`fs`:

```go
func (r Root) EnsureBPFFSMounted(mountInfoPath string) error {
    return bpffs.EnsureMounted(mountInfoPath, r.BPFFSMountPoint())
}
```

The layout under `<base>/fs` is owned by `fs.BPFFS()`.

## Runtime Operations

### Provenance Schema

`PublishBytecode` writes `provenance.json` alongside `bytecode.o`. The
schema follows
[BYTECODE-PERSISTENCE.md](BYTECODE-PERSISTENCE.md):

```go
type Provenance struct {
    Version     int       `json:"version"`
    KernelID    uint32    `json:"kernel_id"`
    ProgramName string    `json:"program_name"`
    Source      string    `json:"source"`
    SourceKind  string    `json:"source_kind"` // "file", "image", "unknown"
    LoadedAt    time.Time `json:"loaded_at"`   // RFC 3339 UTC
}
```

### Constants

```go
const (
    bytecodeName = "bytecode.o"
    provName     = "provenance.json"
    dirMode      = 0755
    fileMode     = 0644
)
```

### Public API

```go
// Runtime is a concrete type with unexported fields. External
// packages cannot construct one; they must obtain it via
// Root.Runtime().
type Runtime struct {
    root Root
}

// PublishBytecode publishes srcPath to:
//   <base>/programs/{id}/bytecode.o
// via staging under <base>/.staging/.
//
// srcPath must refer to a readable regular file containing the
// ELF object. If it does not exist or is not readable, a
// PathError is returned.
//
// If <base>/programs/{id} already exists, PublishBytecode returns
// an error. The caller is expected to have run GC before loading,
// which removes orphan directories. An existing final directory
// after GC indicates an invariant violation.
//
// A provenance.json is written alongside the bytecode. Publish
// is atomic (rename on the same filesystem).
func (rt Runtime) PublishBytecode(id uint32, srcPath string, prov Provenance) error

// RemoveProgram removes <base>/programs/{id}/ and its contents.
// Returns nil if the directory does not exist. Uses safeRemoveAll
// to verify the target is under the programs directory.
func (rt Runtime) RemoveProgram(id uint32) error

// ProgramExists reports whether <base>/programs/{id}/ exists.
func (rt Runtime) ProgramExists(id uint32) bool

// ProgramBytecodePath returns the published bytecode path for
// DB ObjectPath.
func (rt Runtime) ProgramBytecodePath(id uint32) string

// CleanStaging removes all entries under <base>/.staging/.
// Staging is a writer-only concern and is never visible to readers.
// Not called directly by manager operations; the coherency GC rule
// (orphan-staging-dirs) handles staging cleanup via GatherState
// scanning.
func (rt Runtime) CleanStaging() error
```

### Path Safety: `safeRemoveAll`

All removal operations in `fs` use `safeRemoveAll` rather than bare
`os.RemoveAll`:

```go
func safeRemoveAll(parent, target string) error {
    rel, err := filepath.Rel(parent, target)
    if err != nil {
        return ErrOutsideRoot{Parent: parent, Target: target}
    }
    if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
        return ErrOutsideRoot{Parent: parent, Target: target}
    }
    return os.RemoveAll(target)
}
```

Using `filepath.Rel` avoids prefix false positives (e.g.,
`/run/bpfman/programsX` matching `/run/bpfman/programs`). This does
not protect against malicious symlinks inside the tree; that is
acceptable for the threat model (bpfman runs as root managing its own
directories).

`safeRemoveAll` is unexported. Callers use `RemoveProgram(id)` and
`CleanStaging()`.

## BPFFS Operations

### Core Goal

Make bpffs visibility atomic per ownership unit, so readers never
observe "half a thing". Because bpffs is a separate mount from tmpfs
(`/run`), its atomicity must be handled within bpffs, independently
from the runtime filesystem publish.

`bpffs` (the package) remains "mounting only". `fs.BPFFS()` owns
bpfman's layout conventions and pin operations under `<base>/fs`.

### bpffs Constraints

bpffs is a minimal virtual filesystem. Its inode operations support
`mkdir`, `rmdir`, `rename`, `link`, `unlink`, and BPF object pinning
(`bpf_obj_pin`). It does **not** support:

- **Symlinks**: no `.symlink` inode operation.
- **Regular file creation**: only BPF object pins (programs, maps,
  links) and directories.

This means diagnostic metadata (`meta.json`, active-revision pointers)
cannot live in bpffs. Such artefacts belong in the runtime FS domain
or in the DB.

### Ownership Classes

There are two distinct ownership classes under bpffs:

1. **Per-program artefacts** (owned by one `kernel_id`): the program
   pin, its map pins, and its link pins.
2. **Per-attach-point dispatcher artefacts** (owned by an attach
   point, not by a single program): the dispatcher program, its
   links, and revision state.

These have different lifecycles and must have different top-level
trees.

### Target BPFFS Layout

Under `<base>/fs` (bpffs mount):

#### Program-owned tree

```
<base>/fs/programs/<kernel_id>/
  program               # pinned program
  maps/
    <map_key>           # pinned maps (keyed by name or stable ID)
  links/
    <link_key>          # pinned links
```

Key property: everything required to represent "this program's bpffs
presence" sits under one directory that can be published with one
rename.

#### Dispatcher-owned tree (explicit carve-out)

Dispatchers are not owned by a single program. They live separately:

```
<base>/fs/dispatchers/<disp_type>/<ifindex>/
  revisions/<rev>/
    dispatcher_program  # pinned dispatcher program
    links/
      <link_key>        # pinned link handles (XDP, TC, etc.)
```

The unit of atomicity is "a revision for a given attach point", not a
program ID. The active revision pointer and any diagnostic metadata
live in the runtime FS domain or the DB, not in bpffs (see bpffs
Constraints above).

### BPFFS Staging and Atomic Publish

bpffs publish uses its own staging root to guarantee same-filesystem
renames:

```
<base>/fs/.staging/
  programs/<tmpname>/
  dispatchers/<tmpname>/
```

Publish primitives:

- **`PublishProgramPins(kernel_id, buildFn)`**:
  1. Create `<base>/fs/.staging/programs/<tmp>/`.
  2. Pin program, maps, and links into the staging directory.
  3. `rename(<tmp>, <base>/fs/programs/<kernel_id>/)` -- atomic
     within bpffs.

- **`PublishDispatcherRevision(key, rev, buildFn)`**:
  1. Create `<base>/fs/.staging/dispatchers/<tmp>/`.
  2. Pin the dispatcher program and links into staging.
  3. `rename(<tmp>, <base>/fs/dispatchers/<key>/revisions/<rev>/)`.

Staging is opaque: scanners, doctor, and normal readers must not
treat anything under `.staging/` as meaningful. The DB row remains the
global commit boundary.

### Scanner Implications

The bpffs scanner should:

- Scan only published roots: `programs/` and `dispatchers/`
  (excluding `.staging/` and transient temps).
- Treat each published directory as a self-contained unit:
  - `programs/<id>/` describes the full bpffs state for that program.
  - `dispatchers/<key>/revisions/<rev>/` describes the full state for
    that dispatcher revision.

This matches the "one rename = one visibility boundary" design.

### Coherency and GC Implications

Pins not referenced by a DB row are uncommitted residue and safe to
delete, following the same "DB is commit" invariant as the runtime
filesystem domain.

- `.staging/` entries under bpffs are always safe to delete (never
  owned by DB).
- Any `programs/<id>/` directory with no DB program row is
  uncommitted residue, safe to delete (subject to the existing "live
  orphan" policy).
- Dispatcher GC is keyed off dispatcher DB rows and attach-point
  state, not program rows.

GC rule closures in `manager/coherency.go` currently call `os.Remove`
and `os.RemoveAll` directly. These migrate to `fs.BPFFS()` methods
once available, centralising all removal through path-safety guards.

## Intent Model (Internal)

`fs` may use an internal "primitive/macro" split:

- **Public macro ops**: `PublishBytecode`, `RemoveProgram`, etc.
- **Internal primitives** (ops as data): `EnsureDir`, `RemoveAll`,
  `CopyFile`, `Rename`, `MkdirTemp`, `WriteFile`

The OS-backed interpreter executes primitives and is the only code
that calls `os.*`. Public macro ops expand into primitives and call
the interpreter internally.

This improves testability and reuse without exposing a new abstraction
to the rest of the codebase.

## Error Model

`fs` returns normal errors. It should provide a minimal set of typed
errors to allow the manager to translate failures into outcomes:

- `ErrInvalidRoot` -- method called on zero-value `Root`, `Runtime`,
  or `BPFFS`
- `ErrOutsideRoot` -- path safety violation
- `ErrFinalExists` -- `PublishBytecode` found the final directory
  already exists. This is an invariant violation: GC should have
  cleaned orphan directories before the load path executes.
- `PathError{Op, Path, Err}` -- wrapper (like `os.PathError`, but
  with `fs` operation names)

The manager may use `errors.Is`/`errors.As` to decide how to report
and what residual artefacts to emit.

## Testing Strategy

Tests use `fs.Open(t.TempDir())` to obtain a unique root and run
against real OS filesystem semantics. The fake kernel populates
bpffs-like paths under that root. Mocking `Runtime` is unnecessary
and would weaken the capability-token guarantee.

Standard test pattern:

```go
root := fs.MustOpen(t, t.TempDir())  // test helper
rt := root.Runtime()

// Wire manager/kernel/store to root-derived paths:
//   DB at root.DBPath()
//   bpffs layout via root.BPFFS()
//   fake kernel targeting root's directory tree

// Exercise the operation, then assert filesystem state:
//   programs/<id>/bytecode.o exists
//   provenance.json exists
//   rollback cleans them up
```

The internal interpreter seam (primitive ops executed by an OS-backed
interpreter) exists for `fs`-internal unit tests, not for external
mocking. Manager and integration tests exercise `fs` against the real
filesystem.

### Must-have tests

These enforce the load-bearing invariants:

- **No runtime FS writes when DB row exists**: simulate DB containing
  `kernel_id`; verify `PublishBytecode` is never called (or
  `programs/<id>` remains absent after the load attempt fails).
- **GC removes orphan program directories**: create
  `<base>/programs/<id>` without a DB row; run GC (coherency rules);
  assert the orphan directory is removed.
- **PublishBytecode fails if final directory exists**: skip GC; call
  `PublishBytecode` for an ID whose `programs/<id>` already exists;
  assert `ErrFinalExists` is returned and no data is written.

## Relationship to `undoStack` and `outcome`

`fs` does not record outcomes. It returns errors.

The manager remains responsible for:

- recording begin/success/failure steps
- registering undo steps on `undoStack`
- invoking rollback on error
- recording residual artefacts

Concrete example -- after `PublishBytecode(...)` succeeds:

```go
undo.push(func() error { return rt.RemoveProgram(id) })
```

If rollback cleanup fails, record an `outcome.Artefact` such as
`ArtefactProgramDir` at `<base>/programs/{id}`.

## Absorbing `config.RuntimeDirs`

`config.RuntimeDirs` currently owns path derivation. Once `fs` exists,
`RuntimeDirs` is replaced.

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

Configuration becomes "read base path, construct `fs.Root`":

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

`config.RuntimeDirs` is deleted once all call sites migrate.

## `filepath.Join` Restriction

Once `fs` exists, enforce that only `fs` (and tightly-scoped
exceptions) may import `path/filepath`.

Recommended allow-list (temporary):

- `fs/**`
- `config/**` (only for config file discovery)
- `dispatcher/**` (temporary)
- `*_test.go`

Depguard example:

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

## Startup and Maintenance

When the manager is opened, and before it accepts any operations, it
runs a coherency GC pass under the global writer lock:

- Removes orphan runtime bytecode directories under
  `<base>/programs/` (no DB record).
- Removes orphan bpffs artefacts (pins/dirs not backed by DB records),
  subject to the existing "live orphan" policy (no pruning unless
  explicitly requested).
- Removes orphan staging directories under `<base>/.staging/` and
  `<base>/fs/.staging/` (once bpffs restructuring lands).

After this pass completes successfully, the manager operates in a
normalised state. Mutating manager operations do not include bespoke
cleanup logic; they assume coherency has already established the
preconditions they require.

Because `<base>` is ephemeral under `/run`, startup cleanup is
best-effort and may assume missing directories are normal. Operational
code paths do not consult provenance.

## Package Structure

```
bpffs/
    bpffs.go            -- mounting only

fs/
    root.go             -- Root, Open, FromRuntimeDirs bridge,
                           validation, infra paths,
                           EnsureDirectories, EnsureBPFFSMounted
    runtime.go          -- Runtime domain ops, provenance schema,
                           staging publish, safeRemoveAll,
                           CleanStaging
    bpffs.go            -- BPFFS domain ops, bpfman bpffs layout,
                           scanner
    intent.go           -- internal primitives + OS-backed
                           interpreter
```

## Implementation Plan

Adoption is incremental. Each phase keeps the tree building and
passing tests. Bytecode persistence is the first consumer.

### Phase 0 -- Introduce `fs` as an Adapter (No Behaviour Change)

1. Add `fs/root.go` with `Root`, `Open(base)`, and
   `FromRuntimeDirs(d config.RuntimeDirs)`.
2. Mirror existing `RuntimeDirs` paths 1:1 (including `fs/`, `db/`,
   `<base>-sock`, `csi/`, lock file).
3. Tests asserting `FromRuntimeDirs(DefaultRuntimeDirs())` produces
   the same strings as today.

Done: no call sites changed; scaffolding only.

### Phase 1 -- Move Directory/Mount Initialisation Behind `fs.Root`

1. Implement `Root.EnsureDirectories()` doing what
   `RuntimeDirs.EnsureDirectories()` does today.
2. Change setup code to call `root.EnsureDirectories()`.
3. Keep `RuntimeDirs.EnsureDirectories()` as a thin wrapper around
   `fs.FromRuntimeDirs(dirs).EnsureDirectories()` temporarily.

Done: behaviour identical; only one implementation.

### Phase 2 -- Implement Runtime Bytecode Publish in `fs`

1. Add `fs/runtime.go` implementing:
   - `PublishBytecode`
   - `RemoveProgram`
   - `ProgramExists`
   - `ProgramBytecodePath`
   - `CleanStaging`
   - `safeRemoveAll`, `copyFile`, `writeJSON` (internal)
2. Unit tests:
   - Publish creates final dir with bytecode + provenance.
   - Publish is atomic (final appears only after rename).
   - Publish returns `ErrFinalExists` if final dir already exists.
   - `RemoveProgram` is idempotent.
   - `safeRemoveAll` rejects out-of-root deletes.
   - `CleanStaging` removes leftovers.

Done: `fs` is ready; not yet wired into manager.

### Phase 2.5 -- Thread `fs.Root` Through Manager (No Behaviour Change)

1. Add `root fs.Root` field to the manager struct (or runtime
   environment).
2. Construct via `fs.FromRuntimeDirs(dirs)` at startup.
3. No call sites changed beyond plumbing.

Done: `fs.Root` is available in manager; no behaviour change.

### Phase 3 -- Wire Bytecode Persistence into `Manager.Load`

1. Add `<base>/programs/` scan to `GatherState` (new scan phase).
   Emit `FsOrphan{Kind: "program-dir"}` for directories with no DB
   row. Emit `FsOrphan{Kind: "program-dir-unknown"}` for entries
   whose names do not parse as `uint32`.
2. Add `<base>/.staging/` scan to `GatherState`. Emit
   `FsOrphan{Kind: "staging-dir"}` for each entry.
3. Add `orphan-program-dirs` GC rule to `GCRules()` that removes
   orphan `programs/{id}` directories via `fs.Runtime().RemoveProgram`.
4. Add `orphan-staging-dirs` GC rule to `GCRules()` that removes
   orphan staging directories via `fs.Runtime().CleanStaging` (or
   `safeRemoveAll` directly).
5. Wire into `Manager.Load`:
   a. After kernel load yields `kernel_id`:
      - DB existence check: exists -> hard error, rollback kernel, no
        FS writes.
   b. Call `PublishBytecode(kernel_id, spec.ObjectPath(), prov)`.
      GC has already cleaned orphan directories; `PublishBytecode`
      fails if the final directory exists (invariant violation).
   c. Register undo step: `RemoveProgram(kernel_id)`.
   d. Rewrite DB `ObjectPath` to `ProgramBytecodePath(kernel_id)`.
   e. On DB transaction failure:
      - Rollback (undo step removes program dir; kernel unload removes
        pins).
      - Record residual artefacts via outcome if any undo step fails.
6. Add bytecode directory cleanup to `Manager.Unload`:
   `RemoveProgram(kernel_id)` after map pins removed, before DB
   delete.

Done: bytecode persistence shipped; DB `ObjectPath` is stable.

### Phase 4 -- BPFFS Per-Program Directory Layout

1. Implement the target bpffs layout:
   - `<base>/fs/programs/<kernel_id>/` (program, maps, links).
   - `<base>/fs/dispatchers/<disp_type>/<ifindex>/revisions/<rev>/`.
   - `<base>/fs/.staging/` for bpffs staging + atomic rename.
2. Add `fs.BPFFS()` publish primitives:
   - `PublishProgramPins(kernel_id, buildFn)`.
   - `PublishDispatcherRevision(key, rev, buildFn)`.
3. Migrate pin/unpin operations from scattered paths to per-program
   directories.
4. Move or wrap `bpffs.Scanner` into `fs`, scanning only published
   roots (`programs/`, `dispatchers/`), excluding `.staging/`.
5. Add bpffs orphan types to `GatherState`:
   - `bpffs-program-dir` (no DB row).
   - `bpffs-staging-dir` (always safe to delete).
6. Add GC rules for bpffs orphans.
7. Update snapshot/coherency/doctor to use `fs.BPFFS()`.

Done: bpffs visibility is atomic per ownership unit; layout parsing
is centralised.

### Phase 5 -- Replace Remaining `RuntimeDirs` Usage

1. Thread `fs.Root` through runtime wiring.
2. Move CSI paths and socket paths to `fs.Root` accessors.
3. Start enforcing depguard rule for `filepath`.

Done: only `fs` owns layout strings.

### Phase 6 -- Delete/Retire Legacy Layout Code

1. Delete `config.RuntimeDirs` path derivation.
2. Delete old scanner code in `bpffs` if migrated.
3. Tighten depguard allow-list.

Done: single canonical layout owner.

## Verification

1. `go build ./...`
2. `go vet ./...`
3. `go test ./fs/...`
4. `go test ./...`
5. Load from file:
   - DB `ObjectPath` points to `<base>/programs/<id>/bytecode.o`.
6. Delete original source file:
   - `get`/`list` still works (bytecode is persisted).
7. Kill between publish and DB commit:
   - DB has no row; `<base>/programs/<id>` exists; `gc` cleans it.
8. Inject DB persist failure:
   - Rollback removes published bytecode dir and bpffs pins.
9. `depguard` blocks new `filepath.Join` outside allow-list.
