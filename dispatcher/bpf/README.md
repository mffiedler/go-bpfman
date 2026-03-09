# Dispatcher BPF Programs

This directory contains BPF C source for the XDP and TC dispatchers.
These programs solve a fundamental constraint: Linux allows only one
XDP or TC program per interface per direction. The dispatcher occupies
that single slot and internally chains calls to up to 10 user programs.

## File inventory

| File | Version | Config mechanism | Status |
|---|---|---|---|
| `xdp_dispatcher_v1.bpf.c` | XDP v1 | `.rodata` constants | From Rust bpfman |
| `xdp_dispatcher_v2.bpf.c` | XDP v2 | `.rodata` constants | From Rust bpfman |
| `xdp_dispatcher_v3.bpf.c` | XDP v3 | Double-buffered BPF maps | POC |
| `tc_dispatcher.bpf.c` | TC v1 | `.rodata` constants | From Rust bpfman |
| `tc_dispatcher_v2.bpf.c` | TC v2 | Double-buffered BPF maps | POC |

XDP v1/v2 and TC v1 are derived from the Rust bpfman implementation.
XDP v3 and TC v2 are proof-of-concept dispatchers that replace
`.rodata` configuration with double-buffered BPF maps, avoiding the
need to reload the dispatcher when programs are added or removed.

## Motivation for v3

The original dispatchers (XDP v1/v2, TC v1) store all configuration --
`num_progs_enabled`, `chain_call_actions`, `run_prios` -- in
`volatile const` `.rodata` structs. The kernel freezes `.rodata` at
load time, which enables dead code elimination but makes configuration
immutable. Any change to the set of attached programs (add, remove,
reorder) requires rebuilding the entire dispatcher: loading a fresh
program with new `.rodata`, re-attaching every extension via freplace,
atomically swapping the old dispatcher for the new one, and cleaning
up the old revision.

The v3 POC explores whether these issues can be sidestepped by
keeping the dispatcher program loaded for as long as it has attached
extensions and controlling execution order at runtime through BPF
maps. The dispatcher is deleted when its last extension is detached.
In this model, adding or removing a program is a map write followed
by a single `uint32` flip, with no dispatcher reload or swap.

A trade-off is that the `.rodata` dispatchers benefit from dead code
elimination: the kernel verifier knows `num_progs_enabled` at load
time and can prune unreachable slots entirely. The map-based
dispatchers cannot -- all 10 slots are present in the verified
program regardless of how many are actually in use.

Whether this approach (double-buffering) is sound in the general case
is an open question -- there may be good reasons for the full-rebuild
approach that are not yet apparent to me.

## How it works

### Stub functions and freplace

Each dispatcher defines 10 stub functions (`prog0` through `prog9`),
each marked `__attribute__((noinline))`. A stub returns a sentinel
value (`XDP_DISPATCHER_RETVAL = 31` or `TC_DISPATCHER_RETVAL = 30`)
and is never called in practice -- it exists solely as a replacement
target. When a user program is attached to a slot, the kernel's
`freplace` (function replacement) mechanism swaps the stub for the
user's BPF program. The `if (!ctx) return ABORTED` guard prevents the
compiler from optimising the stub away.

A `call_slot()` inline helper maps a slot number to the corresponding
`progN` call via a switch statement:

```c
static __always_inline int call_slot(struct xdp_md *ctx, __u32 slot) {
    switch (slot) {
    case 0: return prog0(ctx);
    case 1: return prog1(ctx);
    ...
    case 9: return prog9(ctx);
    default: return XDP_PASS;
    }
}
```

The original `.rodata` dispatchers use an unrolled linear sequence of
direct calls. The v3 dispatch loop is driven by a runtime `run_order`
array, so it uses a switch to map a runtime slot index to the
corresponding `progN` function. There may be a cheaper way to
implement `run_order` dispatch in BPF.

### Double-buffered runtime configuration

The v3/v2 dispatchers use two BPF maps for lock-free runtime
reconfiguration:

**`dispatcher_config`** -- a `BPF_MAP_TYPE_ARRAY` with 2 entries. Each
entry is a `dispatcher_runtime` struct:

```c
struct dispatcher_runtime {
    __u32 num_progs_enabled;
    __u32 run_order[10];           // slot indices in execution order
    __u32 chain_call_actions[10];  // per-slot proceed-on bitmask
};
```

**`active_config`** -- a `BPF_MAP_TYPE_ARRAY` with 1 entry: a `uint32`
that is either 0 or 1, pointing to the active buffer in
`dispatcher_config`.

To update the configuration, the control plane:

1. Reads the current active index (e.g. 0).
2. Writes the new `dispatcher_runtime` to the inactive buffer (1).
3. Writes 1 to `active_config[0]`, atomically flipping the active
   buffer.

Packets in flight continue reading the old buffer until they
complete. New packets pick up the new configuration.

The `run_order` array decouples execution order from physical slot
position. Physical slots are stable: they are allocated by
`findFreeSlot` (lowest unoccupied) and freed on detach. When
programs are added or removed, the control plane computes a new
`RuntimeConfig` -- sorting occupied slots by
`(priority, program_name)` -- and writes it to the inactive buffer.
Flipping the active index causes subsequent packets to execute
programs in priority order, with no locking required.

### The dispatch loop

On each packet, the dispatcher reads the active configuration and
iterates the `run_order` array:

```c
SEC("xdp")
int xdp_dispatcher(struct xdp_md *ctx) {
    __u32 key = 0;
    __u32 *gen = bpf_map_lookup_elem(&active_config, &key);
    __u32 active_idx = *gen;
    struct dispatcher_runtime *cfg =
        bpf_map_lookup_elem(&dispatcher_config, &active_idx);

    for (int i = 0; i < MAX_DISPATCHER_ACTIONS; i++) {
        if (i >= cfg->num_progs_enabled)
            break;
        __u32 slot = cfg->run_order[i];
        int ret = call_slot(ctx, slot);
        if (!((1U << ret) & cfg->chain_call_actions[slot]))
            return ret;   // chain terminated
    }
    return XDP_PASS;
}
```

### Chain call actions (proceed-on)

Each slot has a `chain_call_actions` bitmask that controls whether the
chain continues after that slot's program returns. If the bit
corresponding to the return code is set, the dispatcher proceeds to
the next program. If the bit is not set, the dispatcher returns
immediately with that return code.

For example, with XDP:

- `chain_call_actions[slot] = 0x04` (bit 2 set) means "proceed if the
  program returns `XDP_PASS` (2), terminate otherwise".
- `chain_call_actions[slot] = 0x06` (bits 1 and 2 set) means "proceed
  on `XDP_DROP` (1) or `XDP_PASS` (2)".

If all programs in the chain pass through, the dispatcher returns
`XDP_PASS` (XDP) or `TC_ACT_OK` (TC) as the default.

## XDP vs TC differences

| Aspect | XDP (v3) | TC (v2) |
|---|---|---|
| Context type | `struct xdp_md *ctx` | `struct __sk_buff *skb` |
| Default return | `XDP_PASS` | `TC_ACT_OK` |
| Stub sentinel | `XDP_DISPATCHER_RETVAL` (31) | `TC_DISPATCHER_RETVAL` (30) |
| Stub error return | `XDP_ABORTED` | `TC_ACT_UNSPEC` |
| Chain call check | `(1U << ret)` | `(1U << (ret + 1))` |
| `.rodata` metadata | Retained for xdp-tools compat | Eliminated entirely |
| Section name | `SEC("xdp")` | `SEC("classifier/dispatcher")` |

### The TC return code shift

TC return codes start at -1 (`TC_ACT_UNSPEC`), which cannot be used as
a bit shift directly. The TC dispatcher therefore checks
`(1U << (ret + 1))` instead of `(1U << ret)`, shifting all return
codes up by 1 to make them non-negative. The Go control plane accounts
for this via `DispatcherType.ChainCallShift()`: TC bitmasks are shifted
left by 1 before being written to the map.

## XDP metadata (.rodata)

The XDP v3 dispatcher retains a `.rodata` section with
`struct xdp_dispatcher_conf` for compatibility with xdp-tools/libxdp.
This struct carries load-time metadata (magic number 236, dispatcher
version 2, `is_xdp_frags`, `program_flags`, `run_prios`) that is
injected by the Go loader before the program is loaded into the
kernel. The dispatch loop does not read from `.rodata` at runtime --
it is purely metadata.

The TC v2 dispatcher has no `.rodata` at all; TC has no equivalent
metadata convention.

## The compat_test function

Both XDP and TC dispatchers include a `compat_test()` stub function.
This exists as an `freplace` target for `xdp_multiprog__check_compat()`
in libxdp. It is unreachable at runtime (guarded by
`num_progs_enabled < 11`, which is always true since the maximum is
10).

## Go integration

The compiled `.bpf.o` files are embedded into the Go `dispatcher`
package via `//go:embed` directives. The Go package provides:

- `LoadXDPDispatcherV3(cfg)` -- loads the XDP v3 ELF spec, injecting
  `.rodata` metadata
- `LoadTCDispatcherV2()` -- loads the TC v2 ELF spec (no `.rodata`
  injection needed)
- `RuntimeConfig` -- Go mirror of `struct dispatcher_runtime`
- `SlotName(position)` -- returns `"prog0"` through `"prog9"` for
  freplace target naming

The control plane (in `platform/ebpf/`) handles:

1. Loading the dispatcher into the kernel and attaching it to the
   interface (BPF link for XDP, netlink filter for TC).
2. Pinning the program, link, and maps to bpffs.
3. Loading user programs as `BPF_PROG_TYPE_EXT` extensions and
   attaching them via `link.AttachFreplace()`.
4. Computing the `RuntimeConfig` from occupied slots sorted by
   priority, then performing the double-buffer flip.

The dispatcher is not reloaded during normal attach/detach
operations. The only code path that recreates a dispatcher is
`attachExtensionWithRetry` in `manager/executor_dispatcher.go`: if
the extension attach fails with `os.ErrNotExist` (indicating a stale
pin, typically after a bpffs remount), it deletes the dispatcher
from the store, creates a fresh one, and retries the attach once.
