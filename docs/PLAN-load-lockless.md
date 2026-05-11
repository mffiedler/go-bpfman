# Plan: lockless `bpfman program load`

## 1. What this document is

A working design memo for taking `bpfman program load` out of
the writer-lock critical section. Follow-on to
`docs/PLAN-load-lock-tightening.md`, which moved gc off the lock
(v1) and explicitly deferred this step (v2). v1 is committed and
shipping; this is the next move.

After v2 ships, `bpfman program load` (file or image, single or
multi-program) takes zero flocks. Two concurrent loads on
unrelated programs run fully in parallel. The motivation is
image-based load specifically: OCI pulls in the lock-held
region are an operational footgun for production, where image
distribution is the production-shaped load path.

Retire this file once v2 lands.

## 2. The central observation

Load is purely additive. It produces three new things, each
with its own namespace:

- A new kernel BPF program. The kernel assigns the id via
  `BPF_PROG_LOAD`; two concurrent callers get two distinct
  ids by the kernel's own allocator. No shared kernel state.
- A new directory `/run/bpfman/programs/<kernel_id>/` for the
  bytecode and provenance. Path is namespaced by the
  kernel-allocated id; two writes cannot collide.
- A new row in `programs` keyed by the same kernel id. The
  INSERT is non-conflicting on the primary key.

Load never reads existing shared state and conditionally
mutates it. There is no chain to rebuild, no counter to
increment, no row to compare-and-swap. The flock exists to
make a read-modify-write atomic from external observers'
perspective. With nothing to read-modify-write, the flock has
nothing to protect.

We were holding it during load because the manager's
mutation-entry pattern (`gcOnEntry` + `RunWithLock`) is
uniform across all mutations, not because load itself needed
serialisation. v2 drops the wrap for load and the load path
becomes lockless by definition.

## 3. Why this is safe by default

Three properties carry the correctness story; none of them
requires new protocol or schema:

1. **Disjointness across concurrent loads.** Kernel id
   uniqueness, namespaced bytecode dirs, and SQLite's primary
   key constraint together ensure that two concurrent loads
   never touch the same kernel program, the same filesystem
   path, or the same sqlite row.

2. **Readers consult the DB as the single source of truth.**
   `bpfman program list`, `get`, and audit dry-run all read
   sqlite. They never enumerate kernel state to surface drift,
   so they never see the in-flight kernel program that exists
   between `BPF_PROG_LOAD` and the load's commit transaction.

3. **Default gc respects the DB-as-SSOT model.** Looking at
   `manager/coherency/rules.go`, the rule that would reap a
   kernel program with no DB record is `PruneRule`, and it is
   opt-in via `GCOptions.Prune`. The gc-on-entry path
   (`gcOnEntry` -> `m.GC` -> `GCWithOptions(opts={})`) passes
   an empty `GCOptions`, so prune is never on. Default gc
   counts kernel-only orphans (the `LiveOrphans` field on
   `GCPlan`) and surfaces them in audit output, but does not
   remediate them. The in-flight kernel program a v2 load
   creates is safely invisible to default gc.

Together these mean: nothing in default operation looks at
kernel-only state and acts on it. The race I worried about in
an earlier draft of this doc (gc reaping an in-flight load's
kernel program) requires `--prune` to be explicitly opted into,
which is an operator-driven action and not a concurrent path
that would fire spontaneously.

## 4. The load path after v2

For a single-program load:

```
Resolve source                              # file stat or OCI pull
Parse ELF, build per-program load specs
BPF_PROG_LOAD                               -> kernel id Y
PublishBytecode to /run/bpfman/programs/Y/
BEGIN TRANSACTION
  INSERT INTO programs ... (kernel_id=Y, ...)
  (optional) INSERT INTO shared_map_pins ...
COMMIT
```

Every step is lockless from bpfman's flock perspective. The
sqlite COMMIT is serialised by sqlite's own writer mutex,
which is fine because the transaction is small and fast (one
or two row inserts).

For a multi-program load (`bpfman program load file --programs
A,B,C`), the existing batch loop in `manager/load.go` carries
over with two changes:

```
For each program in batch:
  BPF_PROG_LOAD                             -> kernel id Y_i
  PublishBytecode to /run/bpfman/programs/Y_i/

BEGIN TRANSACTION
  For each loaded program:
    INSERT INTO programs ... (kernel_id=Y_i, ...)
  (optional) INSERT INTO shared_map_pins ...
COMMIT
```

The batch's kernel and filesystem work runs sequentially per
program but lockless across processes. The commit transaction
folds N inserts into one sqlite write at the end.

## 5. Rollback semantics

The existing `cleanupLoaded` closure (`manager/load.go:130`)
handles batch rollback by walking back through the
partially-loaded programs: unload the kernel program and
remove its bytecode directory. v2 keeps this closure and just
moves the trigger condition: it fires on any failure before
the commit transaction. The closure runs lockless because the
kernel and filesystem cleanups are namespaced by kernel id
(same disjointness as the load itself).

If the COMMIT transaction itself fails, sqlite rolls back the
inserts atomically; we then run `cleanupLoaded` on all the
loaded programs (kernel unload + bytecode dir removal). All
lockless.

Crashed loads (process killed between `BPF_PROG_LOAD` and the
COMMIT) leave a kernel program and a bytecode dir with no
sqlite row. They are not reaped by default gc; an operator
running `bpfman audit checkup --repair --prune` cleans them
up. This matches the behaviour today for any mutation that
crashes mid-flight; v2 does not change it.

## 6. What stays in the flock after v2

- **Attach, detach, unload for TC / XDP / TCX.** Dispatcher
  chain rebuild is a read-modify-write across kernel state
  (which dispatcher BPF program is currently attached to the
  interface) and store state (the ordered chain of extension
  programs). Two concurrent rebuilds for the same interface
  produce a lost update; the flock prevents that. v2 does not
  change this.
- **Non-dispatcher attaches** (tracepoint, kprobe, uprobe,
  fentry, fexit, uretprobe). Less RMW-heavy but still take
  the flock for symmetry with v1's wiring. Could be made
  flock-free by the same argument as load if the analysis
  holds for each, but out of scope here.
- **Unload of a stand-alone program** (no dispatcher
  involvement). Same argument as load applies in reverse:
  unload is a pure deletion namespaced by kernel id. Likely
  also a candidate for flock removal, but deferred to keep
  v2's blast radius narrow.
- **gc.Remediate.** When it does have work, it mutates store
  state; the flock is the right coordination point and v2
  leaves it alone.

## 7. Migration plan

Two commits:

1. **Manager refactor.** Restructure `Manager.Load` so that
   the kernel and filesystem work (`BPF_PROG_LOAD`,
   `PublishBytecode`) runs before the sqlite transaction, and
   the transaction folds the per-program INSERTs (plus
   optional shared map pins) into one commit. The operation
   plan today already does its writes in a roll-back-safe
   shape via the operation runtime; the change is in the
   sqlite-commit grouping, not the kernel-and-fs sequencing.
   The public `Manager.Load` signature stays for backwards
   compatibility but the writeLock parameter becomes
   unused-by-construction; the comment documents that load no
   longer takes the lock.

2. **Drop the CLI wrap.** Replace
   `bpfmancli.RunMutationValue(ctx, cli, mgr, fn)` with a
   direct `fn(ctx, nil)` (or equivalent: stop acquiring the
   flock) for `bpfman program load file` and `bpfman program
   load image` in both `cmd/bpfman/` and `cmd/bpfman-shell/`.
   Other mutating handlers (attach, detach, etc.) keep their
   `RunMutation` wrap because they still need the flock for
   dispatcher rebuilds.

After both commits land, re-run the dispatcher corpus
(`e2e/parallel-scripts.sh -r 100 -j 16`) and an image-load
stress workload. Expected outcome: dispatcher corpus stays at
or below v1's 2 min wall (load wasn't the bottleneck for that
corpus; attach was). Image-load workloads, which previously
serialised on the pull inside the flock, become pull-bound
rather than lock-bound. Update this doc with whatever the
measurements show and retire it.

## 8. Alternatives considered

### Pending-row marker in sqlite

The earlier draft of this doc proposed a `program_pending`
table to let gc tell in-flight loads from crashed orphans.
Rejected on review: the race that motivated it (gc reaping an
in-flight load's kernel program) doesn't fire by default
because `PruneRule` is opt-in. Without that race, the
coordination is unnecessary, and an unnecessary schema change
is exactly the wrong shape.

The pending-row scheme would be the right answer if we changed
the default to auto-prune. The argument *for* that default
change is operational (crashed loads don't accumulate in the
kernel); the argument *against* is exactly the schema-change
cost. The two arguments are reciprocal: keep prune opt-in, no
schema needed; turn prune on by default, schema needed. v2
keeps the current default and is therefore schema-free.

### Time-based grace period in gc

The "wait N seconds before reaping a kernel-only program"
heuristic. Workable but heuristic. Becomes irrelevant once we
recognise that default gc doesn't reap kernel-only programs
at all.

### Daemon-mode request queue

Keeps load behind a request queue served by a long-running
daemon. No flock needed because the daemon is the single
writer. Rejected for the same reasons as in the v1 doc:
changes the deployment shape; orthogonal to the lockless-load
argument.

### Move just the OCI pull out of the flock, keep BPF_PROG_LOAD in

A narrower scope: source resolution moves out, the rest stays
in. Captures the worst-case OCI pull win without changing
load's flock semantics. Rejected because the analysis in
section 2 says the rest also has nothing to protect; doing
half the move would be an unjustified compromise.
