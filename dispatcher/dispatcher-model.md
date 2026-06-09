# Dispatcher Model

This document describes the dispatcher architecture: what dispatchers
are, how they work, their filesystem and database representation, and
the rebuild lifecycle.

The package documentation (`go doc ./dispatcher/`) provides a concise
overview; this document covers the full design.

## Core invariants

These are the rules of the system. Everything else in this document
is explanation.

1. **One dispatcher per attach point.** An attach point is identified
   by (type, nsid, ifindex). At most one dispatcher exists for each.

2. **Rebuilds create a new dispatcher program.** Every attach or
   detach loads a fresh dispatcher program with new .rodata. The new
   program gets a new kernel program ID.

3. **All extensions are re-attached on every rebuild.** Each
   re-attachment creates a new kernel BPF extension link. The old
   extension links from the previous revision are destroyed.

4. **Extension kernel link IDs are not stable across rebuilds.**
   Each member has a stable bpfman link ID that is preserved across
   rebuilds. Its captured kernel link ID is overwritten with the ID
   returned by the latest freplace attach.

5. **After a rebuild transaction commits, the dispatchers table
   holds the current program ID.** ON UPDATE CASCADE propagates
   this to all extension detail rows. The dispatchers table and
   extension detail rows are only guaranteed consistent at stable
   observation points, outside the rebuild transaction window. The
   writer lock ensures other mutating operations never observe the
   mid-rebuild state.

6. **Detaching the last extension tears down the dispatcher
   entirely.** No empty dispatchers exist at rest.

## Why dispatchers exist

The kernel allows only one XDP program per interface and one TC
classifier per (interface, direction). Dispatchers multiplex up to
10 user programs through a single attachment, giving the appearance
of independent concurrent attachments on the same hook.

## Limits

Each network interface can have up to three dispatchers, one per hook:

| Dispatcher type | Hook          |
|-----------------|---------------|
| XDP             | XDP (ingress) |
| TC ingress      | TC ingress    |
| TC egress       | TC egress     |

Each dispatcher chains up to 10 programs (positions 0--9), so a
single interface can host at most 30 dispatched programs: 10 XDP, 10
TC ingress, 10 TC egress. The system-wide limit is bounded by the
number of network namespaces and interfaces.

## Kernel mechanism

A dispatcher is a BPF program containing 10 stub functions (`prog0`
through `prog9`). The stub bodies are not semantically interesting;
they exist solely to provide named `freplace` targets. User programs
attach as BPF extensions (`BPF_PROG_TYPE_EXT` / `freplace`) that
replace these stubs at runtime. When a packet arrives the dispatcher
calls each enabled slot in order. After each call it checks the
return value against a per-slot proceed-on bitmask. If the
corresponding bit is set, the dispatcher continues to the next slot;
otherwise it returns immediately.

Three `.rodata` fields control behaviour:

| Field                | Type         | Purpose                                      |
|----------------------|--------------|----------------------------------------------|
| `num_progs_enabled`  | `u8`         | How many slots (from `prog0`) are active      |
| `chain_call_actions` | `u32[10]`    | Per-slot bitmask of return values that chain  |
| `run_prios`          | `u32[10]`    | Per-slot priority (used by manager for ordering) |

Because the configuration is declared `const volatile` in the BPF
source, the verifier dead-code-eliminates disabled slots at load
time. Changing any field requires loading an entirely new dispatcher
program.

### Proceed-on bitmasks

For XDP the bitmask directly encodes return values:

| Bit | Value        |
|-----|--------------|
| 0   | XDP_ABORTED  |
| 1   | XDP_DROP     |
| 2   | XDP_PASS     |
| 3   | XDP_TX       |
| 4   | XDP_REDIRECT |

For TC the bitmask is shifted left by one to accommodate
`TC_ACT_UNSPEC = -1`:

| Bit | Value            |
|-----|------------------|
| 0   | TC_ACT_UNSPEC (-1) |
| 1   | TC_ACT_OK (0)    |
| 2   | TC_ACT_RECLASSIFY (1) |
| ... | ...              |

If all enabled slots run without terminating the chain, the
dispatcher returns the default: `XDP_PASS` for XDP, `TC_ACT_OK` for
TC.

### Why extensions are loaded as BPF_PROG_TYPE_EXT

The kernel's freplace mechanism requires `BPF_PROG_TYPE_EXT`. A
program's type is fixed at load time and cannot be changed
afterwards, so an already-loaded XDP program (type `BPF_PROG_TYPE_XDP`)
cannot be used as a freplace target. The same ELF bytecode must be
loaded a second time with type `Extension`, `AttachTarget` set to the
dispatcher program, and `AttachTo` set to the slot name (e.g.
`prog0`). The instructions are valid for both XDP and Extension
contexts when the target is an XDP function.

## Dispatcher types

| Type       | Attachment mechanism    | Stable reference      |
|------------|------------------------|-----------------------|
| XDP        | Kernel BPF link (pinned, atomically updatable) | BPF link pin |
| TC ingress | Netlink tc filter with priority | TC filter identity (priority + parent handle) |
| TC egress  | Netlink tc filter with priority | TC filter identity (priority + parent handle) |

XDP dispatchers use a BPF link. On rebuild the existing link is
updated to point to the new program -- no packet-processing gap. TC
dispatchers use legacy netlink: a new filter is created and the old
one removed.

Both provide a single stable attachment per (type, nsid, ifindex).

## Filesystem layout

Dispatcher pins are organised under the bpffs mount point in a
directory named after the type (`xdp`, `tc-ingress`, `tc-egress`).

```
{bpffs}/{type}/
    dispatcher_{nsid}_{ifindex}_link              # stable XDP link pin (XDP only)
    dispatcher_{nsid}_{ifindex}_{revision}/        # versioned revision directory
        dispatcher                                 # dispatcher program pin
        link_0                                     # extension link at position 0
        link_1                                     # extension link at position 1
        ...
        link_9                                     # extension link at position 9
```

The stable link pin sits outside the revision directory so that
rebuilds can swap the underlying program without unpinning or
re-pinning the link -- the link object stays in place and only its
target changes. The revision directory is disposable: after a
successful swap the old one is removed, taking the previous
dispatcher program pin and extension link pins with it.

The `nsid` component ensures dispatchers in different network
namespaces do not collide even when they share the same `ifindex`
(e.g. two containers each with an `eth0` at ifindex 2).

### Example

XDP on loopback (ifindex 1), root namespace (nsid 4026531840),
revision 3:

```
/run/bpfman/fs/xdp/dispatcher_4026531840_1_link
/run/bpfman/fs/xdp/dispatcher_4026531840_1_3/dispatcher
/run/bpfman/fs/xdp/dispatcher_4026531840_1_3/link_0
/run/bpfman/fs/xdp/dispatcher_4026531840_1_3/link_1
```

## Store representation

### dispatchers table

Primary key: `(type, nsid, ifindex)`.

| Column       | Type    | Notes                                             |
|--------------|---------|---------------------------------------------------|
| `type`       | TEXT    | `'xdp'`, `'tc-ingress'`, or `'tc-egress'`         |
| `nsid`       | INTEGER | Network namespace inode number                     |
| `ifindex`    | INTEGER | Network interface index                            |
| `revision`   | INTEGER | Incremented on each rebuild (>= 1)                 |
| `program_id` | INTEGER | Kernel program ID of current dispatcher (UNIQUE)   |
| `kernel_link_id` | INTEGER | Captured kernel link ID (XDP only; NULL for TC) |
| `priority`   | INTEGER | TC filter priority (TC only; NULL for XDP)         |

A CHECK constraint enforces that XDP rows have a non-NULL
`kernel_link_id` and TC rows have `kernel_link_id IS NULL`.

### Extension detail tables

`link_xdp_details` and `link_tc_details` store per-extension metadata.
Each row's `id` is the bpfman-managed link handle and references
`links(id)`. Each row also has a `dispatcher_program_id` foreign key
referencing `dispatchers(program_id)`.

| FK action          | Effect                                           |
|--------------------|--------------------------------------------------|
| ON DELETE CASCADE  | Deleting a dispatcher deletes all its extensions  |
| ON UPDATE CASCADE  | Updating `program_id` propagates to all extensions |

A unique index on `(nsid, ifindex, position)` (plus `direction` for
TC) prevents two extensions from occupying the same slot.

The ON UPDATE CASCADE is central to the rebuild model: when the
dispatcher row is updated with a new program ID after rebuild, every
extension detail row's `dispatcher_program_id` is automatically
updated to match. This keeps the FK relationship consistent without
requiring individual UPDATE statements per extension.

## The rebuild cycle

Every attach or detach triggers a full rebuild. The sequence:

| Step | Action                                | Kernel effect                          | Bpffs effect                          |
|------|---------------------------------------|----------------------------------------|---------------------------------------|
| 1    | Load new dispatcher with updated .rodata | New program Pnew loaded               | `{rev_new}/dispatcher` pinned         |
| 2    | Re-attach all extensions to Pnew      | New extension links created; old destroyed | `{rev_new}/link_0` .. `link_N` pinned |
| 3    | Swap interface attachment             | XDP: link updated. TC: new filter, old removed | XDP: stable link pin unchanged       |
| 4    | Persist in transaction                | (none)                                 | Dispatcher row updated to Pnew; member bpfman IDs preserved or allocated; captured kernel link IDs refreshed; ON UPDATE CASCADE propagates dispatcher_program_id |
| 5    | Remove old revision directory         | Pold ref-count drops                   | `{rev_old}/` deleted                  |

Steps 2 and 3 are the critical consequence: every extension link gets
a new kernel link ID on every rebuild, even for members the user did
not touch. The database preserves each member's bpfman link ID and
refreshes its captured kernel link ID at step 4.

## Lifecycle timelines

The following scenarios trace the three layers (kernel, bpffs, store)
through concrete operations.

### First attach: empty to one extension

No dispatcher exists for (xdp, nsid, ifindex).

| Step | Kernel                              | Bpffs                                         | Store                                   |
|------|-------------------------------------|-----------------------------------------------|-----------------------------------------|
| 1    | Dispatcher program P1 loaded        | `{type}/dispatcher_{n}_{i}_1/dispatcher` pinned | (nothing yet)                          |
| 2    | Extension link K1 created (E1 -> P1/prog0) | `{type}/dispatcher_{n}_{i}_1/link_0` pinned | (nothing yet)                          |
| 3    | XDP link L1 created (P1 -> ifindex) | `{type}/dispatcher_{n}_{i}_link` pinned       | (nothing yet)                          |
| 4    | (none)                              | (none)                                        | dispatchers: (P1, rev=1, kernel_link_id=L1). E1 link row: (id=B1, kernel_link_id=K1, disp_prog_id=P1, position=0) |

### Second attach: one to two extensions

Dispatcher rev 1 exists with extension E1 at position 0. E1's bpfman
link handle is B1 and its current captured kernel link ID is K1.

| Step | Kernel                              | Bpffs                                         | Store                                   |
|------|-------------------------------------|-----------------------------------------------|-----------------------------------------|
| 1    | New dispatcher P2 loaded            | `..._2/dispatcher` pinned                     | (unchanged)                            |
| 2    | K1 destroyed. New K2 (E1 -> P2/prog0), K3 (E2 -> P2/prog1) | `..._2/link_0`, `..._2/link_1` pinned | (unchanged)                            |
| 3    | L1 updated to reference P2 (atomic) | stable link pin unchanged                     | (unchanged)                            |
| 4    | (none)                              | (none)                                        | dispatchers: (P2, rev=2, kernel_link_id=L1). E1: (id=B1, kernel_link_id=K2, disp_prog_id=P2, pos=0). E2: (id=B2, kernel_link_id=K3, disp_prog_id=P2, pos=1) |
| 5    | P1 ref-count drops                  | `..._1/` deleted                              | (unchanged)                            |

Between steps 2 and 4 the store still records K1 as E1's captured
kernel link ID, but K1 no longer exists in the kernel. B1 remains
E1's stable bpfman handle throughout the rebuild. K2 and K3 are the
live kernel links. Step 4 persists the refreshed captured kernel link
IDs (K2, K3), restoring consistency.

### Detach middle extension: three to two

Dispatcher rev N with E1 at 0, E2 at 1, E3 at 2.

| Step | Kernel                              | Bpffs                                         | Store                                   |
|------|-------------------------------------|-----------------------------------------------|-----------------------------------------|
| 1    | New dispatcher loaded, 2 slots      | `..._{N+1}/dispatcher` pinned                 | (unchanged)                            |
| 2    | E1 re-attached to slot 0 (new link K4), E3 to slot 1 (new link K5); positions reassigned by priority/name sort | `..._{N+1}/link_0`, `link_1` pinned | (unchanged) |
| 3    | Link/filter swapped                 | stable pin unchanged                          | (unchanged)                            |
| 4    | (none)                              | (none)                                        | E1: (id=B1, kernel_link_id=K4, pos=0). E3: (id=B3, kernel_link_id=K5, pos=1). E2 link row B2 deleted |
| 5    | Old program ref-count drops         | `..._{N}/` deleted                            | (unchanged)                            |

### Detach last extension: teardown

Dispatcher with one extension E1.

| Step | Kernel                              | Bpffs                                         | Store                                   |
|------|-------------------------------------|-----------------------------------------------|-----------------------------------------|
| 1    | Extension link unpinned, closed     | Extension link pin removed                    | (unchanged)                            |
| 2    | XDP link unpinned, closed (or TC filter removed) | Stable link pin removed              | (unchanged)                            |
| 3    | Dispatcher program unpinned, closed | (none)                                        | (unchanged)                            |
| 4    | (none)                              | Revision directory removed                    | (unchanged)                            |
| 5    | (none)                              | (none)                                        | Dispatcher row deleted (CASCADE removes E1 detail) |

No rebuild. The dispatcher is torn down entirely.

## Related documents

- [dispatcher-gc.md](dispatcher-gc.md) -- coherency behaviour and
  the extension captured-kernel-link staleness problem
- [dispatcher-scenarios.md](dispatcher-scenarios.md) -- concrete
  walkthroughs of attach, detach, rebuild, and coherency operations
- Package documentation: `go doc ./dispatcher/`
