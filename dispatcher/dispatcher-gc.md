# Dispatcher GC

This document describes how garbage collection interacts with
dispatchers, and why naive GC destroys dispatcher state. For the
dispatcher model itself, see [dispatcher-model.md](dispatcher-model.md).

## The problem

Every dispatcher rebuild re-attaches all extensions, creating new
kernel BPF extension links (see
[dispatcher-model.md, "The rebuild cycle"](dispatcher-model.md#the-rebuild-cycle)).
The old extension links are destroyed by the kernel. The database
records for those links become stale: they reference kernel link IDs
that no longer exist.

If GC deletes every link record whose stored kernel ID is absent from
the kernel's alive set, it will remove every extension record after
the first rebuild. The next operation that queries extension links
for the dispatcher will find zero results, and the next rebuild will
produce a dispatcher with no extensions attached.

This is the bug that commit 9bbf3b7 fixed.

## The invariant

**GC must treat extension rows under a live dispatcher as live,
even if their stored kernel link IDs are stale.**

A **live dispatcher** is one whose `program_id` in the dispatchers
table is present in the kernel's alive program set. The program ID
changes on each rebuild (each rebuild loads a new dispatcher
program), but the rebuild transaction commits the new program ID
before releasing the writer lock. GC acquires the same lock, so at
the time GC executes the dispatcher row always reflects the
currently loaded program.

Extension detail rows reference the dispatcher via
`dispatcher_program_id`, kept in sync by ON UPDATE CASCADE. If the
dispatcher's program is alive, all its extension records are
considered live regardless of their individual stored link IDs.

## GC phases

GC (`computeStoreGC` in `manager/gc.go`) runs four phases:

| Phase | What it does                          | Dispatcher relevance                       |
|-------|---------------------------------------|--------------------------------------------|
| 1     | Delete programs whose kernel ID is not alive | Removes dead managed programs. Not dispatcher-specific |
| 2     | Delete dispatchers whose `program_id` is not alive | Removes dispatchers whose program was unloaded externally |
| 3     | Delete non-synthetic links whose kernel link ID is not alive | **Skips extension links of live dispatchers** |
| 4     | Delete orphaned dispatchers (zero remaining extensions) | Catches dispatchers that lost all extensions in phase 3 |

### Phase 3 detail

For each non-synthetic link:

1. If the link is synthetic, skip (synthetic links have no kernel
   representation).
2. **If the link is an extension (XDP or TC) whose
   `dispatcher_program_id` matches a live dispatcher, skip.** This
   is the critical guard.
3. Otherwise, if the link's kernel ID is not in the alive set,
   delete it.

The check is implemented by `isLiveExtensionLink`:

```go
func isLiveExtensionLink(link bpfman.LinkRecord, liveDispatchers map[kernel.ProgramID]bool) bool {
    switch d := link.Details.(type) {
    case bpfman.XDPDetails:
        return liveDispatchers[d.DispatcherID]
    case bpfman.TCDetails:
        return liveDispatchers[d.DispatcherID]
    }
    return false
}
```

The `liveDispatchers` map is built from dispatchers whose
`program_id` is present in the kernel alive set.

### Phase 4 detail

Phase 4 only runs if phase 3 actually deleted links (otherwise no
dispatcher can have lost its last extension). For each dispatcher not
already deleted in phase 2, it counts remaining (non-deleted)
extension links. If zero remain, the dispatcher is deleted.

This handles the case where phase 3 deletes enough extension links
that a dispatcher is left with zero remaining extensions. Such a
dispatcher is orphaned and should be cleaned up.

## Worked example

### Setup

Two user programs E1 and E2 attached via an XDP dispatcher. The
current state:

| Layer  | State                                                        |
|--------|--------------------------------------------------------------|
| Kernel | Dispatcher program P2 alive. Extension links K4, K5 alive   |
| Bpffs  | Rev 2 directory with link_0, link_1. Stable link pin present |
| Store  | dispatchers: program_id=P2. E1: link_id=K4, disp_prog_id=P2. E2: link_id=K5, disp_prog_id=P2 |

Now a third program E3 is attached, triggering a rebuild. The
following table shows the state after the rebuild transaction has
committed (step 4 in the rebuild cycle):

| Layer  | State after rebuild (transaction committed)                  |
|--------|--------------------------------------------------------------|
| Kernel | P3 alive. K4, K5 destroyed. New links K6, K7, K8 alive      |
| Bpffs  | Rev 3 directory with link_0, link_1, link_2. Rev 2 deleted   |
| Store  | dispatchers: program_id=P3. E1: link_id=K6, disp_prog_id=P3. E2: link_id=K7, disp_prog_id=P3. E3: link_id=K8, disp_prog_id=P3 |

### GC runs immediately after the rebuild persists

Kernel alive sets: programs={P3, ...}, links={K6, K7, K8, ...}.

| Phase | Check                                    | Result              |
|-------|------------------------------------------|---------------------|
| 2     | Is P3 alive?                             | Yes. Dispatcher kept |
| 3     | E1 (link_id=K6): is K6 alive?            | Yes. Kept            |
| 3     | E2 (link_id=K7): is K7 alive?            | Yes. Kept            |
| 3     | E3 (link_id=K8): is K8 alive?            | Yes. Kept            |
| 4     | (no deletions in phase 3)                | Skipped              |

No action. Correct.

### GC runs after another rebuild that the store has not yet persisted

This is the dangerous window. Suppose a fourth program E4 is being
attached. The rebuild has completed in the kernel (new program P4,
new links K9-K12) but the store transaction has not yet committed.
The store still has program_id=P3 and link IDs K6-K8.

Kernel alive sets: programs={P4, ...}, links={K9, K10, K11, K12, ...}.
P3 is no longer alive (it was the old dispatcher, now replaced).
K6, K7, K8 are no longer alive (old extension links, destroyed by
rebuild).

| Phase | Check                                    | Result              |
|-------|------------------------------------------|---------------------|
| 2     | Is P3 alive?                             | **No.** Dispatcher deleted |
| 3     | E1 (link_id=K6): live dispatcher?        | No (P3 deleted). Is K6 alive? No. **Deleted** |
| 3     | E2 (link_id=K7): same logic              | **Deleted**         |
| 3     | E3 (link_id=K8): same logic              | **Deleted**         |

This is destructive but cannot happen in practice: GC and dispatcher
operations are serialised by the writer lock. GC cannot run between
the kernel rebuild and the store persist because both happen within
the same lock acquisition. The scenario is listed here to explain
why the serialisation matters.

### GC runs when the dispatcher was externally unloaded

If something outside bpfman removes the dispatcher program from the
kernel (e.g., `bpftool prog delete`), GC correctly tears everything
down:

| Phase | Check                                    | Result              |
|-------|------------------------------------------|---------------------|
| 2     | Is P3 alive?                             | No. Dispatcher deleted |
| 3     | Extensions reference dead dispatcher     | Deleted              |
| 4     | (dispatcher already deleted in phase 2)  | Skipped              |

This is the correct behaviour: the dispatcher is gone, so its
extensions are meaningless.

## Why this is not obvious

The subtlety is that extension link records are **transiently
stale** by design. The rebuild cycle makes this unavoidable: after
re-attachment and before the store transaction commits, stored
extension link IDs do not match live kernel objects. Once the
transaction commits the store is consistent again, but the next
rebuild will invalidate the IDs once more. Extension link IDs are
not stable across rebuilds and must not be used as durable identity.

In most systems, a stale kernel ID means the object is dead. Here
it means the object was replaced and the record has not yet caught
up. GC must distinguish "stale because replaced during rebuild"
from "stale because genuinely dead". It does so by checking the
dispatcher program, not the individual extension links. **A live
dispatcher implies live extensions, by construction.**

## Related documents

- [dispatcher-model.md](dispatcher-model.md) -- dispatcher
  architecture, rebuild cycle, store schema
- [dispatcher-scenarios.md](dispatcher-scenarios.md) -- concrete
  walkthroughs of attach, detach, rebuild, and GC operations
- Package documentation: `go doc ./dispatcher/`
