# Bytecode Persistence and OCI Cache GC

This document describes a design for persisting BPF bytecode (ELF
object files) in a stable runtime directory so that bpfman retains
access to the ELF after the original source disappears, and for
safely garbage-collecting the OCI pull cache once it is no longer
the authoritative copy.

## Problem

`ProgramLoadSpec.ObjectPath` currently points to wherever the
bytecode was loaded from: the OCI cache
(`~/.cache/bpfman/images/sha256_xxx/bytecode.o`) for image loads, or
the user-provided path for file loads. Both are fragile -- the cache
can be evicted, the user's file can be deleted. Once the source
disappears, we lose access to the ELF, which blocks features that
need to re-read it (e.g., live global variable introspection via
MapReplacements).

## Constraint: bpffs Cannot Hold Regular Files

bpffs is a BPF virtual filesystem that only accepts BPF objects
(programs, maps, links). Regular files like ELF bytecode cannot be
stored under `/run/bpfman/fs/`. We need a parallel directory on the
regular filesystem.

## Design

### New Directory: `{base}/programs/{kernel_id}/`

Add a per-program directory on the regular filesystem:

```
/run/bpfman/programs/{kernel_id}/bytecode.o
```

This sits alongside the existing bpffs structure:

```
/run/bpfman/
  fs/                          <- bpffs (BPF objects only)
    prog_{kernel_id}           <- program pin
    maps/{kernel_id}/          <- map pins
    links/{kernel_id}/         <- link pins
  programs/                    <- regular filesystem (NEW)
    {kernel_id}/
      bytecode.o               <- ELF copy (0644)
      provenance.json          <- load provenance (0644)
  db/
    store.db
```

### Flow

1. OCI cache (`~/.cache`) continues as the pull cache (unchanged).
2. Pull policy logic remains unchanged.
3. **New**: after kernel load succeeds (kernel ID is known), copy the
   ELF from the source path to
   `/run/bpfman/programs/{kernel_id}/bytecode.o` and write a
   `provenance.json` alongside it (see schema below).
4. `ProgramLoadSpec.ObjectPath` in the DB points to the runtime copy.
5. On unload, remove the bytecode directory.

This applies to all load sources (image and file) for consistency.
The original file or cache entry can be deleted afterwards without
affecting bpfman's ability to re-read the ELF.

## Changes

### 1. `config/runtime_dirs.go`

Add:

- `programs` field to `RuntimeDirs`:
  `filepath.Join(base, "programs")`
- `Programs() string` getter
- `BytecodeDir(kernelID uint32) string` returning
  `{base}/programs/{id}/`
- `BytecodePath(kernelID uint32) string` returning
  `{base}/programs/{id}/bytecode.o`
- `ProvenancePath(kernelID uint32) string` returning
  `{base}/programs/{id}/provenance.json`
- Add `d.programs` to the `EnsureDirectories()` loop so the parent
  directory is created at startup

### 2. `manager/load.go` -- `Manager.Load()`

After the kernel load succeeds (line 89, `loaded` is populated) and
before the DB persist (line 143), establish safety, clean up any
crash residue, then assemble and publish the bytecode bundle:

```go
id := loaded.Kernel.ID
finalDir := m.dirs.BytecodeDir(id)

// 1. Fail fast: if a committed program owns this kernel ID,
//    abort before doing any runtime I/O. This is an invariant
//    violation -- it means either the single-writer lock is
//    broken, or kernel ID reuse occurred while an entry is
//    still considered live.
if m.store.ProgramExists(ctx, id) {
    rollbackKernel()
    return fmt.Errorf(
        "invariant: program with kernel_id=%d already in DB "+
            "(name=%s, source=%s)",
        id, spec.ProgramName(), spec.ObjectPath(),
    )
}

// 2. Wipe any uncommitted crash residue from a previous attempt.
//    Safe because the DB check above confirmed no committed
//    program owns this ID.
if err := safeRemoveAll(m.dirs.Programs(), finalDir); err != nil {
    rollbackKernel()
    return err
}

// 3. Assemble the bundle in a temp dir on the same filesystem.
tmpDir, err := os.MkdirTemp(m.dirs.Programs(), ".tmp-")
if err != nil {
    rollbackKernel()
    return err
}

defer func() {
    if tmpDir != "" {
        _ = os.RemoveAll(tmpDir)
    }
}()

if err := copyFile(spec.ObjectPath(), filepath.Join(tmpDir, "bytecode.o")); err != nil {
    rollbackKernel()
    return err
}

prov := Provenance{
    Version:     1,
    KernelID:    id,
    ProgramName: spec.ProgramName(),
    Source:      spec.ObjectPath(),
    SourceKind:  sourceKindFromSpec(spec), // "file" | "image"
    LoadedAt:    time.Now().UTC(),
}
if err := writeJSON(filepath.Join(tmpDir, "provenance.json"), prov); err != nil {
    rollbackKernel()
    return err // defer cleans up tmpDir
}

// 4. Publish atomically.
if err := os.Rename(tmpDir, finalDir); err != nil {
    rollbackKernel()
    return err // defer cleans up tmpDir
}
tmpDir = "" // prevent deferred cleanup of the published directory

// 5. Persist DB pointing at the published bundle.
```

Then set `metadata.Load.ObjectPath` to the published
`bytecodePath` instead of `spec.ObjectPath()`.

#### Ordering Rationale

The DB check and crash-residue wipe happen before creating the
temp directory. This means no temporary files are written in the
"should never happen" case (DB already has the kernel ID), and the
crash-residue wipe is guarded by the DB check that confirms no
committed program owns the bundle.

There are two distinct "existence checks" and they drive different
behaviour:

1. **DB record exists for kernel ID**: hard error. A committed
   program owns this ID. Never clobber its runtime state.
2. **Published bundle directory exists, but no DB record**: crash
   residue from a previous attempt where the bundle was published
   but the DB persist never completed. Safe to wipe because
   nothing relies on an uncommitted bundle.

Filesystem state alone cannot distinguish "committed" from
"residue". The DB check is the safety latch: it must be consulted
before deleting anything on the filesystem.

#### Temp-Dir Publish

Writing everything into a temp directory under the same parent
(`dirs.Programs()`) and publishing with a single `os.Rename` gives
one atomic commit for the whole bundle. No partial `bytecode.o`, no
partial `provenance.json`, no per-file temp names. The temp
directory must be on the same filesystem as the final directory for
rename to be atomic; using `dirs.Programs()` as the parent
guarantees this.

#### `safeRemoveAll`

`safeRemoveAll(parent, target string) error` is a small helper that
calls `os.RemoveAll(target)` only after verifying that `target` is
a child of `parent`:

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

This prevents a path construction bug from escalating into "rm -rf
somewhere unintended". Used for both crash-residue wipe and
post-publish DB-persist rollback.

#### `provenance.json` Schema

Keep it intentionally boring and stable:

```json
{
  "version": 1,
  "kernel_id": 1234,
  "program_name": "count_context_switches",
  "source": "/path/to/stats-globals.o",
  "source_kind": "file",
  "loaded_at": "2026-02-05T11:38:06Z"
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

The version field allows adding fields later (e.g., image digest)
without breaking existing readers. The file is written once at load
time and never updated. It exists purely as provenance for debugging
and orphan identification. The DB remains the authoritative record.

#### Permissions

Explicitly set file modes rather than relying on umask:

- Directories: `0755`
- `bytecode.o`: `0644`
- `provenance.json`: `0644`

bpfman typically runs as root. Using `0644` rather than `0600`
allows non-root tooling to inspect provenance and bytecode without
requiring privilege escalation.

#### fsync

Skipped. `/run` is tmpfs on most systems, so fsync is a no-op. The
atomic rename provides crash consistency for the publish step. If
durable persistence becomes a requirement (e.g., the base directory
moves off tmpfs), fsync can be added to the files and the parent
directory without changing the API.

Place `copyFile`, `writeJSON`, and `safeRemoveAll` in
`manager/load.go` or a small `internal/fileutil` package.

### 3. `manager/load.go` -- `Manager.Unload()`

Add cleanup of the bytecode directory to the unload action sequence.
Insert after the maps directory removal and before the DB delete:

```go
action.RemovePath{Path: m.dirs.BytecodeDir(kernelID)}
```

Add a new `action.RemovePath` type that calls `os.RemoveAll`.
`os.RemoveAll` already treats non-existence as a nil error, so
ENOENT tolerance comes for free. `RemovePin` is semantically coupled
to bpffs and should not be reused for regular filesystem paths.
`RemovePath` is a few lines of code and makes the intent
unambiguous.

Guard rails: `RemovePath` should reject empty paths and reject `/`
to prevent accidental recursive deletion of the root filesystem.

### Rollback Failure Matrix

The bytecode directory is treated like any other kernel-adjacent
artefact. "Rollback kernel state" means undo everything the load
flow has done so far: unpin the program, remove any map and link
pins, and unload the kernel object.

The temp-dir pattern simplifies rollback: before publish, the
deferred `os.RemoveAll(tmpDir)` handles cleanup automatically.
After publish, the final directory must be removed explicitly.

| Step that fails | Cleanup required |
|-----------------|------------------|
| DB check (`ProgramExists`) | Rollback kernel state (no FS artefacts yet) |
| Wipe crash residue | Rollback kernel state (no FS artefacts yet) |
| Create tmpDir | Rollback kernel state (no FS artefacts yet) |
| Copy ELF to tmpDir | Rollback kernel state (defer removes tmpDir) |
| Write provenance.json to tmpDir | Rollback kernel state (defer removes tmpDir) |
| Rename tmpDir to finalDir | Rollback kernel state (defer removes tmpDir) |
| DB persist | Rollback kernel state + `safeRemoveAll(finalDir)` |
| Unload | Best-effort `safeRemoveAll(finalDir)` |

### 4. `bpffs/scanner.go` -- Orphan Detection (Follow-up)

The scanner currently only scans bpffs. Orphaned
`programs/{id}` directories on the regular filesystem are a
secondary concern. Two options:

- Add a `BytecodeDirs(ctx)` iterator to the scanner (or a separate
  scanner) that reads `{base}/programs/` and yields entries.
- Handle it in the coherency checker directly by listing the
  `programs/` directory and checking each ID against the DB.

This can be a follow-up since the unload path already cleans up the
directory, and GC handles orphaned bpffs pins.

Prefer handling orphan detection in the coherency checker rather
than the bpffs scanner. Conceptually, bpffs scanning is
kernel/fs(BPF) reconciliation; `programs/` scanning is
db/fs(regular) reconciliation. Keeping those distinct matches the
existing design philosophy. The implementation is straightforward:

- List `dirs.Programs()`.
- Parse each directory name as uint32.
- If no DB entry exists for that kernel ID, report an orphan.
- Surface provenance from `provenance.json` in the diagnostic
  output so the operator can identify what the orphaned directory
  belonged to.

### 5. `config/runtime_dirs_test.go`

Add tests for the new `BytecodeDir` and `BytecodePath` methods.

## Files to Modify

| File | Change |
|------|--------|
| `config/runtime_dirs.go` | Add `programs` field, `Programs()`, `BytecodeDir()`, `BytecodePath()`, `ProvenancePath()`, update `EnsureDirectories()` |
| `config/runtime_dirs_test.go` | Test new methods |
| `action/` | Add `RemovePath` action type (`os.RemoveAll`, reject empty/root paths) |
| `manager/load.go` | Assemble bytecode bundle in temp dir under `Programs()`, publish via atomic rename; set ObjectPath to runtime copy; remove published dir on DB persist failure; add bytecode dir cleanup to unload using `action.RemovePath` |

## OCI Cache Garbage Collection

Once bytecode is copied to the runtime directory, the OCI cache at
`~/.cache/bpfman/images/` becomes a pure pull-acceleration layer.
Entries can be evicted without affecting running programs. This makes
periodic cache GC safe and desirable.

### Cache Structure

```
~/.cache/bpfman/images/
  sha256_{hash}/
    bytecode.o
    metadata.json       <- URL, digest, pull timestamp
```

The cache key is `SHA256(URL)` truncated to 16 bytes. Each entry
contains the extracted bytecode and metadata recording the source
URL, digest, and when it was pulled.

### GC Strategy

A configurable cache TTL controls how long entries are retained after
their last use. Entries older than the TTL are eligible for eviction.

Configuration (in the bpfman config file or CLI flag):

```
cache_gc_interval: 24h      # how often to run GC (0 = disabled)
cache_max_age: 7d            # evict entries unused for this long
```

GC runs:

1. List all entries in `~/.cache/bpfman/images/`.
2. For each entry, check `metadata.json` for the last-used timestamp
   (or fall back to filesystem mtime of `bytecode.o`).
3. If the entry is older than `cache_max_age`, remove the directory.
4. Log evicted entries at debug level.

The last-used timestamp should be updated on every successful load
from cache, not only on pull. This prevents frequently used images
from aging out under `IfNotPresent` policy, where entries are
accessed repeatedly but never re-pulled.

Store `last_used_at` inside the cache entry's `metadata.json`
rather than relying on filesystem mtimes (which are unreliable
across filesystem types). On load-from-cache: read metadata, update
`last_used_at`, write back via temp+rename. To reduce write churn,
coalesce updates: only bump `last_used_at` if the current value is
older than some threshold (e.g., 10 minutes). This does not affect
correctness since the cache TTL is measured in days.

GC can be triggered:

- Periodically by the daemon (if `cache_gc_interval > 0`).
- Manually via a CLI command (e.g., `bpfman cache gc`).

Startup GC defaults to off. Startup should be deterministic and
fast; periodic or manual GC is sufficient.

### Safety

Cache GC is safe because:

- Running programs use the runtime copy at
  `/run/bpfman/programs/{id}/bytecode.o`, not the cache.
- Re-pulling after eviction is handled by the existing pull policy
  logic (`Always` re-pulls regardless; `IfNotPresent` pulls on
  miss).
- The cache is advisory, not authoritative.

### Implementation Scope

Cache GC is a follow-up to the core bytecode persistence work. The
runtime copy must land first so that cache eviction does not break
live programs.

## What Does NOT Change

- OCI puller and cache logic (unchanged)
- Pull policy handling (unchanged)
- `ProgramLoadSpec` struct (ObjectPath field already exists)
- `LoadSpec` type (unchanged)
- Store/SQLite layer (unchanged -- it already stores ObjectPath)
- `interpreter/ebpf/load.go` kernel loading (unchanged -- it reads
  from whatever ObjectPath is given)

## Verification

1. `go build ./...`
2. `go test ./...`
3. Load from file:
   `bpfman load file testdata/stats-globals.o tracepoint -n count_context_switches`
   - Verify `ObjectPath` in `get program <id>` points to
     `/run/bpfman/programs/<id>/bytecode.o`.
   - Verify the bytecode file exists and matches the original.
4. Load from image: `bpfman load image <url> -n <prog>`
   - Same verification as above.
5. Unload: `bpfman unload <id>`
   - Verify `/run/bpfman/programs/<id>/` directory is removed.
6. Delete original source file, verify `get program` still works
   (ObjectPath still accessible).
7. Rollback: inject a DB persist failure after bytecode copy.
   - Verify the kernel object is unloaded (no bpffs pins remain).
   - Verify `/run/bpfman/programs/<id>/` is removed.
   - Verify no orphaned map or link pins remain.
   This proves the failure matrix is actually enforced.
