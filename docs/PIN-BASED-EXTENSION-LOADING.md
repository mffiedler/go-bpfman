# Pin-based extension loading for dispatcher rebuilds

## Problem

Every dispatcher rebuild (triggered by each attach or detach)
reloads extension programs from the original ELF file on disk. This
is wasteful and fragile:

- **Redundant work**: The same ELF is parsed, verified, and loaded
  into the kernel on every rebuild. For a dispatcher with 10
  extensions, detaching one program triggers 9 ELF re-reads.

- **Fragile path dependency**: The rebuild depends on `ObjectPath`,
  the filesystem path to the bytecode. Although go-bpfman copies the
  bytecode to a managed location during Load, the dependency on a
  file path for kernel operations is unnecessary — the program is
  already loaded and pinned in bpffs.

- **Unnecessary map rebinding**: Each ELF reload requires
  `MapReplacements` to rebind the extension's maps to the original
  program's pinned maps. This is complex plumbing that exists only
  because we reload from ELF rather than reusing the existing kernel
  program.

## How Rust bpfman handles this

The Rust bpfman (canonical implementation) uses pin-based extension
loading. Programs are loaded as `BPF_PROG_TYPE_EXT` once, pinned,
and reused from the pin on every rebuild.

### Load phase (`lib.rs`)

When a user loads an XDP or TC program, the Rust bpfman does not
load it as its native kernel type (BPF_PROG_TYPE_XDP or
BPF_PROG_TYPE_SCHED_CLS). Instead:

1. A **test dispatcher** is obtained via `XdpDispatcher::get_test()`
   or `TcDispatcher::get_test()`. This is a minimal dispatcher
   loaded from the standard bytecode, used purely as a verification
   target.

2. The program is loaded as `BPF_PROG_TYPE_EXT` targeting the test
   dispatcher's `compat_test` function:
   ```rust
   let ext: &mut Extension = raw_program.try_into()?;
   let dispatcher = XdpDispatcher::get_test()?;
   ext.load(dispatcher.fd()?, "compat_test")?;
   ```

3. The Extension is pinned at a deterministic path:
   ```rust
   ext.pin(format!("{RTDIR_FS}/prog_{id}"))?;
   ```

The program's kernel type is Extension, but its **stored type** in
the database remains XDP or TC. The user-facing type is the stored
type.

### Rebuild phase (`xdp.rs`, `tc.rs`)

When a dispatcher is rebuilt (on every attach or detach):

```rust
for (i, v) in extensions.iter_mut().enumerate() {
    let id = v.0.get_program_id()?;
    let path = format!("{RTDIR_FS}/prog_{id}");

    // Load the SAME pinned program — no ELF, no bytecode
    let mut ext = Extension::from_pin(&path)?;

    // Create a NEW freplace link to the NEW dispatcher
    let target_fn = format!("prog{i}");
    let new_link_id = ext.attach_to_program(
        dispatcher.fd()?, &target_fn
    )?;

    // Pin the new link at the revision-specific path
    let new_link: FdLink = ext.take_link(new_link_id)?.into();
    new_link.pin(revision_link_path)?;
}
```

Key observations:

- Only the **link** is new on each rebuild. The **program** is
  reused from its pin.
- No ELF file is read. No `MapReplacements`. No
  `LoadCollectionSpec`.
- Maps are already bound from the initial load.

## How go-bpfman currently handles this

### Load phase (`platform/ebpf/load.go`)

Programs are loaded as their native kernel type:

```go
collSpec, err := ebpf.LoadCollectionSpec(spec.ObjectPath())
// ... program is loaded as BPF_PROG_TYPE_XDP or BPF_PROG_TYPE_SCHED_CLS
coll, err := ebpf.NewCollectionWithOptions(collSpec, opts)
prog := coll.Programs[spec.ProgramName()]
prog.Pin(bpffs.ProgPinPath(programID))
```

The pinned program is type XDP or TC, not Extension.

### Rebuild phase (`platform/ebpf/attach_xdp.go`)

Every rebuild re-reads the ELF and creates a fresh Extension:

```go
// Load dispatcher from pin (fine)
dispatcherProg, _ := ebpf.LoadPinnedProgram(spec.DispatcherPinPath, nil)

// Re-read and re-parse the ELF file
collSpec, _ := ebpf.LoadCollectionSpec(spec.ObjectPath)

// Change the program type to Extension
progSpec.Type = ebpf.Extension
progSpec.AttachTarget = dispatcherProg
progSpec.AttachTo = dispatcher.SlotName(spec.Position)

// Rebind maps from the original program's pin directory
for name := range collSpec.Maps {
    mapPath := filepath.Join(spec.MapPinDir, name)
    m, _ := ebpf.LoadPinnedMap(mapPath, nil)
    mapReplacements[name] = m
}

// Load a NEW kernel program from ELF
coll, _ := ebpf.NewCollectionWithOptions(collSpec, ebpf.CollectionOptions{
    MapReplacements: mapReplacements,
})

// Attach via freplace
extensionProg := coll.Programs[spec.ProgramName]
lnk, _ := link.AttachFreplace(dispatcherProg, progSpec.AttachTo, extensionProg)
```

This means every rebuild for every extension involves:
- One ELF file read
- One ELF parse
- One map enumeration and pin load per map
- One full program verification and load into the kernel
- One freplace link creation

The pin-based approach reduces this to:
- One pinned program load (a single `bpf_obj_get` syscall)
- One freplace link creation

## What needs to change

### 1. Test dispatcher infrastructure

Add lazy-loaded test dispatchers to the kernel adapter. These are
minimal dispatchers loaded from the standard bytecode with one slot
enabled, used purely as verification targets during Load.

The test dispatcher must stay alive as long as any Extension loaded
against it exists. It is managed by the kernel adapter and cleaned
up in `Close()`.

### 2. Load XDP/TC programs as Extension

During `bpfman load`, when the program type is XDP or TC, load it
as `BPF_PROG_TYPE_EXT` targeting the test dispatcher's `prog0`
slot. Pin the Extension at `ProgPinPath(programID)`.

The program's kernel type becomes Extension, but the stored type in
the database remains XDP or TC. `bpfman list` shows the stored
type. `bpftool prog list` shows Extension. This matches the Rust
bpfman's behaviour.

Maps are loaded and pinned identically to today.

### 3. Simplify extension attach

Replace the ELF-based extension attach with pin-based loading:

```go
// Load extension from pin (replaces ELF loading + map rebinding)
extensionProg, _ := ebpf.LoadPinnedProgram(spec.ProgPinPath, nil)

// Attach via freplace (unchanged)
lnk, _ := link.AttachFreplace(dispatcherProg, slotName, extensionProg)
```

Remove `ObjectPath` and `MapPinDir` from the extension attach
specs. Replace with `ProgPinPath`.

### 4. Update store queries

The `ListDispatcherSlots` SQL query currently selects
`managed_programs.object_path` and `managed_programs.map_pin_path`
for rebuild. Change to select `managed_programs.pin_path` instead.

Remove `ObjectPath` and `MapPinDir` from the `DispatcherSlot`
struct. Replace with `ProgPinPath`.

## Files to modify

- `platform/ebpf/load.go` — load XDP/TC as Extension targeting
  test dispatcher
- `platform/ebpf/attach_xdp.go` — pin-based extension loading
- `platform/ebpf/attach_tc.go` — pin-based extension loading
- `dispatcher/specs.go` — `ProgPinPath` replaces `ObjectPath` +
  `MapPinDir` in extension specs
- `platform/interfaces.go` — `DispatcherSlot` field changes
- `platform/store/sqlite/dispatchers.go` — SQL query changes
- `manager/executor_dispatcher.go` — `rebuildSlot` field changes
- `manager/attach_dispatcher.go` — spec construction
- Unit test fakes and stubs

## Kernel compatibility

The pin-based approach requires that the kernel allows attaching an
Extension (loaded against a test dispatcher) to a different
dispatcher via `bpf_link_create`. Both dispatchers are loaded from
the same bytecode, so the slot function signatures are identical.

The Rust bpfman demonstrates this works on modern kernels. The
target kernel is 6.12 which supports it.

## Verification

```bash
make test           # unit tests pass
sudo make test-e2e  # e2e tests pass (traffic, counts, positions)
```

E2e tests assert on observable behaviour (traffic flow, extension
counts, link positions), not on the loading mechanism. They should
pass without modification.
