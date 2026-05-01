# Confirmed: bpfman's DetachLink for perf-event link types is inherently async

## Problem

The e2e suite has tests that assert two stronger properties than
the bpfman `DetachLink` API actually guarantees:

1. **Post-detach quiescence (singletons via `assertCounterQuiet`):**
   immediately after `env.Detach` returns, a workload burst should
   not advance the program's counter map.
2. **Staggered-detach exact-equality (multi-prog):** after detaching
   one program in a same-hook chain, subsequent workload waves should
   not be counted by the just-detached program; the per-program
   counter should equal `events_seen × weight` exactly.

Both properties hold for `kernel_supports_BPF_LINK_DETACH(link_type)`
==
`{XDP, TCX, cgroup, netfilter, netkit, struct_ops, sockmap}`. They
do **not** hold for perf-event / tracing link types (kprobe,
kretprobe, uprobe, uretprobe, tracepoint, fentry, fexit). Failures
under load:

```
mup_b: events=10 weight=W want=10×W got=15×W
```

`mup_b` was detached after wave 2; wave 3's 5 events were nevertheless
counted. The over-count is precisely one full wave, suggesting the
program was *still hooked* during wave 3 rather than partially
hooked or racing per-event.

## Mechanism (kernel-source confirmed, v6.12)

For pinned bpf-perf-link types, `perf_event_detach_bpf_prog` --
the only function that removes the BPF program from the
trace_event's `prog_array` so it stops firing -- runs from
`bpf_link_free` on the deferred path. Two compounding deferrals:

**1. Pin's ref drop is RCU-deferred.** The bpffs super_ops
attaches `free_inode = bpf_free_inode`, which calls
`bpf_any_put -> bpf_link_put` (the deferred variant, which
unconditionally `schedule_work`s, never sync). VFS schedules
`free_inode` via `call_rcu(&inode->i_rcu, i_callback)` from
`evict()` -- so even after `os.Remove` returns to userspace, the
inode lives until the next RCU grace period ends.

**2. Closing the link FD only runs `bpf_link_free` synchronously
when it drops the LAST ref.** `bpf_link_release` (file_op) calls
`bpf_link_put_direct`, which is sync but only frees on
`atomic64_dec_and_test == 0`. With pin still holding a ref, FD
close goes 2 -> 1 and returns. The pin's ref drops asynchronously
later, and *that* drop is the one that hits zero -- routed
through the deferred `bpf_link_put` (workqueue), so
`bpf_link_free -> bpf_perf_link_release ->
perf_event_detach_bpf_prog` runs in kworker context, not in any
syscall context bpfman owned.

For pinned bpf-perf-link, no order of (close FD, unpin) makes the
FD close be the last ref drop. The pin's drop is intrinsically
RCU-deferred via the inode-free path. **There is no userspace
primitive for synchronous detach of a pinned bpf-perf-link on a
tracing event.**

For HARDWARE/SOFTWARE perf events the BPF prog is `event->prog`
(overflow handler), and `PERF_EVENT_IOC_DISABLE` would do the
job. Tracing events (kprobe/uprobe/tracepoint) instead dispatch
via `tp_event->prog_array` walked from inside the tracepoint hit;
disabling our own perf_event does not remove the program from
the shared array. So `IOC_DISABLE` is the wrong primitive for
the link types we care about, and was tried and reverted.

`BPF_LINK_DETACH` ioctl works for XDP/TCX/cgroup/netfilter/etc.
because their `link->ops->detach` hooks into the active surface
synchronously, independent of refcount. `bpf_perf_link_lops` has
no `.detach` op, so the ioctl returns `EOPNOTSUPP`.

```
DetachLink(linkPinPath)
  -> link.Detach()                 # EOPNOTSUPP for perf-event types
  -> os.Remove(linkPinPath)        # vfs_unlink -> evict() -> call_rcu(i_callback)
                                   # pin's bpf_link_put fires only after RCU GP
                                   # AND then via schedule_work (workqueue)
  -> link.Close()                  # bpf_link_release -> bpf_link_put_direct
                                   # refcount 2 -> 1, no free
DetachLink returns
                                   # tens of ms later: RCU GP ends ->
                                   # bpf_free_inode -> bpf_link_put ->
                                   # workqueue -> bpf_link_free ->
                                   # bpf_perf_link_release ->
                                   # perf_event_detach_bpf_prog ->
                                   # rcu_assign_pointer(prog_array, new)
event fires (workload syscall)
  -> kprobe/uprobe handler
  -> walks the OLD prog_array, runs our BPF prog
  -> counter map increments
```

## Evidence

- **Kernel source (v6.12)** confirms the mechanism above:
  `bpf_link_put` unconditionally `schedule_work`s; only
  `bpf_link_put_direct` (called from `bpf_link_release` file_op
  and from `BPF_LINK_DETACH` ioctl) is sync; `bpf_free_inode` is
  the bpffs `free_inode` super_op which VFS dispatches via
  `call_rcu` in `evict()`. See `kernel/bpf/syscall.c`,
  `kernel/bpf/inode.c`, `fs/inode.c`.
- **bpftrace histogram on this host** running the failing suite
  shows `bpf_link_free` is invoked **2833 times in `kworker`
  context vs 65 times in `e2e.test` context (97.5% deferred)**,
  matching the deferred-path prediction exactly.
- **Detach internal duration correlates inversely with leak.**
  Instrumented `TestMultiProgUprobe`/`TestMultiProgKretprobe`
  show: when `env.Detach` takes ~80ms internally, leaks
  zero events; when it takes ~10ms, leaks the full 5-event
  wave. The kernel deferral budget (RCU GP + workqueue) is
  fixed; the test's wall-clock spent inside Detach either
  covers it or doesn't.
- **arm64 CI fails consistently** on these tests; x86 CI passes
  more often. arm64 GitHub runner kernel is
  `6.14.0-1017-azure`; the local x86 box is `6.12.82-NixOS`.
  RCU GP timing varies across kernels and runners.
- **x86 reproduces the same failures locally** at sufficient
  iteration count. `sudo -E ./bin/e2e.test -test.count=100
  -test.failfast` reliably surfaces the same shape. Building
  without `-race` (`make NORACE=1 bin/e2e.test`) surfaces it
  more aggressively because the race detector's overhead
  inflates `env.Detach` past the deferral budget.
- **Failure shape is consistent:** over-count is precisely one
  workload wave (`5 × weight` for the singleton burst,
  `5 × weight` per missing-detach in multi-prog). The program
  was *still hooked* for the entire burst, not partially hooked
  per-event.
- **TCX failures cleared once we routed through `Link.Detach()`
  via `LoadPinnedLink`** (commit `66f46a9`). TCX is in the
  Detach-supported set; `BPF_LINK_DETACH` -> `link->ops->detach`
  is genuinely synchronous. The same change does *not* clear
  perf-event-type failures because `Detach()` returns
  `EOPNOTSUPP` for those.
- **Earlier `f3dacb6` (Retry XDP attach on EBUSY after pin
  removal)** is a sibling instance of the same async-teardown
  pattern at the symmetric attach side.

## What we have tried

| Attempt | Outcome | Commit |
|---|---|---|
| Order: unpin-then-close | Fixes x86 same-hook multi-prog regression; insufficient on arm64 | 1459c0b |
| `Link.Detach()` for tracked links | Reaches uprobe / tracing paths only; misses TCX/XDP (closed FD after pin) | eb74ae8 |
| `LoadPinnedLink` + `Detach()` | Catches TCX/XDP/cgroup/etc. via fresh FD; perf-event types still EOPNOTSUPP | 66f46a9 |
| Fixed `time.Sleep` after detach (50ms) | Made arm64 CI green at the cost of bodge-shaped commit; whatever number we pick is wrong eventually | f983ac2 (reverted in 66f46a9 follow-up) |
| Conditional skip on arm64 in `assertCounterQuiet` | Made arm64 singleton CI green; multi-prog tests still failed; bodge | aa26c33 (also reverted) |

Of these, only the first three are kernel-API correct. The
sleeps and conditional skips trade test signal for CI-green;
they don't change the underlying property.

## What we have not tried

- **A poll-until-stable userspace primitive.** Drive a probe
  event, observe whether the counter advances; loop until two
  consecutive probes don't advance the counter; declare quiet.
  Avoids the "fixed-duration is wrong eventually" problem
  because it scales with kernel teardown timing. Open question:
  the probe events themselves drive sibling programs' counters
  in multi-prog tests; need a probe that doesn't pollute or an
  accounting strategy that absorbs the wait-phase events.
- **bpftrace instrumentation locally** to measure
  `DetachLink-return -> bpf_link_release-finishes` interval and
  identify the deferral mechanism (RCU grace, workqueue,
  perf-event refcount).
- **`bpftool prog show ID <id>` polling** as a userspace-
  observable signal that the program has been fully reclaimed.
  May or may not correlate with "won't fire on next event"; the
  program object can outlive its perf-event attachment.
- **A kernel patch** exposing `BPF_LINK_DETACH` for perf-event
  link types. Upstream territory; not a near-term option.

## Position

The bpfman `DetachLink` API for perf-event link types is
*inherently async* under any realistic implementation strategy:
the kernel doesn't expose a synchronous teardown, and userspace
cannot add one without polling for an effect. The correct
treatment is one of:

1. **Document the API as eventual rather than immediate** for
   perf-event link types, and shape tests around that. Tests
   that need synchronous semantics use a poll-based wait
   primitive (option above) and pay the latency.
2. **Add an opt-in `DetachAndWait` to bpfman itself** that polls
   internally and exposes synchronous semantics to callers who
   need them. Tests use this; production paths use plain
   `Detach`. Cost: implementation in the manager + interpreter
   layers.

(1) is lower-cost and follows the Linux kernel philosophy
("syscalls return promptly, callers handle async cleanup").
(2) is more user-friendly but a bigger lift.

Either is more defensible than picking a fixed sleep.

## Local reproduction recipe

```sh
make bin/e2e.test
sudo -E ./bin/e2e.test -test.count=100 -test.failfast
```

Reliably surfaces the bug on x86 6.12. arm64 surfaces it at
much lower count. No external load required.

## Open questions

- What's the actual deferral mechanism (RCU? workqueue? perf
  refcount)? bpftrace can answer.
- Is there a userspace-observable signal that perf-event link
  release has finished? Candidates: program/link IDs in
  `bpftool prog/link show`, perf event references, tracefs.
- Would `synchronize_rcu()`-equivalent from userspace (e.g.
  reading `/sys/kernel/debug/sched/...` or running a no-op
  syscall that crosses an RCU quiescent state) shorten the
  poll loop?
