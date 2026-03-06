# TC Dispatcher E2E Test Gaps

Gaps in e2e coverage for the TC dispatcher, ranked by likelihood of
catching a real bug. Each entry identifies the untested code path and
why it matters.

## 1. Egress direction

All TC dispatcher tests use `TCDirectionIngress`. Egress uses a
different netlink parent handle (`HANDLE_MIN_EGRESS`), a different
dispatcher type (`DispatcherTypeTCEgress`), and different bpffs paths.
A wiring bug in any of those would be invisible today.

**Test:** Load a program, attach to egress, send outbound traffic
through a veth pair, verify the program's stats map shows non-zero
packet counts.

## 2. Slot reuse after detach

We test filling all 10 slots and we test detaching from a full
dispatcher, but we never detach a slot then attach a new program to
verify `findFreeSlot` reclaims the gap correctly and the runtime
config is recomputed with the new occupant in the right priority
position.

**Test:** Fill slots 0-9, detach the program in slot 3, attach a new
program. Assert it occupies slot 3 and the runtime config run_order
reflects its priority relative to the remaining nine.

## 3. Dispatcher lifecycle after last extension removed

`cleanupEmptyDispatcher` has a distinct code path: it queries
`FindTCFilterHandle`, issues `DetachTCFilter`, removes pins, and
deletes the dispatcher from the store. No test verifies that after
removing the last extension the dispatcher is actually gone, and that
a subsequent attach creates a fresh one.

**Test:** Attach one program, detach it, confirm the dispatcher is
absent from the store. Attach again, confirm a new dispatcher is
created.

## 4. Ingress and egress on the same interface

Two dispatchers coexist keyed by `(nsid, ifindex, direction)`. No
test confirms they are independent -- that detaching all egress links
does not disturb the ingress dispatcher.

**Test:** Attach to both ingress and egress on the same interface.
Detach all egress links. Verify the ingress dispatcher config is
unchanged.

## 5. Multiple interfaces with the same program

Dispatchers are keyed by ifindex. No test attaches the same program
to two interfaces and verifies independent dispatcher state.

**Test:** Create two dummy interfaces, attach the same program to
ingress on both, detach from one, verify the other is unaffected.

## 6. Priority ties

The config sorts slots by `(priority, program_name)` as a secondary
key. No test exercises the secondary sort or verifies deterministic
ordering when priorities collide.

**Test:** Load two distinctly-named programs, attach both at the same
priority, read the runtime config, assert the run_order matches
alphabetical program-name ordering.

## 7. Detach from the middle of a full dispatcher

`TestTC_DispatcherConfigRecomputedOnDetach` detaches only the first
link (lowest priority). No test detaches from the middle of the
priority ordering and verifies the remaining programs' run_order is
correct.

**Test:** Fill all 10 slots with ascending priorities, detach the
program at priority 400 (slot 4), assert the config has 9 programs
enabled and the run_order skips the vacated slot.

## 8. Config map double-buffer flip

`UpdateDispatcherConfig` flips the active buffer index on every
attach or detach. No test reads the active map before and after an
operation to verify the index actually toggles.

**Test:** Attach one program, read the active index, attach a second
program, read again, assert the index changed.
