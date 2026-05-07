# Writer-lock granularity: where it bites and what to do next

## Summary

bpfman's writer lock is correct but coarser than it needs to be in
two distinct ways:

1. **Acquire is polling, not kernel-managed.**  `acquireWriter`
   loops on `flock(LOCK_EX|LOCK_NB)` with an exponential backoff
   sleep between retries. Every queued waiter pays at minimum the
   initial backoff before its first retry, regardless of how
   quickly the holder releases. This is a workaround for Go's lack
   of a clean way to interrupt a blocking flock on `ctx`
   cancellation.
2. **Hold scope is the entire client-facing call, not the
   linearisation point.**  `Manager.Load` (and similarly the
   attach / unload / detach paths) takes a `WriterScope` from its
   caller and holds it for everything between the API entry and
   exit, including ELF parsing, OCI image pull, spec building, and
   per-program rollback bookkeeping. Only the per-program
   kernel-load + DB write + fs-publish triple actually needs the
   lock to preserve the invariant `(kernel state, DB, fs publish)`
   are mutated atomically together.

The interaction between these two means contention is much worse
than the sum of the actual work-under-lock. See the
instrumentation paths below for how the picture was built.

## Where we are after the immediate fixes

`docs/PERF-LINK-DETACH-IS-ASYNC.md` covers a separate kernel-side
async-teardown class; this doc is about the userspace lock
specifically.

The recent commits in chronological order:

- `e2e: route runWithLock through lock.RunWithTiming` — every
  acquisition now logs `wait_ms` / `held_ms` at debug level when
  `BPFMAN_LOG=lock=debug` is set, tagged `component=lock`.
- `manager/operation: emit per-step ms timing at debug level` —
  every plan node logs `label`, `target`, and `ms` at
  `component=manager`. Crosswalk `component=lock` and
  `component=manager` to see which step inside a Manager call is
  consuming the held time.
- `e2e: tag lock-timing logs with op=<caller> and test=<t.Name()>`
  — each lock-timing log line carries the calling Manager method
  (LoadFile / Attach / Detach / Unload / GC) and the test name in
  shared-runtime mode. Aggregate by `op` to find the
  worst-contention call site.
- `platform/store/sqlite: pair WAL with synchronous=NORMAL` — was
  paying an `fsync` per commit, ~6 ms each. Changed to
  WAL+NORMAL, which is the documented safe/fast pairing.
- `lock: drop initial backoff from 25ms to 1ms` — cuts the floor
  on first-retry latency by 25x. Same exponential, same 500 ms
  cap.

End-to-end e2e wall-clock impact (NixOS 6.12, ext4):

|                          | isolated |  shared  |
|--------------------------|---------:|---------:|
| baseline (sync=FULL, 25ms backoff) | 11.3 s   | 20.4 s   |
| after sync=NORMAL                  |  9.74 s  | 13.64 s  |
| after sync=NORMAL + 1ms backoff    |  8.93 s  | 12.36 s  |

5-run sample at the latest state: isolated median 9.56 s, shared
median 12.75 s; isolated range 0.64 s, shared range 3.80 s. The
much higher variance in shared mode is queue-ordering luck.

## Where the residual cost is, after all fixes

Per-op aggregate over a single full shared-runtime suite run, with
all the above changes in:

| op       |   n | held_total | held_max | wait_total | wait_max |
|----------|----:|-----------:|---------:|-----------:|---------:|
| LoadFile | 152 |     274 ms |    30 ms |  46 011 ms |  8214 ms |
| Detach   | 329 |    3051 ms |    97 ms |  28 437 ms |  4110 ms |
| Attach   | 278 |    2124 ms |    48 ms |   2098 ms |  1026 ms |
| Unload   | 242 |      55 ms |    23 ms |   1039 ms |  1035 ms |
| GC       |   6 |      84 ms |    15 ms |       0 ms |     0 ms |

LoadFile's wait/held ratio is 168x. Average held is 1.8 ms;
average wait is 303 ms. The wait is no longer dominated by other
holders' work (we drove that down with sync=NORMAL); it is
dominated by the polling backoff.

## Issue 1: polling acquire

`lock/lock.go acquireWriter`:

```go
backoff := 1 * time.Millisecond
const maxBackoff = 500 * time.Millisecond

for {
    err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
    if err == nil { return f, nil }
    if err != syscall.EWOULDBLOCK { ... }

    select {
    case <-ctx.Done(): ...
    case <-time.After(backoff):
    }
    if backoff < maxBackoff { backoff *= 2 }
}
```

Every queued waiter incurs polling latency rather than kernel-
managed wakeup. Even at a 1 ms initial, 8 retries to hit the
500 ms cap: 1+2+4+8+16+32+64+128+256+500+500+... After the cap,
each subsequent retry is 500 ms regardless of when the holder
released. Wait_max of 8214 ms in the table above corresponds to
roughly 16 cycles at the cap.

### Right fix: `F_OFD_SETLKW` with signal-based ctx interrupt

`fcntl(F_OFD_SETLKW)` is the documented blocking, OFD (open-file-
description) advisory record lock on Linux. Two relevant
properties:

- It blocks in the kernel; the kernel wakes the waiter the moment
  the lock becomes available. No polling.
- It is per-fd rather than per-process, so it is safe to use from
  multiple goroutines without the multi-thread-flock gotcha.
- It is interruptible by a signal (returns `EINTR`), which is the
  hook for `ctx` cancellation.

The cancellation path:

1. Goroutine A calls `fcntl(F_OFD_SETLKW)`. Blocks in the kernel.
2. `select { case <-ctx.Done(): ... }` fires in goroutine B (the
   caller's ctx watcher).
3. B sends a signal to A's thread (`unix.Tgkill` or use Go's
   signal mechanism). A's `fcntl` returns `EINTR`.
4. A reports `ctx.Err()` back.

Care points: signals in Go are global; the mechanism most
projects converge on is to use a dedicated OS thread for the
blocking syscall (`runtime.LockOSThread`) so the signal targeting
is precise. An alternative is to write a tiny helper in a child
process and pass the lock fd via SCM_RIGHTS, but that is much more
machinery for the same effect.

### Cheap alternative: shorter initial backoff (already shipped)

We dropped the initial from 25 ms to 1 ms. Same shape, lower
floor. Worth ~10 % on the suite. Worth keeping even after the
proper fix lands, because the proper fix needs a fallback for the
rare case the kernel API is unavailable (e.g. very old kernels
without OFD locks).

## Issue 2: lock scope is the whole client call

Today, `e2e/helpers.go` (and equivalently the daemon's RPC
handlers) wrap the whole `Manager.Load`/`Attach`/etc in
`runWithLock`:

```go
func (e *TestEnv) LoadFile(ctx, path, programs, opts) (...) {
    return e.runWithLock(ctx, func(ctx, scope) error {
        return e.Manager.Load(ctx, scope, source, programs, opts)
    })
}
```

`Manager.Load` then runs:

```go
resolveBatchSource(ctx, source)   // file stat OR OCI image pull
resolveBatchPrograms(...)         // ELF parse, validation
buildLoadSpecs(...)               // pure CPU

for spec := range specs {
    operation.Run(loadPlan(spec)) {
        loaded                    // kernel BPF prog load
        db-consistency-check      // sqlite SELECT
        fs-publish                // bytecode copy + provenance
        store-save                // sqlite INSERT
        save-shared-maps          // sqlite INSERT
    }
}
```

The lock is held for the *entire* function. That includes:

- **resolveBatchSource** doing a file stat or, for OCI, a *network
  image pull* of arbitrary duration.
- **resolveBatchPrograms** doing ELF parsing and validation.
  Sub-millisecond on small e2e bytecode; can be 10s of ms on
  larger production object files.
- **buildLoadSpecs**, pure CPU.

None of these touch shared mutable state (kernel, DB, or pinned
fs). The lock only needs to cover the per-program operation
block, which is the linearisation point: the kernel ID is
allocated, the DB row is inserted, and the bpffs pin is published,
all of which need to be mutually consistent.

### What changes if we move the prep work outside the lock

Today's e2e numbers (after sync=NORMAL+1ms backoff):

- LoadFile held_avg = 1.8 ms. This is *small* because the e2e
  test data is tiny (~10 kB BPF objects) and prep work compiles
  away.

So the *e2e suite* would not improve much from this refactor on
its own; the polling tax (issue 1) dominates. Where the refactor
matters:

- **Production OCI loads.**  An image pull from a registry can be
  many seconds. Today it is multi-second under the writer lock,
  blocking every other client. Moving the pull outside is a clean
  multi-second win for unrelated clients.
- **Large bytecode.**  Production objects can be hundreds of kB
  and ELF parsing is non-trivial. Same shape as the OCI case.
- **Worst-case bound on lock hold.**  Even if our current per-op
  work is fast, defensive scope reduction means an unexpectedly
  slow step (filesystem sync hiccup, sqlite checkpoint stall,
  etc.) only blocks the lock for the actual mutation, not for the
  prep work.

### What's hard

The rollback path (`cleanupLoaded` in `manager/load.go`) depends
on every program already loaded *during this batch* being visible
to the rollback `Unload` calls. If two concurrent
`Manager.Load` calls interleave at program granularity (lock
released between programs), the rollback model has to handle a
state where program 5 of 7 has been loaded by client A, programs
1-3 of 4 have been loaded by client B, A's program 5 fails, A
needs to roll back its 1-4 only, B is still in flight. This is
solvable (each batch tracks its own loaded set, the lock-acquire
is per-program but the cleanup-set is per-batch), but it's where
the care is needed.

The narrower change that avoids this concern: hold the lock for
the *whole batch* still, but only acquire it after the prep work.
That is, change `Manager.Load` from "take the WriterScope passed
in and run everything under it" to "do prep, acquire internally,
run the batch, release."  This:

- Keeps batch atomicity unchanged (the batch is still under one
  lock-hold).
- Moves the heavy prep work (resolveBatchSource, especially for
  OCI) outside.
- Requires changing the API: `Manager.Load` no longer takes a
  `WriterScope`; it manages the lock itself. Affects every
  caller.

The wider change: per-program lock acquire inside the batch loop.
Higher concurrency (two clients can interleave program-by-program)
but the rollback complication above.

## Suggested order

1. **Switch acquire to `F_OFD_SETLKW`** with signal-based ctx
   interrupt. Removes the polling tax entirely. Biggest single
   win on the existing test suite. Self-contained in
   `lock/lock.go`. Half a day of careful Go-runtime / signal
   handling work plus a stress test under `-test.count=N`.

2. **Move prep work outside the lock in `Manager.Load`** by
   inverting the `WriterScope` contract. Same atomicity, smaller
   lock-hold scope. Affects all callers (`e2e/helpers.go`, daemon
   RPC handlers). Half a day to a day, with care to migrate
   every call site cleanly. Biggest production win for OCI image
   pulls and large bytecode.

3. **Per-program lock acquire** inside the batch loop. Highest
   concurrency, but introduces the rollback-set bookkeeping
   described above. Worth re-evaluating after (1) and (2) are in;
   if the wait_total has come down to single-digit seconds across
   a suite, this might not be needed.

## How to reproduce the measurements

```sh
# build without race detector so timing reflects production
make bin/e2e.test

# shared mode (default)
time sudo ./bin/e2e.test -test.failfast > /dev/null

# isolated mode
time sudo BPFMAN_E2E_ISOLATED_RUNTIME=1 ./bin/e2e.test -test.failfast > /dev/null

# shared mode with full lock + step instrumentation captured
sudo BPFMAN_LOG=warn,lock=debug,manager=debug \
     ./bin/e2e.test -test.failfast 2>/tmp/lock_trace.log > /dev/null

# per-op aggregate
awk '
  match($0, /op=([A-Za-z?]+)/, o) {
    op=o[1]
    if (match($0, /held_ms=([0-9]+)/, h)) { hn[op]++; hsum[op]+=h[1]; if (h[1]>hmax[op]) hmax[op]=h[1] }
    if (match($0, /wait_ms=([0-9]+)/, w)) { wn[op]++; wsum[op]+=w[1]; if (w[1]>wmax[op]) wmax[op]=w[1] }
  }
  END {
    printf "%-12s %5s %12s %10s %12s %10s\n","op","n","held_total","held_max","wait_total","wait_max"
    for (op in hn) printf "%-12s %5d %12d %10d %12d %10d\n", op, hn[op], hsum[op], hmax[op], wsum[op], wmax[op]
  }
' /tmp/lock_trace.log

# top outliers
awk '
  /held_ms|wait_ms/ {
    val=0; metric=""
    if (match($0, /held_ms=([0-9]+)/, m)) { val=m[1]; metric="held" }
    if (match($0, /wait_ms=([0-9]+)/, m)) { val=m[1]; metric="wait" }
    if (val<100) next
    if (match($0, /test=([A-Za-z_]+)/, t)) tn=t[1]; else tn="?"
    if (match($0, /op=([A-Za-z?]+)/, o)) on=o[1]; else on="?"
    printf "%6d %-4s %-50s %-12s\n", val, metric, tn, on
  }
' /tmp/lock_trace.log | sort -n -r | head
```
