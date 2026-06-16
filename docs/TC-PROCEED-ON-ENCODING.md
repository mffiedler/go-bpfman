# TC proceed-on encoding and the TC_ACT_UNSPEC gap

This records how TC proceed-on bitmasks are encoded across the layers
(CLI -> store -> dispatcher .rodata -> kernel), why the current scheme
cannot represent `TC_ACT_UNSPEC`, and what a fix would have to change.
It is reference material for RUST-PARITY-REVIEW-2026-06-10.md finding 14;
no behaviour changes with this document.

## What proceed-on means

A TC dispatcher runs a chain of programs at one (netns, ifindex,
direction). After each member returns, the dispatcher decides whether to
keep walking the chain or stop and return that member's verdict.
proceed-on is the per-member set of return codes for which the chain
continues. The codes are the kernel `TC_ACT_*` values:

    unspec=-1  ok=0  reclassify=1  shot=2  pipe=3  stolen=4
    queued=5   repeat=6  redirect=7  trap=8

plus a bpfman sentinel, `dispatcher_return` (TC action code 30), which
the dispatcher itself returns when it falls off the end of the chain.

## How the kernel dispatcher consumes it

`dispatcher/bpf/tc_dispatcher.bpf.c` stores a per-slot mask in
`chain_call_actions[]` and, after calling each member, tests:

    if (!((1U << (ret + 1)) & CONFIG.chain_call_actions[i]))
        return ret;          // bit clear -> stop, return this verdict
    // bit set -> continue to the next member

Note the `ret + 1`. The dispatcher shifts by `ret + 1`, not `ret`,
specifically so that `TC_ACT_UNSPEC == -1` maps to a valid shift
amount (bit 0) rather than an undefined `1 << -1`. So in the bytes the
kernel actually reads, action `a` occupies bit `a + 1`:

    unspec(-1) -> bit 0
    ok(0)      -> bit 1
    pipe(3)    -> bit 4
    dispatcher_return(30) -> bit 31     (TC_DISPATCHER_RETVAL = 30)

This `a + 1` layout is the contract `chain_call_actions[]` must satisfy.
Both this implementation and Rust bpfman write identical bytes here;
the difference is only in how each arrives at them.

## How Go produces the mask

Go keeps a single internal representation, `slot.ProceedOn`, shared by
TC and XDP, and applies a dispatcher-specific shift only when it writes
the `.rodata`.

1. `tcProceedOnBitmask` (`manager/attach_tc.go`) turns the requested
   action codes into `slot.ProceedOn`, setting bit `a` for each action:

       for _, a := range actions {
           if a >= 0 && a < 32 {
               mask |= 1 << uint(a)
           }
       }

   So `ok(0)` -> bit 0, `pipe(3)` -> bit 3, `dispatcher_return(30)` ->
   bit 30. The constants `tcProceedOnOK = 1<<0`, `tcProceedOnPipe =
   1<<3`, `tcProceedOnDispatcherReturn = 1<<30` and `DefaultTCProceedOn`
   follow the same "bit `a`" convention.

2. When building the dispatcher config
   (`manager/executor_dispatcher.go`), the mask is shifted by
   `DispatcherType.ChainCallShift()` (`dispatcher/types.go`):

       cfg.ChainCallActions[i] = slot.ProceedOn << dispType.ChainCallShift()

   `ChainCallShift()` returns 1 for TC and 0 for XDP. So TC's bit `a`
   becomes bit `a + 1` in `chain_call_actions[]` -- exactly the layout
   the dispatcher expects. `dispatcher_return` at bit 30 shifts to bit
   31, matching `TC_DISPATCHER_RETVAL + 1`.

The `<< 1` is how Go reconciles its uniform "bit `a`" internal
representation with the dispatcher's `a + 1` contract, without giving TC
and XDP separate representations.

## How Rust produces the mask (for contrast)

Rust's `TcProceedOn::mask()` (`bpfman/src/types.rs`) folds the offset
into the representation directly:

    proceed_on_mask |= 1 << ((action as i32) + 1);

There is no later shift: Rust's stored mask is already in the `a + 1`
layout. `unspec(-1)` naturally lands on bit 0.

Both implementations therefore write byte-identical `chain_call_actions`
for any set of non-negative actions; the dispatcher .rodata is at
parity. They differ only in their in-memory/persisted representation
(Go: bit `a`, shifted at the edge; Rust: bit `a + 1`, stored directly).

## The gap: TC_ACT_UNSPEC is unrepresentable in Go

`unspec` needs `chain_call_actions` bit 0 (the kernel evaluates
`1 << (ret + 1)` = `1 << 0` when a member returns `TC_ACT_UNSPEC`). Go
cannot produce that bit:

- `tcProceedOnBitmask` discards `a < 0`, so `unspec(-1)` sets nothing in
  `slot.ProceedOn`.
- Even if it did not discard it, `slot.ProceedOn` is built from
  `1 << a`, and `a = -1` has no representable bit.
- And the final `slot.ProceedOn << 1` shifts everything up by one, so
  bit 0 of `chain_call_actions` is always zero regardless of
  `slot.ProceedOn`.

The "bit `a`, then `<< 1`" scheme structurally cannot set the one bit
`unspec` needs. The CLI nonetheless accepts `--proceed-on unspec`
(`TCActionUnspec` carries code -1 in `tc_action.go`): it parses, flows
through as -1, and is silently dropped. A chain member returning
`TC_ACT_UNSPEC` continues the chain under Rust and terminates it here,
with no error to the caller -- accepted but inert.

## Why XDP is not affected

XDP's dispatcher checks `1U << ret` directly (no `+ 1`), and XDP action
codes are all non-negative (`aborted=0 .. redirect=4`,
`dispatcher_return=31`). XDP has no `unspec`, so it needs no offset:
`ChainCallShift()` is 0 for XDP and `slot.ProceedOn` is written as-is
(with the dispatcher_return bit forced on). The gap is specific to TC.

## The fix

Move the offset into the representation, as Rust does, and stop applying
it at the edge. `slot.ProceedOn` (and the dispatcher member's
`ProceedOn`) becomes the `chain_call_actions[]` ABI layout itself, for
both TC and XDP. The bit for an action code `c` is:

    bit(c) = c + offset(dispType)      offset(TC) = 1, offset(XDP) = 0

This mirrors the dispatcher's check exactly (TC tests `1 << (ret + 1)`,
XDP tests `1 << ret`) and is byte-identical to Rust's `mask()`.

Exact bits:

| action                  | code | TC bit (offset 1) | XDP bit (offset 0) |
|-------------------------|------|-------------------|--------------------|
| `unspec`                |  -1  | `1<<0`            | n/a                |
| `ok` / `aborted`        |   0  | `1<<1`            | `1<<0`             |
| `pass` (XDP)            |   2  | --                | `1<<2`             |
| `pipe` (TC)             |   3  | `1<<4`            | --                 |
| `dispatcher_return` TC  |  30  | `1<<31`           | --                 |
| `dispatcher_return` XDP |  31  | --                | `1<<31`            |

TC default `[pipe, dispatcher_return]` is `(1<<4) | (1<<31)`; XDP default
`[pass]` is `1<<2`. Both match Rust.

### One offset source, two unified helpers

The per-type offset has a single home. `DispatcherType.ChainCallShift()`
(today TC=1/XDP=0, applied at the write) is repurposed into the
action-bit offset, now applied at encode/decode rather than at the write.
Two helpers in the `dispatcher` package own all code/bitmask conversion:

- `ProceedOnMask(dt, codes ...int32) uint32` -- `bit := c + offset(dt)`,
  guarded to `0 <= bit < 32`, `mask |= 1 << bit`. Replaces
  `manager.tcProceedOnBitmask` and the old `XDPAction`-typed
  `ProceedOnMask`.
- `ProceedOnActions(dt, mask uint32) []int32` -- for each set bit `b`,
  `append(b - offset(dt))`. Replaces `manager.bitmaskToActions`.

### Sites that change

1. **TC encode** (`manager/attach_tc.go`): `tcProceedOnBitmask` ->
   `dispatcher.ProceedOnMask(dt, codes...)`. Delete the hand-coded
   `tcProceedOnOK/Pipe/DispatcherReturn` bit constants; derive
   `DefaultTCProceedOn` from the `[pipe, dispatcher_return]` codes via the
   encoder.
2. **XDP encode** (`manager/attach_xdp.go`): switch to the code-based
   `ProceedOnMask(DispatcherTypeXDP, codes...)` (offset 0, byte-identical
   to today).
3. **Write** (`manager/executor_dispatcher.go`): TC becomes
   `cfg.ChainCallActions[i] = slot.ProceedOn` (no shift); XDP keeps
   `| (1 << 31)` to force `dispatcher_return`.
4. **Decode for link details** (`executor_dispatcher.go`, XDP and TC
   rebuild paths): `bitmaskToActions(...)` ->
   `dispatcher.ProceedOnActions(dt, ...)`.
5. **Persist** (`platform/store/sqlite/dispatchers.go`): `proceedOnToJSON`
   and the read-path rebuild loop take the dispatcher type (available as
   `key.Type`) and use `ProceedOnActions` / `ProceedOnMask`.
6. Delete `DispatcherType.ChainCallShift()` once the offset moves to
   encode/decode, and update its test.

### No migration

The store persists proceed-on as an action-code array (`[]int32`), never
the bitmask: link details carry codes directly, and the dispatcher member
is walked to codes by `proceedOnToJSON` before storage and rebuilt from
codes on read. Codes are encoding-agnostic, so changing the bit layout
needs no migration and no version marker -- only the four conversion
sites (1, 2, 4, 5) have to apply the offset consistently.

### Validation

`ProceedOnMask` guards `0 <= c + offset < 32`. TC codes are `-1..8` and
`30` (bits `0..9, 31`); XDP codes are `0..4` and `31`. Everything fits,
and the old `a >= 0` filter that silently ate `unspec` is gone.

### Tests, to the bit

- dispatcher unit: `ProceedOnMask(TC, -1) == 1<<0`, `(TC, 0) == 1<<1`,
  `(TC, 3) == 1<<4`, `(TC, 30) == 1<<31`, default `== (1<<4)|(1<<31)`;
  `(XDP, 2) == 1<<2` (regression); round-trip
  `ProceedOnActions(dt, ProceedOnMask(dt, codes)) == codes`, including
  `-1`. These expected masks equal Rust's `mask()`.
- executor: build a TC config with `--proceed-on unspec` and assert the
  generated `cfg.ChainCallActions[i]` has bit 0 set and equals the
  expected mask -- the assertion is on the array the kernel reads, not on
  parsed actions.
- persistence round-trip: attach TC with `unspec`, reload the snapshot,
  assert both the rebuilt mask (bit 0) and the persisted `[]int32`
  contain `-1`.
- e2e: `--proceed-on unspec` on a TC chain; dump the dispatcher `.rodata`
  and assert bit 0; assert `link get` reports `unspec`.

The XDP path runs at offset 0 throughout, so its bytes are unchanged; the
unit regressions pin that.
