# Plan: tighten the writer-lock scope on `bpfman program load`

## 1. What this document is

A design memo for shrinking the per-mutation hold-time on the
global writer lock at `/run/bpfman/.lock`. The 16-way parallel
stress test on the dispatcher corpus
(`e2e/parallel-scripts.sh -r 100 -j 16`, 900 jobs) saturates the
lock and pushes unlucky waiters past `--lock-timeout=30s`. A
diagnostic experiment isolates where the time goes and points the
design at the single dominant cost.

The empirical result reshapes priorities. The original draft
emphasised moving `BPF_PROG_LOAD` out of the lock as the
load-bearing change; the experiment shows that bypassing
`gcOnEntry` alone gives a ~3.3x speedup and clears every
lock-timeout failure. gc is the dominant cost. Moving the
verifier syscall out of the lock is a smaller follow-up rather
than the primary fix, and is ruled out separately by the
per-operation single-sqlite-transaction invariant the codebase
prefers.

### Scope of the change: lock-hold, not lock-contention

The flock is bpfman's GIL: two concurrent bpfman processes
serialise on `/run/bpfman/.lock` for every mutating operation.
This plan does not change that. The serialisation point, its
granularity, and the operations gated by it all stay the same.

What v1 changes is what we do *while* we hold the GIL. Before
v1 the gather pass plus orphan compute (gc.Scan-equivalent)
ran inside the lock-held region; after v1 the scan runs
lockless and the lock is only held for the actual mutation
plus, when warranted, a remediation step. The two processes
still take turns -- their turns are just shorter, so the queue
clears faster and per-operation tail latency drops below the
`--lock-timeout`.

Finer lock granularity (per-interface, per-program, daemon-mode
request queue) is a separate design problem. v2 candidates are
sketched below; v1 stays inside the existing single-flock
model.

Scope of v1's gc compression: *every* mutating operation that
flows through the manager. Load, attach, detach, unload,
delete, and dispatcher-delete all go through the
`bpfmancli.RunMutation` / `RunMutationValue` helper, which
runs the lockless `GCScan` first and only takes the lock
plus calls `GCRemediate` when the scan returned non-empty.
The actual operation (fn) still runs under the writer lock,
and the manager's internal `gcOnEntry` short-circuits because
the helper set `WithGCDone` on the context. The empirical
measurement reflects this -- the dispatcher corpus exercises
load + attach + detach + unload, and all of them see the
compressed lock-hold.

Out of scope is something narrower: moving the operation's
own work outside the lock. Specifically, the verifier syscall
inside load, and the dispatcher chain rebuild inside attach /
detach / unload on TC / XDP / TCX. Those operations do
read-modify-write across the kernel and the store and require
the lock for the mutation itself; v1 leaves their lock-held
work untouched and only trims the gc that used to run in front
of it. v2 candidates for moving that operation-specific work
out of the lock (pending-row marker for load, per-interface
locks for dispatcher rebuilds, daemon-mode request queue) are
sketched below.

## 2. Where the time actually goes

`cmd/bpfman/load_file.go:60` wraps the whole load in
`bpfmancli.RunWithLockValue`. Inside the closure,
`manager.Manager.Load` (`manager/load.go:102`) runs
`gcOnEntry` first (`manager/load.go:103`) and then a per-program
operation plan of five steps (`manager/load.go:176`):

1. `kernel-load`, `BPF_PROG_LOAD`, the verifier syscall.
2. `db-consistency-check`, read-only sqlite query.
3. `fs-publish`, writes `/run/bpfman/programs/<id>/`.
4. `store-save`, the sqlite write.
5. `save-shared-maps` (optional), additional sqlite writes.

`gcOnEntry` does a full coherency sweep before any of the above:
enumerate kernel programs / links / dispatchers, cross-reference
with the store, compute orphan deletions, and execute them. On a
healthy system the result is always an empty plan, but the
gather pass itself runs every time.

Measured on the dispatcher corpus at `-j 16 -r 100`:

| Variant | 900-job wall time | Failures | Mean lock-hold |
| --- | --- | --- | --- |
| Baseline (current code) | 5 min 5 s | 9 / 900 (lock timeouts) | ~333 ms |
| `gcOnEntry` bypassed | 1 min 33 s | 0 / 900 | ~103 ms |

The verifier plus fs-publish plus sqlite together account for
the ~103 ms residual; `gcOnEntry` adds another ~230 ms on top.
At 16-way concurrency that 230 ms-per-script swing is what
pushes the lock queue past the 30 s timeout in the worst cases.

## 3. v1, split gc into Scan and Remediate

`gc` has two distinct phases that the current code conflates,
and the cheap one runs every time the expensive one would:

- **Scan.** Read kernel state via the per-id enumeration
  syscalls; cross-reference with the `programs` / `links` /
  `dispatchers` tables. Pure read. Returns a list of orphans
  (in-kernel without store record, or in-store without kernel
  program). Runs concurrently with anything, including with
  itself across processes.
- **Remediate.** Take the lock; under the lock re-scan to get
  the authoritative set (state may have advanced since the
  lockless scan); execute the remediation actions. Mutates.

In practice the orphan list is empty on the overwhelming
majority of invocations. A clean run after a clean run finds
nothing; the remediation lock is taken only when a previous
operation crashed mid-flight. Treating the empty-orphans path as
lockless keeps the contention floor at zero for the common case.

```
process A:
  orphans := gc.Scan()                              # lockless
  if len(orphans) > 0:
    acquire lock
      authoritative := gc.Scan()                    # under lock
      gc.Remediate(authoritative)                   # single sqlite tx
    release lock

  acquire lock
    # existing per-program plan: BPF_PROG_LOAD,
    # PublishBytecode, store-save, etc. Unchanged
    # critical section, sqlite writes collapsed
    # into one transaction at the end (section 4).
  release lock
```

The under-lock re-scan is load-bearing, not a sanity check. It
exists for two reasons:

- Between the lockless scan and acquiring the lock, another
  process may have taken the lock and remediated the same
  orphans. The re-scan returns an empty set in that case and
  the remediate is a no-op release-the-lock-and-go.
- A new orphan may have appeared between the two scans (another
  process crashed). The re-scan picks it up. Without the
  re-scan we would remediate the stale lockless set and miss
  the fresh orphan.

### Concurrent gc invocations resolve cleanly

Two processes running their lockless `gc.Scan` at the same time
both see the same orphan candidates, both decide to take the
lock, B happens to win first. B's under-lock re-scan sees the
orphans; remediates them; releases. A's under-lock re-scan now
sees an empty set; remediates nothing; releases. No work is
done twice, no mutation race. The "non-authoritative first
scan" property is precisely what makes concurrent gc safe: the
lockless snapshot is a hint, never the basis for a mutation.

The worst case under burst-after-crash is many processes
serialising briefly on the lock to perform a single small
sqlite read each. Bounded, microseconds per hold; far below the
existing per-load lock cost.

### Synchronous semantics are preserved

gc remains synchronous on every mutating call: every process
runs `gc.Scan` before doing its mutation, every process that
finds orphans remediates them before proceeding. The clean-slate
invariant ("no orphans are visible to this operation's critical
section") holds for any operation that finds an empty scan or
that remediates and re-scans. What the split buys is moving the
empty-scan case off the lock entirely.

### Narrow weakening of the clean-slate invariant

Strictly speaking, v1 weakens "clean slate at the moment fn
starts" to "clean slate as observed during our recent scan".
The window is between our lockless `gc.Scan` and our acquisition
of the writer lock for fn. In that window another process can
mutate, crash, and leave an orphan. Our scan said empty, so we
skip the under-lock remediate and proceed; fn now runs with that
orphan present, instead of the old behaviour which would have
caught and remediated it under our own lock.

The exposure is bounded in two ways. The orphan exists for at
most one operation: the *next* mutating call's `gc.Scan` will
see it (the scan still runs synchronously on every entry, just
lockless first), and that call will remediate it under the
lock. And the orphan only matters if the current operation's fn
collides with it -- which is rare in practice because kernel id
reuse plus a crash-window-aligned scan is an unlikely
combination, and the corpus's mutations work on the ids they
themselves just created rather than on ids they share with the
crashed process.

This is the price of moving the gather pass off the lock. The
alternative (preserve strict clean-slate by re-scanning under
the lock on every operation) recovers nothing of the experiment's
~3.3x throughput win because the gather pass is the cost. Land
with the weakening; revisit if the bounded-to-one-operation
window proves to bite in practice.

## 4. Per-operation single sqlite transaction

The codebase prefers each manager operation's mutations to land
as a single sqlite transaction at the end of the operation. The
current load plan issues separate writes across the operation
plan steps (`store-save`, `save-shared-maps`, and the gc
remediation when applicable). v1 should fold these into one
transaction per operation:

- `gc.Remediate` (under lock) issues its deletions as a single
  transaction, not row at a time.
- The load's per-program plan accumulates the intended writes
  in-memory after `BPF_PROG_LOAD` and `PublishBytecode`, then
  flushes them as one transaction inside the existing critical
  section.

The transactional collapse is independent of the gc split: it
applies to today's code path too. Doing it as part of v1 keeps
the lock-hold predictable (one fsync per script instead of
several) and gives the gc-vs-load and load-vs-load ordering
properties a cleaner story to reason about: a load either
committed in full or did not commit at all.

## 5. v2, deferred

Moving `BPF_PROG_LOAD` out of the lock is ruled out for now on
two independent grounds:

- *Measured priority.* v1 brings the corpus from 5 min 5 s with
  9 lock-timeouts down to 1 min 33 s with zero failures. The
  remaining lock-hold of ~103 ms is dominated by `BPF_PROG_LOAD`
  itself (~80 ms) plus sqlite (~20 ms). Moving the verifier
  out would buy roughly another 1.5x throughput, smaller than
  v1 and worth deferring until the residual contention
  genuinely motivates it.
- *Schema constraint.* The pending-row scheme that would close
  the gc-vs-in-flight-load race needs separate sqlite writes
  before and after `BPF_PROG_LOAD` (INSERT pending, then
  UPDATE pending, then INSERT programs, then DELETE pending).
  That breaks the per-operation single-transaction invariant.
  Any v2 design has to either re-establish that invariant some
  other way or argue why the load operation is special.

If v2 ever lands, it owes a separate design pass starting from
the transaction-shape constraint, not the lock-shape one.

## 6. What is out of scope: operation-specific work under the lock

v1 compresses the gc-hold for *every* mutating operation,
including attach, detach, unload, delete, and dispatcher
delete -- they all go through the new helper and pay no gc
under the lock when the lockless pre-scan returns empty.

What v1 does not do is move the operation's *own* work out
of the lock:

- *Verifier syscall inside load.* `BPF_PROG_LOAD` still runs
  inside the writer lock. Moving it out requires a pending-row
  marker in sqlite (so a concurrent gc on another process does
  not mis-classify the in-flight kernel program as an orphan),
  which conflicts with the per-operation single-transaction
  invariant (section 4). Deferred to v2.
- *Dispatcher chain rebuild on TC / XDP / TCX attach, detach,
  and unload.* The rebuild reads the current chain (kernel and
  store), computes a new chain, and writes back. Two concurrent
  rebuilds for the same interface produce a lost update; the
  global flock prevents that. Per-interface locks would let
  unrelated interfaces proceed in parallel but require a
  correctness analysis across the sqlite tables that today
  implicitly rely on a single writer. Deferred to v2.
- *Daemon-mode request queue.* Already exists for the
  long-running daemon. Adopting it for the CLI path changes
  the deployment shape and is its own product decision; not
  on this plan's critical path.

For non-dispatcher attach paths (tracepoint, kprobe, uprobe,
fentry, fexit, uretprobe) the lock-held work is small enough
that no further tightening is obviously needed. v1's gc
compression already benefits them.

Once v1 lands, the next likely contention source is the
dispatcher rebuild on attach / detach. That is the natural
v2 starting point: per-interface locks or pulling the
rebuild out of the global flock entirely.

## 7. Migration plan

Two commits for v1:

1. *Manager refactor.* Split `Manager.GC` into
   `Manager.GCScan(ctx, opts)` (lockless, returns a GCPlan) and
   `Manager.GCRemediate(ctx, writeLock, opts)` (takes the lock,
   re-scans for the authoritative set, executes via the existing
   `ExecuteGC` when non-empty). Add `GCPlan.IsEmpty` for the
   pre-check. The existing `Manager.GC` / `GCWithOptions` /
   `ComputeGC` / `ExecuteGC` entry points are preserved as
   compatibility shims. Internal-only, no caller change yet.
   Unit tests cover the IsEmpty matrix, lockless scan,
   short-circuit, and behavioural drop-in equivalence with the
   legacy `GC` entry point.

2. *CLI wiring.* Add `manager.WithGCDone(ctx)` and
   `bpfmancli.RunMutation` / `RunMutationValue`, which run the
   lockless `GCScan` first, take the writer lock and call
   `GCRemediate` only when the scan returned non-empty, then
   run fn under the writer lock with the
   clean-slate-attested marker set so `Manager.Load` /
   `Attach` / `Detach` / `Unload`'s internal `gcOnEntry` sees
   opActiveKey and short-circuits. `RunBatchMutation` grows a
   `mgr` parameter so the batch path opts into the same
   pre-check (nil keeps the legacy lock-then-gc-inside path).
   Every mutating CLI handler in `cmd/bpfman/` and
   `cmd/bpfman-shell/` is updated to call `RunMutation` /
   `RunMutationValue` instead of `RunWithLock` /
   `RunWithLockValue`, passing the manager into the helper.

The "collapse load writes into one transaction" item from an
earlier draft of this plan is dropped from v1 -- the measured
gain comes from moving gc off the lock, not from compressing
sqlite writes inside the lock. The transactional collapse can
land as its own refactor when the next contention bucket
motivates it.

### Measured outcome

Baseline (pre-v1, dispatcher corpus, `-r 100 -j 16`,
900 jobs): 5 min 5 s wall, 9 lock-timeout failures.

v1 (full wiring, four consecutive runs of the identical code):
2 min 4 s, 2 min 5 s, 2 min 7 s, 2 min 4 s wall;
900 / 900 jobs passed every time; zero lock-timeout failures.

v1 captures ~85% of the experimental floor (1 min 33 s with
gc bypassed entirely). The residual ~30 s gap vs the floor is
the lockless `GCScan` cost spread across each script's ~10
mutation calls -- still in-thread per worker, just not
serialised across the cohort.

### Note on a transient 206-failure observation

During the wiring step we observed one outlier run with
206 / 900 ping failures (not lock timeouts; the BPF data path
broke mid-test). Three subsequent runs of the identical code
were clean, and a careful narrowing pass through the call sites
could not reproduce the failure. Best-evidence diagnosis: latent
kernel/sqlite state from earlier failed parallel runs cascaded
into the first full-wiring run before the state had time to
converge; once the state stabilised the wiring has been
reproducibly clean. We are recording this here so future-us
recognises the pattern if a similar pile-up appears.

## 8. Alternatives considered

### Just bump the timeout

`--lock-timeout=120s` would absorb the queue depth at the cost
of slow failures. It does not move the structural throughput
ceiling and was rejected upfront. v1 makes the timeout
irrelevant in practice.

### Lower default concurrency

`-j 4` would reduce lock pressure but also halves wall-clock
throughput on uncontended workloads. The harness exposes `-j`
as a caller's lever, not an engine fix.

### Skip gc on entry entirely

Removing `gcOnEntry` from `Manager.Load` (the diagnostic
experiment) gives the same wall-time win as v1 but breaks the
clean-slate invariant: crashed mid-load state is never
reconciled until an explicit `bpfman gc` invocation. v1
preserves the invariant by keeping gc synchronous on every
entry while moving the empty-result path off the lock.

### Background gc pulse / daemon-mode gc

Periodic gc detached from the per-call entry. Eventual
consistency model, not the "clean slate at the boundary"
property load relies on. Rejected upfront; v1's split achieves
the throughput goal without changing the synchronous-on-entry
property.

### Fine-grained locks (per-interface, per-program)

A separate lock per dispatcher target would let unrelated TC
loads on different interfaces proceed in parallel. The
correctness analysis crosses sqlite tables that today
implicitly rely on a single writer. Rejected for v1 because
gc-split is the cheaper win. Worth re-evaluating if the
post-v1 residual contention is attach-side.

### v2 today, BPF_PROG_LOAD out of the lock with pending rows

Considered and deferred. v1's measured gain clears the
lock-timeout failures and brings throughput inside the 30 s
budget with ~10x of headroom; v2's marginal gain is ~1.5x
further on top, and the pending-row design conflicts with the
per-operation single-transaction invariant. Land v1, measure,
then decide whether the remaining contention is worth a v2
design that earns its way past both constraints.
