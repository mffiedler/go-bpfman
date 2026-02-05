# Live Global Data in Program Status

This document describes a design for reading BPF global variable values
from the live kernel at display time, rather than showing only the
stale load-time snapshot from the store.

## Problem

Global data today lives only in `ProgramSpec.Load.GlobalData` -- the hex
bytes the user passed at load time via `-g NAME=HEX`. This is spec
(intent), not status (observation). Once persisted, these values are never
updated.

The `get program` output shows "what the user asked for", not "what the
kernel has now". For `.rodata` variables (frozen after load) the two are
always identical. For `.data` variables they can diverge if something
modifies the map after loading.

There is no way to inspect the current kernel-side value of a global
variable through bpfman today.

## Background: BPF Global Variables

BPF programs can declare global variables that the loader injects before
the program enters the kernel. The ELF section a variable lands in
determines its mutability:

| Declaration | ELF Section | Kernel Map | Mutable After Load? |
|-------------|-------------|------------|---------------------|
| `volatile const __u64 X = 0` | `.rodata` | Array, frozen | No |
| `__u64 X = 0` | `.data` | Array, writable | Yes |
| `__u64 X` (uninitialised) | `.bss` | Array, writable | Yes |

`volatile const` is the common pattern for load-time configuration. The
`const` places the variable in `.rodata` (allowing the kernel verifier to
perform dead-code elimination). The `volatile` prevents the compiler from
constant-folding the default value before the loader can override it.

### Loader Flow

```
CLI: -g VERBOSE=0x01 -g LOG_INTERVAL_NS=0x005ed0b200000000
  |
  v
ParseGlobalData("NAME=HEX") -> map[string][]byte
  |
  v
LoadSpec.WithGlobalData(data)
  |
  v
collSpec.Variables["VERBOSE"].Set([]byte{0x01})   // modifies MapSpec.Contents
collSpec.Variables["LOG_INTERVAL_NS"].Set(...)
  |
  v
ebpf.NewCollectionWithOptions(collSpec, opts)      // loads into kernel
```

The `Set()` call operates on `VariableSpec`, which writes into the
underlying `MapSpec.Contents` byte slice before the map is created in the
kernel.

### Map Pinning

Data section maps (`.rodata`, `.data`, `.bss`) are **not pinned** to the
BPF filesystem. In `interpreter/ebpf/load.go`, maps whose names start with
`.` are skipped during pinning. However, the maps remain accessible via
their kernel IDs, which are reported in the program's `MapIDs` list.

## Spec vs Status

Following the conventions in [SPEC-STATUS-GUIDELINES.md](SPEC-STATUS-GUIDELINES.md):

- **Spec** (`ProgramSpec.Load.GlobalData`): what the user requested at load
  time. Persisted in the store. Audit/provenance data. Never changes.

- **Status** (`ProgramStatus.LiveGlobalData`): what the kernel has now.
  Ephemeral, derived from observation. May differ from spec for `.data`
  variables.

Display should show both. JSON output includes `spec.load.global_data`
(requested) and `status.live_global_data` (observed). The table format
prefers live values when available, falling back to stored values if the
live read fails.

## How cilium/ebpf Exposes Variables

The library provides two levels of variable access:

### VariableSpec (pre-load)

Defined in `variable.go`. Operates on `MapSpec.Contents` -- a byte slice
representing the map's initial data. Used during load to inject values.

```go
type VariableSpec struct {
    name   string
    offset uint64
    size   uint64
    m      *MapSpec
    t      *btf.Var
}
```

`Set(in any)` marshals a value into `MapSpec.Contents` at the variable's
offset. `Get(out any)` unmarshals from the same location.

### Variable (post-load)

Also in `variable.go`. Operates on mmap'd kernel memory via a `Memory`
object. Reads and writes go directly to the kernel map's backing pages.

```go
type Variable struct {
    name   string
    offset uint64
    size   uint64
    t      *btf.Var
    mm     *Memory
}
```

`Get(out any)` reads live values from `mm.b[offset:offset+size]`.
`Set(in any)` writes to the same region (fails for read-only maps).

Memory-mapped access requires Linux 5.5+ (`BPF_F_MMAPABLE`). On older
kernels, `mm` is nil and `Get()`/`Set()` return `ErrNotSupported`.

### MapReplacements

`CollectionOptions.MapReplacements` allows constructing a `Collection`
that reuses already-loaded kernel maps instead of creating new ones:

```go
type CollectionOptions struct {
    Maps            MapOptions
    Programs        ProgramOptions
    MapReplacements map[string]*Map
}
```

When a replacement is provided, `loadMap()` clones the existing `*Map`
and checks compatibility (type, key/value size, max entries, flags). The
resulting `Collection.Variables` are backed by `Memory` objects pointing
at the live kernel maps.

### BTF Variable Metadata

Each global variable has a `btf.Var` type with a `Linkage` field:

| Linkage | Value | Meaning |
|---------|-------|---------|
| `StaticVar` | 0 | Compiler-generated (format strings, etc.) |
| `GlobalVar` | 1 | User-declared global variable |
| `ExternVar` | 2 | External reference |

Only `GlobalVar` entries represent user-configured globals. `StaticVar`
entries (like `bpf_printk` format strings) should be excluded from
display.

## Approach: Reconstruct Collection via MapReplacements

Rather than manually walking BTF Datasec entries and computing offsets:

1. **Reload the ELF**: `LoadCollectionSpec(objectPath)` to recover
   `VariableSpec` metadata (names, offsets, sizes, BTF types).

2. **Get the program's kernel maps**: open the program by ID, call
   `Info().MapIDs()` to get the list of associated map IDs.

3. **Open each map by ID**: `NewMapFromID(id)` for each ID. Read the
   map's `Info()` to get its name.

4. **Match names to CollectionSpec**: find which kernel maps correspond
   to `.rodata`, `.data`, etc. in the CollectionSpec. Build a
   `MapReplacements` map.

5. **Strip programs from the spec**: clear `collSpec.Programs` to prevent
   the library from attempting to re-load BPF programs into the kernel.

6. **Reconstruct the Collection**: `NewCollectionWithOptions(collSpec,
   {MapReplacements: ...})`. The library wires up `Variable` objects
   with `Memory` pointing at the live kernel maps.

7. **Read variables**: iterate `coll.Variables`, filter to `GlobalVar`
   linkage, call `Get()` to read live values.

This delegates all offset arithmetic, endianness handling, mmap
management, and BTF interpretation to cilium/ebpf.

**Requirement**: the original ELF object file must be accessible on disk.
We store this as `ProgramSpec.Load.ObjectPath`. If the file has been
deleted, fall back to stored values.

## Implementation Sketch

### New interface

```go
// interpreter/interfaces.go
type GlobalVariableReader interface {
    ReadGlobalVariables(ctx context.Context, programID uint32, objectPath string) (map[string][]byte, error)
}
```

Embedded in `KernelOperations`.

### Implementation

New file `interpreter/ebpf/globals.go` implementing
`ReadGlobalVariables` on `kernelAdapter`, following the steps above.

### Domain model

Add to `ProgramStatus` in `program.go`:

```go
LiveGlobalData map[string][]byte `json:"live_global_data,omitempty"`
```

### Manager

In `Manager.Get()` (`manager/list.go`), after fetching kernel maps:

```go
if metadata.Load.ObjectPath != "" {
    liveGlobals, err := m.kernel.ReadGlobalVariables(ctx, kernelID, metadata.Load.ObjectPath)
    if err != nil {
        m.logger.WarnContext(ctx, "failed to read live globals", "error", err)
    }
    // ... populate status
}
```

Non-fatal: if reading fails (ELF deleted, kernel too old, no BTF),
fall back silently to stored values.

### Display

In `cmd/bpfman/cli/format.go`, prefer live values:

```go
globalData := prog.Status.LiveGlobalData
if globalData == nil {
    globalData = prog.Spec.Load.GlobalData
}
```

### Files to modify

| File | Change |
|------|--------|
| `interpreter/interfaces.go` | Add `GlobalVariableReader`, embed in `KernelOperations` |
| `interpreter/ebpf/globals.go` | New file: `ReadGlobalVariables` implementation |
| `program.go` | Add `LiveGlobalData` to `ProgramStatus` |
| `manager/list.go` | Call `ReadGlobalVariables` in `Get()` |
| `cmd/bpfman/cli/format.go` | Prefer live values in display |
| Test fakes | Stub the new interface method |

## Scope Decisions

- **Only `get program`**: not `list programs` (avoids N extra syscall
  sets per program in the list).
- **Non-fatal**: if reading fails, fall back silently to stored values.
- **Requires ELF on disk**: if object file deleted, uses stored values.
- **Same format**: `map[string][]byte` matches existing `GlobalData`.
- **Filter by linkage**: only `GlobalVar`, not `StaticVar` (excludes
  compiler-generated variables like format strings).
- **`.bss` excluded**: runtime state (counters, timestamps), not
  user-configured globals. Could be reconsidered later.

## Open Questions

1. **Program stripping**: does clearing `collSpec.Programs` before
   `NewCollectionWithOptions` work cleanly, or does the library still
   try to resolve program references? Needs verification.

2. **Map name matching**: kernel map names for data sections are
   mangled (e.g. `.rodata` may become `prog_name.rodata`). Need to
   confirm the matching strategy handles this.

3. **Future: mutable globals**: should `update program` support
   modifying `.data` globals on a running program? This document only
   covers reading; writing is a separate design decision.
