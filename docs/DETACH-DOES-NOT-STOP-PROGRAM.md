# Detach Does Not Always Stop the BPF Program

## Symptom

After `Manager.Detach(linkID)` returns successfully, the kernel keeps
invoking the BPF program on its attach surface.  The bpfman view is
consistent (`ListLinks` no longer reports the link, `GetLink` returns
not-found), and the kernel-side BPF link object is also gone (cilium's
`link.NewFromID(id)` returns `os.ErrNotExist`).  Yet the program keeps
firing on every matching event.

Reproduced with three tracepoint programs attached to
`syscalls/sys_enter_kill`: detaching one link, then firing N more
kills, increments the detached program's counter by N.  Same
behaviour for kprobe/kretprobe attached to `do_unlinkat`.

## Root cause: order of operations during detach

The bug is **not** that bpfman leaks the link FD or fails to remove
the pin.  Both are done at attach time -- `pinWithRetry(lnk, …)`
followed by `lnk.Close()` -- which is the documented cilium pattern
and which works for a single program attached to a hook.  The bug is
that for **probe-style attachments where multiple BPF programs share
a kernel hook** (e.g. several tracepoint links on the same
`syscalls/sys_enter_kill` event), the cleanup sequence

  1. `lnk.Close()` (closes the BPF link FD and the perf_event FD)
  2. `os.Remove(linkPinPath)` (drops the last kernel reference)

does not run `perf_event_free_bpf_prog` for the released link's
program.  The kernel-side BPF link is destroyed (its ID disappears
from the link table) but the program stays attached to its perf_event
and keeps firing.

The reverse sequence

  1. `os.Remove(linkPinPath)` (drops the pin reference)
  2. `lnk.Close()` (drops the FD reference, link refcount hits zero
     while the kernel still considers the perf_event the link's
     "active" hook)

does the right thing: `bpf_perf_link_release` runs
`perf_event_free_bpf_prog`, the program is detached from the
perf_event, the perf_event's `perf_file` reference is dropped, and on
the last drop the perf_event itself is freed.

`cmd/repro` bisects this:  build with `make repro`, run
`sudo bin/repro -mode={A,B,C,D}`.  With three programs attached, only
mode D (`os.Remove` then `lnk.Close`) detaches `tp_c`; modes A, B, C
all leave it firing.  With one program (the original single-prog
form, replayed by editing the loop) modes B and D both work, which is
why the bug stayed hidden in the existing single-program tests.

The exact kernel mechanism for the order dependence has not been
chased into the kernel source -- empirically the order matters and
the fix is cheap, so it is enough to encode the working order in the
adapter.

## Why no existing test caught this

Two reasons compound:

1. Every pre-existing e2e test follows the pattern

   ```
   attach -> trigger workload -> assert counter > 0 -> detach -> unload
   ```

   with no workload between detach and unload.  Detach is verified
   by `ListLinks` (empty) and `GetLink` (not-found), both of which
   check bpfman's store, not the kernel.  Empirically the program
   keeps running, but nothing observes that.

2. The behavioural assertions are lower-bound (`require.Greater(t,
   count, uint64(0))` or `require.GreaterOrEqual`).  A lower-bound
   check is satisfied by "the program ran at least N times, possibly
   more" -- which is exactly what a still-attached program looks like
   from the outside.  Only an exact-equality assertion (`count ==
   events × weight`) makes the extra increments observable.

The multi-program load tests on the `multi-program-load` branch
discovered this when they were adapted to stagger detaches mid-
workload to produce distinct per-program event counts.  With test-
controlled weights and exact-equality assertions, the still-attached
program produced a wrong product, not a coincidentally-too-large
counter, and the failure surfaced.  `ListLinks` after each detach
correctly excluded the link and `link.NewFromID` returned `ENOENT`,
so both bpfman's store and the kernel link table agreed the link was
gone; only the program-on-hook state disagreed.

Lesson worth keeping: behavioural tests for "X stops happening"
(detach stops counting, gc stops scheduling, dispatcher stops
forwarding, …) need exact-equality assertions on a quantity the test
itself controls.  Lower-bound counter assertions silently admit
"still happening" as a passing case.

## Fix

Two pieces in `platform/ebpf/`:

1. `kernelAdapter` keeps a `liveLinks sync.Map` keyed by link pin
   path; the value is the live `*link.Link` returned by cilium at
   attach time.  Each probe-style attach path (`AttachTracepoint`,
   `AttachKprobe`, `attachTracing` for fentry/fexit, uprobe local)
   stores the link via `k.trackLink(linkPinPath, lnk)` after pinning
   instead of calling `lnk.Close()`.  Unpinned attaches still close
   immediately (no entry to track).

2. `DetachLink` performs the operations in the order that works:

   ```go
   if err := os.Remove(linkPinPath); err != nil {
       // (best-effort releaseLink for the missing-pin case)
       return …
   }
   if err := k.releaseLink(linkPinPath); err != nil {
       k.logger.Warn(…)
   }
   ```

`releaseLink` does `LoadAndDelete` on the map and calls `lnk.Close()`
on the entry.  The Close runs after the pin has been removed, which
is the order that triggers `perf_event_free_bpf_prog` for the
released program.

Pin-only detach (no entry in `liveLinks`) remains the correct
fallback for the out-of-process recovery path: the original attach
process has already exited, all FDs it held are closed, and the pin
is the only reference left.  `os.Remove` alone is sufficient there.

The dispatcher detach paths in `platform/ebpf/attach_tc.go` and
`platform/ebpf/attach_xdp.go` still close after pinning.  XDP and TC
go through dispatcher programs and netlink rather than per-program
perf_event attachments, so the failure mode (if any) differs.  Worth
exercising separately with the same staggered-detach + exact-equality
pattern before assuming they need the same fix.

## Reproducer

`cmd/repro/main.go`, built with `make repro`.  Loads three
tracepoint programs from
`e2e/testdata/bpf/multi_prog_tracepoint_counter.bpf.o`, attaches all
three to `syscalls/sys_enter_kill`, fires five SIGUSR1 to itself,
detaches `tp_c` using one of four sequences, fires five more
SIGUSR1, and reports whether `tp_c`'s counter stayed flat
(`DETACHED`) or kept incrementing (`STILL FIRING`).

```
=== mode A ===  lnk.Close() only                  STILL FIRING
=== mode B ===  lnk.Close() then os.Remove(pin)   STILL FIRING
=== mode C ===  os.Remove(pin) only               STILL FIRING
=== mode D ===  os.Remove(pin) then lnk.Close()   DETACHED
```

The reproducer pulls in cgo via a no-op `import "C"` so the static-
glibc Nix flake's external linker can resolve `runtime/cgo`; without
it the build fails with `loadinternal: cannot find runtime/cgo`.

## E2E coverage

`TestMultiProgTracepoint_LoadAttachDetachUnload` and
`TestMultiProgMixed_LoadAttachDetachUnload` in `e2e/e2e_test.go` now
stagger detaches across three workload waves (5/5/5 events) and
assert per-program counters with exact equality:

```
wave 1           -> all three programs see 5 events
detach prog[2]   -> wave 2 sees only programs 0 and 1
detach prog[1]   -> wave 3 sees only program 0

want: prog[0] = 15 * weight[0]
      prog[1] = 10 * weight[1]
      prog[2] =  5 * weight[2]
```

Without the fix, the detached programs see all 15 events, so their
counters land at `15 * weight[i]` instead of `10 * weight[i]` or `5 *
weight[i]`, and the assertion fails with a clear "expected X, got
1.5X" or "expected X, got 3X" mismatch.
