# Dispatcher Rebuild: Priority-Based Slot Assignment

## Problem

go-bpfman assigns dispatcher extension slots sequentially by
insertion order. When multiple programs are attached to the same
interface, each new program is placed in the next available slot
regardless of its priority. Rust bpfman, by contrast, rebuilds the
entire dispatcher on every attach, sorting all programs by priority
and re-attaching them to slots in priority order.

This means the slot positions reported by go-bpfman do not reflect
priority ordering, and the actual execution order within the
dispatcher does not respect priority either. The bpfman-operator
integration test `verifyLinkOrder` (added by operator PR #485)
checks that links sorted by position have non-decreasing priorities,
which fails against go-bpfman.

### Example

Programs attached with priorities [1000, 0, 500, 1000]:

| | go-bpfman (current) | Rust bpfman |
|---|---|---|
| Slot 0 | priority 1000 | priority 0 |
| Slot 1 | priority 0 | priority 500 |
| Slot 2 | priority 500 | priority 1000 |
| Slot 3 | priority 1000 | priority 1000 |

## Rust bpfman Reference

The Rust attach flow for XDP/TC dispatcher-backed programs
(`bpfman/src/lib.rs`):

1. **Sort** -- `add_and_set_link_positions` gathers all existing
   extensions for the dispatcher (same interface, direction, nsid),
   adds the new link, sorts by `(priority, attached, program_name)`,
   and assigns positions 0, 1, 2, ...

2. **Bump revision** -- `next_revision = old_dispatcher.next_revision()`

3. **Rebuild** -- `Dispatcher::new` creates a fresh dispatcher
   program with updated `.rodata` config (e.g. `num_progs_enabled`,
   `chain_call_actions`), then calls `attach_extensions` which:
   - Sorts extensions by `current_position`
   - For each extension, loads the user program from its existing
     kernel pin (`Extension::from_pin`)
   - Attaches it to the new dispatcher's slot function via freplace

4. **Swap** -- For XDP: `bpf_link_update` atomically replaces the
   dispatcher program in the stable XDP link. For TC: a new TC
   filter is added and the old one removed.

5. **Cleanup** -- Old dispatcher revision's pins and links are
   removed.

The same rebuild happens on **detach**: `set_program_positions`
re-sorts remaining programs and `Dispatcher::new` rebuilds without
the removed program.

## go-bpfman Current State

### What exists

- Revision tracking in the DB schema and pin path structure
  (`dispatcher_{nsid}_{ifindex}_{revision}/`)
- A stable XDP link pin outside the revision directory
  (`dispatcher_{nsid}_{ifindex}_link`)
- Stale-dispatcher recovery in `attachExtensionWithRetry` that
  deletes and recreates the dispatcher when the pin is missing
  (this is effectively a rebuild, but only triggered by error)

### What is missing

- No priority-based sorting when assigning slots
- No dispatcher rebuild on normal attach (only on error recovery)
- No revision bump during the attach flow
- No re-attachment of existing extensions to a new dispatcher
- No dispatcher rebuild on detach
- Position is `store.CountDispatcherLinks(ctx, ds.ProgramID)` --
  the next sequential slot number

## Design

### Approach: Full Dispatcher Rebuild

Match Rust bpfman's behaviour: rebuild the dispatcher on every
attach and detach. This is the only approach that ensures both the
reported positions and the actual execution order reflect priority
ordering.

### Attach Flow

When attaching a new extension to a dispatcher:

1. **Gather** -- Query all existing links for this dispatcher
   (by `dispatcher_id` from the store). Include the new program.

2. **Sort** -- Sort by `(priority, program_name)`. Assign positions
   0, 1, 2, ... based on the sorted order. Tie-breaking by program
   name matches Rust bpfman.

3. **Load new dispatcher** -- Load a fresh dispatcher program from
   embedded bytecode with updated `.rodata` config:
   - `num_progs_enabled` = total number of extensions
   - `chain_call_actions[i]` = proceed-on mask for program at
     position i
   - `run_prios[i]` = `dispatcher.DefaultPriority` (internal
     dispatcher slot priority, not the user-facing priority)

4. **Pin new dispatcher** -- Pin at the new revision path:
   `dispatcher_{nsid}_{ifindex}_{revision+1}/dispatcher`

5. **Re-attach all extensions** -- For each program in sorted order:
   - Load the user program from its existing prog pin
     (`ebpf.LoadPinnedProgram`)
   - Load the ELF collection spec from the stored `ObjectPath`
   - Set `progSpec.Type = Extension`, `AttachTarget = newDispatcher`,
     `AttachTo = slotName(position)`
   - Load the extension with map replacements from pinned maps
   - Attach via `link.AttachFreplace`
   - Pin the extension link at the new revision path

6. **Swap** --
   - XDP: Load the stable link from pin, call `RawLink.Update` to
     point it at the new dispatcher program. This is an atomic
     `BPF_LINK_UPDATE`.
   - TC: Add a new TC filter pointing to the new dispatcher, then
     remove the old filter.

7. **Cleanup** -- Remove old revision directory (extension link
   pins, old dispatcher prog pin).

8. **Update store** -- Update the dispatcher record with new
   revision, program ID, link ID. Update link records with new
   positions.

### Detach Flow

When detaching a program:

1. Remove the extension link pin for the detached program.
2. Delete the link record from the store.
3. If extensions remain, rebuild the dispatcher (steps 1-8 above,
   minus the detached program).
4. If no extensions remain, delete the dispatcher entirely (current
   `CleanupEmptyDispatcher` logic).

### cilium/ebpf Primitives

All required operations are supported:

| Operation | API | Status |
|---|---|---|
| Load program from pin | `ebpf.LoadPinnedProgram(path, nil)` | Used today |
| Load ELF as Extension | `progSpec.Type = ebpf.Extension` | Used today |
| Attach freplace | `link.AttachFreplace(target, fn, ext)` | Used today |
| Atomic XDP link update | `RawLink.Update(newProg)` | Available, not yet used |
| Load pinned maps | `ebpf.LoadPinnedMap(path, nil)` | Used today |

Note: freplace links do not support `Update()` (returns
`ErrNotSupported`). Each extension must be detached from the old
dispatcher and re-attached to the new one. This matches Rust
bpfman's approach.

### Loading Extensions from Pin vs ELF

The current `AttachXDPExtension` implementation reloads from the
original ELF file (`ObjectPath`) every time. This is necessary
because the kernel's freplace mechanism requires the extension
program to be loaded with `AttachTarget` set to the specific
dispatcher it will attach to. You cannot load an extension targeting
dispatcher A and then re-target it to dispatcher B.

For the rebuild, each extension must be loaded fresh from the ELF
with the new dispatcher as its `AttachTarget`. The existing prog
pin (from the initial `Load`) is not usable for this -- it was
loaded as the original program type (XDP/TC), not as Extension.

The `ObjectPath` is already stored in the program record's
`LoadSpec`, so this is straightforward.

### Position in Link Details

Currently, position is stored in the link details (e.g.
`bpfman.TCDetails.Position`) at attach time and never updated.
After a rebuild, positions may change. The store must be updated:

- Option A: Update link detail records in place after rebuild
- Option B: Delete and recreate link records with new positions

Option A is simpler and avoids changing link IDs.

### Where This Fits in the Architecture

The rebuild is a composite operation, similar to the existing
`attachExtensionWithRetry` but more involved. It belongs in
`executor_dispatcher.go` as a new method on `executor`, since it
orchestrates multiple kernel and store operations with rollback.

The plan layer in `attach_dispatcher.go` would call a new deep
action (e.g. `RebuildDispatcher`) that encapsulates the entire
rebuild sequence. This keeps the plan simple while the executor
handles the multi-step transaction.

## Scope

### In Scope

- Priority-based position assignment on attach
- Full dispatcher rebuild (new revision, re-attach all extensions)
- Atomic swap (XDP link update, TC filter replacement)
- Dispatcher rebuild on detach (re-sort remaining programs)
- Store updates for changed positions

### Out of Scope

- Proceed-on per-program configuration (currently hardcoded)
- XDP flags per-program (frags support)
- CLI `--proceed-on` flag
- Dispatcher config changes without attach/detach (pure reconfig)

## Risks

1. **Transient packet loss during rebuild** -- During the window
   between creating the new dispatcher and completing the atomic
   swap, the old dispatcher is still active. The swap itself is
   atomic. For XDP, `bpf_link_update` is a single syscall. For TC,
   there is a brief window between adding the new filter and
   removing the old one where both filters exist (packets may be
   processed twice).

2. **Failure mid-rebuild** -- If extension re-attachment fails
   partway through, the old dispatcher is still active and
   functional. Rollback: clean up the partial new revision and
   leave the old one in place.

3. **Performance on high-churn interfaces** -- Each attach/detach
   triggers a full rebuild with N re-attachments. For the typical
   case (1-3 programs per interface), this is negligible. At the
   maximum (10 programs), it is still fast since each re-attachment
   is a single `BPF_PROG_LOAD` + `BPF_LINK_CREATE`.
