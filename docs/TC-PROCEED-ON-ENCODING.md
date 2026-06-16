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
For TC's non-negative actions under the current Go encoding, Go and Rust
produce the same final dispatcher bits; `unspec` is the exception
(unrepresentable in Go today), and explicit XDP sets that omit
`dispatcher_return` also differ -- both described below.

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
   31, matching `TC_DISPATCHER_RETVAL + 1`. For XDP the shift is 0, so
   `slot.ProceedOn` is written unshifted -- but the executor also ORs in
   the XDP `dispatcher_return` bit (`| (1 << 31)`) at this write, so
   XDP's stored mask omits a bit the kernel actually sees.

The `<< 1` is how Go reconciles its uniform "bit `a`" internal
representation with the dispatcher's `a + 1` contract, without giving TC
and XDP separate representations.

## How Rust produces the mask (for contrast)

Rust stores the dispatcher ABI bits directly and writes them verbatim,
with no edge shift and no forced bit. Read off the code, not the
comments (`bpfman/src/types.rs`, `bpfman/src/multiprog/`):

- TC `TcProceedOn::mask()`: `proceed_on_mask |= 1 << ((action as i32) +
  1)`. The `TcProceedOnEntry` discriminants are the kernel `TC_ACT_*`
  values (`Unspec = -1`, `Ok = 0`, ... `Trap = 8`) plus
  `DispatcherReturn = 30`, so `unspec` lands on bit 0 and
  `dispatcher_return` on bit 31.
- XDP `XdpProceedOn::mask()`: `proceed_on_mask |= 1 << (action as u32)`.
  `XdpProceedOnEntry` is `Aborted = 0 .. Redirect = 4` and
  `DispatcherReturn = 31`, so `pass` lands on bit 2 and
  `dispatcher_return` on bit 31.
- Both dispatchers assign the mask straight into `chain_call_actions`
  (`multiprog/xdp.rs`, `multiprog/tc.rs`:
  `chain_call_actions[pos] = ...mask()`) -- no extra OR at the write.
- An empty `--proceed-on` is stored as no entries; on read, collecting
  zero entries yields the type's `Default` -- `[pass, dispatcher_return]`
  for XDP, `[pipe, dispatcher_return]` for TC. So `dispatcher_return` is
  an ordinary (default) entry in the mask, not a value injected at the
  write. An explicit `--proceed-on pass` stores and writes exactly
  `1 << 2`.

These bit positions follow from the kernel dispatcher contract alone (TC
tests `1 << (ret + 1)` with `TC_DISPATCHER_RETVAL = 30`; XDP tests
`1 << ret` with `XDP_DISPATCHER_RETVAL = 31`), so they can be inferred
without trusting any comment. Go's `tc_action.go` / `xdp_action.go`
action codes already match these discriminants; only the bit-mapping and
the forced/edge handling differ.

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

## Why the unspec gap is TC-specific

The `unspec` representability problem is TC-only: XDP's dispatcher checks
`1 << ret` with no `+ 1` and XDP has no negative action, so offset 0
suffices and `unspec` never arises.

XDP does carry a separate divergence on the same code path, though. Go's
manager fallback for an empty proceed-on stores `[pass]` and then forces
`dispatcher_return` unconditionally at the write
(`slot.ProceedOn | (1 << 31)`); Rust stores `[pass, dispatcher_return]`
and writes the mask verbatim. (Go's CLI and shell already default
`--proceed-on` to `pass,dispatcher_return`, so the stored-default gap
shows only on the fallback path -- chiefly a gRPC request that omits
proceed_on.) The forced write OR applies on every path, so for any
explicit `--proceed-on` set that omits `dispatcher_return` Rust honours
the omission while Go silently adds bit 31. Matching Rust closes both
divergences with one model, so the fix below covers XDP as well as TC.

## The fix

Make `slot.ProceedOn` (and the dispatcher member's `ProceedOn`) the
final `chain_call_actions[]` ABI mask for both TC and XDP, exactly as
Rust's `mask()` does, and write it verbatim -- no edge shift, no forced
bit. The bit for an action code `c` is:

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

Defaults are stored in the mask, not forced at the write, and match
Rust: TC `[pipe, dispatcher_return]` is `(1<<4) | (1<<31)`; XDP
`[pass, dispatcher_return]` is `(1<<2) | (1<<31)`.

### One offset source, two unified helpers

The per-type offset has a single home. `DispatcherType.ChainCallShift()`
(today TC=1/XDP=0, applied at the write) is repurposed into the
action-bit offset, now applied at encode/decode rather than at the write.
Two helpers in the `dispatcher` package own all code/bitmask conversion:

- `ProceedOnMask(dt, codes ...int32) (uint32, error)` -- for each code,
  first validate that `c` is a known action for `dt`, then compute
  `bit := c + offset(dt)`. An unknown action code, or a resulting `bit`
  outside `[0, 32)`, returns an error, never a silently dropped bit and
  never a panic. Replaces `manager.tcProceedOnBitmask` and the old
  `XDPAction`-typed `ProceedOnMask`.
- `ProceedOnActions(dt, mask uint32) ([]int32, error)` -- for each set
  bit `b`, `code := b - offset(dt)`; a `code` that is not a known action
  for `dt` returns an error rather than a bogus code. Decode is **not**
  total over the domain action sets: with TC offset 1, bits `0..9` and
  `31` are valid (codes `-1..8`, `30`) but bits `10..30` are not; with
  XDP offset 0, bits `0..4` and `31` are valid but bits `5..30` are not.
  Rejecting the invalid bits keeps a corrupt store row or a stray bit
  from being reported or re-persisted as a real action (e.g. TC bit 10 as
  code 9). Replaces `manager.bitmaskToActions`.

### Sites that change

1. **TC encode** (`manager/attach_tc.go`): `tcProceedOnBitmask` ->
   `dispatcher.ProceedOnMask(dt, codes...)`. Delete the hand-coded
   `tcProceedOnOK/Pipe/DispatcherReturn` bit constants; derive
   `DefaultTCProceedOn` from the `[pipe, dispatcher_return]` codes via the
   encoder.
2. **XDP encode** (`manager/attach_xdp.go`): `ProceedOnMask(
   DispatcherTypeXDP, codes...)`, and the empty-input default becomes
   `[pass, dispatcher_return]` (today it is `[pass]`).
3. **Write** (`manager/executor_dispatcher.go`): both TC and XDP become
   `cfg.ChainCallActions[i] = slot.ProceedOn`. Remove the TC
   `<< ChainCallShift()` and the XDP `| (1 << 31)`.
4. **Decode for link details** (`executor_dispatcher.go`, XDP and TC
   rebuild paths): `bitmaskToActions(...)` ->
   `dispatcher.ProceedOnActions(dt, ...)`, propagating its error up the
   rebuild path (which already returns errors).
5. **Persist** (`platform/store/sqlite/dispatchers.go`): `proceedOnToJSON`
   and the read-path rebuild loop take the dispatcher type (available as
   `key.Type`) and use `ProceedOnActions` / `ProceedOnMask`, surfacing
   their errors (a stored code array with an invalid action, or a decoded
   mask with invalid bits, fails rather than reporting a bogus code).
6. Delete `DispatcherType.ChainCallShift()` once the offset moves to
   encode/decode, and update its test.
7. **Validate at the spec boundary, and remove the raw setter** (domain,
   not the gRPC handler). The no-error `WithProceedOn([]int32)` on both
   specs is the bypass to close: a direct library caller can push
   unvalidated codes through it today. Replace it so proceed-on enters a
   spec only two ways -- `WithProceedOnActions([]TCAction/[]XDPAction)`
   (validated by type; used by CLI/shell) and a new error-returning
   `WithProceedOnCodes([]int32) (Spec, error)` that parses each code via
   `TCActionFromInt32` / the new `XDPActionFromInt32`. The gRPC handler
   uses `WithProceedOnCodes` and surfaces the error. With no public
   unvalidated setter, no front-end -- nor a future direct caller -- can
   reach the manager with an invalid code. The CLI/shell already pass
   validated names; the gRPC handler becomes a thin transport that
   performs this parse and surfaces the error. Placing the check here, not
   in `server/attach.go`, keeps it once the gRPC layer is removed.

### No migration

The store persists proceed-on as an action-code array (`[]int32`), never
the bitmask: link details carry codes directly, and the dispatcher member
is walked to codes by `proceedOnToJSON` before storage and rebuilt from
codes on read. Codes are encoding-agnostic, so changing the bit layout
needs no migration and no version marker -- only the four conversion
sites (1, 2, 4, 5) have to apply the offset consistently.

### Behavioural deltas

- TC `--proceed-on unspec` becomes representable (bit 0) instead of being
  accepted and silently dropped.
- TC and XDP default emitted bytes are unchanged: `(1<<4)|(1<<31)` and
  `(1<<2)|(1<<31)`.
- An explicit XDP `--proceed-on` set that omits `dispatcher_return` no
  longer has bit 31 forced on -- it is written exactly as requested,
  matching Rust.
- On the manager-fallback path (an empty proceed-on, e.g. a gRPC request
  that omits it) `link get` / list now report `dispatcher_return` in the
  XDP set, because it is stored in the mask rather than injected at the
  write. CLI and shell already reported it, since they pass
  `pass,dispatcher_return` explicitly.

### Validation

TC codes are `-1..8` and `30` (bits `0..9, 31`); XDP codes are `0..4` and
`31`. The old `a >= 0` filter that silently ate `unspec` is gone.

Invalid action codes are rejected, not silently masked, matching Rust's
`from_int32s` (which errors before `mask()`). The check lives in the
durable layer -- the attach-spec construction that every front-end
crosses -- not in the gRPC handler, because the gRPC server is
transitional and will eventually be removed; a check placed there would
not survive a future CLI that calls the library directly. Concretely: the
CLI and shell already parse names through `ParseTCAction` /
`ParseXDPActions`, and a raw `[]int32` from a proto request is parsed
through a code-validating step that rejects unknown codes when the spec
is built (`TCActionFromInt32` already exists; the symmetric
`XDPActionFromInt32` is added here), so the manager only ever sees valid
codes regardless of front-end.
`ProceedOnMask` returns an error for an unknown action code or an
out-of-range bit rather than masking it away (and never panics -- this
codebase always surfaces a return error). On already-validated input that
error is unreachable, but the path exists so an invalid code can never
silently become a wrong mask. Boundary tests cover unknown in-range codes
and an out-of-range code being rejected rather than dropped.

This follows the same rule as the attach-kind guard, which lives in
`Manager.Attach`, not the server: semantic checks belong in the library
that outlives the gRPC layer. The server stays a thin transport that
parses wire values into validated domain types and maps the resulting
domain error onto a status code.

### Tests, to the bit

- dispatcher unit (both helpers return a value and an error). Encode,
  asserting `(mask, nil)`: `ProceedOnMask(TC, -1)` -> `1<<0`, `(TC, 0)` ->
  `1<<1`, `(TC, 3)` -> `1<<4`, `(TC, 30)` -> `1<<31`, TC default ->
  `(1<<4)|(1<<31)`; `ProceedOnMask(XDP, 2)` -> `1<<2`, `(XDP, 31)` ->
  `1<<31`, XDP default -> `(1<<2)|(1<<31)`. These expected masks equal
  Rust's `mask()`. Round-trip in two steps (the helpers cannot nest now
  that both return errors): `m, err := ProceedOnMask(dt, codes)` then
  `got, err := ProceedOnActions(dt, m)`, asserting both errors nil and
  `got == codes`, including `-1` for TC. Error cases:
  `ProceedOnMask(TC, 9)`, `ProceedOnMask(XDP, 5)`, and
  `ProceedOnMask(TC, 99)` return errors (not dropped bits), and
  `ProceedOnActions(TC, 1<<10)` returns an error (bit 10 -> code 9 is not
  a TC action), as does `ProceedOnActions(XDP, 1<<5)`.
- executor: build TC and XDP configs and assert the generated
  `cfg.ChainCallActions[i]` equals `slot.ProceedOn` exactly -- in
  particular TC `unspec` sets bit 0, and an explicit XDP `[pass]` writes
  `1<<2` with no forced bit 31. The assertion is on the array the kernel
  reads, not on parsed actions.
- persistence round-trip (TC and XDP, since bit 31 moves from a
  write-time injection into the stored/rebuilt mask):
  - TC `unspec`: the rebuilt mask has bit 0 and the persisted `[]int32`
    contains `-1`.
  - XDP empty/default: persists and reports `[pass, dispatcher_return]`
    and rebuilds `(1<<2)|(1<<31)`.
  - XDP explicit `[pass]`: persists and reports `[pass]` and rebuilds
    `1<<2`, with no bit 31.
- e2e: anchor the real dispatcher ABI with one TC and one XDP case. For
  TC, attach `--proceed-on unspec`, dump the dispatcher `.rodata`, assert
  bit 0, and assert `link get` reports `unspec`. For XDP, attach an
  explicit `--proceed-on pass`, dump `.rodata`, and assert the mask is
  exactly `1<<2`, with no forced bit 31.
