# Plan: Revert to full-rebuild dispatcher approach

## Context

Feedback from Toke Hoiland-Jorgensen (xdp-tools/libxdp author) confirmed
that the full-rebuild approach used by Rust bpfman is intentional. The
v3 POC (double-buffered BPF maps) trades away dead code elimination,
the single-program fast path, and code path simplicity, and
reimplements atomicity that the kernel already provides. This plan
switches go-bpfman back to the .rodata-based dispatchers (XDP v2, TC
v1) and implements the Rust-style full rebuild on every attach/detach.

## Key mechanism: atomic swap

- **XDP**: The dispatcher link is pinned at a stable path
  (`dispatcher_{nsid}_{ifindex}_link`). On rebuild, the new dispatcher
  program is loaded and the pinned link is updated in-place via
  `RawLink.Update(newProg)` -- this is `BPF_LINK_UPDATE`, an atomic
  kernel operation. cilium/ebpf already exposes this API.

- **TC**: A new netlink filter is added with the new dispatcher
  program, then the old filter is removed. There is a brief window
  where both filters exist. (This is what Rust bpfman does.)

## Changes

### 1. Switch embedded bytecode back to .rodata dispatchers

**Files**: `dispatcher/dispatcher.go`

- Change the `//go:embed` directives used by the active load functions:
  - `LoadXDPDispatcherV3` -> remove or rename; use `LoadXDPDispatcher`
    (already exists, loads `xdp_dispatcher_v2.bpf.o` with `.rodata`
    injection)
  - `LoadTCDispatcherV2` -> remove or rename; use `LoadTCDispatcher`
    (already exists, loads `tc_dispatcher.bpf.o` with `.rodata`
    injection)
- Remove `RuntimeConfig`, `ConfigMapName`, `ActiveMapName` -- no longer
  needed.
- Keep `XDPConfig`, `TCConfig`, `NewXDPConfig`, `NewTCConfig` -- these
  configure the `.rodata` section.
- Keep `SlotName(position)` -- still used for freplace targets.
- The `.rodata` config must now include the actual per-slot
  `chain_call_actions` and `num_progs_enabled` for the current set of
  extensions (not just `MaxPrograms` as today).

### 2. Remove double-buffer map infrastructure

**Files**: `platform/ebpf/dispatcher_config.go`, `platform/ebpf/attach_xdp.go`, `platform/ebpf/attach_tc.go`

- Delete `UpdateDispatcherConfig` method entirely.
- In `AttachXDPDispatcher` and `AttachTCDispatcher`: remove the
  `dispatcher_config` and `active_config` map initialisation and
  pinning. The dispatcher is loaded with its config baked into
  `.rodata`; no maps to pin.
- Remove `UpdateDispatcherConfig` from the `DispatcherAttacher`
  interface in `platform/interfaces.go`.

**Files**: `dispatcher/specs.go`

- Remove `ConfigMapPinPath` and `ActiveMapPinPath` from
  `XDPDispatcherAttachSpec` and `TCDispatcherAttachSpec`.

**Files**: `fs/bpffs_layout.go`

- Remove `DispatcherConfigMapPath` and `DispatcherActiveMapPath`
  methods.

### 3. Add `UpdateXDPLink` to kernel adapter

**Files**: `platform/interfaces.go`, `platform/ebpf/attach_xdp.go`

- Add a new method to `DispatcherAttacher`:
  ```go
  UpdateXDPLink(ctx context.Context, linkPinPath string, newProg *ebpf.Program) error
  ```
  or more likely, since we don't pass raw `*ebpf.Program` across the
  boundary, something like:
  ```go
  UpdateXDPDispatcherLink(ctx context.Context, linkPinPath string, newProgPinPath string) error
  ```
  This loads the pinned link, loads the pinned new dispatcher program,
  and calls `RawLink.Update(newProg)`.

### 4. Extend dispatcher attach specs to carry per-slot config

**Files**: `dispatcher/dispatcher.go`, `dispatcher/specs.go`

The `.rodata` config needs to know `num_progs_enabled` and the
`chain_call_actions` for each slot at dispatcher load time. Currently
`XDPDispatcherAttachSpec` has `NumProgs` and a single `ProceedOn`.

Change: the attach spec (or the `XDPConfig`/`TCConfig` passed to the
load function) must accept per-slot `chain_call_actions` and actual
`num_progs_enabled` for the current extension set.

### 5. Implement full rebuild on attach

**Files**: `manager/executor_dispatcher.go`, `manager/executor.go`

Replace `attachExtensionWithRetry` / `tryAttachExtension` with a
rebuild flow:

1. **Gather** -- List all existing extension links for this dispatcher
   from the store (via `ListDispatcherSlots` or similar). Include the
   new program.

2. **Sort** -- Sort by `(priority, program_name)`. Assign positions
   0, 1, 2, ... in sorted order.

3. **Load new dispatcher** -- Call `LoadXDPDispatcher` (or
   `LoadTCDispatcher`) with `.rodata` config reflecting the full
   extension set: `num_progs_enabled`, per-slot `chain_call_actions`.
   Pin the new dispatcher program at the new revision path.

4. **Re-attach all extensions** -- For each program in sorted order:
   - Build an `XDPExtensionAttachSpec` / `TCExtensionAttachSpec`
     targeting the new dispatcher's pin path
   - The extension is loaded from its original ELF (`ObjectPath` from
     the program record in the store) as `BPF_PROG_TYPE_EXT`
   - Attach via `link.AttachFreplace` to the new dispatcher's slot
   - Pin the extension link at the new revision's link path

5. **Atomic swap** --
   - XDP: `UpdateXDPDispatcherLink(linkPinPath, newProgPinPath)` --
     loads pinned link, calls `RawLink.Update(newProg)`
   - TC: Add new netlink filter with new dispatcher, then remove old
     filter

6. **Cleanup old revision** -- Remove old revision directory (old
   dispatcher prog pin, old extension link pins).

7. **Update store** -- Increment revision, update dispatcher record
   with new program ID. Update link detail records with new positions
   if they changed.

The `extensionOps` struct and `computeRuntimeConfig` / `findFreeSlot`
/ `sortSlotsByPriority` are replaced by the rebuild logic.

### 6. Implement full rebuild on detach

**Files**: `manager/executor.go`, `manager/detach.go`

`cleanupEmptyDispatcher` currently recomputes the map config when
extensions remain. Change:

- If extensions remain after detach: trigger a full rebuild with the
  remaining programs (same steps 1-7 as attach, minus the detached
  program).
- If no extensions remain: full cleanup as today (remove pins, delete
  store record). This path is largely unchanged.

Rename `CleanupEmptyDispatcher` action to something like
`RebuildOrCleanupDispatcher` since it now does more than cleanup.

### 7. Store the ObjectPath for re-attachment

The rebuild needs to reload each extension from its original ELF file.
The `ObjectPath` is already stored in the program record's `LoadSpec`
in the store. Verify that this path is accessible and the file still
exists at rebuild time. (This matches Rust bpfman's approach.)

### 8. Revision management

**Files**: `manager/executor_dispatcher.go`, `platform/store/sqlite/dispatchers.go`

- `IncrementRevision` already exists in the store interface and
  implementation but is currently unused. Start calling it on every
  rebuild.
- The new revision determines the new revision directory path for
  dispatcher and extension link pins.

### 9. Update coherency/GC

**Files**: `manager/coherency/gather.go`, `manager/coherency/rules.go`

- Remove any logic that checks for config/active map orphans (those
  maps no longer exist).
- Stale revision directories may need cleanup rules (e.g., old
  revision dir left behind after a crash mid-rebuild).

### 10. Update detach cleanup actions

**Files**: `manager/detach.go`

- Remove config map and active map paths from
  `computeDispatcherCleanupActions`.

### 11. Update tests

**Files**: `manager/executor_dispatcher_test.go`, `e2e/dispatcher_*.go`

- Unit tests for `computeRuntimeConfig`, `findFreeSlot` -- these are
  replaced by the new rebuild logic and need corresponding tests.

#### E2E tests: `e2e/dispatcher_config_test.go`

Most tests in this file assert on double-buffer internals (reading
`RuntimeConfig` from BPF maps, checking the active buffer index).
The underlying behaviours being tested are valid -- priority
ordering, slot reuse, config recomputation on detach -- but the
assertions should verify observable behaviour (traffic counters,
link records with correct positions, store state) rather than
implementation details (map contents, buffer index).

**Delete** (purely tests double-buffer mechanics):

- `TestDispatcher_DoubleBufferFlip` -- tests that the active buffer
  index alternates 0/1. No behavioural equivalent.

**Rewrite to assert on behaviour** (the scenarios are valid, the
assertions are implementation-specific):

- `TestDispatcher_PriorityOrdering` -- verify that programs execute
  in priority order by observing traffic or querying link positions
  from the store, not by reading `RuntimeConfig.RunOrder`.
- `TestDispatcher_SlotReusedAfterDetach` -- verify that detaching
  and re-attaching succeeds and the program executes, not by
  inspecting `RunOrder` contents.
- `TestDispatcher_ConfigRecomputedOnDetach` -- verify that
  remaining programs still execute correctly after detach, not by
  reading `RuntimeConfig` from the map.
- `TestDispatcher_MultipleInterfacesIndependent` -- verify that
  detaching from one interface does not affect traffic on another.
- `TestDispatcher_LifecycleAfterLastDetach` -- the lifecycle
  assertions (dispatcher exists/absent from store, program ID
  changes on re-create) are valid. Remove the `readConfig` calls
  and config/active map pin existence checks.

**Keep as-is**:

- `TestDispatcher_AttachExceedsMaxPrograms` -- tests the 10-slot
  limit. Still valid under full-rebuild.

**Helpers to delete**:

- `readActiveIndex`, `readDispatcherConfig` -- load pinned BPF maps.
- `dispatcherTestHarness.readConfig`, `readActiveIdx`,
  `configMapPin`, `activeMapPin` methods.

#### E2E tests: `e2e/dispatcher_xdp_test.go`, `e2e/dispatcher_tc_test.go`

Many tests in these files read config/active maps as secondary
assertions alongside traffic-based tests. The traffic assertions
(send packet, verify it flows through the chain) survive unchanged.
The map-reading assertions need removing. The tests themselves remain
valid -- they just lose the double-buffer verification layer.

## Files to modify (summary)

Must change:
- `dispatcher/dispatcher.go` -- remove v3/v2 load functions, RuntimeConfig
- `dispatcher/specs.go` -- remove map pin paths from attach specs
- `platform/interfaces.go` -- remove UpdateDispatcherConfig, add UpdateXDPDispatcherLink
- `platform/ebpf/attach_xdp.go` -- remove map init/pin, add link update
- `platform/ebpf/attach_tc.go` -- remove map init/pin
- `platform/ebpf/dispatcher_config.go` -- delete entirely
- `manager/executor_dispatcher.go` -- replace extension attach with rebuild
- `manager/executor.go` -- update cleanupEmptyDispatcher, action dispatch
- `manager/detach.go` -- remove map paths from cleanup, add rebuild-on-detach
- `manager/action/action.go` -- update action types
- `fs/bpffs_layout.go` -- remove config/active map path helpers
- `manager/coherency/gather.go` -- remove map orphan logic

May change:
- `manager/coherency/rules.go` -- stale revision cleanup
- `dispatcher/state.go` -- if State needs new fields
- `manager/executor_dispatcher_test.go` -- new rebuild tests
- `e2e/dispatcher_*.go` -- if internal assertions need updating

## Verification

1. `make test` -- unit tests pass
2. `make test-e2e` -- dispatcher e2e tests pass (these test actual
   traffic flow through dispatched programs, so they validate the
   rebuild approach end-to-end)
3. Manual: load two XDP programs on same interface with different
   priorities, verify `bpfman link list` shows correct position
   ordering, remove one, verify the remaining program is re-positioned

## Open questions

1. **TC swap window**: Rust bpfman adds the new filter then removes
   the old one, accepting a brief window where both exist. Should we
   match this exactly, or investigate whether we can minimise the
   window? (Recommendation: match Rust bpfman for now.)

2. **Extension link re-use**: When rebuilding, each extension must be
   loaded fresh from the ELF with the new dispatcher as its
   `AttachTarget`. The old extension links are discarded. Need to
   verify cilium/ebpf handles this cleanly (unpinning old links,
   loading fresh extensions targeting the new dispatcher).

3. **Rollback on mid-rebuild failure**: If extension re-attachment
   fails partway through, the old dispatcher is still active. We
   should clean up the partial new revision and leave the old one in
   place. The Rust implementation follows this pattern.
