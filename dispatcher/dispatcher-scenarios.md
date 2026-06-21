# Dispatcher Scenarios

Scenario-driven walkthroughs for the dispatcher package.

This document complements the reference docs:

- [dispatcher-model.md](dispatcher-model.md) explains the architecture
  and invariants
- [dispatcher-gc.md](dispatcher-gc.md) explains the coherency
  correctness rule
- this document explains what happens for concrete operations

Each scenario shows:

- user action
- kernel objects
- bpffs state
- SQLite state
- what stayed logically the same
- what was physically recreated

## How to read these scenarios

A dispatcher has two kinds of identity:

- **logical identity**: "the dispatcher for (type, nsid, ifindex)"
- **physical identity**: the specific kernel program, extension
  links, pins, and rows representing the current revision

Most user actions preserve the logical identity but replace some or
all physical objects.

Unless stated otherwise, scenarios assume:

- .rodata-based dispatchers (configuration baked in at load time)
- rebuild-on-attach and rebuild-on-detach
- XDP uses a stable BPF link pin
- TC uses a stable filter identity (priority + parent handle)
- the writer lock serialises dispatcher operations and coherency work

## Notation

- P1, P2, ... -- dispatcher kernel program IDs
- L1, L2, ... -- XDP kernel link IDs
- K1, K2, ... -- extension kernel link IDs
- B1, B2, ... -- bpfman-managed link handles
- F1, F2, ... -- TC filter identities
- E1, E2, ... -- attached user programs (extensions)
- rev 1, rev 2, ... -- dispatcher revision numbers

Identifiers are illustrative per scenario. They are not globally
sequenced across scenarios.

Example dispatcher key: (type=xdp, nsid=4026531840, ifindex=1)

Example bpffs paths:

```
/run/bpfman/fs/xdp/dispatcher_4026531840_1_link
/run/bpfman/fs/xdp/dispatcher_4026531840_1_2/dispatcher
/run/bpfman/fs/xdp/dispatcher_4026531840_1_2/link_0
```

These scenarios use simplified IDs and abbreviated state for clarity.
The reference behaviour is defined by the implementation and the
model docs.

---

# Attach scenarios

## Scenario 1: Attach the first XDP program

### Initial state

No dispatcher exists for (xdp, nsid, ifindex).

| Layer  | State  |
|--------|--------|
| Kernel | no dispatcher program, no XDP link, no extension links |
| Bpffs  | no dispatcher pin directory, no stable XDP link pin |
| SQLite | no dispatchers row, no extension detail rows |

### Action

Attach XDP program E1 at this attach point.

### Steps

1. Load dispatcher rev 1 with one enabled slot.
2. Attach E1 as extension to prog0, creating extension link K1.
3. Create XDP link L1 attaching dispatcher program P1 to the
   interface.
4. Persist dispatcher and extension rows in a single transaction.
5. Leave rev 1 pinned as the current revision.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher program P1; extension link K1 (E1 -> P1/prog0); XDP link L1 (P1 -> ifindex) |
| Bpffs  | `.../dispatcher_{n}_{i}_1/dispatcher`; `.../dispatcher_{n}_{i}_1/link_0`; `.../dispatcher_{n}_{i}_link` |
| SQLite | dispatchers(type=xdp, nsid, ifindex, rev=1, program_id=P1, kernel_link_id=L1); E1 link row (id=B1, kernel_link_id=K1, position=0, dispatcher_program_id=P1) |

### Identity

| Category      | Items |
|---------------|-------|
| Created       | dispatcher program P1, extension link K1, XDP link L1, bpfman link handle B1, rev 1 directory, all store rows |
| Stable        | the attach point (type, nsid, ifindex) |

---

## Scenario 2: Attach a second XDP program

### Initial state

Dispatcher rev 1 exists with one extension.

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher P1; extension link K1 for E1; XDP link L1 |
| Bpffs  | `..._1/dispatcher`; `..._1/link_0`; stable XDP link pin |
| SQLite | dispatcher row (program_id=P1, kernel_link_id=L1, rev=1); E1 link row (id=B1, kernel_link_id=K1, position=0) |

### Action

Attach XDP program E2.

### Steps

1. Load dispatcher rev 2 as program P2.
2. Re-attach E1 to P2/prog0, creating new link K2. Old link K1
   destroyed.
3. Attach E2 to P2/prog1, creating new link K3.
4. Update XDP link L1 to point to P2 (atomic, no packet gap).
5. Persist the new dispatcher program ID and refreshed captured
   extension kernel link IDs in a single transaction.
6. Remove rev 1 pins.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher P2; extension links K2 (E1), K3 (E2); XDP link L1, now pointing at P2 |
| Bpffs  | `..._2/dispatcher`; `..._2/link_0`; `..._2/link_1`; stable XDP link pin unchanged; `..._1/` removed |
| SQLite | dispatcher row updated (program_id=P2, rev=2, kernel_link_id=L1); E1 keeps id=B1 and records kernel_link_id=K2; E2 gets id=B2 and records kernel_link_id=K3 |

### Identity

| Category      | Items |
|---------------|-------|
| Logically same | dispatcher key (xdp, nsid, ifindex); E1 is still attached as bpfman link B1; XDP link identity L1; stable link pin path |
| Recreated     | dispatcher program (P1 -> P2); all extension links (K1 -> K2, plus new K3); revision directory (rev 1 -> rev 2) |

### Key lesson

Adding one extension rebuilds the dispatcher and re-attaches all
existing extensions. Every extension keeps its bpfman link handle,
but gets a new captured kernel link ID.

---

## Scenario 3: Fill all 10 slots

For illustration, assume this starts from an empty attach point and
walks all the way to 10 extensions.

### Initial state

No dispatcher exists.

### Action

Attach 10 programs, one at a time.

### Steps

Each attach follows the same rebuild pattern:

1. Load a new dispatcher revision with N+1 enabled slots.
2. Re-attach all N existing extensions to the new revision.
3. Attach the new extension into its assigned slot.
4. Swap the interface attachment.
5. Persist the new revision and all new captured kernel link IDs.
6. Remove the old revision directory.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | current dispatcher program; 10 extension links, one per slot; one stable interface attachment (L1 for XDP) |
| Bpffs  | current revision directory containing: `dispatcher`, `link_0` through `link_9`; stable XDP link pin (for XDP) |
| SQLite | one dispatcher row at the current revision; 10 extension detail rows with positions 0..9 |

### What churned

Every attach rebuilt the dispatcher and re-attached every existing
extension. After 10 attaches the dispatcher program has been loaded
10 times, and the first extension's captured kernel link ID has been
replaced 9 times. Its bpfman link handle has not changed.

### Attempting the 11th attach

The 11th attach is rejected before any mutation occurs:

```
no free dispatcher slots (all 10 occupied)
```

No new revision is loaded, no pins are created, no store rows are
modified.

---

# Detach scenarios

## Scenario 4: Detach a middle program

### Initial state

Three programs attached: E1 at slot 0, E2 at slot 1, E3 at slot 2.
Dispatcher rev N.

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher program PN; extension links for E1, E2, E3 |
| Bpffs  | `..._{N}/dispatcher`; `..._{N}/link_0`; `..._{N}/link_1`; `..._{N}/link_2` |
| SQLite | dispatcher row (rev=N); extension rows for E1, E2, E3 |

### Action

Detach E2.

### Steps

1. Load dispatcher rev N+1 with two enabled slots.
2. Recompute slot ordering for the surviving programs (sorted by
   priority ASC, then program name ASC).
3. Re-attach E1 and E3 to the new dispatcher, creating new links
   K4 and K5. Old extension links destroyed.
4. Swap interface attachment.
5. Persist updated dispatcher state and surviving extension rows.
   Delete E2's extension row.
6. Remove the old revision directory.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | new dispatcher program; new extension links K4 (E1), K5 (E3); no link for E2 |
| Bpffs  | `..._{N+1}/dispatcher`; `..._{N+1}/link_0`; `..._{N+1}/link_1`; `..._{N}/` removed |
| SQLite | dispatcher row updated (new revision, new program_id); E1 keeps id=B1 and records kernel_link_id=K4; E3 keeps id=B3 and records kernel_link_id=K5; E2 link row B2 deleted |

### Identity

| Category      | Items |
|---------------|-------|
| Logically same | dispatcher key; E1 and E3 remain attached with their bpfman link handles |
| Recreated     | dispatcher program; surviving extension links; revision directory; slot positions may change |

### Key lesson

Detaching one non-final program still rebuilds the whole dispatcher.
The surviving programs preserve their bpfman link handles, but are
re-attached with new captured kernel link IDs and potentially new
slot positions.

---

## Scenario 5: Detach the last program

### Initial state

One program E1 attached through a dispatcher.

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher program P1; extension link K1; interface attachment |
| Bpffs  | revision directory with dispatcher and link_0; stable link pin (XDP) |
| SQLite | dispatcher row; extension row for E1 |

### Action

Detach E1.

### Steps

1. Unpin and close the extension link.
2. Remove the interface attachment:
   - XDP: unpin and close the BPF link.
   - TC: remove the tc filter.
3. Unpin and close the dispatcher program.
4. Remove the revision directory.
5. Delete the dispatcher row (ON DELETE CASCADE removes E1's
   detail row).

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | no dispatcher program; no extension links; no interface attachment |
| Bpffs  | no revision directory; no stable link pin |
| SQLite | no dispatcher row; no extension detail rows |

### Identity

| Category | Items |
|----------|-------|
| Removed  | dispatcher program, extension link, interface attachment, revision directory, all store rows |
| Stable   | nothing remains at this attach point |

### Key lesson

Detaching the last extension is not a rebuild. It is full teardown.
Nothing is recreated; everything is removed.

---

# TC attach scenarios

TC egress behaves the same way as TC ingress, differing only in the
attach point (parent handle and direction). The scenarios below use
TC ingress; the mechanics are identical for egress.

## Scenario 6: Attach the first TC ingress program

### Initial state

No dispatcher exists for (tc-ingress, nsid, ifindex).

| Layer  | State  |
|--------|--------|
| Kernel | no dispatcher program; no tc filter; no extension links |
| Bpffs  | no tc-ingress/dispatcher_* |
| SQLite | no dispatcher row; no extension detail rows |

### Action

Attach TC ingress program E1.

### Steps

1. Load dispatcher rev 1 as P1.
2. Attach E1 as extension link K1.
3. Create tc filter F1 pointing at P1 (fixed filter priority 50).
4. Persist dispatcher row (program_id=P1, priority=50,
   kernel_link_id=NULL) and extension row.
5. Pin the revision directory.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher program P1; extension link K1; tc filter F1 (priority 50) |
| Bpffs  | `.../tc-ingress/dispatcher_{n}_{i}_1/dispatcher`; `.../tc-ingress/dispatcher_{n}_{i}_1/link_0` |
| SQLite | dispatchers(type=tc-ingress, nsid, ifindex, rev=1, program_id=P1, priority=50, kernel_link_id=NULL); E1 link row (id=B1, kernel_link_id=K1) |

### Identity

| Category | Items |
|----------|-------|
| Created  | dispatcher program P1, extension link K1, bpfman link handle B1, tc filter F1, rev 1 directory, all store rows |
| Stable   | the attach point (tc-ingress, nsid, ifindex) |

### What differs from XDP

| Aspect         | XDP                       | TC                          |
|----------------|---------------------------|-----------------------------|
| Link pin       | stable BPF link pin       | no link pin                 |
| Attachment     | BPF link (atomically updatable) | tc filter (create new, remove old) |
| dispatchers.kernel_link_id | non-NULL captured kernel link ID | NULL |
| priority column | NULL                     | 50 (fixed filter priority)  |

---

## Scenario 7: Attach a second TC program

### Initial state

One TC program already attached through a dispatcher.

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher P1; extension link K1; tc filter F1 |
| Bpffs  | `..._1/dispatcher`; `..._1/link_0` |
| SQLite | dispatcher row (program_id=P1, priority=50); extension row for E1 |

### Action

Attach E2.

### Steps

1. Load new dispatcher rev 2 as P2.
2. Re-attach E1 to P2, creating new extension link K2. K1
   destroyed.
3. Attach E2 to P2, creating K3.
4. Record the old tc filter's handle.
5. Create new tc filter F2 pointing at P2.
6. Remove old filter F1.
7. Persist new dispatcher row values and extension rows.
8. Remove old revision directory.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher P2; extension links K2, K3; tc filter F2 |
| Bpffs  | only rev 2 directory remains |
| SQLite | dispatcher row updated (program_id=P2, rev=2); extension rows preserve their bpfman link handles and refresh kernel_link_id |

### Identity

| Category      | Items |
|---------------|-------|
| Logically same | the tc-ingress dispatcher for this attach point |
| Recreated     | dispatcher program; all extension links; tc filter; revision directory |

### Key difference from XDP

For TC there is no `bpf_link_update` on a stable BPF link. The swap
is: record old filter handle, create new filter, remove old filter.
The tc filter object itself is recreated on every rebuild.

---

# Ordering and capacity scenarios

## Scenario 8: Rebuild due to ordering change

### Initial state

Multiple extensions attached. A change causes a different execution
order -- for example, a priority change or a name change that
affects the deterministic tie-break.

### Slot ordering rule

Extensions are sorted by (priority ASC, program name ASC). Priority 0
is stored verbatim and sorts before positive values. After sorting,
slot positions are assigned sequentially from 0.

### Steps

1. Recompute ordering.
2. Load new dispatcher revision with updated .rodata reflecting new
   slot assignments.
3. Re-attach all extensions according to the new slot/order
   mapping.
4. Swap interface attachment.
5. Persist new revision, refreshed captured kernel link IDs, and new
   slot positions.
6. Remove old revision directory.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | new dispatcher program; new extension links |
| Bpffs  | new revision directory; old revision directory removed |
| SQLite | same bpfman link handles, but: new dispatcher_program_id; refreshed kernel_link_id values; possibly new positions |

### Identity

| Category      | Items |
|---------------|-------|
| Logically same | same set of attached programs |
| Recreated     | dispatcher program; all extension links; slot positions may differ |

### Key lesson

A rebuild is triggered by configuration change, not just cardinality
change. The same number of extensions can produce a different
dispatcher revision.

---

# Coherency and failure scenarios

## Scenario 9: Coherency work runs after a normal rebuild

### Initial state

A rebuild has committed successfully. The store is consistent.

### Action

Coherency work runs.

### Checks

1. Dispatcher `program_id` is alive in the kernel.
2. Extension rows reference that live dispatcher via
   `dispatcher_program_id`.
3. The check sees the post-commit consistent state for this revision.
4. No dispatcher or extension rows are removed.

### Final state

No change.

### Key lesson

Normal rebuild followed by coherency work is safe. No state is lost.

---

## Scenario 10: The hypothetical dangerous window

This is not a reachable runtime scenario under correct locking. It
is a correctness thought experiment.

### Hypothetical state

- Kernel has finished rebuilding to P4, with new extension links
  K9-K12.
- Store still points to P3 with extension captured kernel link IDs
  K6-K8.
- Coherency work is (hypothetically) allowed to run concurrently.

### What the coherency check would do

1. Phase 2: P3 is not alive. Dispatcher row deleted.
2. Phase 3: extension rows reference dead dispatcher. Deleted.
3. The persisted state is torn down incorrectly; subsequent rebuild
   or reconciliation would treat the dispatcher as absent.

### Why this does not happen

Dispatcher rebuild and persist happen under the writer lock.
Coherency work acquires the same lock. It can only observe stable
post-commit state. The serialisation is part of the correctness
model, not a performance detail.

---

## Scenario 11: Dispatcher externally removed, then coherency work runs

### Initial state

A dispatcher exists in SQLite, but an external tool (e.g.,
`bpftool prog delete`) has removed the dispatcher program from the
kernel.

### Action

Coherency work runs.

### Steps

1. Phase 2: dispatcher `program_id` is not in the kernel alive set.
   Dispatcher row deleted.
2. Phase 3: extension rows no longer reference a live dispatcher
   and their captured kernel link IDs are not alive. Extension rows
   deleted.
3. Phase 4: dispatcher already deleted in phase 2. Skipped.

### Final state

| Layer  | State  |
|--------|--------|
| Kernel | dispatcher already gone |
| Bpffs  | pins still present but now point to dead kernel objects (pre-cleanup state; coherency rules will remove them) |
| SQLite | no dispatcher row; no extension detail rows |

### Key lesson

The "preserve extension rows under a live dispatcher" rule is not
"never delete extension rows". It is specifically keyed to the
dispatcher program being alive. When the dispatcher is dead, GC
correctly tears everything down.

---

## Cross-scenario summary

### Logical identities (stable across rebuilds)

- Dispatcher key (type, nsid, ifindex)
- The fact that a given user program is attached at that attach
  point

### Physical identities (recreated on rebuild)

- Dispatcher kernel program ID
- Extension kernel link IDs
- Revision directory and all pins within it
- TC filter object (for TC dispatchers)

### XDP-specific stable items

- Stable XDP link pin path
  (`{bpffs}/{type}/dispatcher_{nsid}_{ifindex}_link`)
- XDP link kernel object (updated to point at the new dispatcher,
  not recreated)

### Two kinds of priority

- **Extension slot priority**: user-supplied per extension. Determines
  slot ordering via (priority ASC, program name ASC); priority 0 is a
  real value that sorts first.
- **TC filter priority**: fixed at 50 for the dispatcher's netlink
  filter. Not user-configurable.

### State stores shown in every scenario

| Store  | What to look for |
|--------|------------------|
| Kernel | dispatcher program, extension links, interface attachment (BPF link or tc filter) |
| Bpffs  | stable link pin (XDP only), revision directory, extension link pins |
| SQLite | dispatchers row (program_id, revision, kernel_link_id, priority); links rows (id, kernel_link_id); extension detail rows (id as bpfman handle, position, dispatcher_program_id) |

## Related documents

- [dispatcher-model.md](dispatcher-model.md) -- architecture,
  invariants, rebuild cycle, store schema
- [dispatcher-gc.md](dispatcher-gc.md) -- coherency behaviour and
  the extension captured-kernel-link staleness problem
- Package documentation: `go doc ./dispatcher/`
