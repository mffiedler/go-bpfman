# Plan: tighten the writer-lock scope on `bpfman program load`

## 1. What this document is

A design memo for shrinking `bpfman program load`'s critical
section so the global writer lock at `/run/bpfman/.lock` is held
only for the cross-process exclusion that genuinely needs it.
The current code wraps the entire load -- gc, verifier syscall,
bytecode publish, sqlite writes -- in one lock-held window;
under heavy parallel load (16 workers each doing ~10 mutations
per script) the lock queue dominates and unlucky waiters blow
through `--lock-timeout=30s`. The proposal here splits gc into
its scan and remediate halves, moves the slow kernel and
filesystem steps outside the lock, and uses a short-lived
pending-row marker to close the gc-vs-in-flight-load race the
move introduces.

Out of scope: link attach, link detach, program unload, and
the TC / XDP / TCX dispatcher rebuild. Those operations
read-modify-write the dispatcher chain or the kernel<->store
correspondence and genuinely need the lock.

## 2. Current critical section

`cmd/bpfman/load_file.go:60` wraps the whole load in
`bpfmancli.RunWithLockValue`. Inside the closure,
`manager.Manager.Load` (`manager/load.go:102`) runs `gcOnEntry`
first (`manager/load.go:103`) and then a five-step operation
plan per program (`manager/load.go:176`):

1. `kernel-load` -- `action.LoadProgram` invokes the
   cilium/ebpf loader, which constructs map fds and issues
   `BPF_PROG_LOAD`. The verifier runs here. This is the slow
   step: tens to hundreds of milliseconds for non-trivial
   programs.
2. `db-consistency-check` -- read-only sqlite query.
3. `fs-publish` -- writes `/run/bpfman/programs/<id>/`. The
   destination is namespaced by the kernel-assigned program ID
   and is cross-process-safe by construction.
4. `store-save` -- the sqlite write that records the program.
5. `save-shared-maps` (optional) -- additional sqlite writes
   for shared-map pin metadata.

Of those, only step 4 and 5 actually mutate shared sqlite
state. Step 1 and 3 are namespaced by a kernel-assigned ID
and need no inter-process serialisation. SQLite already
serialises writes through its own per-database lock, so the
bpfman flock on top of steps 4 and 5 is belt-and-braces. What
makes the lock load-bearing today is `gcOnEntry`: it does a
read-modify-write across the `programs`, `links`, and
`dispatchers` tables to remediate orphans, and that RMW must
be serialised against any concurrent mutation.

## 3. gc splits cleanly into scan and remediate

`gc` has two distinct phases that the current code conflates:

- **Scan.** Read kernel state via
  `bpf(BPF_PROG_GET_NEXT_ID, ...)` and the per-link
  enumeration syscalls; cross-reference with the
  `programs` / `links` / `dispatchers` tables. Pure read.
  Returns a list of orphans (in-kernel without store record,
  or in-store without kernel program, depending on the
  direction). Runs concurrently with anything.
- **Remediate.** Take the lock; under the lock re-scan to
  confirm the orphans are still orphans (state may have
  advanced since the lockless scan); then issue the
  remediation actions. Mutates.

In practice the orphan list is empty on the overwhelming
majority of invocations. A clean run after a clean run finds
nothing; the remediation lock is taken only when a previous
operation crashed mid-flight. Treating the empty-orphans path
as lockless keeps the contention floor at zero for the common
case.

## 4. The race the tightening introduces

Moving `BPF_PROG_LOAD` and `fs-publish` out of the lock creates
a window where the kernel program exists with no sqlite record.
A concurrent process B's `gc.Scan` running during that window
sees the kernel program as an orphan. If B then takes the lock
to remediate, the re-scan under the lock still sees it as an
orphan (truth has not advanced -- process A is waiting for the
lock to commit), and B kills A's in-flight program. A's
`store-save` then fails because its kernel ID is gone.

The race exists any time `BPF_PROG_LOAD` is outside the same
critical section as `store-save`. To close it without giving
up the tightening, the in-flight load registers a
**pending-row marker** in sqlite before the kernel work begins:

1. `INSERT INTO program_pending(txn_id, started_at)` -- a
   short-lived sentinel keyed by a per-invocation transaction
   id (pid + atomic counter, the same shape `uniqueLinkBase`
   uses).
2. `BPF_PROG_LOAD` -- the slow step, now lockless.
3. `PublishBytecode` -- lockless, namespaced by kernel id.
4. Under the lock:
   - `UPDATE program_pending SET kernel_id = Y WHERE txn_id = X`
     -- bind the kernel id to the pending row.
   - `INSERT INTO programs ...` -- the real record.
   - `DELETE FROM program_pending WHERE txn_id = X` -- consume
     the marker.

`gc.Scan` ignores any in-kernel program whose id appears in
`program_pending` with a `started_at` newer than a small bound
(say 60s, well above the worst plausible verifier time on the
slowest target). Programs whose pending row is older than the
bound are treated as orphans (a process crashed before binding
its kernel id), which is the correct outcome.

The pending row is a one-row insert, then a one-row update, then
a one-row delete -- three cheap sqlite operations sandwiching
the slow kernel call. None of them needs the bpfman flock,
because sqlite's own serialisation is sufficient for the writes
and the lookup is keyed by a per-process txn id that no other
process generates.

## 5. Proposed flow

```
process A:
  orphans := gc.Scan()                              # lockless
  if len(orphans) > 0:
    acquire lock
      reverified := gc.Scan()                       # under lock
      gc.Remediate(reverified)
    release lock

  txn := uniqueTxnID()                              # pid:counter
  INSERT INTO program_pending(txn_id=txn, ...)      # sqlite-only

  BPF_PROG_LOAD                                     # lockless, slow
  PublishBytecode                                   # lockless

  acquire lock
    UPDATE program_pending SET kernel_id = Y ...    # bind
    INSERT INTO programs ...                        # the record
    DELETE FROM program_pending WHERE txn_id = txn  # consume
  release lock
```

The two locked regions are independent and almost always tiny:

- The gc-remediate region is reached only when the lockless
  scan found something, which is rare on a clean system.
- The commit region holds three single-row sqlite operations
  under the lock. Empirically these are well under 10ms each
  with WAL-mode sqlite on tmpfs (`/run`).

Lock-hold per script drops from the current ~200-500ms
(verifier + kernel + fs + sqlite + gc) to ~10-30ms (three
small sqlite writes). At 16 contending workers that moves the
worst-case 16th-waiter wait from ~5-8s to ~200-500ms,
comfortably under the 30s timeout even at the tail.

## 6. Rollback semantics

The two-phase split introduces a new failure window: the
pre-lock work (`INSERT pending`, `BPF_PROG_LOAD`,
`PublishBytecode`) succeeds, then the commit region fails (lock
acquire times out, or sqlite write fails). The rollback path
mirrors the existing `cleanupLoaded` closure
(`manager/load.go:130`): on commit-region failure, walk back
through the pre-lock outputs:

- Unload the BPF program (kernel cleanup via
  `action.UnloadProgram`).
- Remove the bytecode directory
  (`action.RemoveMapsPins`, `action.RemoveProgramDir`).
- Delete the pending row outside the lock (sqlite handles its
  own serialisation; even if the lock acquisition timed out,
  this `DELETE` does not require the bpfman flock).

The undo actions are already defined in the operation plan
(`operation.UndoFrom` calls in `loadPlan`); the change is in
when they fire, not what they do.

Lock-timeout is a new failure mode for the pre-lock phase
(it cannot happen today). Rollback handles it identically to
any other commit-region error. No state is corrupt, and the
caller's error message points at the lock, not at the kernel.

## 7. Why not also tighten attach / detach

Both attach and detach for TC / XDP / TCX go through the
dispatcher chain, which is the only multi-program-aware
dispatcher mechanism on those hooks. Rebuilding the chain is
RMW across two surfaces: the kernel (which dispatcher BPF
program is currently attached to the interface) and the store
(which programs comprise the chain in what order). Two
concurrent rebuilds for the same interface produce a lost
update; the lock prevents that.

For non-dispatcher attach paths (tracepoint, kprobe, uprobe,
fentry, fexit, uretprobe) the lock is theoretically excessive
in the same way load is. That can be a follow-up after this
work lands and measures favourably; the dispatcher cases are
the harder design problem and stay in the lock by default.

## 8. Why not also tighten unload

Unload of a dispatcher-resident program needs to rebuild the
chain, so it stays in the lock for the same reason attach
does. Unload of a stand-alone program (no dispatcher) is
bookkeeping-only and a candidate for the same pre-lock /
commit-region split, but is left for a follow-up because the
corpus's lock pressure is dominated by load and attach, not
unload.

## 9. Schema changes

One new table:

```sql
CREATE TABLE program_pending (
    txn_id      TEXT    PRIMARY KEY,    -- pid:counter, unique per invocation
    kernel_id   INTEGER NULL,           -- bound after BPF_PROG_LOAD succeeds
    program_name TEXT   NOT NULL,
    source_path  TEXT   NOT NULL,
    started_at  INTEGER NOT NULL        -- unix nanos
);
CREATE INDEX program_pending_kernel_id ON program_pending(kernel_id);
```

`gc.Scan`'s orphan check becomes:

```sql
-- kernel program with no real record and no fresh pending row
SELECT kernel_id FROM kernel_view  -- enumerated via syscalls
WHERE kernel_id NOT IN (SELECT program_id FROM programs)
  AND kernel_id NOT IN (
      SELECT kernel_id FROM program_pending
      WHERE kernel_id IS NOT NULL
        AND started_at > <now - grace_period>
  );
```

`grace_period` defaults to 60s. A pending row older than the
grace period whose `kernel_id` is still set in the kernel is a
crashed-mid-load orphan and gc remediates both halves (kernel
unload + delete pending row).

## 10. Migration plan

Three commits, mirroring the auto-subnet arc:

1. *Schema + manager refactor.* Add `program_pending` table
   and migration. Add `Manager.LoadPhaseA(ctx, ...)` returning
   loaded program info + txn id, and
   `Manager.LoadPhaseB(ctx, writeLock, info, txn)` performing
   the commit. Internal-only, no caller change yet. Split gc
   into `Manager.GCScan(ctx)` and
   `Manager.GCRemediate(ctx, writeLock, orphans)`. Unit tests
   under `manager/load_test.go` cover both phases independently
   and the rollback between them.
2. *Wire the CLI.* `cmd/bpfman/load_file.go` calls
   `mgr.GCScan` lockless, then conditionally takes the lock for
   `GCRemediate` if needed, then calls `mgr.LoadPhaseA`
   lockless, then takes the lock for `mgr.LoadPhaseB`. The
   rollback closure that today fires on plan-step failure
   becomes the rollback closure that fires on commit-region
   failure. The user-visible CLI is unchanged.
3. *Measurement.* Re-run `e2e/parallel-scripts.sh -r 100 -j 16`
   on the dispatcher corpus and record the failure rate.
   Expected outcome: zero lock-timeout failures on `program
   load`. If a residual contention bucket appears it is
   attach-side dispatcher churn, which is the next candidate.

## 11. Alternatives considered

### Just bump the timeout

`--lock-timeout=120s` would absorb the queue depth at the cost
of slow failures. It does not move the structural throughput
ceiling (~3 jobs/sec aggregate at `-j16` because of the lock);
the system is still mutation-rate-bound. Rejected because
hiding contention is not fixing it.

### Lower default concurrency

`-j 4` would reduce lock pressure but also halves wall-clock
throughput on uncontended workloads. The harness already
exposes `-j`; this is the caller's lever, not the engine's
solution.

### Fine-grained locks (per-interface, per-program)

A separate lock per dispatcher target would let unrelated TC
loads on different interfaces proceed in parallel. The design
is straightforward (interface name -> flock path) but the
correctness analysis crosses sqlite tables that today implicitly
rely on a single writer. Rejected for v1 because the load
tightening is the cheaper win and unblocks the parallel-scripts
stress test without changing the locking semantics callers
depend on. Worth re-evaluating once the load split shows the
attach side is the next bottleneck.

### Keep `BPF_PROG_LOAD` inside the lock

The conservative shape: only split gc into scan and remediate;
keep the rest of the operation plan inside the lock. Easier
correctness story (no pending rows, no new race window). Loses
the bulk of the tightening because the verifier syscall is the
biggest contributor to lock-hold duration. Rejected for v1
because the throughput target is the verifier step coming out
of the lock.

### Pre-register a sentinel BPF program

Instead of a sqlite pending row, register a sentinel in the
kernel (e.g. a pinned dummy program) under the would-be id.
Race-free but invasive and adds an extra syscall pair per
load. The sqlite pending row achieves the same correctness
at much lower cost.

### gc as a daemon-mode background pulse

Already debated upthread. Rejected because gc must be
synchronous: we need to start from a known clean slate before
the per-invocation operation proceeds. A background pulse
gives eventual consistency, not the "clean slate at the
boundary" property load relies on for its kernel<->store
correspondence guarantee.
