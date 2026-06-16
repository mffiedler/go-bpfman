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

## What a fix would have to change

The clean, parity-matching fix is to move the offset into the
representation, as Rust does, and stop relying on the edge shift:

- `tcProceedOnBitmask`: set bit `a + 1` for each action, including
  `unspec(-1)` -> bit 0; drop the `a >= 0` filter (keep an upper bound).
- `tcProceedOnOK/Pipe/DispatcherReturn` and `DefaultTCProceedOn`: move to
  the `a + 1` layout (ok -> 1<<1, pipe -> 1<<4, dispatcher_return ->
  1<<31).
- `DispatcherType.ChainCallShift()` for TC: becomes 0 (the offset is now
  in the mask, not the shift).
- The reverse mapping `bitmaskToActions` (`manager/executor_dispatcher.go`)
  is shared with XDP and assumes bit `a`. TC would need a TC-specific
  reverse that subtracts 1, or the helper must be parameterised by
  dispatcher type. This is the main entanglement to design around.

Two consequences to weigh:

- **Persisted encoding changes.** `slot.ProceedOn` is stored in sqlite;
  under the fix its meaning shifts by one. Pre-release this is a clean
  break, but any on-disk record written before the change would read
  back wrong.
- **Validation.** The change should be proven against the actual
  dispatcher `.rodata`: load a TC chain with `--proceed-on unspec`, dump
  `chain_call_actions`, and confirm bit 0 is set (and that every other
  action still lands where Rust puts it).

Alternatives considered: keep the `<< 1` scheme and special-case
`unspec` by reserving a marker bit and OR-ing bit 0 in after the shift
(works, but reintroduces the per-action special case the uniform
representation was meant to avoid); or reject `--proceed-on unspec` at
the boundary with a clear error instead of supporting it (honest, small,
but a deliberate divergence from Rust, which supports it).

This is deliberately out of scope for the attach-kind / load-type
integrity work: it changes the dispatcher encoding and the persisted
mask, an axis unrelated to which verb may attach which program.
