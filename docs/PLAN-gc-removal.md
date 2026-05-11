# Plan: rip out audit and gc

## 1. What this document is

A working design memo for removing the audit and gc machinery
from bpfman entirely. After this change there is no `bpfman gc`,
no `bpfman audit`, no coherency rule engine, no `gcOnEntry` on
the mutation path, and no reconciliation between DB, kernel, and
filesystem performed by bpfman at runtime. The DB is the source
of truth for "what's managed"; mutations are atomic at the flock
boundary plus SQLite ACID; everything outside that is operator
territory, with the same kernel and filesystem tools the operator
already uses.

This is a deletion task. Nothing new lands in its place except,
optionally and separately, a small `bpfman doctor` that prints
the DB view alongside the kernel and filesystem views with no
actions and no rule engine. That is not part of this change.

Retire this file once the change lands.

## 2. The position

Audit and gc exist to reconcile state across three sources of
truth: the SQLite DB, the kernel, and the bpffs/bytecode tree.
The premise was that any of the three could drift from the
others -- through crashes, recycled kernel IDs, external
interference -- and bpfman should detect and repair the drift on
the way through each mutation. The implementation grew a rule
engine, a dry-run transaction pattern, an intent / action lowering
layer, and a coherency-state gather pipeline that runs on every
CLI invocation.

Three things make that scaffolding the wrong shape for bpfman as
it stands now.

First, drift is rare. The flock makes attach, detach, unload, and
the dispatcher operations atomic at the flock boundary; load is
atomic at SQLite's commit. The kernel will not recycle a program
ID while the bpffs pin holds the reference -- and the pin lives
exactly as long as the bytecode dir, modulo external `rm`. The
failure modes the rules were designed to detect are dominated by
"kill -9 mid-operation" and "operator manually deleted state",
both of which are rare and recoverable with kernel and shell
tools the operator already has.

Second, the runtime cost is non-trivial. A full coherency gather
syscalls per-program over `BPF_PROG_GET_NEXT_ID` (every program
on the host, not just bpfman's), walks the bpffs and bytecode
trees, and fans out a handful of sqlite queries. Twice per
mutation, in the current `RunMutationValue` shape (lockless
pre-scan plus under-flock re-scan). The parallel-scripts harness
shows roughly a 35% wall-clock reduction with `BPFMAN_NO_GC=1`,
on a workload that produces nothing for gc to react to.

Third, the rules encode invariants the surrounding code is then
obliged to maintain. The v2 lockless-load work broke one such
invariant (DB-kernel-fs consistency outside a mutation) and the
race that followed produced false-positive reaps that destroyed
live programs. Each rule we keep is a constraint on every future
mutation design.

The cost of keeping audit and gc is high, and the value they
deliver -- self-healing through coherency reconciliation -- is
narrowly applicable to failure modes the operator can already
recover from manually.

## 3. What goes

### Code

- The `bpfman audit` and `bpfman gc` CLI commands.
- `Manager.GC`, `Manager.GCScan`, `Manager.GCRemediate`,
  `Manager.ExecuteGC`, `Manager.GCWithOptions`, `gcOnEntry`,
  `WithGCDone`, `opActiveKey`.
- The `manager/coherency` package in full: rules, intents,
  gather, action lowering.
- The dry-run transaction inside `GCScan`.
- `internal/bpfmancli.RunMutationValue`, `RunMutation`,
  `RunBatchMutation`. Mutations call `RunWithLock` /
  `RunWithLockValue` directly.
- The Phase B flock that this branch added around the load
  commit. Load returns to fully lockless, per
  `docs/PLAN-load-lockless.md` v2.
- The patches to `inspect.Snapshot` (store-first reorder) and
  the `orphan-program-dirs` `KernelAlive` filter. Both were
  added on this branch to tolerate the in-flight states that
  the rules misread; with no rule engine, neither carries
  weight.
- The `BPFMAN_NO_GC` env var.
- Bpfman-rpc daemon startup gc call.

### Concepts

- Live orphans, prune rule, recycled-ID detection (the
  `InStore && !InFS` shape).
- The idea that bpfman maintains coherency across the three
  state sources at runtime.

### Tests

- `manager/coherency/*_test.go`.
- `manager/gc_*_test.go`, `manager/audit_*_test.go`.
- e2e tests that assert audit/gc behaviour. The dispatcher tests
  have an implicit reliance on cross-test gc on the shared
  runtime; if any fail, fix the teardown to be explicit rather
  than re-adding the engine.

## 4. What we accept

### Crash residue is a benign leak

A `kill -9` after a load's Phase A completes (kernel program
loaded, bpffs pin in place, bytecode dir published) but before
or during the Phase B commit leaves the kernel program alive,
the pin in place, the bytecode dir on disk, and no DB row. The
kernel will not recycle the ID while the pin holds, so the
program is unmanageable but harmless: subsequent loads receive
fresh IDs, no double-mapping, no traffic redirection.

Recovery is operator-driven. `bpftool prog show` enumerates the
kernel side; `ls /run/bpfman/programs/` enumerates the bytecode
side; subtracting `bpfman program list` from either gives the
leak set. `rm -rf` of the bytecode dir plus dropping the pin
unloads the program and removes the residue. Or the operator
ignores it; the leak is bounded by however many crashes
happened.

Same shape applies to attach/detach/unload crashes, with one
sharp exception in section 5.

### Recycled-ID detection is gone

If an external actor unpins a bpfman program (manual `rm` of
the bpffs pin), the kernel frees the program once references
drop and may recycle the ID. A subsequent third-party load
might receive the same ID. Our DB row then points at a
stranger's program, and `bpfman program get X` returns the
stranger's name and tag.

This requires external interference -- a kernel pin survives
process death, so bpfman itself does not produce this state.
Operators who suspect drift compare `bpfman program list`
against `bpftool prog show`.

### No bpfman-side reporting of drift

`bpfman audit` was the "tell me what's weird" command. After
this change it does not exist. Operators investigate by
composing `bpfman list`, `bpftool prog show`, `ls
/run/bpfman/programs/`, and `mount | grep bpffs` themselves.
This is more verbose than running one command, and that is the
trade-off.

A `bpfman doctor` command that prints the three views side by
side with no rule engine and no actions is a separate, scoped
follow-up. Not part of this change.

## 5. The dispatcher sharp edge

Attach and detach for XDP, TC, and TCX rebuild a dispatcher BPF
program on each call. The sequence under the flock is: compute
the new member set, build the new dispatcher BPF program, pin
it, run the DB transaction (snapshot row, members, link), swap
the kernel attachment from old to new, drop the old dispatcher's
artefacts. The DB transaction is the only atomic boundary; the
kernel-side steps around it are not.

A kill mid-attach can leave any of: a new dispatcher pinned with
no DB reference (leak), a DB row pointing at a dispatcher kernel
ID that exists but is not the one currently attached to the
interface (functional mismatch), or an old dispatcher's link pin
lingering in fs with no DB row (leak).

Today the coherency rules detect and repair these. Without the
rules, recovery is manual: operators identify the inconsistency
and fix it with `bpftool` and `rm`. For shared-host production
deployments the burden is real.

This is independent of audit/gc removal. Even with audit/gc in
place, the dispatcher sequence is not cleanly atomic at the
kernel boundary; the rules paper over the gap with retry and
remediation logic. The correct fix is making attach atomic at
the kernel boundary -- staging the new dispatcher fully, then
flipping kernel attachment and DB state together with a
deterministic rollback on either side's failure. That is a
separate plan doc.

For this change, we accept that dispatcher crashes leave residue
that the operator clears manually. If experience shows the
burden is too sharp, the response is the atomic-attach redesign,
not bringing back audit/gc.

## 6. What we preserve

- `bpfman program load`, `bpfman program unload`,
  `bpfman program list`, `bpfman program get`.
- `bpfman link attach`, `bpfman link detach`, `bpfman link
  list`, `bpfman link get`.
- The flock at `/run/bpfman/.lock` as the GIL for attach,
  detach, unload, dispatcher mutations.
- Load fully lockless, per `docs/PLAN-load-lockless.md` v2.
- SQLite as the SSOT for "what's managed". ACID transactions
  remain the DB-side atomicity boundary.
- All read paths. None of them depend on coherency state today
  -- they consult the DB.

## 7. Migration

One commit, scoped to deletion. No incremental landing, no
compatibility shims, no feature flag.

Order inside the commit:

1. Remove the `bpfman gc` and `bpfman audit` CLI subcommands.
2. Remove the manager surface (`Manager.GC` and friends,
   `gcOnEntry`, `WithGCDone`).
3. Delete `manager/coherency`.
4. Collapse `internal/bpfmancli` mutation helpers; update call
   sites in `cmd/bpfman`, `cmd/bpfman-shell`, `server`, and the
   e2e test fixtures to call `RunWithLock` /
   `RunWithLockValue` directly.
5. Remove the Phase B flock from `Manager.Load`.
6. Revert the `inspect.Snapshot` reorder and the
   `orphan-program-dirs` filter patch on this branch. (If the
   coherency package is deleted in step 3, the orphan-rules
   revert is a no-op; `inspect.Snapshot` is independent and
   needs its own revert.)
7. Drop the `BPFMAN_NO_GC` env var.
8. Remove the daemon-startup gc invocation in `bpfman-rpc`.
9. Delete the corresponding tests.

After the commit, run `make`, `make test`, and `make test-e2e`.
The e2e suite is the load-bearing check: it exercises every
mutation path on real kernels and will catch any latent
dependency on gc-driven cleanup.

The dispatcher tests are the riskiest. They run on the shared
runtime by default and have been relying on gc to clear residue
between tests. If they regress, the fix is making per-test
teardown explicit -- not re-adding the engine.

## 8. Open questions

- Is `bpfman doctor` needed immediately, or do we wait until an
  operator asks for it? Lean: wait. The command is small enough
  to add later, and adding it now anchors a feature we don't
  yet know the shape of.
- Does any production deployment rely on the daemon's
  startup-gc as a recovery mechanism? If so, the operator
  workflow changes -- crash recovery moves from automatic to
  manual. Surface this before landing if there is a known
  consumer.
- Do the dispatcher tests pass without gc clearing residue
  between cases on the shared runtime? Verify by running them
  before committing.
